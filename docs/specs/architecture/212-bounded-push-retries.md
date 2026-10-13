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
- [Concurrency] SHOULD FIX — retry accounting must stay atomic against closure and retain original daemon; timer cancellation must join within the existing job wait group. Preserve shared-refresh receipt time and last-waiter ownership.
- [Threat alignment] No findings — amplification limited to three charged calls, 45s and 32 jobs, no dependencies/endpoints/deploy changes. Device ownership remains deliberately unverified as documented; dead-token notification is a separate slice from #173.

**Reviewer:** builder (self-review per checklist)
**Date:** 2026-10-13
