package relay

import (
	"sync"
	"time"
)

// IPRateLimiter is a per-source-IP token-bucket rate limiter with bounded
// memory under address-space scanning. Safe for concurrent use; Close()
// stops the background eviction goroutine and is idempotent.
//
// The limiter is the primitive only — it does not extract IPs from requests
// and does not produce responses. The wiring ticket composes it with an
// IP-extraction helper and the upgrade-handler middleware.
//
// Two timestamps per bucket decouple two concerns: refillLast anchors
// integer-token refill math (advances in token-sized chunks, preserving
// fractional elapsed time); pollLast anchors the last Allow call (advances
// to now on every call, including denies). Separating them lets eviction
// drop genuinely-idle buckets without dropping a bucket that an attacker is
// actively hammering — eviction keyed on pollLast keeps actively-polled
// buckets resident even when their tokens stay at 0.
type IPRateLimiter struct {
	refillEvery time.Duration
	burst       int

	mu      sync.Mutex
	buckets map[string]*ipBucket

	now       func() time.Time
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type ipBucket struct {
	tokens     int
	refillLast time.Time
	pollLast   time.Time
}

// NewIPRateLimiter builds a limiter that refills one token every refillEvery,
// up to burst tokens per bucket. It launches a single eviction goroutine that
// sweeps idle buckets every evictionInterval. Close() must be called to stop
// the goroutine (e.g. on process shutdown or test cleanup).
//
// refillEvery and burst must be positive; evictionInterval must be positive
// (passing zero or a negative duration panics via time.NewTicker). Out-of-
// bound values are a wiring bug — the limiter does not validate them.
func NewIPRateLimiter(refillEvery time.Duration, burst int, evictionInterval time.Duration) *IPRateLimiter {
	l := &IPRateLimiter{
		refillEvery: refillEvery,
		burst:       burst,
		buckets:     make(map[string]*ipBucket),
		now:         time.Now,
		done:        make(chan struct{}),
	}
	l.wg.Add(1)
	go l.evictLoop(evictionInterval)
	return l
}

// Allow decrements the bucket for ip and returns whether the attempt is
// permitted. The first Allow for an ip starts the bucket at burst tokens
// and consumes one. An empty ip is treated as any other map key — callers
// wanting to deny on empty-string must do so before calling Allow.
func (l *IPRateLimiter) Allow(ip string) bool {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &ipBucket{
			tokens:     l.burst - 1,
			refillLast: now,
			pollLast:   now,
		}
		return true
	}

	// Integer-token refill. Advance refillLast by the integer number of
	// tokens credited (not to now) so the fractional remainder of elapsed
	// time is preserved for the next call.
	elapsed := now.Sub(b.refillLast)
	if elapsed > 0 {
		tokensAdd := int(elapsed / l.refillEvery)
		if tokensAdd > 0 {
			b.tokens += tokensAdd
			if b.tokens > l.burst {
				b.tokens = l.burst
			}
			b.refillLast = b.refillLast.Add(time.Duration(tokensAdd) * l.refillEvery)
		}
	}
	// pollLast advances on every call, including denies — load-bearing for
	// the eviction invariant (keeps actively-hammered buckets resident).
	b.pollLast = now

	if b.tokens > 0 {
		b.tokens--
		return true
	}
	return false
}

// Close stops the background eviction goroutine and blocks until it has
// returned. Idempotent: a second concurrent caller also blocks until the
// goroutine exits. After Close, Allow must not be called (not enforced at
// runtime — review-enforced at the wiring site).
func (l *IPRateLimiter) Close() {
	l.closeOnce.Do(func() { close(l.done) })
	l.wg.Wait()
}

func (l *IPRateLimiter) evictLoop(interval time.Duration) {
	defer l.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.sweep()
		}
	}
}

// sweep drops buckets whose pollLast is at or beyond burst*refillEvery in the
// past. Invariant: an evicted bucket would have refilled to capacity (proof
// in the spec) — eviction never drops a bucket below capacity. Unexported
// but in-package so tests can drive it synchronously under a fake clock.
func (l *IPRateLimiter) sweep() {
	now := l.now()
	threshold := time.Duration(l.burst) * l.refillEvery
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if now.Sub(b.pollLast) >= threshold {
			delete(l.buckets, ip)
		}
	}
}
