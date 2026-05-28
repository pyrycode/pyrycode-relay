package relay

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a goroutine-safe wall-clock stand-in. Tests assign its Now
// method to IPRateLimiter.now (the unexported field is accessible because
// these tests live in package relay), then drive time forward with Advance.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestLimiter constructs a limiter wired to a fake clock with a long
// eviction interval (the goroutine effectively never fires during the test).
// Tests that need to exercise eviction call sweep() directly under the lock.
func newTestLimiter(t *testing.T, refillEvery time.Duration, burst int) (*IPRateLimiter, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	l := NewIPRateLimiter(refillEvery, burst, time.Hour)
	l.now = clk.Now
	t.Cleanup(l.Close)
	return l, clk
}

func TestIPRateLimiter_BurstThenDeny(t *testing.T) {
	t.Parallel()
	l, _ := newTestLimiter(t, time.Second, 3)

	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("Allow call %d: got false, want true (within burst)", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("Allow after burst exhausted: got true, want false")
	}
}

func TestIPRateLimiter_RefillAfterAdvance(t *testing.T) {
	t.Parallel()
	l, clk := newTestLimiter(t, time.Second, 2)

	// Exhaust the burst at t=0.
	burst1 := l.Allow("a")
	burst2 := l.Allow("a")
	if !burst1 || !burst2 {
		t.Fatal("burst Allows: got false, want true")
	}
	if l.Allow("a") {
		t.Fatal("Allow after exhaust at t=0: got true, want false")
	}

	// Advance by exactly one refill window — one token credited.
	clk.Advance(time.Second)
	if !l.Allow("a") {
		t.Fatal("Allow after 1s advance: got false, want true (one token refilled)")
	}
	if l.Allow("a") {
		t.Fatal("second Allow after 1s advance: got true, want false (only one token refilled)")
	}

	// Advance by 2s (cap clamps at burst=2) — two tokens, third denies.
	clk.Advance(2 * time.Second)
	refill1 := l.Allow("a")
	refill2 := l.Allow("a")
	if !refill1 || !refill2 {
		t.Fatal("two Allows after 2s advance: expected both true (burst cap)")
	}
	if l.Allow("a") {
		t.Fatal("third Allow after 2s advance: got true, want false (cap at burst)")
	}
}

// TestIPRateLimiter_FractionalRefillPreserved asserts that the integer-token
// refill math advances refillLast by `tokensAdd * refillEvery` rather than to
// `now`, so the fractional remainder of elapsed time is preserved across calls.
func TestIPRateLimiter_FractionalRefillPreserved(t *testing.T) {
	t.Parallel()
	l, clk := newTestLimiter(t, time.Second, 2)

	burst1 := l.Allow("a")
	burst2 := l.Allow("a")
	if !burst1 || !burst2 {
		t.Fatal("burst Allows: got false, want true")
	}
	if l.Allow("a") {
		t.Fatal("Allow after exhaust: got true, want false")
	}

	// 500ms in — no integer token yet. The poll updates pollLast but does
	// NOT advance refillLast (no token credited), so the next 500ms still
	// composes with the first to credit a token at t=1s.
	clk.Advance(500 * time.Millisecond)
	if l.Allow("a") {
		t.Fatal("Allow at +500ms: got true, want false (no integer token credited)")
	}

	// Another 500ms — cumulative 1s since exhaust. Exactly one token now.
	clk.Advance(500 * time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("Allow at +1s: got false, want true (one token credited)")
	}
}

func TestIPRateLimiter_PerIPIsolation(t *testing.T) {
	t.Parallel()
	l, _ := newTestLimiter(t, time.Second, 1)

	if !l.Allow("a") {
		t.Fatal("Allow(a) #1: got false, want true")
	}
	if l.Allow("a") {
		t.Fatal("Allow(a) #2: got true, want false")
	}
	if !l.Allow("b") {
		t.Fatal("Allow(b): got false, want true (per-IP isolation)")
	}
}

func TestIPRateLimiter_EvictionDropsIdleBucket(t *testing.T) {
	t.Parallel()
	l, clk := newTestLimiter(t, 10*time.Millisecond, 2)

	if !l.Allow("a") {
		t.Fatal("seed Allow: got false, want true")
	}

	// Advance just past burst*refillEvery so the eviction threshold is
	// crossed. The bucket has not been polled since, so pollLast is stale.
	clk.Advance(time.Duration(2)*10*time.Millisecond + time.Millisecond)
	l.sweep()

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("buckets after sweep: got %d, want 0", n)
	}
}

// TestIPRateLimiter_EvictionPreservesActiveBucket exercises the load-bearing
// two-field design: a bucket that is being actively denied (attacker hammering
// a depleted bucket) must NOT be evicted, because evicting it would grant the
// attacker a fresh burst on the next request. pollLast is updated on every
// Allow (including denies); refillLast is not. So a hammered bucket survives
// sweep even when burst*refillEvery has elapsed since the last credit.
func TestIPRateLimiter_EvictionPreservesActiveBucket(t *testing.T) {
	t.Parallel()
	l, clk := newTestLimiter(t, 10*time.Millisecond, 2)

	// Exhaust the burst (so tokens=0, subsequent Allows deny).
	burst1 := l.Allow("a")
	burst2 := l.Allow("a")
	if !burst1 || !burst2 {
		t.Fatal("burst Allows: got false, want true")
	}

	// Advance past the eviction threshold — but the attacker keeps polling.
	clk.Advance(time.Duration(2)*10*time.Millisecond + time.Millisecond)

	// Crucially: poll right now. The Allow refills tokens up to burst (since
	// elapsed >= burst*refillEvery), consumes one, returns true — and
	// updates pollLast to "now". A sweep immediately after must NOT drop
	// this bucket: the attacker's most recent poll is t=now.
	//
	// We don't assert on the Allow return value here; the design contract
	// the test is locking in is that pollLast advances on EVERY call.
	_ = l.Allow("a")
	l.sweep()

	l.mu.Lock()
	_, present := l.buckets["a"]
	l.mu.Unlock()
	if !present {
		t.Fatal("active bucket evicted after recent poll: two-field invariant violated")
	}
}

// TestIPRateLimiter_EvictionGoroutineRunsAndDrops is the only test that uses
// real wall-clock sleeping; it verifies the goroutine path end-to-end.
func TestIPRateLimiter_EvictionGoroutineRunsAndDrops(t *testing.T) {
	t.Parallel()
	l := NewIPRateLimiter(10*time.Millisecond, 1, 5*time.Millisecond)
	t.Cleanup(l.Close)

	if !l.Allow("a") {
		t.Fatal("seed Allow: got false, want true")
	}

	// Sleep well past several eviction-interval cycles and past the
	// burst*refillEvery threshold. Goroutine should sweep the idle bucket.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		n := len(l.buckets)
		l.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("bucket not evicted within 1s")
}

func TestIPRateLimiter_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	l := NewIPRateLimiter(time.Second, 1, time.Hour)
	l.Close()
	l.Close() // must not panic
}

// TestIPRateLimiter_CloseStopsEvictionGoroutine asserts that after Close the
// background goroutine has actually exited, not just that done was signalled.
// Strategy: short eviction interval; populate the map AFTER Close; sleep
// longer than several intervals; assert the entry is still there (no sweep
// fired). Note: we update pollLast manually via Allow, so the bucket is fresh
// regardless of the actual wall-clock delta the sweep would see.
func TestIPRateLimiter_CloseStopsEvictionGoroutine(t *testing.T) {
	t.Parallel()
	l := NewIPRateLimiter(10*time.Millisecond, 1, 5*time.Millisecond)
	l.Close()

	// Insert a bucket whose pollLast is far in the past so that, if the
	// goroutine were still running, the next sweep would drop it.
	l.mu.Lock()
	l.buckets["x"] = &ipBucket{tokens: 0, refillLast: time.Unix(0, 0), pollLast: time.Unix(0, 0)}
	l.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	l.mu.Lock()
	_, present := l.buckets["x"]
	l.mu.Unlock()
	if !present {
		t.Fatal("bucket evicted after Close: goroutine did not exit")
	}
}

// TestIPRateLimiter_RaceFreedom hammers Allow from many goroutines with
// overlapping and disjoint IPs, against a sweep goroutine running on a tight
// interval. Run with: go test -race -count=20 -run TestIPRateLimiter_RaceFreedom ./internal/relay
func TestIPRateLimiter_RaceFreedom(t *testing.T) {
	t.Parallel()
	l := NewIPRateLimiter(time.Millisecond, 4, time.Millisecond)
	t.Cleanup(l.Close)

	const goroutines = 32
	const opsPer = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			ip := fmt.Sprintf("ip-%d", g%4)
			for i := 0; i < opsPer; i++ {
				_ = l.Allow(ip)
			}
		}()
	}
	wg.Wait()

	// Sanity: state is still legible under the lock; no panic.
	l.mu.Lock()
	_ = len(l.buckets)
	l.mu.Unlock()
}
