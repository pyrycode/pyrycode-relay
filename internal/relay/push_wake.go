package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// pushWakeEvictionInterval sweeps idle server-id buckets so the
	// limiter's memory stays bounded as server-ids come and go.
	pushWakeEvictionInterval = 5 * time.Minute
	// pushWakeMaxInFlight caps concurrent sends across the whole relay.
	// Each send holds one goroutine for at most two fcmSendTimeout-bounded
	// HTTP calls; the cap bounds goroutines and outbound sockets however
	// many server-ids a set of binaries claims.
	pushWakeMaxInFlight = 32
)

var (
	// ErrPushOff is returned when push is not configured or the waker is
	// closed.
	ErrPushOff = errors.New("relay: push is off")
	// ErrPushWakeRateLimited is returned when a server-id is over its
	// wake rate. The wake is dropped, not queued.
	ErrPushWakeRateLimited = errors.New("relay: push_wake rate-limited for server-id")
	// ErrPushWakeInFlightCap is returned when pushWakeMaxInFlight sends are
	// already running. The wake is dropped, not queued.
	ErrPushWakeInFlightCap = errors.New("relay: push_wake in-flight cap reached")
)

// pushSender is the send capability PushWaker needs. Send returns a
// validated message id on success, or "". *FCMSender satisfies it; tests
// substitute a fake.
type pushSender interface {
	Send(ctx context.Context, deviceToken string) (string, error)
}

// PushWaker admits wake requests from binaries and sends each one off the
// binary read loop, under a per-server-id rate and a relay-wide in-flight
// cap. A nil *PushWaker means push is off. Safe for concurrent use.
type PushWaker struct {
	sender  pushSender
	limiter *IPRateLimiter
	slots   chan struct{}
	logger  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	// mu makes admission atomic with Close: no wg.Add races wg.Wait, and
	// limiter.Allow is never called after limiter.Close.
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// NewPushWaker builds a waker around sender with the production limits.
// Close must be called to stop the limiter and cancel in-flight sends.
func NewPushWaker(sender pushSender, logger *slog.Logger) *PushWaker {
	return newPushWaker(sender, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight, logger)
}

func newPushWaker(sender pushSender, refillEvery time.Duration, burst, maxInFlight int, logger *slog.Logger) *PushWaker {
	ctx, cancel := context.WithCancel(context.Background())
	return &PushWaker{
		sender:  sender,
		limiter: NewIPRateLimiter(refillEvery, burst, pushWakeEvictionInterval),
		slots:   make(chan struct{}, maxInFlight),
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
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
// send and never queues: a wake over either limit is refused with an error
// for the caller to log. No returned error carries the token.
func (w *PushWaker) Request(serverID string, raw json.RawMessage) error {
	if w == nil {
		return ErrPushOff
	}
	wake, err := parsePushWake(raw)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrPushOff
	}
	if !w.limiter.Allow(serverID) {
		return ErrPushWakeRateLimited
	}
	select {
	case w.slots <- struct{}{}:
	default:
		return ErrPushWakeInFlightCap
	}
	w.wg.Add(1)
	go w.send(serverID, wake.Token)
	return nil
}

// send runs one wake and logs exactly one outcome line for it, keyed by a
// fingerprint of the token, never the token. It exits when Send returns,
// which the sender's own timeout and the waker's context (cancelled by
// Close) both bound.
func (w *PushWaker) send(serverID, token string) {
	defer w.wg.Done()
	defer func() { <-w.slots }()
	start := time.Now()
	id, err := w.sender.Send(w.ctx, token)
	duration := time.Since(start)
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
}

// Close refuses further wakes, cancels in-flight sends, waits for them to
// return and stops the limiter. Idempotent.
func (w *PushWaker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()

	w.cancel()
	w.wg.Wait()
	w.limiter.Close()
}
