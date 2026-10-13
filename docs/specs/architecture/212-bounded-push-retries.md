# Bounded transient push retries (#212)

## Files read

- `internal/relay/push_wake.go`: `Request`, `send`, `Close`, `requestedBucket` own atomic budgets, fixed-window jobs and shutdown.
- `internal/relay/push.go`: `FCMSender.Send`, `fcmErrorReason` own empty high-priority messages, phase sentinels and sanitised enum reasons.
- `internal/relay/push_refresh.go`: `fetch`, `leave`, `fcmRefreshTransport.RoundTrip` publish shared failures and join last-waiter cancellation.
- `internal/relay/push_wake_budget_test.go`, `push_wake_coalesce_test.go`, `push_refresh_test.go`, `push_test.go`, `metrics_push_test.go`: controlled clocks, allowance snapshots, held OAuth and FCM peers, secret-free fixed outcomes.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`: package conventions and feature ownership.
- `docs/knowledge/features/push-wake-dispatch.md`: completed-window expiry must never erase debt; duplicate test requests must not stand in for independent jobs.
- `docs/knowledge/features/fcm-push-sender.md`: OAuth context must bind at transport; surviving refresh waiters retain cancellation ownership.
- `docs/architecture.md` (Outbound calls to Google), `docs/threat-model.md` (Outbound network calls / Log hygiene): bounded amplification, no raw secrets in errors or logs.
- [Protocol push_wake and Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#push_wake): unchanged content-free wake contract.
- [Firebase retry recommendations](https://firebase.google.com/docs/cloud-messaging/scale-fcm), [error codes](https://firebase.google.com/docs/cloud-messaging/error-codes): backoff, jitter and quota waiting.

## Context

Retry transient wake failures inside #211's admitted device job, without a queue or a follow-on job. #169 and #172 already supply shared-refresh cancellation and atomic allowances. One deliverable, estimated at 240 production + 410 tests/helpers + 95 plan lines (745 total), zero exported types, zero public signature migrations, five acceptance criteria and at most ten stopping/reject branches. No overlap: fetched `origin/feature/127` touches none of the planned files. No new dependency or decision record is needed; update ADR-0013 through documentation.

## Design

Keep `Request` and sender signatures. Each job captures an immutable admission deadline at +45s, while its existing +10s completion-coalescing expiry stays independent. Hold its existing reserved slot through sending/backoff: this bounds retained active work and simultaneous sends to 32. Each job calls Send at most three times; the sender keeps its whole-call 10s cap, limited by the job context. Origin daemon and complete-device digests never change.

Extract the existing atomic allowance admission into a private helper reusable before retries. Every invocation consumes all three allowances once; failure never refunds. A retry refusal records one dropped outcome and stops, without refill waiting or partial charging. Requests coalescing during a call or wait charge/count/log nothing and cannot replace the origin or extend deadlines. Completion retains only existing fixed-window metadata, never a raw token.

A private sanitised error wrapper carries retryability and an absolute not-before time, preserving phase/context/unregistered identities through wrapping. HTTP classification allows FCM 429/500/503 and OAuth 429/500/502/503/504. UNREGISTERED overrides status. Transport classification requires wrapped net.Error timeout/temporary; preserve only safe context sentinels, never original transport causes or URLs. Malformed OAuth replies and invalid credentials remain permanent. A sender's own timeout can retry only while the parent job is live.

Capture HTTP receipt time before draining bodies, including OAuth in `fcmRefreshTransport.RoundTrip`; publish its sanitised guidance unchanged to all shared refresh waiters. Parse Retry-After digits or HTTP date, discarding text. Saturate overflowing positive seconds above any job horizon instead of wrapping; past dates give zero and invalid/negative values give ordinary backoff. Status 429 imposes at least receipt+60s. Next attempt is max(receipt guidance, call completion + 10s/20s + independently sampled [0,1s] jitter). At or beyond deadline stops immediately. Inject private wait/jitter seams alongside the existing clock for deterministic tests; production waiting uses a cancellable stopped timer.

Each invocation records its existing phase outcome and log once, using existing allowlisted keys and first daemon. Cancellation between calls, deadline/backoff exclusion and attempt exhaustion add no synthetic outcome.

## Concurrency model

One goroutine and reserved slot per job; one job context descends from waker context. `w.mu` guards buckets, admission, retry charging, jobs and clock reads; never hold it through HTTP or waits. Shared OAuth work retains its independent refresh context and last-waiter cancel/join ownership. `Close` sets closed, cancels contexts, joins jobs/authentication/timers and sweep, clears jobs; concurrent closes all join the same work.

## State transitions and identity reuse

| Event | Race-detector proof |
|---|---|
| Transient failure then success / three failed attempts | `TestPushWaker_RetrySequence` |
| Same/cross-daemon duplicate during held 503 or backoff | `TestPushWaker_RetrySequence` |
| Retry allowance depletion or fresh admission at capacity | `TestPushWaker_RetryRefusal`, existing retained-capacity tests |
| Guidance/deadline exclusion; cancelled wait or send | `TestPushWaker_RetryBounds`, `TestPushWaker_RetryClose` |
| Job expiry during authentication with surviving waiter | `TestPushWaker_RetryAuthenticationExpiry`, existing independent-waiter tests |
| Completed job reused at fixed window without debt refund | existing `TestPushWaker_CompletedWindow`, `TestPushWaker_ActivePastWindow` |
| Shared OAuth failure publication/replacement | `TestFCMSender_RetryClassification`, existing shared-refresh tests |

## Error handling

Sanitise once at network boundaries; errors retain phase identity and bounded validated reasons only. Stop on permanent failure, cancelled/expired job, third failure, excluded next time or allowance refusal. Keep the five fixed metric labels. No wire response, token callback or durable guarantee.

## Testing strategy

Write and observe failing tests before production edits. Table-test both phase status allowlists, net.Error classification, secret-free wrapping and Retry-After (dates/digits/malformed/overflow/429 floor). Controlled local OAuth/FCM responses plus injected waiting/jitter prove exact call counts, receipt anchoring, immutable deadlines, duplicate suppression, attribution, budgets and shutdown. Reuse existing empty-payload/forwarding/metric proofs. Run focused race tests, package race suite, vet/build and user-required `make check` before push.

## Open questions

None.

## Documentation handoff

Pending documentation stage:
- `docs/knowledge/features/push-wake-dispatch.md`: Request / Allowances / Metrics / Concurrency — three attempts, immutable 45s deadline, 10s/20s+jitter backoff, retained 10s completed window, duplicate suppression, retry accounting/refusals, per-attempt outcomes and shutdown.
- `docs/knowledge/features/fcm-push-sender.md`: Sending / Error handling / Concurrency — sanitised typed retry classifications and receipt-anchored guidance, preserved refresh cancellation, sender 10s versus job 45s budget.
- `docs/architecture.md` Outbound calls to Google; `docs/threat-model.md` Outbound network calls; `docs/knowledge/decisions/0013-push-wake-async-dispatch-bounded-not-verified.md` Decision/Consequences — bounded retries/retained work, empty payloads, in-memory reset, best-effort delivery. Operator deploy remains pending.

## Security review

**Verdict:** PASS

**Findings:**
- [Trust boundaries] No findings — `parsePushWake` remains the structural boundary; no forwarding payload inspection or wire change.
- [Tokens/secrets] SHOULD FIX — raw transport errors may include credential URLs; sanitise before wrapping, retaining only classification and safe context sentinels. Completed jobs retain no token.
- [File operations/Subprocesses] No findings — no runtime files or subprocesses added.
- [Cryptography] No findings — complete SHA-256 accounting unchanged; math/rand is used only for jitter.
- [Network/I/O] SHOULD FIX — overflow guidance must saturate beyond the job horizon; 429 floor and deadline comparison prevent early retries. Existing bounded body reads, redirect refusal and ten-second call caps remain.
- [Errors/logs/telemetry] No findings — typed guidance never formats header text; existing allowlisted keys and exactly five fixed outcomes, no new labels or logging keys.
- [Response/read-error precedence, rework] Resolved MUST FIX — `FCMSender.Send` and `fcmTokenCache.fetch` preserve HTTP status and receipt-anchored guidance despite body-read failure. Complete permanent rejection JSON overrides both status and read retryability. Only a 2xx response defers retryability to sanitised read classification; cancellation always stops and safe context/phase identities survive. `fcmRefreshBody.Read` retains at most the JWT reader's existing 1 MiB cap locally to recognise complete rejection JSON; no body or raw read error is published.
- [Concurrency] SHOULD FIX — retry accounting must stay atomic against closure and retain original daemon; timer cancellation must join within the existing job wait group. Preserve shared-refresh receipt time and last-waiter ownership.
- [Threat alignment] No findings — amplification limited to three charged calls, 45s and 32 jobs, no dependencies/endpoints/deploy changes. Device ownership remains deliberately unverified as documented; dead-token notification is a separate slice from #173.

**Reviewer:** builder (self-review per checklist)
**Date:** 2026-10-13

## Revisions

- 2026-10-13: Keep scheduling on private `jobNow` separately from allowance `now`; existing allowance tests use historical dates that must not expire real contexts. Both default to real time and are read under `w.mu`. Ordinary backoff anchors immediately after call return, before outcome logging. OAuth credential rejection codes (`invalid_client`, `invalid_grant`, `unauthorized_client`) override even transient HTTP statuses; `TestFCMSender_RetryPermanentReplies` caught the invalid-grant/503 case before its fix. Shared guidance receipt is proved by `TestFCMSender_RetrySharedReceipt`; own-call timeout and later guidance by `TestPushWaker_RetryGuidanceAndCallTimeout`.

- 2026-10-13: The pinned JWT implementation formats network/read errors with `%v` and does not populate `RetrieveError.ErrorCode`. Capture sanitised transport/read classification inside `fcmRefreshTransport`/`fcmRefreshBody`, parse only the bounded OAuth rejection field, and reject invalid successful tokens before sending. `TestFCMSender_RetryTransportBoundary` also exposed an outer `url.Error` hiding a wrapped temporary cause; walk wrappers for positive net.Error evidence. Final work is approximately 845 inserted lines including tests and plan, exceeding 800. Per `builder/handoffs.md`'s floor rule, classification has exactly this one retry consumer and cannot be split into an independently verifiable sibling; keep this inseparable deliverable together. No new public type, dependency, call-site migration or acceptance criterion.

- 2026-10-13: `FCMSender.Send` checks UNREGISTERED before the HTTP success branch, so permanent identity overrides even a contradictory 2xx reply; the permanent-replies test covers both 503 and 200.

- 2026-10-13: Full race checks caught reordered fresh slot refusal skipping `requestedBucket` idle timestamps. Keep slot reservation after accounting checks and before charging in the shared helper; retries reuse their job's slot. Existing `TestPushWaker_StateBoundsAndExpiry` verifies this preserved refusal contract.

- 2026-10-13 (verifier rework, findings 1 and 2): Combine HTTP and body-read evidence with permanent reply > HTTP status > transport retryability precedence; context cancellation always stops. Preserve receipt-anchored Retry-After even when the body fails, and preserve safe context/phase identities without the original read cause. OAuth captures sanitised status/guidance at `fcmRefreshTransport.RoundTrip` and bounded local body bytes in `fcmRefreshBody.Read`, before the JWT source discards them. `TestFCMSender_RetryResponseReadFailure`, `TestFCMSender_RetrySharedReceiptReadFailure` and `TestFCMSender_RetryRealBodyTimeout` cover both phases, complete permanent replies, context identities, overflow, quota floors and shared publication. Update `NewPushMetrics` HELP to state per-invocation outcomes, admission/retry refusals and suppressed duplicates, retaining five labels. No lifecycle, wire contract or public signature changes. Documentation stage must include combined response/read-error precedence in the sender's Error handling section.

- 2026-10-13 (rework test result): The real OAuth body-timeout regression initially lost `context.DeadlineExceeded`: `http.Client` annotates its body error outside `fcmRefreshBody`, after JWT will discard it. `fcmRefreshBody.Read` now captures the original request's client-timeout context signal when a read fails, retaining only safe cancellation/deadline identity. Refresh cancellation ownership still belongs to the independent refresh context; no goroutines or shutdown paths change. Ten repeated race-detector runs of the real-peer regression pass.
