# Spec: per-IP token-bucket rate-limit type with eviction (#50)

## Files to read first

- `internal/relay/registry.go:49-79` — the "passive in-memory store guarded by one mutex, constructor returns `*Type` with the map pre-allocated" shape this spec mirrors.
- `internal/relay/registry.go:148-184` — `ScheduleReleaseServer` + `handleGraceExpiry`: the existing pattern for a background goroutine that touches a guarded map and races against the lock-holder. Note the pattern is `time.AfterFunc` here (one-shot per key); this spec uses `time.NewTicker` (one shared sweep). Different shape, same race discipline.
- `internal/relay/registry_test.go:11-45` — the `package relay` test-file convention + `fakeConn` shape; `closeCh`-based "block until X fires" pattern reused by eviction tests.
- `internal/relay/registry_test.go:419-484` — `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles` and `TestRegistry_RaceFreedom` — the canonical "hammer the API from many goroutines, run under `-race`" structure this spec's race test mirrors directly.
- `docs/PROJECT-MEMORY.md` — § *Patterns established* bullets that this spec leans on: "Passive in-memory stores guard mutation under one RWMutex; reads return copies, never references" (the limiter doesn't expose reads, but the lock discipline is the same), "Per-conn goroutines exit cleanly via LIFO defers, not by closing the conn themselves" (related but distinct: this spec's goroutine owns the lifecycle of the *limiter*, not a per-conn resource — closed via the dedicated `Close()` method, not a defer chain).
- `docs/threat-model.md` § *DoS resistance — connection floods, slow-loris, fork-bomb retry* — the threat this primitive begins to mitigate. The "future hardening" line ("A token-bucket rate limit on `/v1/server` and `/v1/client`") is what this ticket + its wiring sibling start delivering. The threat-model doc update itself is the **wiring ticket's** job, not this one.
- `go.mod` — confirms the project's stdlib + `golang.org/x/crypto` + `nhooyr.io/websocket` policy. No new direct dep is introduced; in particular `golang.org/x/time/rate` is **not** added (a hand-rolled token bucket of ≤30 lines is cheaper than a new dep + ADR).

## Context

`docs/threat-model.md` § *DoS resistance* names connection floods and fork-bomb retry loops as in-scope concerns and lists "a token-bucket rate limit on `/v1/server` and `/v1/client`" as the future-hardening item to land. #29 already capped per-frame size at 256 KiB and #30 will cap phones per server-id; this ticket plus its wiring sibling (#47) close the third leg — pre-upgrade attempt throttling.

The ticket is the **limiter type only**. The IP-extraction helper lives in a sibling ticket; the upgrade-handler wiring lives in #47. Splitting these is deliberate: each primitive lands and is reviewed in isolation, and the limiter type is interesting enough on its own (eviction lifecycle, integer-token math, fake-clock testability) to merit its own review.

The shape is the same one `Registry` established: a passive in-memory store guarded by a mutex, with a single owned goroutine for background bookkeeping that exits cleanly on `Close()`.

## Design

### File layout

Two new files, both `package relay`:

- `internal/relay/ratelimit.go` — type, constructor, `Allow`, `Close`, unexported `sweep`/`evictLoop`.
- `internal/relay/ratelimit_test.go` — burst, refill, isolation, eviction, idempotent close, race tests.

No other files change. `cmd/pyrycode-relay/main.go` is explicitly untouched per the AC.

### Type & API

```go
// IPRateLimiter is a per-source-IP token-bucket rate limiter with bounded
// memory under address-space scanning. Safe for concurrent use; Close()
// stops the background eviction goroutine and is idempotent.
type IPRateLimiter struct {
    refillEvery time.Duration // duration between successive token credits
    burst       int           // bucket capacity (max tokens)

    mu      sync.Mutex
    buckets map[string]*ipBucket

    now       func() time.Time // injectable clock; defaults to time.Now
    done      chan struct{}
    closeOnce sync.Once
    wg        sync.WaitGroup
}

type ipBucket struct {
    tokens     int       // current tokens, in [0, burst]
    refillLast time.Time // anchor for integer-token refill math; advances in token-sized chunks
    pollLast   time.Time // last Allow() call against this IP; used by eviction
}

// NewIPRateLimiter builds a limiter that refills one token every refillEvery,
// up to burst tokens per bucket. It launches a single eviction goroutine that
// sweeps idle buckets every evictionInterval. Close() must be called to stop
// the goroutine (e.g. on process shutdown or test cleanup).
func NewIPRateLimiter(refillEvery time.Duration, burst int, evictionInterval time.Duration) *IPRateLimiter

// Allow decrements the bucket for ip and returns whether the attempt is
// permitted. The first Allow for an IP starts the bucket at burst tokens and
// returns true (consuming one). An empty ip is treated as any other key —
// callers wanting to deny on empty-string must do so before calling Allow.
func (l *IPRateLimiter) Allow(ip string) bool

// Close stops the background eviction goroutine and blocks until it has
// returned. Idempotent: a second call returns once the first has finished
// stopping the goroutine. After Close, the limiter must not be used further;
// Allow's behaviour is unspecified (the package-level expectation is "do not
// call Allow after Close", enforced by review, not at runtime).
func (l *IPRateLimiter) Close()
```

### Refill math (integer tokens, no float drift)

Token-bucket with `refillEvery time.Duration` per token (not float64 tokens/sec — integer math, no drift). On every `Allow(ip)`:

```
elapsed   := now - b.refillLast
tokensAdd := int(elapsed / refillEvery)
if tokensAdd > 0:
    b.tokens     = min(burst, b.tokens + tokensAdd)
    b.refillLast = b.refillLast + tokensAdd * refillEvery   // NOT now — preserves fractional time
b.pollLast = now
if b.tokens > 0:
    b.tokens--
    return true
return false
```

Advancing `refillLast` by the **integer** number of tokens credited (rather than to `now`) preserves the fractional remainder of elapsed time, so the next call gets exact credit. Setting `refillLast = now` instead would discard fractional time and over-throttle slightly (security-positive, but unnecessary).

`pollLast` is updated on **every** `Allow`, including denies. This separation is the load-bearing part of the eviction design (next subsection).

A first-time IP (`buckets[ip]` not present) gets a fresh bucket with `tokens: burst - 1, refillLast: now, pollLast: now` and returns true — one token consumed.

### Eviction rule

```
threshold := time.Duration(burst) * refillEvery
for ip, b := range buckets:
    if now - b.pollLast >= threshold:
        delete(buckets, ip)
```

Invariant: if `now - pollLast >= burst * refillEvery`, the bucket is at capacity at the moment of eviction. Proof: since `pollLast` was last set, no `Allow` has been called for this IP (otherwise `pollLast` would be more recent). So `tokens` is unchanged since then and `refillLast ≤ pollLast`. Therefore `now - refillLast ≥ burst * refillEvery`, which credits `≥ burst` tokens, clamped to `burst`. The AC's "eviction never drops a bucket below capacity" is structurally satisfied.

This is why we need **two** timestamps. With a single `last`-field-of-any-flavour, you cannot simultaneously (a) preserve fractional refill time and (b) prevent eviction from dropping a bucket that's being actively denied (an attacker hammering a depleted bucket). The two fields decouple the two concerns:

- `refillLast` — anchored to the last *credited refill*, advances in integer-token chunks.
- `pollLast` — anchored to the last *call to Allow*, advances to `now` on every call.

### Eviction goroutine lifecycle

Standard ticker + done-channel + waitgroup:

```go
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
```

`sweep` is unexported but in-package, so tests can call it synchronously without waiting for the ticker. This is the test seam: most eviction tests construct the limiter with a long `evictionInterval` (so the goroutine effectively never fires during the test), advance the fake clock manually, and call `sweep()` directly. One race-and-lifecycle test uses a short interval to exercise the goroutine path under `-race`.

`Close` closes `done` exactly once and waits for the goroutine to exit:

```go
func (l *IPRateLimiter) Close() {
    l.closeOnce.Do(func() { close(l.done) })
    l.wg.Wait()
}
```

`wg.Wait()` is *outside* `closeOnce.Do` so a second concurrent `Close` caller also blocks until the goroutine has finished (rather than returning immediately while the first caller is still waiting). This matches the established "Close is idempotent and synchronous" expectation on the registry's `Conn.Close`.

### Constructor

```go
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
```

The `now` field defaults to `time.Now`; in-package tests assign a fake clock directly (`l.now = fakeNow`). No exported "WithClock" constructor — the field is package-private and the test surface is package-internal.

### Parameter contracts (not enforced at runtime)

- `refillEvery > 0` — `refillEvery == 0` would make refill math degenerate (every call refills `burst` tokens). The wiring ticket (#47) is responsible for choosing sensible policy values; this primitive does not panic, does not return errors, and does not validate. The convention matches `Registry` (no defensive checks on `serverID`).
- `burst ≥ 1` — burst of `0` would make `Allow` always return false. Same posture: out of bounds is a wiring-ticket bug; this type stays simple.
- `evictionInterval > 0` — `time.NewTicker` panics on `≤ 0`, which is the right loud-failure for a wiring bug.

These are documented in the doc comments on `NewIPRateLimiter` ("`refillEvery` and `burst` must be positive; `evictionInterval` must be positive — passing zero panics via `time.NewTicker`"); not enforced with `if` checks. Loud failure at the wiring site is the project's convention (`docs/PROJECT-MEMORY.md` § *Loud failure over silent correction*).

### Concurrency model

- One shared `sync.Mutex` (not RWMutex). `Allow` is the only frequent path and it always writes (token decrement and/or `pollLast`/`refillLast` update); read-only callers don't exist. RWMutex's overhead is unjustified.
- The eviction goroutine grabs the same `Mutex` during sweep. Sweep holds the lock for the duration of one full map walk — bounded by the number of distinct active IPs. Under realistic load (thousands of IPs maximum), this is a microsecond-scale critical section. Under address-space-scan abuse (millions of IPs), the sweep walk is longer but exactly as long as the abuse warrants; the limit on map growth is `evictionInterval` × `attempt rate` × `burst` — orders of magnitude smaller than process RSS budget.
- The eviction goroutine does not call any externally-supplied callback. It does not `Send` or `Close` anything held by the application. Sweep mutates `l.buckets` only; map walks under the lock are safe with concurrent delete.
- `Close` synchronises via `wg.Wait()`. Any in-flight sweep completes before `Close` returns. After `Close` returns, no more sweeps will fire and the goroutine has exited.

The "Per-conn goroutines exit cleanly via LIFO defers" pattern (`docs/PROJECT-MEMORY.md`) is **related but distinct**: that pattern is for goroutines launched *per WebSocket connection* whose lifecycle is owned by an HTTP handler. The limiter's eviction goroutine is owned by the limiter itself and stopped via the type's own `Close()` method. The wiring ticket (#47) is what registers `defer limiter.Close()` at the composition root.

### Error handling

This type returns no errors. `Allow` returns `bool`. `Close` returns nothing. `NewIPRateLimiter` is infallible aside from the documented panic on `evictionInterval ≤ 0` (delegated to `time.NewTicker`).

Failure modes that the wiring ticket must handle (out of scope here, listed for the next ticket's spec):

- An empty IP string from the extraction helper — the wiring ticket decides whether to deny or to call `Allow("")`. The limiter does not.
- A clock that goes backwards (NTP step) — `elapsed` could go negative, in which case `int(elapsed / refillEvery)` is non-positive, so refill is skipped and the bucket continues until time moves forward again. This is acceptable; we do not need to detect or compensate.

### Testing strategy

All tests live in `internal/relay/ratelimit_test.go`, `package relay`, mirroring the `registry_test.go` conventions (in-package fakes, direct field assignment for clock injection).

#### Fake clock helper

```go
type fakeClock struct {
    mu  sync.Mutex
    now time.Time
}

func (c *fakeClock) Now() time.Time    { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }
```

A test constructs the limiter, then immediately overrides `l.now = clock.Now` to swap in the fake. (The single eviction goroutine reads `l.now` on every tick; assigning before any tick fires is race-free in practice and tests cover this with `-race`.)

For tests that touch only `Allow` and `sweep` synchronously, the limiter's eviction interval can be set to `time.Hour` — the background goroutine effectively never fires, so the fake-clock + direct-`sweep()`-call pattern is fully deterministic without any sleep.

#### Tests

1. **`TestIPRateLimiter_BurstThenDeny`** — burst=3, refillEvery=1s, fake clock at t=0. Three `Allow("1.2.3.4")` → all true. Fourth → false.

2. **`TestIPRateLimiter_RefillAfterAdvance`** — burst=2, refillEvery=1s. Exhaust burst at t=0. `Advance(1s)`. Next `Allow` → true. Next → false (only one token refilled). `Advance(2s)`. Two more `Allow`s → true, true (capped at burst). Third → false.

3. **`TestIPRateLimiter_FractionalRefillPreserved`** — burst=2, refillEvery=1s. Exhaust at t=0. `Advance(500ms)`; `Allow` → false (no integer token yet). `Advance(500ms)` (cumulative 1s); `Allow` → true. Asserts `refillLast` advances in token-sized chunks, not to `now`.

4. **`TestIPRateLimiter_PerIPIsolation`** — burst=1, refillEvery=1s. `Allow("a")` → true; `Allow("a")` → false; `Allow("b")` → true.

5. **`TestIPRateLimiter_EvictionDropsIdleBucket`** — burst=2, refillEvery=10ms, evictionInterval=time.Hour (goroutine quiescent). `Allow("a")` creates bucket. `Advance(burst * refillEvery + 1ms)` (just past threshold). Call `l.sweep()` directly. Assert `len(l.buckets) == 0` (lock-guarded read inside the test).

6. **`TestIPRateLimiter_EvictionPreservesActiveBucket`** — burst=2, refillEvery=10ms, evictionInterval=time.Hour. `Allow("a")` exhaust the burst (so tokens=0, denied). `Advance(burst * refillEvery + 1ms)`. **Critically**: call `Allow("a")` again (still denied — bucket at 0; this is the "actively-throttled attacker" path). Call `sweep()`. Assert the bucket is **not** dropped, because the most-recent `pollLast` was just now. This test exercises the load-bearing two-field design.

7. **`TestIPRateLimiter_EvictionGoroutineRunsAndDrops`** — burst=1, refillEvery=10ms, evictionInterval=5ms (short, real-clock). `Allow("a")` creates bucket; no fake clock — real `time.Now`. Sleep `time.Sleep(50 * time.Millisecond)` (≥ several evictInterval cycles, well past the 10ms threshold). Lock and assert `len(l.buckets) == 0`. This is the only test that uses real wall-clock sleeping; it verifies the goroutine path end-to-end. Mirrors the `closeCh`-based "block until X happens" idiom in `registry_test.go:288-326`.

8. **`TestIPRateLimiter_CloseIsIdempotent`** — `NewIPRateLimiter` → `Close()` → `Close()`. No panic, both return.

9. **`TestIPRateLimiter_CloseStopsEvictionGoroutine`** — short evictionInterval (5ms). After `Close`, `Allow("x")` populates the map; sleep longer than the interval; assert the map still has the entry (no sweep fired). Validates that the goroutine actually exited rather than just `done` being closed.

10. **`TestIPRateLimiter_RaceFreedom`** — pattern lifted from `TestRegistry_RaceFreedom` (`registry_test.go:457-484`). 32 goroutines × 200 ops each. Each goroutine hammers `Allow` with `ip := fmt.Sprintf("ip-%d", g%4)` (some shared IPs, some not). Short evictionInterval (1ms) so the sweep goroutine races against `Allow`. After the wait, call `Close()` and `Counts()`-equivalent (lock-and-len) — assert no panic. Run with `-race`. Run command in test comment: `go test -race -count=20 -run TestIPRateLimiter_RaceFreedom ./internal/relay`.

`make vet`, `make test` (`-race`), and `make build` must all pass per the project convention.

### Out of scope (reaffirming the ticket)

- IP-extraction helper (`X-Forwarded-For` parsing, `RemoteAddr` host-stripping). Sibling ticket.
- Upgrade-handler middleware wiring. #47.
- `--trust-x-forwarded-for` CLI flag. #47.
- 429 response shape. #47.
- Log line on deny. #47.
- Policy defaults (refill, burst, eviction interval values). #47.
- Threat-model doc update. #47 (once the rate limit is actually applied; this primitive on its own changes nothing about the running system).
- Per-server-id or per-token rate limiting. Separate threat surface.
- Multi-instance / shared-state rate limiting. v1 is single-instance.

### Doc updates the developer must make

Per the 2026-05-10 convention (`docs/PROJECT-MEMORY.md` § *What's built*): create `docs/knowledge/codebase/50.md` with the implementation summary (file paths, public API, eviction rule one-liner, the two-field invariant). Add a one-line entry to `docs/knowledge/INDEX.md`.

No update to `docs/threat-model.md` § *DoS resistance* in this ticket — that update is the wiring ticket's job, because until #47 lands, no rate limit is actually applied to traffic. (Updating the doc here would falsely claim the threat is partially mitigated when the primitive is sitting unused.)

No update to `docs/PROJECT-MEMORY.md` § *What's built* table — per the same 2026-05-10 convention, that table is frozen pre-2026-05-10 and the per-ticket summary in `codebase/50.md` is canonical.

### Open questions

None. The two-field invariant is the only non-obvious design choice and is justified inline; the test surface fits the existing `registry_test.go` patterns without new infrastructure; no new dependencies are introduced.

---

## Security review (label: `security-sensitive`)

### Trust boundaries this spec touches

| Boundary | Trust posture | Where enforced |
|---|---|---|
| `Allow(ip string) bool` — caller-supplied IP string from wiring middleware | Adversarial. The IP may be forged (if `--trust-x-forwarded-for` is on in #47), spoofed, or be the empty string. The limiter does not validate the string. | The string is used only as a map key — no parsing, no `net.IP` allocation, no logging. Validity / trust is the wiring ticket's contract. |
| Eviction goroutine ↔ `Allow` concurrent access | Trusted (in-process). | Single `sync.Mutex` guards both `buckets` and per-bucket field mutation. |
| Background goroutine lifecycle | Trusted (in-process). | `done` channel + `sync.WaitGroup`; `Close` is idempotent via `sync.Once` and synchronous via `wg.Wait`. |

### Adversarial walk

**(1) Memory exhaustion via address-space scanning.** Without eviction, an attacker scanning IPs (one Allow per IP) grows the map indefinitely. With eviction, a bucket lives for at most `evictionInterval + burst * refillEvery` before being collected (the sweep that follows the threshold-crossing tick). Steady-state map size is bounded by `(active IP count) × max(evictionInterval, burst * refillEvery)` worth of recent traffic. With AC defaults the wiring ticket will choose (~minutes scale), this is fine. Pathological case: an attacker rotates IPs faster than eviction fires — but each new bucket entry is ~40 bytes, evicted after one sweep cycle, and the rate at which the attacker can *make* requests is itself bounded by their connection ramp. The limit is bounded by `bandwidth × evictionInterval / requestSize`, not by anything malicious in this type.

**(2) Free-burst recycling.** An attacker hammers an IP until denied, then expects the bucket to be evicted (granting them a fresh `burst`-worth of tokens on the next request). This is what the two-field design exists to prevent: `pollLast` is updated on every `Allow` (including denies), so an actively-hammering bucket is **never** evicted regardless of how long the attack runs. The bucket survives until the attacker stops polling for `burst * refillEvery`, at which point eviction is safe because the bucket would have refilled to capacity anyway. Test #6 (`TestIPRateLimiter_EvictionPreservesActiveBucket`) is the explicit regression-guard for this.

**(3) Time-source manipulation (NTP step, clock skew).** A clock that jumps forward grants free tokens (refill credits the elapsed gap). The injectable `now` is `time.Now` in production — same exposure as any Go program reading the wall clock. Mitigation if it matters: the wiring ticket can use `time.Since(processStartMonotonic)` instead, but that's a wiring-side choice. For v1, accepting wall-clock as the time source matches every other timer in the relay (`http.Server` timeouts, autocert renewal, registry grace window). The threat-model doc does not currently call this out and this ticket does not change the threat surface relative to existing code.

**(4) Integer overflow on `tokens` or duration math.** `tokens` is bounded above by `burst` (clamped via `min`) and below by 0 (never decremented below it — the deny path returns before decrement). `tokensAdd := int(elapsed / refillEvery)` — `elapsed` is `time.Duration` (int64 nanoseconds); for `refillEvery` of any reasonable size (≥ 1µs), `elapsed / refillEvery` fits in int easily even for 100-year `elapsed`. Not a concern at human-timescale operation. `time.Duration(l.burst) * l.refillEvery` in `sweep` — `burst` is an int chosen by wiring (single-digit to two-digit values per the threat-model rationale); product fits in int64 for any sane policy.

**(5) Negative elapsed (clock-backwards).** If `now.Sub(b.refillLast)` is negative, `int(elapsed / refillEvery)` is ≤ 0, the refill branch is skipped (`if tokensAdd > 0`), `pollLast` is set to `now` (which is < the old `pollLast`), and the bucket continues to operate. `sweep`'s `now.Sub(b.pollLast) >= threshold` will be false (negative), so the bucket is not dropped. Safe-by-default: no panic, no free tokens, no premature eviction.

**(6) Logging — does the limiter log the IP?** No. The limiter has no logger. Logging is the wiring ticket's responsibility. The threat-model rule (`docs/threat-model.md` § *Log hygiene* — "remote host (IP) MAY be logged; payload bodies and tokens MUST NOT") applies at the wiring site; this primitive does not log anything.

**(7) Information leak via `Allow` timing.** The deny path returns `false` after either an O(1) map lookup miss-or-hit and a small amount of arithmetic — same code path regardless of which IP is denying or how many tokens it had. No timing side channel meaningful at the WS-upgrade granularity (where every code path is dwarfed by TLS handshake cost).

**(8) Goroutine leak on improper close.** If callers forget `defer limiter.Close()` at the composition root, the eviction goroutine runs until process exit. This is benign (one goroutine, ticker firing every `evictionInterval`) and matches the registry's `time.AfterFunc` goroutines, which the registry also does not "stop" explicitly. Test #8 (`TestIPRateLimiter_CloseIsIdempotent`) and #9 (`TestIPRateLimiter_CloseStopsEvictionGoroutine`) verify the close path works; correct use is enforced by code review at the wiring site (#47 AC: "limiter.Close() called on shutdown").

**(9) `sync.Once` correctness on `Close`.** `closeOnce.Do(func() { close(l.done) })` ensures `close` is called once. A second `Close` proceeds past `Do` and into `wg.Wait` — which is a no-op if the goroutine has already exited (the WaitGroup counter is 0). If the first `Close` is mid-`wg.Wait` and a second `Close` arrives, both block on the same WaitGroup; both return once the goroutine exits. Correct.

**(10) `evictLoop` and `done` ordering.** `evictLoop` selects on `<-l.done` and `<-ticker.C`. On `close(l.done)`, the receive unblocks immediately; `defer ticker.Stop()` and `defer l.wg.Done()` run in LIFO order. `Close` then unblocks from `wg.Wait()`. If `done` is closed while a `sweep` is in flight (between two ticks), the sweep completes first (the select re-checks after sweep returns) and then the loop exits on the next iteration — no torn sweep, no held lock.

**(11) Map-iteration delete safety.** Go's spec guarantees that `delete(m, k)` during `range` over `m` is safe (the deleted entry is omitted from subsequent iterations; entries not yet visited may or may not be visited). Used correctly in `sweep`.

**(12) Lock held during sweep walk.** As noted in *Concurrency model*, the sweep holds the lock for one full map walk. Under abuse, this could starve `Allow`. Mitigation already-in-place: the eviction interval is chosen to be much longer than expected `Allow` rate × walk cost. Defaults the wiring ticket will pick (minutes-scale interval, ≤ low-thousands of active buckets) keep the worst-case lock hold well under 100µs. If this ever becomes a problem in practice, the sweep can be sharded or moved to a snapshot-and-delete pattern; not warranted in v1 by any observed failure.

### Findings

None. Verdict: **PASS**.

The primitive is self-contained, stdlib-only, and has no external trust dependencies beyond the wall clock (which is no different from any other Go program). The two-field eviction invariant — `pollLast` separate from `refillLast` — is the only non-obvious security-relevant design choice and is justified inline + regression-tested.
