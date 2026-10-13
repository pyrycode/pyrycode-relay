# SIGTERM close during an in-flight write

## Files read

- `internal/relay/ws_conn.go` — `Send`, `CloseWithCode`: writes inherit the context cancelled before the close handshake.
- `internal/relay/ws_conn_test.go` — `startEcho`, existing concurrent-send and close assertions.
- `internal/relay/phone_outbox.go` — `run`, `deliver`, `CloseWithCode`: an in-flight phone write delegates to the same adapter; its failure path closes through the same one-shot guard.
- `internal/relay/client_endpoint.go` — `ClientHandler`, `notifyPhoneClosed`: handler cleanup and close notices must retain their existing contracts.
- `internal/relay/forward.go` — `StartPhoneForwarder`: phone traffic also writes to the binary adapter.
- `internal/relay/shutdown.go` — `Shutdown`: concurrent 1001 close fan-out and bounded drain.
- `cmd/pyrycode-relay/main_e2e_test.go` — `TestRun_ProcessSIGTERMCompleteShutdown`, `TestRun_ProcessSIGTERMHelper`: real signal, ongoing opaque traffic and joined authentication proof.
- Pinned `github.com/coder/websocket` module — `setupWriteTimeout`, `writeFrame`, `Close`: cancelling an active write runs a callback that closes the transport, whereas `Close` serializes its control frame against a data-frame write and retains its own handshake bounds.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md` — conventions and feature ownership.
- `docs/knowledge/features/graceful-shutdown.md`, `docs/knowledge/features/ws-conn-adapter.md` — admission versus dial readiness, concurrent close and context contracts.
- `docs/architecture.md`, `docs/threat-model.md` (DoS resistance, log hygiene), `docs/security-followups.md` — content blindness, bounded shutdown and existing deferrals.
- [Wire lifecycle and security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#connection-lifecycle) — existing reconnect behavior; no new acknowledgement or message shape.
- `$AGENTS_REPO_PATH/docs/working-practice.md`, `$AGENTS_REPO_PATH/builder/security-review.md` — workflow and adversarial checklist.

## Context

At baseline `72f40c5`, an observed process shutdown under traffic returned phone EOF instead of wire close 1001. The adapter currently cancels an active `Write` before asking the library to send the close frame. The library's cancellation callback closes the underlying transport, so it can destroy the socket before that frame is written. A synchronized test must establish this causal ordering before production changes. No decision record is needed for this local context correction.

Size: one deliverable (preserve real shutdown close under concurrent writes), three acceptance criteria, approximately 300 written lines including plan/tests, zero exported types, zero caller signature changes, one new rejection guard. Analogue #136 / PR #138 wrote 115 lines; the extra work here is transport synchronization and diagnosis. Fetch/branch comparison found no overlap in the planned adapter/test files.

## Design

Limit production edits to `WSConn` in `internal/relay/ws_conn.go`. Keep `closeCtx` as the close-start marker and keep `closeOnce` choosing the first close code. Under `writeMu`, `Send` refuses if closing has begun, then starts its existing ten-second write deadline from `context.Background()` rather than the close marker. An admitted write can finish without close cancellation destroying its transport. A check/close race is safe: the library serializes data and control frames, and refuses data after a sent close. `CloseWithCode` still marks closing first and invokes the library handshake immediately, without acquiring `writeMu` or adding handler joins.

Do not change outbox queues, envelope routing, close notices, admission, protocol shapes, deadlines, or process authentication cleanup. Update adapter comments to reflect the actual close/context contract. No dependencies or log keys are added.

## Concurrency model

`Send` retains its per-adapter mutex and per-call timeout. `CloseWithCode` keeps its one-shot guard and does not take that mutex. The library's frame mutex orders an active frame and close control frame. Close's own write/handshake bounds and the outer drain deadline still apply to stalled peers. New sends see the cancelled close marker and return an error without invoking the library. No production goroutine is added.

Test-only transport gating holds one data write before bytes are sent, confirms it has entered transport I/O, then starts shutdown. A bounded negative assertion checks the held write's transport is not cancelled at close start; this measures absence of the diagnosed destructive action, not readiness via a sleep. Releasing the gate then requires an intact frame and the actual 1001 close. All readers, writers and close goroutines are joined or force-unblocked in cleanup.

## State transitions and identity reuse

| Event | Race-detector proof |
| --- | --- |
| Shutdown begins while adapter Send is in transport I/O | `TestShutdown_InFlightWriteGoingAway` (direct binary adapter) |
| Shutdown begins while phone outbox delivers a frame | `TestShutdown_InFlightWriteGoingAway` (phone outbox) |
| A Send starts after close begins | `TestShutdown_InFlightWriteGoingAway` and existing `TestWSConn_SendAfterClose_ReturnsError` |
| Close is called again through cleanup | `TestShutdown_InFlightWriteGoingAway`, existing `TestWSConn_DoubleClose_DoesNotPanic` |
| Real SIGTERM under bidirectional traffic and shared refresh waiters | existing `TestRun_ProcessSIGTERMCompleteShutdown` |

No routing identity allocation or reuse changes; existing registry lifetime tests retain ownership of that contract.

## Error handling

Send after close starts returns a closed-connection error. Active writes still fail on their own deadline or transport error. Close-handshake failures remain best effort, as before; drain deadline and HTTP force-close behavior stay unchanged. An independently failed or stalled peer is not promised a successful close handshake.

## Testing strategy

Write the transport-gated regression first and run it on unchanged production code: require failure caused by transport close during the held data write, with real 1001 assertions retained. Exercise both a registered binary adapter and a registered phone outbox. Use an in-process httptest server, real WebSocket dial, bounded contexts and channel barriers; no retries, EOF acceptance, readiness sleeps or relaxed cleanup assertions.

Keep the process test unchanged, including admission probes, six refresh waiters, one OAuth/no FCM request, authentication join and 25-second exit budget. Record baseline focused stress including failures. After the fix run focused race checks, two complete uncached `go test -race -count=1 ./...` runs, `make vet`, the binary build and the repository-required `make check` before push. Preserve every executed failure in the PR. The verifier owns its subsequent gate.

## Open questions

None for the proposed contract. The pre-fix regression must confirm the cancellation cause; any different observed cause requires a revision before production changes.

## Documentation handoff

- Pending documentation stage: `docs/knowledge/features/graceful-shutdown.md`, **Testing** — causal ordering and what the synchronized regression proves.
- Pending documentation stage: the same feature's **Contract** / **Concurrency model**, and `docs/knowledge/features/ws-conn-adapter.md`'s close/context description — close-start rejects new sends without cancelling an active write; the library's bounded handshake owns termination.
- Describe local offline evidence without claiming Fly deployment or a universal shutdown guarantee.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] `WSConn.Send` accepts opaque bytes exactly as before; no payload interpretation, header handling or routing decision changes.
- [Tokens, secrets and credentials] No credential path changes; existing process tests use synthetic local authentication and preserve opaque token handling.
- [File operations] No runtime disk state is introduced; only the plan and code/tests are written.
- [Subprocesses] No production subprocess is added; the existing hermetic process helper and unconditional child cleanup remain intact.
- [Cryptography] No cryptographic changes or new dependencies.
- [Network & I/O] SHOULD FIX — preserve `writeTimeout` for active writes and leave `CloseWithCode` outside `writeMu`, so hostile slow peers cannot make close wait on an unbounded adapter lock. Existing read caps, read deadlines, public HTTP timeouts and ten-second drain remain unchanged.
- [Errors, logs and telemetry] No new logs, metrics or public response bodies. Send rejects with a fixed closed-connection error; close reason remains fixed and contains no secrets.
- [Concurrency] SHOULD FIX — check close-start under `writeMu` before a new write, and cover held active writes plus post-close sends in the regression. Retain `closeOnce` so outbox failure/handler cleanup cannot replace a shutdown close already chosen.
- [Threat model alignment] This repairs an observed close cancellation race within the existing DoS bounds. No new endpoint, dependency, replica assumption or deployment change trips a threat-model trigger. Deferred raw-source and trusted-PROXY hardening remain owned by the existing security-followups catalog.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-13
