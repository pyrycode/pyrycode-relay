# Canonical invalid-token feedback

## Files read

- `internal/relay/envelope.go` → `parsePushWake`, structural wake validation and opaque routing envelopes.
- `internal/relay/forward.go` → `StartBinaryForwarder`, sole production Request consumer and binary lifetime.
- `internal/relay/push_wake.go` → `Request`, `send`, `expireJobsLocked`, `Close`; admission, retry and retained-job ownership.
- `internal/relay/ws_conn.go` → `Send`, `CloseWithCode`; writer serialization and graceful active-write behavior.
- `internal/relay/server_endpoint.go` → `serverHandler`, replacement closes the original socket.
- `internal/relay/shutdown_write_test.go` → `TestShutdown_InFlightWriteGoingAway`, ordinary writes must finish before graceful close.
- `internal/relay/push_wake_coalesce_test.go`, `push_retry_test.go`, `forward_test.go` → existing held senders, clocks and forwarding harnesses.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md` → conventions and knowledge map.
- `docs/knowledge/features/push-wake-dispatch.md` → fixed coalescing window, atomic budgets, five outcome labels and joined shutdown.
- `docs/knowledge/features/fcm-push-sender.md` → sanitised permanent/retry classification; provider diagnostics remain unchanged.
- `docs/architecture.md`, `docs/threat-model.md` → outbound FCM and log hygiene boundaries.
- [Published protocol](https://github.com/pyrycode/pyrycode/blob/d400bc3a4ad7e8bba3fffd6be53fc025e7c1f002/docs/protocol-mobile.md#correlated-wake-opt-in) → correlation, result, bounds, connection safety and C1–C8/C10 contracts.

## Context

Only terminal ErrFCMUnregistered may tell an opted-in originating daemon that a registration is invalid. Core #3423 owns consumption and conditional removal. This is advisory, volatile feedback with no durable delivery guarantee. No decision record is needed.

Sizing: one observable deliverable, about 1050 total written lines, five existing/new production files plus tests, no required legacy caller migration, no new exported type/interface, five criteria and fewer than ten feedback rejection/terminal classes. The shared handoffs floor overrides the 800-line ceiling: subscription state and cancellable writer plumbing each have only this feedback consumer. No overlapping in-flight branch was found after fetch.

## Design

`parsePushWake` decodes request_id independently as raw JSON; wrong type or invalid 1–64 ASCII allowlist syntax disables feedback without affecting platform/token validation. Unknown fields remain tolerated.

Keep `Request(serverID, raw)` unchanged and delegate to a private origin-aware request method. StartBinaryForwarder creates one private origin for the captured writer capability and cancels/joins its feedback on exit. The WS capability supplies its close context and a context-bounded result write; no registry lookup supplies a destination. Legacy fixtures need no new methods.

Each retained job has a completion signal, eviction signal, sanitised invalid flag and a map keyed by (origin pointer, request_id). Each accepted binding is one charged record for its entire pending/queued/seen lifetime. Under the existing waker mutex, enforce 64/job, 128/origin and 2048/global before inserting. Decline only the new binding on overflow. Active repeats do nothing. Successful/other terminal jobs retain dedup metadata but never send invalid feedback. Completion stamps invalid bindings' delivery deadlines at min(completion + 5s, acceptance + 60s); cached bindings start that budget on acceptance. The cached invalid flag contains no token. Completion after the original window queues existing bindings then evicts, with no renewed window.

Eviction closes the cache signal and removes the lookup entry. Pending/completed records release unless an original invalid delivery is still queued/in progress; those retain their original job and origin charges through delivery completion/cancellation. Evicted jobs cannot accept bindings or return to the lookup map. Every record expires at acceptance + 60s, independently of the five-minute accounting sweep.

WSConn uses a context-aware writer gate in place of the mutex, preserving Lock/Unlock behavior for ordinary Send. The private result method checks cancellation before/after acquiring the gate and writes under the supplied remaining deadline, returning only a static failure sentinel. Ordinary Send retains its independent ten-second active-write deadline, preserving #207 graceful shutdown.

## Concurrency model

The waker mutex owns record maps/counts, job completion/eviction and admission versus Close. Each accepted record has one worker, bounded globally by 2048, which waits for completion/expiry/cancellation, performs at most one bounded write, and retains dedup state until eviction/expiry/cancellation. It holds no accounting lock across I/O. No extra goroutine is created for repeats or failed binding admission. Context callbacks cancel records on origin close; bounded workers are joined by both origin teardown and waker Close. The lock order is waker accounting then nonblocking context operations; writer acquisition happens outside accounting. Close sets closed and cancels under the mutex before joining workers/provider jobs and the sweep; concurrent closes all join the same work. No new invocation/write passes its cancellation gate after closure.

## State transitions and identity reuse

| Event | Race-detector proof |
|---|---|
| Same token across origins, equal IDs, active repeats | TestPushFeedback_Coalescing |
| Cached invalid/new ID, repeated ID after success or failed write | TestPushFeedback_CacheAndOutcomes |
| Exact original window, late completion, eviction with queued records | TestPushFeedback_Eviction |
| Disconnect/replacement/reconnect under same identity | TestPushFeedback_OriginCancellation |
| Acceptance + 60s and no renewal | TestPushFeedback_Expiry |
| Exact/one-over job/origin/global caps, including evicted queued state | TestPushFeedback_Caps |
| Cancellation/shutdown concurrent with pending or queued delivery, repeated Close | TestPushFeedback_Close |
| Production writer contention, continued reads and bounded delivery | TestPushFeedback_WSContention |

## Error handling

Invalid correlation, duplicate or feedback overflow silently disables only that new binding. Ordinary malformed wake/admission errors retain existing sanitised sentinels. Only terminal unregistered, while the provider job remains uncancelled, marks invalid. Other outcomes/cancellation produce no result. Feedback transport errors are discarded after conversion to a static sentinel; no feedback logs or metrics are added. Existing provider attempt logs and five fixed outcome labels retain their semantics.

## Testing strategy

Write failing tests first. Raw literal envelopes cover syntax bounds, malformed JSON, unknown-field and phone-envelope compatibility. Held fake senders, independent origins and controlled clocks/barriers count provider calls, write attempts/deliveries and retained records independently. Prove all lifecycle rows, accounting transitions and non-invalid outcomes. Real httptest WebSocket peers exercise the production writer gate and forwarder reads, preserving the active-write shutdown regression. Inspect synthetic secret markers in responses/errors/captured logs/metrics. Run focused tests while developing, then `make check` (required by AGENTS.md) and `go build ./cmd/pyrycode-relay` before pushing.

## Open questions

None. Core persistence and consumer conformance are explicitly owned by #3423.

## Documentation handoff

Pending documentation stage:

- `docs/knowledge/features/push-wake-dispatch.md` (API; Request: validate, admit, dispatch; Error handling; Concurrency model) and `docs/knowledge/features/routing-envelope.md` (push_wake shape): canonical opt-in/result shapes, compatibility, cardinality, numeric caps/deadlines, cleanup and queued accounting after eviction; link the published protocol revision.
- `docs/knowledge/features/fcm-push-sender.md` (Error handling — nothing leaks; Out of scope), `docs/architecture.md` (Outbound calls to Google), `docs/threat-model.md` (Outbound network calls; Log hygiene): replace no-callback wording with sanitised bounded origin feedback, retaining advisory/device-ownership limits and distinct existing provider diagnostics.
- `docs/deploy.md` (coordinated push-result release check): require recorded protocol/relay/daemon revisions, completed Core #3423, new/new and both backwards-compatibility cases, matching/stale registration and disconnect/replacement/reconnect/expired/unknown-result evidence before manual activation. Deployment and real-daemon verification remain operator-owned; activation stays off pending that evidence.

## Security review

**Verdict:** PASS

- [Trust boundaries] `parsePushWake` validates only outer fields; inner frames stay opaque. Results contain only allowlisted request_id and fixed invalid_token, only on the captured daemon writer.
- [Tokens, secrets and credentials] No new credentials or token retention; jobs retain sanitised outcome and bounded correlation metadata. No feedback token hash or raw transport cause enters diagnostics.
- [File operations / subprocesses / cryptography] No new runtime disk state, subprocess or crypto. Existing complete-token SHA-256 accounting is unchanged and never echoed by feedback.
- [Network and I/O] SHOULD FIX — test writer contention using the production adapter; a timeout around ordinary Send would fail the total five-second bound. Context-aware gate and direct bounded write are required. Existing read caps and HTTP timeouts remain unchanged.
- [Errors, logs and telemetry] No new diagnostic fields or labels. Only a static feedback write sentinel escapes the adapter; existing sanitised provider attempts remain separate.
- [Concurrency] SHOULD FIX — keep queued records charged after cache eviction and join origin/waker workers under cancellation; tests must exercise this transition and concurrent Close. Admission and worker Add occur under the closed gate.
- [Threat model alignment] Bounded volatile state preserves the single-instance/content-blind model; no dependency, endpoint or deploy target changes. OUT OF SCOPE — device ownership and advisory-relay trust remain protocol limitations; Core #3423 owns conditional clearing, and the operator owns coordinated activation.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-15

## Revisions

- 2026-10-15: The full gate exposed two legacy tests assuming every admission invokes the sender even when Close wins scheduling. The planned shutdown gate cancels such jobs before invocation, retaining their admission charge and adding no synthetic metric/log outcome. The rate-limit metrics test now waits for its intended actual invocation; the admission/Close race checks joined slots and exactly one outcome per actual invocation, bounded by admissions. No production API or provider outcome-label meaning changes. Feedback workers also arm a timer at the immutable original window after completion so seen records/cache can be evicted without waiting for the accounting sweep.
- 2026-10-15: `TakeoverServer` publishes the new holder before the caller asynchronously closes the incumbent; grace reclaim can also replace a still-open transport. A failing `TestPushFeedback_RegistryReplacement` demonstrated that close-context cancellation alone leaves stale pending/queued feedback. Add `internal/relay/registry.go` (`replaceBinaryLocked`) to the touched files: cancel the old WSConn's separate feedback context under the registry identity lock before replacement. This private capability performs only nonblocking cancellation, no I/O or accounting-lock acquisition. Origin contexts descend directly from that feedback context and inherit cancellation synchronously; ordinary Send/close behavior remains independent. The test covers both pending and queued replacement with the old transport deliberately kept open. This is six production files rather than the estimated five, still one consumer/deliverable and within the stated floor exception.
- 2026-10-15: FCMSender can preserve a terminal UNREGISTERED classification alongside a trailing read's cancellation/deadline sentinel. Failing cancellation-chain cases in `TestPushFeedback_CacheAndOutcomes` require suppressing feedback for either sentinel even while the parent job context is live. Provider-attempt classification/logging stays unchanged. Negative outcome proofs evict and join records without cancelling the waker, preventing shutdown scheduling from masking an unwanted result.
- 2026-10-15 final recount: approximately 1165 inserted lines including this plan, tests/helpers and reformatted production declarations, six production files, no exported types/interfaces, one production Request consumer, five criteria and at most ten feedback terminal/rejection classes. The additional replacement-fence and cancellation-chain regression proofs remain within the same indivisible feedback deliverable; the documented one-consumer floor continues to apply. `TestPushFeedback_ExpiryBoundary` covers just-before/exact subscription expiry; `TestPushFeedback_RetryTerminal` separately counts three provider attempts and zero/one terminal feedback attempt.
