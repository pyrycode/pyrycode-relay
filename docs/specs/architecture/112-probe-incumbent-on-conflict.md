# #112 — Probe an unresponsive incumbent on a server-id conflict

## Files read

- `internal/relay/registry.go` → `ClaimServer`, `ScheduleReleaseServer`, `handleGraceExpiry`, `closeWithCode`, `reclaimCloseCode`. The grace-reclaim path (#127) that a takeover mirrors, and the pointer-identity stale-fire guard on `r.timers`.
- `internal/relay/server_endpoint.go` → `ServerHandler`. The conflict branch (4409 on the stillborn conn) and the disconnect defer that calls `ScheduleReleaseServer`. That defer is the hazard the ticket names.
- `internal/relay/heartbeat.go` → `runHeartbeat`, `heartbeatInterval`, `heartbeatTimeout`. These stay unchanged. The probe reuses the heartbeat's close code and reason for an unanswered ping (1011 `"heartbeat timeout"`).
- `internal/relay/ws_conn.go` → `WSConn.Ping`, `WSConn.CloseWithCode`, `writeTimeout`. The probe primitive, and the Send bound that limits how long a live incumbent's pong can be delayed (see Security review).
- `internal/relay/forward.go` → `StartBinaryForwarder`. This is the incumbent's only reader, so pongs are processed only while it sits in `Read`. It calls `phone.Send` synchronously, and each call can block for up to `writeTimeout`.
- `github.com/coder/websocket` v1.8.14 `Conn.ping`. It supports concurrent pings through `activePings`, and on ctx expiry it returns an error without closing the conn.
- `internal/relay/server_endpoint_test.go` → `startServer`, `dialWith`, `validHeaders`, `TestServerEndpoint_DuplicateClaim_4409`. The harness the new endpoint tests extend.
- `internal/relay/metrics_upgrade_test.go` → subtest `reject_409`. Its incumbent never reads either (see Testing strategy).
- `internal/relay/registry_test.go` → `fakeConn`, `codedFakeConn`, `TestClaimServer_ReclaimDuringGrace_EvictsPhonesWith4404`. The fakes the registry tests reuse.
- `internal/relay/push_wake_test.go` → `lockedBuffer`. A race-safe log sink for observing `server_released`.
- `cmd/pyrycode-relay/main_e2e_test.go` → `dialServerUntilClaimed`. It already tolerates a 4409 or a successful claim during a reconnect, so it is unaffected.
- `docs/knowledge/features/connection-registry.md` § "Grace-period reclaim", § "Pointer-identity stale-fire defence". A takeover must cancel the pending timer under the same lock so the existing guard covers expiry.
- The protocol spec, `protocol-mobile.md` § Authentication → Binary → relay. The daemon retries 4409 for ≥ ~73s, and 1011 is a generic "server error" close. No new close code or message shape is needed.

## Context

On a silent drop the relay keeps the dead binary in `binaries` until the heartbeat notices, which takes up to 60s. Until then, a crash-restarted daemon's claim gets `4409`. The fix is an on-demand liveness probe on conflict: ping the incumbent with a bounded timeout. If it answers, the claim still gets 4409. If it doesn't, the new binary takes the slot with grace-reclaim semantics.

This is a second route into the ADR-0006 reclaim semantics. The documentation stage should amend ADR-0006 rather than write a new ADR. See Documentation handoff.

## Design

### Registry (`registry.go`)

- **`TakeoverServer(serverID string, incumbent, conn Conn) error`** (new, exported). Under `mu`:
  - If `binaries[serverID]` is held by a conn `!= incumbent`, return `ErrServerIDConflict` and touch nothing. This makes the takeover identity-scoped: a concurrent reclaim or another prober's takeover wins, and the loser gets 4409.
  - Otherwise (held by `incumbent`, or empty): cancel and delete any pending grace timer for `serverID`, set `binaries[serverID] = conn`, and snapshot-and-delete `phones[serverID]`. Outside the lock, close each evicted phone with `reclaimCloseCode`/`reclaimCloseReason`, one goroutine per phone. This is exactly the ClaimServer grace path.
  - `incumbent == nil` means "the slot was empty when I looked". It is still refused if someone has claimed it since.
  - The registry does not close `incumbent`. The caller owns that, because the close code is a handler concern.
- **Shared helper** `replaceBinaryLocked(serverID string, conn Conn) []Conn`. It does the timer cancel, the binary swap and the phone snapshot-delete, and it is used by both `ClaimServer`'s grace branch and `TakeoverServer`. There is then one implementation of the reclaim semantics.
- **`ScheduleReleaseServerIfHeld(serverID string, conn Conn, d time.Duration) bool`** (new, exported). Under `mu`: if `binaries[serverID] != conn`, return false and arm nothing. Otherwise, arm the grace timer exactly as `ScheduleReleaseServer` does and return true. `ScheduleReleaseServer` and this method share an `armGraceLocked(serverID, d)` helper. The unscoped `ScheduleReleaseServer` stays as it is: it has 18 call sites across 5 test files, so changing its signature would trip the fan-out boundary, and nothing in production calls it after this change.
- **Expiry needs no change.** Every replacement of `binaries[serverID]` while a timer is pending (the `ClaimServer` grace branch, `TakeoverServer`) deletes the timer entry under the same lock. The existing pointer-identity guard in `handleGraceExpiry` therefore no-ops any stale fire. With the scoped schedule, a timer is armed only while its conn holds the slot, and it is removed whenever the slot changes hands. So an expiry can never delete a different conn's slot. AC3's test proves this end to end.

### Handler (`server_endpoint.go`)

- `ServerHandler` keeps its signature (9 call sites) and delegates to an unexported `serverHandler(reg, logger, grace, probeTimeout, maxFrameBytes, metrics)`, passing `conflictProbeTimeout`. Tests call `serverHandler` with a short timeout.
- `conflictProbeTimeout = 10 * time.Second`. This is the ticket's maximum, chosen because a live incumbent's pong can be delayed by one synchronous `phone.Send` in its forwarder, which is bounded by `writeTimeout` (10s). A shorter probe would displace live binaries more easily. It is still 6× faster than the 60s it replaces.
- The conflict path becomes:
  1. `ClaimServer` returns `ErrServerIDConflict`, and the handler calls `takeoverUnresponsive(ctx, reg, serverID, wsconn, probeTimeout)`.
  2. `takeoverUnresponsive` reads the incumbent with `BinaryFor`. If it is held and implements a small `pinger` interface (`Ping(ctx) error`; `*WSConn` does), it pings under `context.WithTimeout(r.Context(), probeTimeout)`. This runs outside any registry lock.
     - The ping succeeds → return `ErrServerIDConflict`. The existing 4409 branch runs unchanged: same log, metric, code and reason.
     - The ping fails and `r.Context()` is done (server shutdown) → return that ctx error. The handler's defensive close path runs. Shutdown is never read as a dead peer.
     - The ping fails otherwise → `reg.TakeoverServer(serverID, incumbent, wsconn)`. On success, `go closeWithCode(incumbent, websocket.StatusInternalError, "heartbeat timeout")`, the same code and reason `runHeartbeat` sends. It runs in a goroutine because a close handshake with a dead peer blocks.
     - An incumbent that is not a pinger (test fakes only) → `ErrServerIDConflict`. That is the conservative choice.
     - Not held (freed between the two calls) → `TakeoverServer(serverID, nil, wsconn)`.
  3. On a nil error, log `server_id_takeover` with `server_id` and `remote` (both already allowlisted), then fall through to the normal accept path (`ServerAccept`, `server_claimed`, heartbeat, forwarder).
- The disconnect defer calls `ScheduleReleaseServerIfHeld(serverID, wsconn, grace)` instead of `ScheduleReleaseServer`. For a displaced handler this is a no-op, so the stale grace timer that would have deleted the new binary 30s later is never armed.

### Data flow (takeover)

```
new claim ─ClaimServer→ ErrServerIDConflict
          ─BinaryFor→ incumbent ─Ping(≤probeTimeout)→ timeout
          ─TakeoverServer(s, incumbent, new)→ [lock: cancel timer, swap binary, take phones] → phones ←4404
          ─go closeWithCode(incumbent, 1011)
incumbent handler: forwarder Read errors → defer ScheduleReleaseServerIfHeld(s, incumbent) → false (no timer)
```

## Concurrency model

- The probe runs on the new claimant's handler goroutine, outside `r.mu`, and is bounded by `probeTimeout` and `r.Context()`. Concurrent pings on the same conn are supported by the library (`activePings`), so the probe and `runHeartbeat` can overlap.
- Atomicity: the identity check and the swap in `TakeoverServer` happen under one write lock. If two claimants both probe the same dead incumbent, the first swaps and the second sees `binaries[s] != incumbent` and gets 4409.
- New goroutines: one `closeWithCode` for the displaced incumbent, and one per evicted phone (already true for reclaim). Each ends when its close handshake completes or times out inside the library.
- No new locks. Lock order is unchanged.

## Error handling

- Probe answered → 4409 (existing behaviour).
- Probe cancelled by shutdown → defensive `wsconn.Close()` with no log. The comment in the defensive branch is updated to name this case.
- Takeover lost to a concurrent claimant → 4409.
- Incumbent close errors are ignored, as they are everywhere else.

## Testing strategy

Registry (`registry_test.go`), with fakes:
- A takeover of an idle incumbent replaces the binary, evicts phones with 4404 (`codedFakeConn`), and does not close the incumbent.
- A takeover whose incumbent is no longer the holder (another conn has claimed) returns `ErrServerIDConflict`, and the binary and phones are untouched.
- A takeover while the incumbent is gracing cancels the timer: after 3×grace the new binary still holds the slot.
- `ScheduleReleaseServerIfHeld` for a displaced conn returns false, and after 3×grace the new binary still holds the slot. For the holder it returns true and the release fires.

Endpoint (`server_endpoint_test.go`), real dials through `serverHandler` with `probeTimeout = 200ms` and `grace = 100ms`:
- **AC1:** incumbent `c1` never reads, and a fake phone is registered. Dial `c2`. The registry's binary becomes `c2`'s conn within about the probe timeout, the phone gets 4404, and a `c1.Read` returns a close error with code 1011.
- **AC2:** `TestServerEndpoint_DuplicateClaim_4409` keeps passing. Its `c1` gains a `CloseRead` so it answers pings; without it, that client is by definition unresponsive. `reject_409` in `metrics_upgrade_test.go` gets the same one-line change. A new assertion adds a registered phone and checks that it stays open.
- **AC3:** after the AC1 takeover, wait for the displaced handler's `server_released` log line (`lockedBuffer` logger), then sleep 3×grace and assert `c2` still holds the slot. Before the fix this reddens because the unscoped schedule deletes `c2`.

## Open questions

- None blocking. The value of `conflictProbeTimeout` is decided above (10s).

## Documentation handoff (pending, for the documentation stage)

- `docs/knowledge/features/server-endpoint.md`: the conflict path now probes the incumbent (`conflictProbeTimeout`, 10s). Update the close-code table note for 4409 (sent only when the incumbent answers the probe), and the "Squatting on a serverID" threat entry (per the ticket and the Security review below). Also cover the new `server_id_takeover` log event and the fact that a displaced incumbent is closed with 1011 `"heartbeat timeout"`.
- `docs/knowledge/features/connection-registry.md` and ADR-0006: `TakeoverServer` is a second route into the reclaim semantics, `ScheduleReleaseServerIfHeld` makes releases identity-scoped, and the reason expiry needs no guard of its own.
- `docs/threat-model.md`: the pong-starvation residual risk (Security review, Concurrency) should be named there too.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings. The only untrusted input is the `X-Pyrycode-Server` header, already presence-checked by the header gate in `ServerHandler` before `websocket.Accept`. The probe is an RFC 6455 control frame and reads no payload. Inner frames stay `json.RawMessage` in `StartBinaryForwarder`, which is untouched.
- [Tokens, secrets] No findings. `/v1/server` carries no token. The new log line carries only `server_id` and `remote`, and deliberately omits `binary_version`, matching the conflict log's version-oracle rationale.
- [File operations / subprocess / crypto] Not applicable: no file, subprocess or crypto surface is added. The conn id still uses `randHex8` (`crypto/rand`).
- [Network & I/O] No findings. Anyone who knows a server-id can trigger one probe per upgrade. That upgrade is already behind the per-IP token bucket (`rateLimit` in `cmd/pyrycode-relay/main.go`). Each probe costs one ping frame and holds one handler goroutine for at most `conflictProbeTimeout`, which is no worse than today's upgraded-then-closed conflict. There is no amplification: one claim, one ping. A live binary answers and the attacker gets 4409, as today. A dead binary could already be taken over after detection plus grace (≤ 90s), and now it takes ≤ 10s, so the takeover window only shrinks.
- [Concurrency] No findings on atomicity. The identity check and swap in `TakeoverServer` happen under one lock, so a concurrent reclaim, release or second prober is never overwritten. The displaced handler's release is identity-scoped (`ScheduleReleaseServerIfHeld`), which closes the stale-timer hazard the ticket names. Expiry is covered by the existing pointer-identity guard, because every swap deletes the timer under the lock. Shutdown during a probe is detected through `r.Context().Err()` and is never read as a dead peer.
- [Concurrency] OUT OF SCOPE (residual risk, recorded). The incumbent's pong is processed only while `StartBinaryForwarder` sits in `Read`. A synchronous `phone.Send` to a stalled phone blocks it for up to `writeTimeout` (10s), and several stalled phones in sequence block it for longer. A *live* incumbent in such a stall can miss a 10s probe and be displaced by a concurrent second claim. After that it reconnects into 4409 against the new holder. An attacker cannot induce the stall without a phone session the daemon accepts, because an unauthenticated phone receives only a small reject frame that fits in the socket buffers. So the exposure is a live binary with authenticated slow phones meeting a genuine duplicate binary, which is itself an operator misconfiguration. The heartbeat has the same pong-starvation property with a 30s margin. The structural fix is to stop pong handling from depending on the forwarder: per-phone send queues, or a Send that does not block the read pump. I will file that as a relay follow-up ticket in Phase B and name it in the PR.
- [Errors, logs] No findings. The close reasons are fixed strings (`"heartbeat timeout"`, `reclaimCloseReason`, the existing 4409 reason). No header value or `err.Error()` reaches the wire. The only new log key usage is `server_id` and `remote`, both allowlisted. `TestLogKeysAreAllowlisted` enforces this.
- [Threat model alignment] The "Squatting on a serverID" entry changes: a squatter still cannot displace a binary that answers pings. This goes in the Documentation handoff. No new endpoint, dependency or deploy target, so no re-review trigger. The design stays single-instance, and all state is in the in-process registry.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
