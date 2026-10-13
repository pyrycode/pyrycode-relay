package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	// pushWakeRefillEvery and pushWakeBurst rate-limit wakes per server-id.
	// Wakes are human-paced (a finished turn, a waiting permission prompt)
	// for a handful of paired devices: a burst of 6 covers both events for
	// three devices back to back, and one wake per 10s sustained is far
	// above that while bounding how fast one binary can push at Google.
	pushWakeRefillEvery = 10 * time.Second
	pushWakeBurst       = 6
	// Device budgets are shared by every daemon targeting the complete token.
	pushWakeDeviceRefillEvery = 10 * time.Second
	pushWakeDeviceBurst       = 6
	pushWakeCoalesceWindow    = 10 * time.Second
	pushWakeJobTimeout        = 45 * time.Second
	pushWakeMaxAttempts       = 3
	// Global attempt rate is distinct from concurrent sends: completed or
	// failed sends do not return tokens to this allowance.
	pushWakeGlobalRefillEvery = time.Second
	pushWakeGlobalBurst       = 32
	// Keyed accounting expires only after idle state has fully refilled.
	pushWakeEvictionInterval = 5 * time.Minute
	pushWakeMaxBuckets       = 1024 // independently for daemons and devices
	// pushWakeMaxInFlight caps retained device jobs, including completed jobs
	// inside their coalescing window, and concurrent sends across the relay.
	// Each job holds one goroutine through its bounded retry lifetime;
	// the cap bounds goroutines and outbound sockets however
	// many server-ids a set of binaries claims.
	pushWakeMaxInFlight = 32
)

var (
	// ErrPushOff is returned when push is not configured or the waker is
	// closed.
	ErrPushOff = errors.New("relay: push is off")
	// ErrPushWakeRateLimited is returned when any wake allowance is depleted.
	// The wake is dropped, not queued.
	ErrPushWakeRateLimited = errors.New("relay: push_wake rate-limited")
	// ErrPushWakeStateCap refuses a new accounting key at retained capacity.
	ErrPushWakeStateCap = errors.New("relay: push_wake accounting capacity reached")
	// ErrPushWakeInFlightCap is returned when pushWakeMaxInFlight sends are
	// already running. The wake is dropped, not queued.
	ErrPushWakeInFlightCap = errors.New("relay: push_wake in-flight cap reached")
)

// pushWakeOutcome is the closed set of push wake outcomes the metrics hook
// counts. NewPushMetrics maps each to a hard-coded label value.
type pushWakeOutcome int

const (
	pushWakeSent             pushWakeOutcome = iota // 2xx reply, with or without a message id
	pushWakeUnregistered                            // FCM errorCode UNREGISTERED
	pushWakeSendFailed                              // any other send failure, including cancel by Close
	pushWakeTokenFetchFailed                        // access-token fetch failed
	pushWakeDropped                                 // refused by rate, state capacity or in-flight cap
	pushWakeOutcomeCount
)

// classifyPushWake maps a Send result to its outcome by error identity.
func classifyPushWake(err error) pushWakeOutcome {
	switch {
	case err == nil:
		return pushWakeSent
	case errors.Is(err, ErrFCMUnregistered):
		return pushWakeUnregistered
	case errors.Is(err, ErrFCMTokenFetch):
		return pushWakeTokenFetchFailed
	default:
		return pushWakeSendFailed
	}
}

// pushSender is the send capability PushWaker needs. Send returns a
// validated message id on success, or "". *FCMSender satisfies it; tests
// substitute a fake.
type pushSender interface {
	Send(ctx context.Context, deviceToken string) (string, error)
}

// PushWaker admits wake requests from binaries and sends each one off the
// binary read loop, under daemon, device and global allowances and a
// relay-wide retained-job cap. Same-device requests coalesce for a fixed window
// or until the send ends, whichever is later. A nil *PushWaker means push is off.
// Safe for concurrent use.
type PushWaker struct {
	sender pushSender
	slots  chan struct{}
	logger *slog.Logger

	refillEvery time.Duration // daemon allowance; device/global use fixed limits
	burst       int
	daemons     map[[sha256.Size]byte]*pushWakeBucket
	devices     map[[sha256.Size]byte]*pushWakeBucket
	global      pushWakeBucket
	jobs        map[[sha256.Size]byte]*pushWakeJob
	now         func() time.Time // called only under mu
	jobNow      func() time.Time // scheduling clock, called only under mu
	wait        func(context.Context, time.Duration) bool
	jitter      func() time.Duration

	// onOutcome counts outcomes; nil is a no-op. Set once at boot by
	// setOutcomeHook. It must not call back into the waker.
	onOutcome func(pushWakeOutcome)

	ctx    context.Context
	cancel context.CancelFunc

	// mu owns all accounting and makes admission atomic with Close.
	// No wg.Add can race wg.Wait after the closed transition.
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	sweepWG sync.WaitGroup
}

// Completed jobs retain only bounded metadata, never a raw device token.
type pushWakeJob struct {
	until  time.Time
	active bool
}

type pushWakeBucket struct {
	tokens     int
	refillLast time.Time
	pollLast   time.Time // every request, including refusals
}

// refill preserves the remainder of elapsed time while capping credit at burst.
func (b *pushWakeBucket) refill(now time.Time, every time.Duration, burst int) {
	steps := now.Sub(b.refillLast) / every
	if steps <= 0 {
		return
	}
	if steps >= time.Duration(burst-b.tokens) {
		b.tokens = burst
	} else {
		b.tokens += int(steps)
	}
	b.refillLast = b.refillLast.Add(steps * every)
}

// NewPushWaker builds a waker around sender with the production limits.
// Close must be called to stop sweeping and cancel in-flight sends.
func NewPushWaker(sender pushSender, logger *slog.Logger) *PushWaker {
	return newPushWaker(sender, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight, logger)
}

func newPushWaker(sender pushSender, refillEvery time.Duration, burst, maxInFlight int, logger *slog.Logger) *PushWaker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &PushWaker{
		sender: sender, slots: make(chan struct{}, maxInFlight), logger: logger,
		refillEvery: refillEvery, burst: burst,
		daemons: make(map[[sha256.Size]byte]*pushWakeBucket),
		devices: make(map[[sha256.Size]byte]*pushWakeBucket),
		jobs:    make(map[[sha256.Size]byte]*pushWakeJob),
		global:  pushWakeBucket{tokens: pushWakeGlobalBurst, refillLast: time.Now()},
		now:     time.Now, jobNow: time.Now, wait: waitPushRetry,
		jitter: func() time.Duration { return time.Duration(rand.Int64N(int64(time.Second) + 1)) }, ctx: ctx, cancel: cancel,
	}
	w.sweepWG.Add(1)
	go w.sweepLoop()
	return w
}

func (w *PushWaker) sweepLoop() {
	defer w.sweepWG.Done()
	ticker := time.NewTicker(pushWakeEvictionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.sweep()
		}
	}
}

func (w *PushWaker) sweep() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepLocked(w.now())
}

func (w *PushWaker) sweepLocked(now time.Time) {
	w.expireJobsLocked(now)
	w.evictIdle(w.daemons, now, w.refillEvery, w.burst)
	w.evictIdle(w.devices, now, pushWakeDeviceRefillEvery, pushWakeDeviceBurst)
}

func (w *PushWaker) expireJobsLocked(now time.Time) {
	for key, job := range w.jobs {
		if !job.active && !now.Before(job.until) {
			delete(w.jobs, key)
		}
	}
}

func (w *PushWaker) evictIdle(buckets map[[sha256.Size]byte]*pushWakeBucket, now time.Time, every time.Duration, burst int) {
	for key, b := range buckets {
		if now.Sub(b.pollLast) < pushWakeEvictionInterval {
			continue
		}
		b.refill(now, every, burst)
		// Never erase debt, even with slow test limits or capacity pressure.
		if b.tokens == burst {
			delete(buckets, key)
		}
	}
}

// requestedBucket touches existing state even if a different limit refuses.
// Missing state remains temporary until all admission checks succeed.
func requestedBucket(buckets map[[sha256.Size]byte]*pushWakeBucket, key [sha256.Size]byte, now time.Time, every time.Duration, burst int) *pushWakeBucket {
	if b := buckets[key]; b != nil {
		b.refill(now, every, burst)
		b.pollLast = now
		return b
	}
	return &pushWakeBucket{tokens: burst, refillLast: now, pollLast: now}
}

// setOutcomeHook installs the outcome hook. Call once at boot, before any
// Request; concurrent calls during serving are undefined.
func (w *PushWaker) setOutcomeHook(fn func(pushWakeOutcome)) {
	w.onOutcome = fn
}

func (w *PushWaker) record(o pushWakeOutcome) {
	if w.onOutcome != nil {
		w.onOutcome(o)
	}
}

// NewPushWakerFromEnv builds a waker backed by FCM from
// PYRYCODE_RELAY_FCM_CREDENTIALS. It returns (nil, nil) when the variable
// is unset: push is off. CheckEnvConfig has already validated a set value,
// so an error here is unexpected; it never carries the credentials.
func NewPushWakerFromEnv(lookup func(string) (string, bool), logger *slog.Logger) (*PushWaker, error) {
	creds, ok := lookup(envFCMCredentials)
	if !ok {
		return nil, nil
	}
	sender, err := NewFCMSender([]byte(creds))
	if err != nil {
		return nil, fmt.Errorf("building fcm sender: %w", err)
	}
	return NewPushWaker(sender, logger), nil
}

// Request validates the raw push_wake object from serverID's binary and,
// if admitted, sends the wake on its own goroutine. It never blocks on the
// send and never queues: a wake over any limit is refused with an error
// for the caller to log. A duplicate of an active or unexpired device job
// returns nil without charging or extending that job. No error carries the token.
func (w *PushWaker) Request(serverID string, raw json.RawMessage) error {
	if w == nil {
		return ErrPushOff
	}
	wake, err := parsePushWake(raw)
	if err != nil {
		return err
	}
	// Full digests keep accounting keys bounded without retaining raw tokens
	// or conflating complete tokens through the truncated log fingerprint.
	daemonKey := sha256.Sum256([]byte(serverID))
	deviceKey := sha256.Sum256([]byte(wake.Token))

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrPushOff
	}
	now := w.now()
	if job := w.jobs[deviceKey]; job != nil && (job.active || now.Before(job.until)) {
		return nil
	}
	w.expireJobsLocked(now)
	if len(w.jobs) >= cap(w.slots) {
		w.record(pushWakeDropped)
		return ErrPushWakeInFlightCap
	}
	if err := w.chargeAttemptLocked(daemonKey, deviceKey, now, true); err != nil {
		return err
	}
	deadline := w.jobNow().Add(pushWakeJobTimeout)
	w.jobs[deviceKey] = &pushWakeJob{until: now.Add(pushWakeCoalesceWindow), active: true}
	w.wg.Add(1)
	go w.send(serverID, wake.Token, deviceKey, deadline)
	return nil
}

// chargeAttemptLocked commits all three allowances together or records one drop.
func (w *PushWaker) chargeAttemptLocked(daemonKey, deviceKey [sha256.Size]byte, now time.Time, reserveSlot bool) error {
	daemon := requestedBucket(w.daemons, daemonKey, now, w.refillEvery, w.burst)
	device := requestedBucket(w.devices, deviceKey, now, pushWakeDeviceRefillEvery, pushWakeDeviceBurst)
	w.global.refill(now, pushWakeGlobalRefillEvery, pushWakeGlobalBurst)
	newDaemon, newDevice := w.daemons[daemonKey] == nil, w.devices[deviceKey] == nil
	if newDaemon || newDevice {
		w.sweepLocked(now)
	}
	if (newDaemon && len(w.daemons) >= pushWakeMaxBuckets) || (newDevice && len(w.devices) >= pushWakeMaxBuckets) {
		w.record(pushWakeDropped)
		return ErrPushWakeStateCap
	}
	if daemon.tokens == 0 || device.tokens == 0 || w.global.tokens == 0 {
		w.record(pushWakeDropped)
		return ErrPushWakeRateLimited
	}
	if reserveSlot {
		select {
		case w.slots <- struct{}{}:
		default:
			w.record(pushWakeDropped)
			return ErrPushWakeInFlightCap
		}
	}
	w.daemons[daemonKey], w.devices[deviceKey] = daemon, device
	daemon.tokens--
	device.tokens--
	w.global.tokens--
	return nil
}

func waitPushRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// send owns one bounded device job, logging/counting each actual invocation.
// Its reserved slot bounds active work through backoff as well as delivery.
func (w *PushWaker) send(serverID, token string, deviceKey [sha256.Size]byte, deadline time.Time) {
	defer w.wg.Done()
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		<-w.slots
		job := w.jobs[deviceKey]
		job.active = false
		if !w.now().Before(job.until) {
			delete(w.jobs, deviceKey)
		}
	}()
	ctx, cancel := context.WithDeadline(w.ctx, deadline)
	defer cancel()
	for attempt := 1; attempt <= pushWakeMaxAttempts; attempt++ {
		start := time.Now()
		id, err := w.sender.Send(ctx, token)
		duration := time.Since(start)
		w.mu.Lock()
		completion := w.jobNow()
		w.mu.Unlock()
		w.record(classifyPushWake(err))
		fp := tokenFingerprint(token)
		switch {
		case err != nil:
			// FCMSender's errors carry at most an HTTP status and FCM's enum
			// reason codes, never the token or free text.
			w.logger.Warn("push_wake_send_failed",
				"server_id", serverID,
				"token_fp", fp,
				"duration", duration,
				"err", err)
		case id != "":
			w.logger.Info("push_wake_sent",
				"server_id", serverID,
				"token_fp", fp,
				"duration", duration,
				"fcm_message_id", id)
		default:
			w.logger.Info("push_wake_sent",
				"server_id", serverID,
				"token_fp", fp,
				"duration", duration)
		}
		var failure *fcmRetryError
		if attempt == pushWakeMaxAttempts || ctx.Err() != nil || errors.Is(err, ErrFCMUnregistered) || !errors.As(err, &failure) || !failure.retry {
			return
		}
		w.mu.Lock()
		now := w.jobNow()
		w.mu.Unlock()
		next := completion.Add(time.Duration(attempt)*10*time.Second + w.jitter())
		if failure.notBefore.After(next) {
			next = failure.notBefore
		}
		if !next.Before(deadline) || !w.wait(ctx, next.Sub(now)) {
			return
		}
		w.mu.Lock()
		if w.closed || ctx.Err() != nil || !w.jobNow().Before(deadline) {
			w.mu.Unlock()
			return
		}
		err = w.chargeAttemptLocked(sha256.Sum256([]byte(serverID)), deviceKey, w.now(), false)
		w.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// Close refuses further wakes, cancels in-flight sends, waits for them to
// return, stops sweeping and releases retained jobs. Concurrent callers
// also join admitted work.
func (w *PushWaker) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()

	w.cancel()
	w.wg.Wait()
	w.sweepWG.Wait()
	w.mu.Lock()
	w.jobs = nil
	w.mu.Unlock()
}
