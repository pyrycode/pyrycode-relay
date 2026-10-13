# Bounded device wake coalescing (#211)

## Files read

- `internal/relay/push_wake.go` → `PushWaker.Request`, `send`, `Close`, `requestedBucket`, `sweepLocked`: atomic attempt accounting and shutdown ownership.
- `internal/relay/push_wake_budget_test.go` → `budgetWaker`, allowance/refill/refusal/concurrent tests: controlled clock and full-token collision fixtures.
- `internal/relay/push_wake_test.go` → `fakePushSender`, outcome/secret tests, `TestPushWaker_CloseCancelsAuthentication`: held sends and joined shared refresh.
- `internal/relay/metrics_push_test.go` → refusal tests: outcomes count actual attempts, not accepted duplicates.
- `internal/relay/daemon_admission_test.go` → `TestDaemonAdmission_ReconnectForwardPush`: real sockets, allowance continuity and opaque forwarding.
- `internal/relay/forward.go` → `StartBinaryForwarder`: sole production request caller; no contract migration.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`: package conventions and owning documentation.
- `docs/knowledge/features/push-wake-dispatch.md`, `fcm-push-sender.md`: allowance debt must survive job expiry; sends own authentication cancellation.
- `docs/architecture.md` → Outbound calls to Google; `docs/threat-model.md` → Outbound network calls / Log hygiene: payload-free delivery and bounded secret-safe state.
- [Canonical protocol](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#push_wake) → push_wake and Security model: structural relay-addressed metadata only; no wire changes.
- `$AGENTS_REPO_PATH/builder/security-review.md`: adversarial checklist applied below.

## Context

Repeated requests currently charge and send separately even for one complete device token. Coalesce one device job across daemon identities while preserving #172 accounting and #169 cancellation. This extends ADR-0013; its documentation update is pending below. One deliverable, four criteria; estimated under 700 written lines across one production file, five test files and this plan, zero new exported types, zero consumer migrations and fewer than ten refusal/lifecycle branches. The #172 analogue wrote 580 lines in the owning production/budget-test files. Required branch fetch found no overlapping in-flight feature branches.

## Design

Add an unexported job record containing only the fixed expiry timestamp and active flag, keyed by the full SHA-256 of the complete token. `PushWaker` owns a job map capped by the existing constructor's `maxInFlight` (32 in production). Keep the existing channel for active-send reservation; completed retained jobs also consume map capacity.

After structural validation and the closed check, `Request` checks for an active job or a job whose admission window has not ended. It returns nil before touching allowances, daemon state, outcomes or logging. The fixed window is ten seconds from first admission, independent of completion; duplicates do not change it. Active jobs suppress beyond expiry. Reclaim expired completed jobs before checking retained capacity; a full map returns the existing static `ErrPushWakeInFlightCap` with one dropped outcome. Otherwise preserve atomic allowance/slot checks and charge all three allowances before inserting the active job and spawning its one send. New jobs retain only the first daemon in the send goroutine; no requester lists exist.

`send` continues using the sender's existing whole-send deadline and outcome/logging code. Its deferred completion, under `mu`, frees the active slot and marks the digest-only job completed; an already-expired job is removed immediately. No raw token enters job state. Return releases the goroutine's raw-token reference. Reuse the existing sweep for expired completed job cleanup; admission cleanup ensures idle retained entries never obstruct later work. No new timer or cleanup goroutine is needed. Job expiry never removes accounting debt.

## Concurrency model

The existing mutex owns job lookup, expiry, reservation, allowances, completion and the closed transition. No lock spans sender I/O. `wg.Add` remains under the admission mutex and cannot race a post-close `Wait`. Completion runs before `wg.Done`; each outcome remains attributed to the first admitting daemon. `Close` cancels sends/shared authentication, joins sends and the sweep, then clears job state under the mutex. Repeated and concurrent closes join all the same work.

## State transitions and identity reuse

| Event | Race-detector regression |
|---|---|
| Concurrent same/cross-daemon duplicate, including exhausted allowances and full accounting state | `TestPushWaker_CoalescesConcurrentDuplicates` |
| Successful/failed completion before expiry; duplicate just before expiry; exact boundary readmission; second window | `TestPushWaker_CompletedWindow` |
| Active job beyond expiry; completion then same-token reuse | `TestPushWaker_ActivePastWindow` |
| Distinct complete tokens, including a shared logging fingerprint | `TestPushWaker_PairedDeviceBurstAndCompleteKeys` |
| Active and completed retained capacity; duplicate at capacity; expired-state reclamation preserving debt | `TestPushWaker_RetainedCapacity` |
| Requests and duplicate acceptance racing repeated/concurrent closes; retained state cleared | `TestPushWaker_AdmissionRacesClose`, `TestPushWaker_CoalescingRacesClose` |
| Shared authentication shutdown with several distinct device jobs | `TestPushWaker_CloseCancelsAuthentication` |
| Reconnect under same daemon identity; allowances persist | `TestDaemonAdmission_ReconnectForwardPush` |

## Error handling

No new errors, wire replies, retry or queues. Existing validation and static refusal errors remain. Coalesced requests emit no outcome; actual sends retain existing phase-specific outcomes and one safe log line. Retained-job capacity is checked before allowance mutation.

## Testing strategy

Write controlled-clock/held-sender regressions first and observe failures against the old implementation. Adapt burst, metric, reconnect and authentication-shutdown tests to distinct attempts. Since ten-second coalescing equals production device refill, prove depleted device exhaustion/refill with controlled bucket fixtures instead of impossible repeated-token depletion. Keep atomic refusal, full-digest collision, payload-free HTTP and secret/opaque-forwarding assertions. Run focused tests while implementing, then `make check` as required by AGENTS.md and the ticket, plus `go build ./cmd/pyrycode-relay`.

## Open questions

None. Cleanup reuses the existing sweep; job capacity follows the constructor's existing test-configurable capacity.

## Documentation handoff

Pending for the documentation stage:

- `docs/knowledge/features/push-wake-dispatch.md` → Request: validate, admit, dispatch / Allowances and concurrency cap / Metrics / Concurrency model: fixed ten-second window, active suppression beyond expiry, non-extending duplicates, 32 retained jobs, unchanged actual-attempt charging and no queue. Distinguish accepted duplicates from sends in outcomes and paired-device examples.
- `docs/architecture.md` → Outbound calls to Google; `docs/threat-model.md` → Outbound network calls — FCM push wake; `docs/knowledge/decisions/0013-push-wake-async-dispatch-bounded-not-verified.md` → Decision/Consequences: bounded coalescing, best-effort payload-free delivery, no refused-work queue and in-memory reset on restart.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] Structural `parsePushWake` validation precedes coalescing; `StartBinaryForwarder` continues forwarding opaque inner frames. No device-ownership trust is inferred.
- [Tokens / cryptography] Job keys use full standard SHA-256 digests, never truncated log fingerprints. Records contain only timestamps/flags; raw tokens remain local to active sends and are released on return.
- [File operations / subprocesses] No runtime disk state or subprocess is introduced; coalescing resets on restart.
- [Network & I/O] At most 32 retained jobs and active sends, with no queue or amplification from duplicates. Existing frame bounds and FCMSender's ten-second whole-send budget remain intact. No endpoint, dependency or TLS changes.
- [Errors / logs / telemetry] Static existing errors and fixed outcome labels remain. Duplicates produce no event; first-daemon attribution and allowlisted token fingerprint logging are unchanged. No new log keys or digest logging.
- [Concurrency] Lookup, reservation, charges and closed transition share one mutex. Active entries cannot expire; completed entries are reclaimed before capacity checks. Existing sweep cancellation and send joins precede clearing retained state.
- [Threat model] Device ownership remains unverified under the existing threat model; arbitrary-device abuse remains bounded by unchanged allowances. No durable/exactly-once or multi-replica promise. Dead-token callbacks remain deferred to relay #132's follow-up scope; retries remain outside #211.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-13

## Revisions

- 2026-10-13: The full `make check` gate exposed another duplicate-based authentication fixture in `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_ProcessSIGTERMCompleteShutdown`. Read its process helper and held OAuth transport; adapt its six requests to distinct tokens, preserving six shared-refresh waiters and phase-specific outcomes under actual SIGTERM. The cached overlap check found `origin/feature/127` in this additional file; its diff adds the separate grace-reclaim test and helpers, so the one-line push fixture change is local and independent. No production design changes; the final surface is one production file, six test files and this plan, still below 700 written lines. This process test also covers joined shared-authentication shutdown in the lifecycle table above.
