# Shutdown admission boundary (#184)

## Files read

- `internal/relay/registry.go` → `ClaimServer`, `TakeoverServer`, `RegisterPhoneCapped`, `Snapshot`: one mutex covers admission and snapshots; replacement evicts phones outside it.
- `internal/relay/shutdown.go` → `Shutdown`: independent HTTP shutdown and concurrent WebSocket close fan-out, bounded by context.
- `internal/relay/server_endpoint.go` → `serverHandler`, `takeoverUnresponsive`: upgrade precedes claim; conflict probes run outside the registry lock.
- `internal/relay/client_endpoint.go` → `ClientHandler`: upgrade precedes capped registration, with teardown installed only after admission.
- `internal/relay/shutdown_test.go` → `shutdownFakeConn`, shutdown close/deadline tests: existing close-code and bounded-drain assertions.
- `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_SigtermClosesWSConnsWithGoingAway`: successful dial currently triggers cancellation before admission is observable.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`: sentinel errors, opaque payloads and ownership conventions.
- `docs/knowledge/features/graceful-shutdown.md` → Contract, Sequence, Concurrency model: HTTP shutdown's asynchronous launch cannot prevent late admission of hijacked connections.
- `docs/knowledge/features/connection-registry.md`, `server-endpoint.md`, `client-endpoint.md`: identity-scoped takeover, grace reclaim and stillborn close contracts.
- `docs/architecture.md`, `docs/threat-model.md` → DoS resistance, Log hygiene, Error response leakage; `docs/security-followups.md`: bounded shutdown and fixed close reasons.
- [Protocol mobile](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#binary--relay) → Binary → relay, Connection lifecycle, Security model: header-based registration, native ping/pong, no application acknowledgement.

## Context

An upgraded WebSocket can register after shutdown's only snapshot and escape its close fan-out. This repair establishes one permanent admission boundary for the registry's lifetime and preserves bounded, best-effort drain. No decision record is needed.

Size: one deliverable, three acceptance criteria; estimated 550–650 written lines including plan and tests, four production files, no new exported types/interfaces, no signature migrations, three new admission rejects and two endpoint translations. Analogue relay PR #128 added 545 lines and deleted 32; this repair needs less production code and targeted synchronization fixtures.

Overlap: remote `feature/127` touches `registry.go` and `main_e2e_test.go`; its reclaim changes are already present locally, and these edits are local/additive. No dependency on that branch.

## Design

- Add `ErrRegistryDraining` and a mutex-protected `draining` flag to `Registry`.
- Add unexported `beginDrain() []Conn`: under the write lock, permanently mark draining and snapshot registered handles. Share a lock-requiring snapshot helper with the existing read-only `Snapshot` contract.
- Check draining before any other check or mutation in `ClaimServer`, `TakeoverServer` and `RegisterPhoneCapped`. Rejection leaves binary, phone and grace-timer identity intact and never closes under the lock. `RegisterPhone` inherits the guard through delegation.
- `Shutdown` calls `beginDrain` before launching HTTP shutdown and retains its existing concurrent close fan-out and context/error behavior.
- Both upgraded endpoint handlers translate `ErrRegistryDraining` to `WSConn.CloseWithCode(StatusGoingAway, "shutting down")` and return before installing success cleanup or starting forwarders/heartbeats. A failed conflict probe reaches the same guard through `TakeoverServer`.
- No new payload handling, acknowledgement, dependency, logging key or metric outcome.

## Concurrency model

Admission and drain linearize on `Registry.mu`: an admitted connection is included in the drain snapshot unless independently removed; a subsequent attempt fails. Locks cover only memory operations. HTTP shutdown and per-snapshot closes remain concurrent and context-bounded. A late handler owns its own library-bounded close outside the shutdown wait group; deliberately paused handlers do not delay shutdown completion. Existing handler defers, outbox stop, heartbeat cancellation and identity-scoped release remain unchanged.

## State transitions and identity reuse

| Event | Race-detector coverage |
|---|---|
| Fresh binary upgrade pauses before claim; drain completes; handler resumes | `TestShutdown_LateUpgradeGoingAway/fresh` |
| Grace reclaim of same server-id after boundary preserves incumbent, phones and timer | `TestShutdown_LateUpgradeGoingAway/grace`, `TestRegistry_DrainRejectsAdmission/grace` |
| Takeover attempts reuse held, released or changed incumbent identity after boundary | `TestRegistry_DrainRejectsAdmission/takeover*` |
| Conflict probe starts before boundary and fails after drain completes | `TestShutdown_ConflictProbeDuringDrain` |
| Phone upgrade pauses before registration, including no-server/cap precedence | `TestShutdown_LateUpgradeGoingAway/phone*`, `TestRegistry_DrainRejectsAdmission/phone*` |
| Admission wins before boundary; independent peers close concurrently | `TestShutdown_AdmittedPeersGoingAway`, existing `TestShutdown_ClosesAllConnsWithGoingAway` |
| Repeated admission attempts or snapshots cannot reopen drained registry | `TestRegistry_DrainRejectsAdmission` |
| Unresponsive peer exceeds deadline | existing `TestShutdown_DeadlineExpiryReturnsCtxErr`, `TestShutdown_UnresponsivePeerDeadline` |
| Process SIGTERM follows observable admission | `TestRun_SigtermClosesWSConnsWithGoingAway` |

## Error handling

Draining takes precedence over conflicts, missing binary and phone cap so late upgraded connections receive 1001. Sentinels remain directly returned and handlers use `errors.Is`. Existing close reasons and HTTP error handling remain unchanged. Drain expiry still returns `ctx.Err()` and closes HTTP servers; hijacked peer cleanup remains best-effort and bounded by the WebSocket library.

## Testing strategy

Write regressions first and observe failure before production edits. Use a test-only hijacking ResponseWriter that wraps the socket's handshake write: write the successful upgrade to the peer, signal a channel and block before returning to `websocket.Accept`. Run real `Shutdown` to completion before releasing the handler, then assert wire 1001 and no late installation. Gate a fake incumbent's `Ping` separately to force takeover after the drain boundary. Table tests check reject precedence and identity/timer preservation. Real admitted peers and an unread peer pin close fan-out and deadline behavior.

In the SIGTERM test, start the existing three-second read before cancellation and use a successful native `Ping` round trip to prove the handler reached its post-admission read loop. Retain exit-zero and `drainDeadline + 2s` assertions; add no sleep or timeout increase.

Run targeted race regressions, 100 focused SIGTERM race iterations, `make check` as explicitly required by the ticket/repository, and `go build ./cmd/pyrycode-relay`.

## Open questions

None. Test synchronization stays outside production handlers; no unbounded handler join or registry I/O is introduced.

## Documentation handoff

Pending documentation stage: `docs/knowledge/features/graceful-shutdown.md` § Contract, Sequence and Concurrency model must describe the registry admission/drain boundary, the handler-owned 1001 close for already-upgraded late arrivals, and upgrade success versus observable registry admission. Replace the claim that launching HTTP shutdown prevents post-snapshot admission; retain bounded, best-effort shutdown explicitly.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — header gates remain in `serverHandler` and `ClientHandler`; no payload is inspected, and native ping/pong adds no application acknowledgement.
- [Tokens, secrets and credentials] No findings — opaque phone tokens remain presence-checked and discarded; no new credentials.
- [File operations] No findings — no production disk state or paths added.
- [Subprocesses] No findings — no subprocess added.
- [Cryptography] No findings — TLS and connection-ID randomness unchanged.
- [Network and I/O] SHOULD FIX — a paused upgraded handler must close after drain completion without a shutdown join. Use handler-owned `CloseWithCode` outside the lock and assert it with real peers; preserve existing caps and library close bounds.
- [Errors, logs and telemetry] No findings — fixed 1001 reason, no header/error reflection, no new logging keys or metric labels.
- [Concurrency] SHOULD FIX — draining must precede all admission mutations, including grace reclaim and identity-scoped takeover. Test timer/phone identity preservation and a gated conflict probe, not only fresh claims.
- [Threat model alignment] No findings — closes the observed DoS/shutdown gap with in-memory single-instance state; no new endpoint, dependency or deployment trigger. Existing public admission authentication remains owned by relay #167.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-12

## Revisions

- 2026-10-12: The test upgrade barrier pauses immediately after the underlying `http.Hijacker.Hijack` succeeds; net/http has already flushed the 101 response. This is simpler than wrapping socket writes and still holds `websocket.Accept` before registry admission. The admitted-peer test also blocks a separate close on a channel to prove real peers receive 1001 independently of a stalled close.

- 2026-10-12: `make check` exposed `TestSourceConnCap_FailureAndTeardown/shutdown` in `internal/relay/source_conncap_test.go`, which reused the source reservation by expecting a fresh claim in the same registry after shutdown. Update that case to assert successful upgrade followed by 1001, no fresh claim, and reservation cleanup; other teardown cases still assert fresh admission. This test now covers reuse of a source identity against a permanently drained registry.
