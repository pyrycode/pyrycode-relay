package relay

import (
	"context"
	"sync"
	"time"
)

const (
	pushFeedbackPerOrigin    = 128
	pushFeedbackPerJob       = 64
	pushFeedbackGlobal       = 2048
	pushFeedbackLifetime     = 60 * time.Second
	pushFeedbackWriteTimeout = 5 * time.Second
)

// This capability is deliberately separate from Conn and binarySource: old
// request callers and forwarding fixtures remain source-compatible.
type pushResultWriter interface {
	pushResultContext() context.Context
	sendPushResult(context.Context, []byte) error
}

type pushWakeOrigin struct {
	ctx    context.Context
	writer pushResultWriter
	wg     sync.WaitGroup
}

type pushWakeFeedbackKey struct {
	origin *pushWakeOrigin
	id     string
}

// One record remains charged through pending, queued and seen states. Its job
// pointer survives cache eviction while the original delivery is in progress.
type pushWakeFeedback struct {
	key      pushWakeFeedbackKey
	job      *pushWakeJob
	expires  time.Time
	delivery time.Time
	ctx      context.Context
	cancel   context.CancelFunc
}

func (w *PushWaker) bindFeedbackLocked(job *pushWakeJob, origin *pushWakeOrigin, id string) {
	if origin == nil || id == "" || origin.ctx.Err() != nil {
		return
	}
	key := pushWakeFeedbackKey{origin: origin, id: id}
	if job.feedback[key] != nil || len(job.feedback) >= pushFeedbackPerJob ||
		w.feedbackOrigins[origin] >= pushFeedbackPerOrigin || w.feedbackCount >= pushFeedbackGlobal {
		return
	}
	now := w.jobNow()
	expires := now.Add(pushFeedbackLifetime)
	ctx, cancel := context.WithDeadline(w.ctx, expires)
	r := &pushWakeFeedback{key: key, job: job, expires: expires, delivery: now.Add(pushFeedbackWriteTimeout), ctx: ctx, cancel: cancel}
	job.feedback[key] = r
	w.feedbackOrigins[origin]++
	w.feedbackCount++
	w.wg.Add(1)
	origin.wg.Add(1)
	stop := context.AfterFunc(origin.ctx, cancel)
	go w.runFeedback(r, stop)
}

// Called under mu for controlled-clock checks as well as the ordinary sweep.
// The real context deadline enforces expiry independently of that sweep.
func (w *PushWaker) expireFeedbackLocked(now time.Time) {
	for _, job := range w.jobs {
		for _, r := range job.feedback {
			if !now.Before(r.expires) {
				r.cancel()
			}
		}
	}
}

func (w *PushWaker) runFeedback(r *pushWakeFeedback, stop func() bool) {
	defer w.wg.Done()
	defer r.key.origin.wg.Done()
	defer func() {
		stop()
		r.cancel()
		w.mu.Lock()
		delete(r.job.feedback, r.key)
		w.feedbackCount--
		w.feedbackOrigins[r.key.origin]--
		if w.feedbackOrigins[r.key.origin] == 0 {
			delete(w.feedbackOrigins, r.key.origin)
		}
		w.mu.Unlock()
	}()
	select {
	case <-r.ctx.Done():
		return
	case <-r.job.done:
	}
	w.mu.Lock()
	invalid := r.job.invalid && !w.closed && r.ctx.Err() == nil &&
		r.key.origin.ctx.Err() == nil && w.jobNow().Before(r.expires)
	deadline := r.delivery
	w.mu.Unlock()
	if invalid {
		ctx, cancel := context.WithDeadline(r.ctx, deadline)
		// The ID's ASCII allowlist makes direct fixed-shape encoding safe. No
		// token, provider text or inner frame is accessible to this writer.
		if ctx.Err() == nil && r.key.origin.ctx.Err() == nil {
			_ = r.key.origin.writer.sendPushResult(ctx, []byte(`{"push_wake_result":{"request_id":"`+r.key.id+`","code":"invalid_token"}}`))
		}
		cancel()
	}
	// A failed attempt is still seen. Eviction releases dedup metadata only
	// after the bounded write returns, preserving all original charges.
	timer := time.NewTimer(time.Until(r.job.until))
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		return
	case <-r.job.evicted:
		return
	case <-timer.C:
		w.mu.Lock()
		w.expireJobsLocked(w.now())
		w.mu.Unlock()
	}
	select {
	case <-r.ctx.Done():
	case <-r.job.evicted:
	}
}
