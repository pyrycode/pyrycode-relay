# ADR-0006: Binary disconnect grace window IS the reclaim path

**Status:** Accepted (#20), amended 2026-09-15 (#127): phones are no longer inherited across a reclaim; amended 2026-09-24 (#112): a caller can also trigger the reclaim path by liveness probe, without a grace window. See the amendment sections at the end.
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

## Amendment 2026-09-15 (#127): phones are closed at reclaim, not inherited

**What changed.** When `ClaimServer` succeeds during a pending grace window it now removes every phone registered for that server-id under the lock and closes each one with WebSocket close code `4404` outside the lock, one goroutine per phone, through the same `CloseWithCode` type-assertion seam that `Shutdown` uses. The slot reclaim itself is unchanged: any claim during the window still wins, the timer is still cancelled, `RegisterPhone` still succeeds during the window, and expiry still evicts everything. The no-grace `ClaimServer` path and `handleGraceExpiry` are untouched.

**Why the "phones still attached" rationale no longer holds.** Option 3 was chosen so a quick binary reconnect would keep its phones without a re-handshake. Under Mobile Protocol v2 every phone conn carries a Noise session that lives in the daemon process. The relay is content-blind and cannot tell a same-process reconnect from a restarted daemon, but only the same-process case keeps those sessions alive. A restarted daemon has no session for an inherited conn id, so the phone's next `noise_msg` is answered with a fatal `4421` and the client stops re-dialling. Observed twice against production on 2026-09-15, daemon restarts at 18:19 and 20:53 Helsinki time: the desktop client's relay socket survived the restart, sent `noise_msg` on the inherited conn, got `4421` three times in a row, and showed "Pairing error - Re-pair" until a human restarted it. A re-handshake costs one round trip plus microseconds of crypto and the daemon replays missed events from `hello.last_event_id`. A stranded client is dead until restarted. The trade is no longer close.

A phone that registers during the grace window is in the same position: its `noise_init` went to the closed prior binary conn and it is waiting for a `noise_resp` that never arrives. Evicting it at reclaim is what gets it a live handshake.

**Why close at reclaim rather than at release.** Closing at release would leave the window open for a phone to re-dial, register against the closed conn, send `noise_init` into it, and hit its own 10-second `noise_resp` timeout, which is a fatal `4421` on the client side as well. Reclaim is the first moment a live binary exists for the phones to land on.

**Why `4404`.** It is the code `/v1/client` already emits when no binary holds the slot, and every client already classifies it as "relay reachable, daemon absent" and retries with its normal backoff. IANA `1012` Service Restart would have been semantically closer but no client classifies it, so it would have been a new fatal-by-default path. The reason string is `binary reclaimed server-id` so relay logs and client debug output can tell the two `4404` sources apart.

**Consequences.**

- The "phones survive a reclaim with no separate dance" and "asymmetric outcomes for phones registered during grace" points above are historical. Both outcomes now converge on eviction; only the close code differs (`4404` at reclaim, `1000` at expiry).
- `Counts` drops the evicted phones synchronously with the reclaim, so the connection-count gauge stays honest.
- The daemon keeps stale `V2Session` entries for the evicted conn ids until its idle sweep, which sends a `4408` for a conn the relay no longer has. The wire spec documents that as a harmless no-op.
- Two backstops are deliberately out of scope and may get their own tickets: the daemon answering a `noise_msg` on an unknown conn with a retryable close instead of `4421`, and the desktop client bounding `4421` on an already-authenticated session the way the daemon bounds `4409`.
- The wire spec's close-code table in the pyrycode repo should gain a sentence on its `4404` row saying the relay also sends it to phones evicted by a binary reclaim.

Implementation notes: [codebase/127.md](../codebase/127.md).

## Amendment 2026-09-24 (#112): a second, probe-driven route into the same reclaim path

**What changed.** Option 3's reclaim semantics — cancel any pending timer, swap the binary, evict phones with `4404` — are no longer reached only through `ClaimServer` during an active grace window. A new method, `Registry.TakeoverServer(serverID, incumbent, conn)`, reaches the identical behaviour (implemented as one shared `replaceBinaryLocked` helper, so there is exactly one reclaim implementation) from a different trigger: the `/v1/server` handler, on a conflicting claim, pings the incumbent for up to 10s before deciding it's unresponsive. If it doesn't answer, `TakeoverServer` hands the slot to the new claim — whether or not any grace timer happens to be pending — and the handler closes the displaced incumbent with `1011 "heartbeat timeout"`. See [`/v1/server`](../features/server-endpoint.md) and [Connection registry § Probe-driven takeover](../features/connection-registry.md).

**Why this needed a new entry point rather than reusing `ClaimServer`.** `ClaimServer`'s grace branch trusts that a pending timer for `serverID` implies the incumbent is gone — the timer only exists because the disconnect path already ran. A probe-driven takeover has no such timer: the incumbent may still be fully registered, mid-heartbeat, with nothing yet having detected it as dead. `TakeoverServer` therefore takes the incumbent `Conn` explicitly and checks *that specific conn* still holds the slot, rather than inferring liveness from timer presence. This identity check is also what keeps a takeover safe against a race: if two claimants both probe the same dead incumbent, the first to reach `TakeoverServer` swaps the binary, and the second sees a different current holder and gets `ErrServerIDConflict` instead of overwriting the winner.

**The hazard this amendment closes.** A displaced incumbent's own handler is still running — it entered `ServerHandler` normally, and its forwarder's `Read` eventually errors once the handler closes it, which runs the same disconnect defer every binary handler runs. Before #112 that defer called the unscoped `ScheduleReleaseServer(serverID, grace)` unconditionally. Fired 30s later, that timer would delete whatever is in `binaries[serverID]` — by then, the *new* binary that displaced it, not the empty slot the old code assumed. The fix, `ScheduleReleaseServerIfHeld(serverID, conn, d) bool`, checks under the same lock that `conn` is still the current holder before arming the timer; for a displaced conn it's a no-op. Every route that reassigns `binaries[serverID]` while a timer is pending — the pre-existing `ClaimServer` grace branch and the new `TakeoverServer` alike — deletes that timer entry via `replaceBinaryLocked`, so the pointer-identity stale-fire defence above continues to be the only guard expiry needs; no second guard was added.

**Consequences.**

- A crash-restarted daemon whose predecessor dropped silently (no FIN/RST — power loss, NAT timeout) no longer waits for the heartbeat (up to 60s) to reclaim its `serverID`; the very next conflicting claim reclaims it within the probe timeout (≤10s).
- A live incumbent is never displaced by this path: the probe only proceeds to `TakeoverServer` once the incumbent has failed to answer within the timeout, and the handler treats a request-context cancellation (server shutdown) during the probe as "inconclusive," not "dead."
- The "Squatting on a serverID" threat entry in [`server-endpoint.md`](../features/server-endpoint.md) is updated: the window a squatter can hold a slot after going silent only shrinks, it does not grow, and a squatter that keeps answering probes is unaffected.
- One residual risk was recorded rather than fixed: an incumbent's pong is only processed while its forwarder sits in `Read`, and a synchronous `phone.Send` to a stalled phone can hold that `Read` for up to `writeTimeout` (10s) — so a *live* incumbent stalled on a slow phone can, in principle, also miss the probe and be displaced. Tracked as issue #140 and in `docs/threat-model.md` § DoS resistance.

Implementation notes: [codebase/112.md](../codebase/112.md).

