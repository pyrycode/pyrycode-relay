# Spec — Registry: schedule deferred binary release with grace-period reclaim (#20)

## Files to read first

- `internal/relay/registry.go` — full file. The new method, the timer-state field, and the `ClaimServer` change all live here. The doc-comment block at lines 22-46 (the `Conn` interface contract) constrains how the grace-expiry handler may invoke `Close()` and is referenced again below.
- `internal/relay/registry.go:78-86` — current `ClaimServer`. The grace-aware fast path inserts before the existing `ErrServerIDConflict` check.
- `internal/relay/registry.go:93-101` — current `ReleaseServer`. Doc-comment foreshadows this ticket ("orphan phones survive a release until their owner unregisters them, so the grace period (#8) can keep phones reachable across a binary disconnect window"). Stays unchanged.
- `internal/relay/registry.go:163-173` — `PhonesFor`'s snapshot pattern (allocate, copy, return; no locks held while caller iterates). The grace-expiry handler reuses this pattern *internally* — snapshot under the lock, release the lock, then iterate `Close()`.
- `internal/relay/registry_test.go` — full file. The new tests append here; they reuse `fakeConn` (lines 14-22) but `fakeConn.closed` becomes cross-goroutine state for the first time, so the developer must add a sync primitive — see "Testing strategy" below.
- `docs/PROJECT-MEMORY.md:31-32` — established patterns: passive store guarding mutation under one RWMutex; reads return copies. The grace-expiry handler honours both.
- `docs/knowledge/decisions/0003-connection-registry-passive-store.md` — full file, especially "ReleaseServer is narrow" (lines 43-45). This ticket *adds* a deferred sibling without touching the immediate primitive.
- `pyrycode/pyrycode/docs/protocol-mobile.md` § Authentication → Binary → relay — wire-protocol source for the 30-second grace window. The relay carries the **mechanism**; the duration is a parameter the WS handler will pass in (`30*time.Second`) in #21. Tests use ms-scale durations.
- Ticket #20 issue body (re-read for the AC structure; subtest names map 1:1 to AC bullets).

## Context

Ticket #3 shipped a passive registry with immediate `ReleaseServer`. Ticket #20 adds a deferred sibling so the `/v1/server` handler (sibling ticket #21) can wait 30 seconds before tearing down phones, giving a transient binary disconnect a chance to reclaim its slot without forcing every phone to receive `4404` and reconnect.

The shape adopted here is **the grace window IS the reclaim path**. While a grace timer is pending:

- The binary entry stays in the registry (now pointing at a closed `Conn` whose `Send` will return errors — that's the existing contract; the registry adds no new error semantics).
- Phones can be registered (`RegisterPhone` succeeds), so a phone that connects during the grace window lands in the slot atomically.
- A new `ClaimServer` for the same server-id **succeeds and replaces the binary `Conn`** instead of returning `ErrServerIDConflict`. The pending timer is cancelled.

If the timer fires (no reclaim), the binary entry is removed *and* every phone for that server-id is removed and `Close()`'d, with `Close` calls happening outside the registry's lock (snapshot-and-iterate, same shape as `PhonesFor`).

This ticket ships the registry primitive only. No call sites migrate. The handler swap (`ReleaseServer` → `ScheduleReleaseServer`) lives in #21.

## Design

### Files

Modified: `internal/relay/registry.go`, `internal/relay/registry_test.go`. No new files. No new package. No edits outside `internal/relay`.

### New state on `*Registry`

```go
type Registry struct {
    mu       sync.RWMutex
    binaries map[string]Conn
    phones   map[string][]Conn
    timers   map[string]*time.Timer  // NEW: pending grace-period timers, keyed by serverID
}
```

`NewRegistry` initialises `timers` to an empty map. The map is guarded by the same `mu` as `binaries`/`phones` — no second lock, consistent with ADR-0003's "one RWMutex over both maps" rationale (which now reads "over all three").

### New method

```go
// ScheduleReleaseServer arms (or replaces) a deferred release of serverID
// after d. The grace window is the reclaim path: until the timer fires,
//
//   - BinaryFor(serverID) continues to return the (now-closed) binary Conn —
//     callers that Send through it observe whatever error the underlying
//     impl returns.
//   - RegisterPhone(serverID, conn) continues to succeed.
//   - ClaimServer(serverID, conn) replaces the binary atomically and
//     cancels the pending timer (returns nil, NOT ErrServerIDConflict).
//
// If a timer is already pending for serverID, it is stopped and replaced
// (last call wins). Calling ScheduleReleaseServer for a serverID with no
// binary holding it arms the timer anyway; on expiry it removes only
// whatever state happens to be present (defensive, matches the no-op
// semantics of ReleaseServer on an unheld id).
//
// On expiry (no reclaim within d): the binary entry for serverID is
// removed, every phone currently registered for serverID is removed from
// the registry and Close() is invoked on it. Close calls happen OUTSIDE
// the registry lock (snapshot-and-iterate, same shape as PhonesFor).
//
// This method does not block on d; the timer fires on Go's runtime
// goroutine for time.AfterFunc.
func (r *Registry) ScheduleReleaseServer(serverID string, d time.Duration)
```

`ReleaseServer` (immediate) is unchanged. The two coexist — `/v1/server`'s old "synchronous release" use case (e.g. failed claim cleanup) keeps working; the grace path is opt-in.

### `ClaimServer` change

The current method is a check-then-set under the write lock. The new shape inserts a grace-aware fast path **before** the conflict check:

```go
func (r *Registry) ClaimServer(serverID string, conn Conn) error {
    r.mu.Lock()
    defer r.mu.Unlock()

    if t, gracing := r.timers[serverID]; gracing {
        // Grace window in flight: cancel the pending release and
        // atomically replace the binary Conn. Phones registered during
        // the grace window stay; the new binary inherits them.
        t.Stop()
        delete(r.timers, serverID)
        r.binaries[serverID] = conn
        return nil
    }
    if _, ok := r.binaries[serverID]; ok {
        return ErrServerIDConflict
    }
    r.binaries[serverID] = conn
    return nil
}
```

Note the asymmetry between `t.Stop()` and the stale-firing concern: `Stop()` returning `false` means the timer has already fired or is currently running. If the func is currently running, it is blocked on `r.mu` (we hold it). When we release the lock and the func runs, it would otherwise erroneously remove the binary we just installed and close the new claimant's phones. The expiry handler defends against this with a pointer comparison — see below.

### Grace-expiry handler

The closure passed to `time.AfterFunc` captures the timer pointer and a back-pointer to the registry, so it can verify on fire that it has not been superseded:

```go
func (r *Registry) ScheduleReleaseServer(serverID string, d time.Duration) {
    r.mu.Lock()
    defer r.mu.Unlock()

    if existing, ok := r.timers[serverID]; ok {
        existing.Stop()
        delete(r.timers, serverID)
    }

    var t *time.Timer
    t = time.AfterFunc(d, func() { r.handleGraceExpiry(serverID, t) })
    r.timers[serverID] = t
}

func (r *Registry) handleGraceExpiry(serverID string, self *time.Timer) {
    r.mu.Lock()
    if r.timers[serverID] != self {
        // Superseded: ClaimServer or a later ScheduleReleaseServer
        // replaced the timer between fire and us acquiring the lock.
        // No-op: the new owner manages whatever state is present.
        r.mu.Unlock()
        return
    }
    delete(r.timers, serverID)
    delete(r.binaries, serverID)
    snapshot := r.phones[serverID]
    delete(r.phones, serverID)
    r.mu.Unlock()

    for _, p := range snapshot {
        p.Close()
    }
}
```

The pointer-identity check (`r.timers[serverID] != self`) is the synchronisation point. After `Stop()` runs in `ClaimServer` or `ScheduleReleaseServer`, the *next* line replaces or deletes the entry; if the AfterFunc body has already started executing and is blocked on `r.mu`, by the time it acquires the lock the map either has a different `*time.Timer` (replaced) or no entry at all. Either way, `self` no longer matches and the body returns without touching state.

We do **not** need to copy `snapshot` into a fresh slice the way `PhonesFor` does. We hold the lock during `delete(r.phones, serverID)`; after the delete, no other goroutine can reach `snapshot` via the registry, so the slice is exclusively ours. Releasing the lock before iterating is purely about not holding a mutex during `Close()` calls (which may block on the network).

`Close()` invocation is a no-op for an already-closed conn (existing `Conn` contract: idempotent). Binaries that disconnected and triggered the grace window typically have a closed underlying socket already; the registry doesn't care.

### Methods unchanged

- `ReleaseServer` — still immediate, narrow, no phone touch. Doc comment unchanged.
- `RegisterPhone` — gates on `binaries[serverID]` presence. During a grace window the binary entry IS still present (only the timer is pending), so `RegisterPhone` returns `nil` as required by AC. After expiry, the binary entry is gone and `RegisterPhone` returns `ErrNoServer` — also required by AC.
- `UnregisterPhone`, `BinaryFor`, `PhonesFor`, `Counts` — no changes. `Counts` continues to count whatever is in the maps; a server-id in its grace window contributes 1 to the binary count (the entry is still there) and the live count of phones, which matches the operational mental model ("we still consider this binary connected; we are about to find out").

### Concurrency model

- Same single `sync.RWMutex` as ADR-0003. The new `timers` map is guarded by it.
- The expiry-handler goroutine is the *only* new goroutine introduced by this ticket. Lifetime: bounded by `d`; no possibility of leakage because (a) `time.AfterFunc` does not retain the goroutine after the func returns; (b) the func is non-blocking on the lock side (acquires write lock, releases it, then iterates `Close()`); (c) `Close()` is the WS adapter's responsibility — the registry's contract requires `Close()` to be idempotent and safe to call concurrently with `Send`, but does NOT require it to be non-blocking. If `Close()` blocks indefinitely, the runtime goroutine pool absorbs it; the registry has no visible state stuck in cleanup.
- Lock graph remains a single node. The expiry handler does not call back into any other registry method; it manipulates the maps directly.
- `Stop()` is the documented cancellation primitive for `time.AfterFunc`. Returning `false` (already fired / already stopped) is fine — the pointer-identity check downstream catches the stale-firing case.

### Error handling

No new sentinels. `ScheduleReleaseServer` returns nothing (it's fire-and-arm; no failure mode the caller can act on). Calling it for an unheld server-id arms a timer that on expiry does nothing — same shape as `ReleaseServer("unknown")` returning `false`. The doc comment names this explicitly so the developer doesn't add a "binary not found" return value.

### What this ticket deliberately does not do

- **No phone-side close code.** `Conn.Close()` emits `1000` per ADR-0005 / `WSConn` contract. The protocol spec requests `1011 "binary did not reconnect"` on the phone side; adopting that requires extending the `Conn` interface or adding a parallel close primitive. Out of scope per AC; tracked as follow-up. Phones treat any close as "reconnect" — the user-visible behaviour is correct.
- **No handler wiring.** `/v1/server` continues to call `ReleaseServer` synchronously. #21 swaps the call site.
- **No grace-window phone limit.** A server-id whose binary disconnected can still accept new phone registrations during the grace window. Per-server caps are #5's concern.
- **No metrics for grace-period expiry vs reclaim.** `/healthz` `Counts` stays as-is. If we want to surface "grace pendings" later, that's a `Counts2`-style addition; not now.

### Algorithm summary

| Operation | Behaviour |
|---|---|
| `ScheduleReleaseServer(s, d)` — no pending timer | Arm timer; on fire, snapshot+delete binary+phones, then `Close()` phones outside lock. |
| `ScheduleReleaseServer(s, d)` — pending timer exists | `Stop()` old timer; arm new one. Last call wins. |
| `ClaimServer(s, c)` — pending timer exists | Cancel timer; replace binary; return `nil`. (Phones inherited.) |
| `ClaimServer(s, c)` — no timer, slot held | Return `ErrServerIDConflict`. (Existing behaviour.) |
| `ClaimServer(s, c)` — no timer, slot free | Insert binary; return `nil`. (Existing behaviour.) |
| `RegisterPhone(s, c)` — during grace window | Succeed (binary entry still present). |
| `RegisterPhone(s, c)` — after expiry | `ErrNoServer` (binary entry gone). |
| Timer fires, has not been `Stop()`'d | Pointer-identity check passes; remove binary, snapshot+delete phones, `Close()` outside lock. |
| Timer fires, has been `Stop()`'d after fire latched | Pointer-identity check fails; no-op. |

## Testing strategy

`internal/relay/registry_test.go`, append to existing file. Subtest names map 1:1 to AC bullets.

### `fakeConn` extension

`fakeConn.Close` is currently `func (c *fakeConn) Close() { c.closed = true }`. With the grace-expiry handler running on `time.AfterFunc`'s goroutine, the test's read of `c.closed` from the test goroutine becomes a data race. Fix in this ticket:

```go
type fakeConn struct {
    id     string
    sent   [][]byte
    mu     sync.Mutex
    closed bool
    closeCh chan struct{}  // optional; allocated by tests that wait for close
}

func (c *fakeConn) Close() {
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.closed {
        return  // honour idempotence
    }
    c.closed = true
    if c.closeCh != nil {
        close(c.closeCh)
    }
}

func (c *fakeConn) isClosed() bool {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.closed
}
```

Existing tests that mutate `closed` (none currently — they use `fakeConn.closed` only as an instance field; the `Close` writer is the only writer in the existing suite) need no changes; the bool-field read sites in existing tests don't exist (grep `closed` in the test file — only `Close()` calls are present, no reads). The mutex-guarded form is safe for the existing race test (which doesn't read `closed`).

### Functional tests

Each uses a small grace duration (10–50ms is plenty; `time.AfterFunc` resolution is fine on macOS/Linux; CI runs Linux). Where the test waits for expiry, prefer `closeCh`-style synchronisation over `time.Sleep` to keep the suite fast and deterministic.

1. **`TestScheduleReleaseServer_ReclaimWithinGrace_NoReleaseFires`** (AC: schedule + immediate reclaim → no release fires; new conn is the active binary; previously-registered phones remain.)
   - Claim `s1` with `b1`, register `p1`.
   - Schedule release of `s1` with `d=50ms`.
   - Immediately `ClaimServer(s1, b2)` — expect `nil` (NOT `ErrServerIDConflict`).
   - `BinaryFor(s1)` returns `b2`.
   - `PhonesFor(s1)` returns `[p1]`.
   - Sleep 100ms (longer than the original `d`).
   - Assert `p1.isClosed() == false`. (If the timer had fired stale and bypassed the pointer check, `p1` would have been closed.)

2. **`TestScheduleReleaseServer_ExpiryRemovesBinaryAndClosesPhones`** (AC: grace expires with no reclaim → binary entry gone, phones `Close()`-called and removed, subsequent `ClaimServer` succeeds, subsequent `RegisterPhone` returns `ErrNoServer`.)
   - Claim `s1`, register `p1` and `p2` with `closeCh` allocated.
   - Schedule release with `d=20ms`.
   - Wait on both `p1.closeCh` and `p2.closeCh` (`select` with a 1-second test timeout per channel).
   - Assert `BinaryFor(s1)` returns `(_, false)`.
   - Assert `PhonesFor(s1)` returns `nil`.
   - Assert `RegisterPhone(s1, p3)` returns `ErrNoServer`.
   - Assert `ClaimServer(s1, b2)` returns `nil`.

3. **`TestScheduleReleaseServer_PhoneRegisteredDuringGrace_SurvivesReclaim`** (AC bullet identical.)
   - Claim `s1` with `b1`.
   - Schedule release with `d=50ms`.
   - During the grace window: `RegisterPhone(s1, p1)` — expect `nil`.
   - `ClaimServer(s1, b2)` — expect `nil`.
   - `PhonesFor(s1)` returns `[p1]`.

4. **`TestScheduleReleaseServer_PhoneRegisteredDuringGrace_ClosedOnExpiry`** (AC bullet identical.)
   - Claim `s1`.
   - Schedule release with `d=20ms`.
   - During the grace window: `RegisterPhone(s1, p1)` (with `closeCh`).
   - Wait on `p1.closeCh`.
   - Confirm `p1.isClosed() == true`.

5. **`TestScheduleReleaseServer_RaceFreedomUnderRapidCycles`** (AC: rapid disconnect/reclaim cycles do not leak goroutines or pending timers; race-detector clean; `time.AfterFunc` cancellation paths exercised.)
   - 16 goroutines × 200 iterations each, hammering a small set of server-ids (`s-0..s-3`).
   - Each iteration: `ClaimServer`, `RegisterPhone` (best-effort), `ScheduleReleaseServer(d=1ms)` (about half fire and half get cancelled by the next claim), `ClaimServer` again, `UnregisterPhone`, `Counts`.
   - After `wg.Wait()`, sleep ~50ms (longer than max `d`), then `Counts()` and verify the registry is in a consistent state (no panic, no `nil`-deref). Goroutine-leak detection is via `runtime.NumGoroutine()` before/after the test with a small tolerance — or skipped if too flaky; the race detector catches the data-race class which is the more important property.
   - Doc comment names the manual stress invocation: `go test -race -count=20 -run TestScheduleReleaseServer_RaceFreedomUnderRapidCycles ./internal/relay`.

6. **`TestScheduleReleaseServer_ReplacesPendingTimer`** (AC: "Calling it again for the same `serverID` cancels and replaces any pending timer (last call wins).")
   - Claim `s1`, register `p1` with `closeCh`.
   - `ScheduleReleaseServer(s1, 1*time.Hour)` (effectively never fires within test).
   - `ScheduleReleaseServer(s1, 20ms)` — should *replace*, not add.
   - Wait on `p1.closeCh`. The 20ms timer fires; the 1-hour timer was `Stop()`'d before its closure can run.
   - This proves the second call cancelled the first (otherwise we'd see two firings, neither blocked by the other; idempotent `Close` would mask one but the test fails if the binary entry is *re-deleted* — actually, idempotence on the registry side is fine since `delete` of an absent key is a no-op and pointer-identity prevents double cleanup. Net: this test mostly proves the stop-and-replace path doesn't panic and that the 1-hour timer's stale firing is handled cleanly. Adequate signal.).

7. **`TestScheduleReleaseServer_OnUnheldID_NoOp`** (defensive; not strictly an AC but cheap.)
   - `ScheduleReleaseServer("nope", 10ms)`.
   - Sleep 30ms.
   - Assert no panic; `Counts()` returns `(0, 0)`.

### Tests not added

- Stop-then-fire interleaving — the pointer-identity check is the test's *premise*; we cover the stale-firing path implicitly via tests 1 and 6.
- Goroutine-leak strict counts — `runtime.NumGoroutine` is flaky under parallel tests; the race detector + the bounded-by-`d` lifetime contract is the actual enforcement.

## Open questions

1. **Method name: `ScheduleReleaseServer` vs `ReleaseServerAfter`.** Picked `ScheduleReleaseServer` for the verb-up-front shape — "schedule a release", with the cancellation-by-re-call semantic implicit in "schedule". `ReleaseServerAfter` reads as a fluent extension of `ReleaseServer` but elides the cancellation primitive. Either is defensible; the spec commits to `ScheduleReleaseServer`. If the developer's gut on reading the call site disagrees, raise it before implementation — renaming after the fact costs nothing here (one consumer in #21) but is annoying.
2. **Should `ScheduleReleaseServer` return `bool` to indicate "armed vs replaced"?** No. The caller never branches on it (#21's call site is a single line in the disconnect handler). The void return matches the AC's wording ("schedules a deferred release ... last call wins") and avoids inviting a check that doesn't add behaviour.
3. **`Counts` during grace window.** Returns the binary as still claimed and phones as still present. Documented above; no change to `Counts` contract. If `/healthz` operators need to distinguish "live" vs "in grace", that's a separate field; not now.

## Done means

- `internal/relay/registry.go` has the `timers` field on `Registry`, initialised in `NewRegistry`; the new `ScheduleReleaseServer` method; the `handleGraceExpiry` helper (unexported); the grace-aware fast path in `ClaimServer`. `ReleaseServer`, `RegisterPhone`, `UnregisterPhone`, `BinaryFor`, `PhonesFor`, `Counts` — unchanged.
- `internal/relay/registry_test.go` has the seven new subtests above. `fakeConn` updated with mutex-guarded `closed` and optional `closeCh`. The race test runs clean under `make test` (`-race`).
- `make vet`, `make test`, `make build` all clean.
- One commit on `feature/20`: `feat(relay): schedule deferred binary release with grace-period reclaim (#20)`.

---

## Security review

### Threat surface for THIS ticket

This ticket extends the registry with a deferred-release mechanism. The registry remains pass-through to adversarial input — `serverID` and `Conn` handles flow in from #16's WS upgrade handler, which validates the header gate. The new attack surface is **timer behaviour under adversarial reconnect patterns**: an attacker who can drive binary disconnects and reclaims rapidly might try to leak goroutines, exhaust memory via the `timers` map, or trick the grace handler into closing phones it shouldn't.

### Categories walked

- **Trust boundaries.** `ScheduleReleaseServer` is called by the WS upgrade handler (#21) when the binary's read loop ends. The handler decides the grace duration (the spec says "30s from #21"); the registry trusts the duration is non-negative and finite. **Finding:** `time.AfterFunc(d, …)` with `d <= 0` fires immediately — that's safe (degenerates to "release now"), not exploitable. With `d` of e.g. `math.MaxInt64`, the timer arms but never fires before the process exits — also safe. No bound needed at the registry layer; the handler picks `30*time.Second` and that's correct.
- **Tokens, secrets, credentials.** N/A — this ticket touches no auth state.
- **File operations.** N/A.
- **Subprocess / external command execution.** N/A.
- **Cryptographic primitives.** N/A.
- **Network & I/O.** The registry has none directly. The grace-expiry handler invokes `Close()` on phone conns — that's a network operation owned by the `WSConn` adapter (which has a 10s `Send` deadline per ADR-0005, but `Close` itself is best-effort). **Finding:** `Close` blocking the AfterFunc goroutine is bounded by `WSConn`'s own behaviour. The registry's responsibility ends at "call `Close` outside the lock," which it does. SHOULD-FIX-elsewhere: if `WSConn.Close()` ever grew an unbounded blocking path, the AfterFunc goroutine would leak — but this is a property of the `Conn` impl, not the registry. Tracked by ADR-0005's existing constraints.
- **Error messages, logs, telemetry.** No new logging. No error returns from `ScheduleReleaseServer`. **Finding:** none.
- **Concurrency.**
  - **Lock ordering.** Single mutex; ordering is trivial.
  - **TOCTOU.** The "stop the timer, replace the entry" sequence in `ClaimServer` and `ScheduleReleaseServer` is atomic under the write lock. The pointer-identity check in `handleGraceExpiry` defends against the one race the lock can't prevent: a timer that has *already fired* (its goroutine started) cannot be `Stop()`'d, but is queued behind us on the lock. When it gets the lock, it finds either no entry (cancelled and replaced) or a different `*time.Timer` (replaced) and bails. **Finding:** correct; verified by tests 1 and 6.
  - **Shutdown safety.** Process exit cancels nothing in `time.AfterFunc` — pending timers are dropped when the runtime tears down. That's fine; no on-disk state.
  - **Goroutine lifecycle.** Each timer firing spawns one runtime-managed goroutine that runs `handleGraceExpiry`, returns. Lifetime ≤ `d` + cleanup time. Cancelled timers (via `Stop()` returning true) never spawn the goroutine. Cancelled-after-fire timers spawn the goroutine but it returns immediately at the pointer-check. **Finding:** no leak path.
  - **Rapid reconnect = `timers` map growth?** Each new `ScheduleReleaseServer` for a given `serverID` `Stop()`'s and replaces the prior entry; the map cap stays at "number of distinct server-ids in grace at this instant." Cleanup: `handleGraceExpiry`'s success path calls `delete(r.timers, serverID)`; `ClaimServer`'s grace fast-path does the same. **Finding:** no unbounded growth.
- **Threat model alignment.** Protocol spec § Authentication → Binary → relay specifies the 30-second grace on the protocol level; this ticket ships the relay's mechanism. Phone-side close code (`1011`) is explicitly out of scope per the ticket's "Out of scope" section and tracked as a follow-up. Out-of-scope deferral named in the spec.

### Adversarial framings considered

- *"Attacker rapidly cycles binary connect/disconnect for a server-id they own — does the registry leak timers or goroutines?"* — Each cycle: claim (cancels timer if present, deletes entry); disconnect (handler calls `ScheduleReleaseServer`, arms a new timer). The map holds at most one entry per server-id. Goroutines from cancelled-after-fire timers run and exit. No leak. Race test (5) hammers this exact pattern.
- *"Attacker arms a grace timer with a huge `d` to keep a server-id occupied indefinitely."* — The attacker is the binary that *just disconnected*. They no longer hold the slot beyond the grace window's reclaim path; the entry is in pseudo-stale state but `ClaimServer` from a competing binary inside the grace window now **succeeds** (it's the reclaim path), so a competing legit binary can take the slot by completing the WS handshake. Outside the grace window, `ClaimServer` succeeds normally (entry was removed on expiry). The attacker does not gain durable squatting power. (Pre-grace squatting via `ClaimServer` and never-disconnecting is a different threat handled by header validation and is out of scope here.)
- *"Attacker triggers a phone disconnect-flood during the grace window to register many phones, then cancels the grace via reclaim — do the phones survive forever?"* — Yes, that's the AC. Phones are bounded only by the per-server cap which is #5's responsibility. The registry already documents this in ADR-0003 and PROJECT-MEMORY's "lifecycle expectations" section. Out of scope here.
- *"Attacker schedules grace for server-ids they don't hold, hoping to mass-spam timer arms."* — `ScheduleReleaseServer` is called by the WS upgrade handler, not by network input directly; an attacker would need to trigger the handler's disconnect path, which requires having connected as that server-id (passing #16's header gate and `ClaimServer`). Even if abused, the registry caps at one timer per server-id and timers stop firing after expiry — no amplification.
- *"`Close()` panics in a phone Conn impl, killing the AfterFunc goroutine."* — The `Conn` contract specifies `Close` is idempotent and safe; a panic is a bug in the impl, not a registry concern. `time.AfterFunc`'s goroutine panicking propagates as a normal Go panic and crashes the process. That's the right behaviour for a contract violation. The registry does not `recover()`; consistent with the rest of the codebase's "trust your interfaces" stance (ADR-0003 § "ConnID is a getter").
- *"What if `ScheduleReleaseServer` is called for a server-id that's also currently held by a normal claim (no prior disconnect)?"* — Allowed; arms a timer that on expiry deletes the binary and closes phones. This is the production happy path — `/v1/server` calls it on every disconnect. There's no way for the registry to distinguish "normal disconnect" from "buggy caller scheduling release on a healthy binary"; both produce the same action. The handler is responsible for only calling `ScheduleReleaseServer` on the disconnect path — documented in #21's spec, not enforced by the registry.

### Verdict: PASS

The new method's API and lock discipline maintain the passive-store invariant from ADR-0003. The pointer-identity stale-firing check closes the only race the lock can't (timer-already-fired). Goroutine and map growth are bounded. No new untrusted-input boundaries are introduced — the registry continues to trust the WS handler at its API surface, and the WS handler's adversarial-input handling is reviewed in #16's spec.

**Findings:**

- [Trust boundaries] No findings — the duration is fully trusted from #21's caller; degenerate values (`d<=0`, very large `d`) are safe.
- [Network & I/O] No findings — `Close()` is invoked outside the lock; bounded by `WSConn`'s own contract. Mentioned for the developer so they don't add a deadline at the registry layer.
- [Concurrency] No findings — pointer-identity stale-firing check covers the one race the lock can't; map cleanup is in both ClaimServer and the expiry handler.
- [Threat model alignment] OUT OF SCOPE — phone-side close code `1011` per protocol spec; tracked as follow-up per AC.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-08
