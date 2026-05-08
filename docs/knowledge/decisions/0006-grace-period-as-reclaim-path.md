# ADR-0006: Binary disconnect grace window IS the reclaim path

**Status:** Accepted (#20)
**Date:** 2026-05-08

## Context

The protocol spec ([`protocol-mobile.md` § Authentication → Binary → relay](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#binary--relay)) requires that when a binary's holding connection drops, its server-id slot is held for 30 seconds before being released. The motivation is operational: a transient drop followed by a quick reconnect should land the binary back in the *same* slot with its phones still attached — instead of forcing every phone to receive a no-server close and re-handshake.

The registry built in #3 ([ADR-0003](0003-connection-registry-passive-store.md)) deliberately left grace logic out. `ReleaseServer` is narrow and immediate; orphan phones survive a release. The doc-comment foreshadowed this ticket.

Three shapes were on the table for how the grace window should interact with a *competing* claim during the window:

1. **Block reclaims during grace.** A reclaim within the window returns `ErrServerIDConflict` (or a new `ErrGracePending`). The original binary must wait for the timer to fire, then re-claim cleanly.
2. **Reclaim only by the original binary, by some identity match.** Compare the new conn's identity with the prior one and accept only if they match.
3. **Grace window IS the reclaim path.** Any `ClaimServer` during the window succeeds, replaces the binary `Conn` atomically, cancels the timer, and inherits the phones registered for that slot.

## Decision

**Option 3.** During the grace window:

- The binary entry stays in the registry, pointing at the (now-closed) prior `Conn`. `BinaryFor` returns it; `Send` through it errors per the underlying impl's contract.
- `RegisterPhone` continues to succeed — the slot is still considered claimed.
- `ClaimServer(serverID, conn)` for the same `serverID` returns `nil`, replaces the binary `Conn`, and `Stop()`s the pending timer. Phones registered during the grace window are inherited by the new binary.

If the timer fires (no reclaim within the window), the binary entry is removed *and* every phone for that server-id is removed and `Close()`-called outside the registry's lock.

The new method is `ScheduleReleaseServer(serverID, d time.Duration)`. The synchronous `ReleaseServer` is unchanged. Adopted in `internal/relay/registry.go` (#20). The `/v1/server` handler call-site swap lives in #21.

## Rationale

### Why not block reclaims (option 1)

The protocol spec explicitly motivates the grace window with "a quick reconnect should land the binary back in the same slot." A 30-second hard block contradicts that goal: a binary that disconnects and immediately retries — the canonical fast-reconnect case — would receive `4409` and have to back off for 30 seconds before its slot becomes available. That's worse for the user than the no-grace baseline.

### Why not identity-match reclaims (option 2)

The relay has no durable identity for a binary. The `x-pyrycode-server` header is just a string; the relay cannot distinguish "the same binary reconnecting" from "a different binary with the same configured server-id." Any identity check would either reduce to the header (which is already the lookup key) or require new credential plumbing. The protocol spec doesn't mandate identity verification at this layer; that's the binary's auth concern.

The pragmatic answer: anyone who can pass the `/v1/server` header gate ([#16](../features/server-endpoint.md)) is already authoritative for that server-id. If a competing claim arrives during a grace window, treating it as the legitimate reclaim is the same trust model as the pre-grace baseline (where claims succeed once the slot is free). The grace window doesn't *expand* the trust surface.

### Why option 3 is structurally clean

- **No new error sentinel.** The grace path produces the same return shape as a normal claim (`nil`). Callers don't branch.
- **Phones survive a reclaim with no separate dance.** The registry already keeps phones across `ReleaseServer` (the orphan-phones invariant from ADR-0003). The grace path simply doesn't run the cleanup that the timer would have run; phones land on the new binary by virtue of never having been removed.
- **`Counts` stays meaningful.** A server-id in its grace window contributes `1` to the binary count and its live phone count — matching the operational mental model ("we still consider this binary connected; we are about to find out").

### Pointer-identity stale-fire defence

`time.AfterFunc(d, fn)`'s `Stop()` returns false if the timer has already fired *or* its `fn` is currently executing. The "currently executing" case is the race: another goroutine takes the registry lock first (e.g. `ClaimServer`'s grace fast path), `Stop()`s the timer (returns false because `fn` already started), replaces the entry, and returns. The `fn` is meanwhile blocked on the lock. When it acquires the lock, naïvely it would delete the binary entry and close the new claimant's phones.

The defence: every armed timer is wrapped in a `*graceEntry` and stored in `r.timers[serverID]`. The `time.AfterFunc` closure captures the `*graceEntry` pointer. On fire, the handler checks `r.timers[serverID] == self` under the lock — if `ClaimServer` (or a later `ScheduleReleaseServer`) replaced the entry, the pointer no longer matches and the handler returns without touching state.

The wrapper indirection (`graceEntry` rather than `*time.Timer` directly) is so the `AfterFunc` closure can capture a stable pointer at construction; capturing `*time.Timer` requires the local var to be assigned *after* `AfterFunc` returns, which both creates a self-reference and trips the race detector under stress.

### Why the duration is a parameter, not a constant

The protocol spec says 30 seconds; the handler in #21 passes `30*time.Second`. Tests use ms-scale durations to keep the suite fast. Pushing the constant into the registry would force tests to either wait 30s or expose a knob — both worse than just taking it as an argument. The registry trusts the duration; degenerate values (`d <= 0` fires immediately, `d` near `MaxInt64` never fires) are safe.

## Consequences

- **The reclaim path is opt-in for callers.** `ReleaseServer` (immediate) is still there for the "claim failed, clean up" use cases that don't want a timer. Only the disconnect path in `/v1/server` (and equivalents) calls `ScheduleReleaseServer`.
- **Phones registered during a grace window have asymmetric outcomes.** Reclaim → they survive on the new binary. Expiry → they get `Close()`d alongside the original phones. Documented in the registry doc-comments and the feature doc; bounded only by per-server caps (#5's concern).
- **The expiry handler is the only goroutine the registry spawns.** Lifetime is bounded by `d`; the `Stop()` + pointer-identity check rules out leaks. The registry itself is still passive — the goroutine is `time.AfterFunc`'s, not a long-running registry worker.
- **Phone-side close code on grace expiry is `1000`, not protocol's `1011`.** ADR-0005 fixes `WSConn.Close()` to `StatusNormalClosure`. Adopting `1011 "binary did not reconnect"` requires extending the `Conn` interface or adding a parallel close primitive. Phones treat any close as "reconnect" — user-visible behaviour is correct. Tracked as a follow-up.
- **`Counts` does not distinguish "live" from "in grace."** Operators reading `/healthz` see the binary as still claimed during the grace window. If a future ticket needs grace-window metrics, that's a separate field on `Counts` (or a sibling).
- **`ScheduleReleaseServer` on an unheld server-id is a defensive no-op.** It arms a timer that, on fire, deletes whatever happens to be present (nothing). Same shape as `ReleaseServer("unknown")` returning `false`. Documented to head off a "binary not found" return value.

## Related

- [ADR-0003: Connection registry as a passive store](0003-connection-registry-passive-store.md) — the orphan-phones invariant that makes option 3 work.
- [ADR-0005: Application WS close codes](0005-application-close-codes-via-underlying-conn.md) — why `Close()` on grace expiry emits `1000` rather than the protocol's `1011`.
- [Connection registry feature](../features/connection-registry.md) — current API surface including `ScheduleReleaseServer`.
