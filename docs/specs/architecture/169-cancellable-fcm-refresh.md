# #169 — Cancellable shared FCM refresh and whole-send deadline

## Files read

- `internal/relay/push.go` → `newFCMSender`, `FCMSender.Send`, `parseFCMCredentials`: current background token source, deadline placement, sanitisation and redirect policy.
- `internal/relay/push_wake.go` → `PushWaker.Request`, `send`, `Close`: admission is atomic with close; close cancels the send context and waits for every send.
- `internal/relay/push_test.go` → `testServiceAccountJSON`, `fakeOAuth`, `fakeFCM`, `TestFCMSender_CancelledContext`: local endpoint fixtures, request shape, caching and secret-free errors.
- `internal/relay/push_wake_test.go` → `captureLogger`, `TestPushWaker_FCMEndToEndLogsNoSecrets`: outcomes and cleanup assertions.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`: package conventions and feature map.
- `docs/knowledge/features/fcm-push-sender.md` → Sending a message, Error handling — nothing leaks, Concurrency: preserve cache validity and sanitised token errors; replace background refresh ownership.
- `docs/knowledge/features/push-wake-dispatch.md` → The two limits, Concurrency model: preserve admission and make existing close join authentication cleanup through Send.
- `docs/architecture.md`, `docs/threat-model.md` → Outbound network calls, Supply chain, Log hygiene, Error response leakage; `docs/security-followups.md`: existing content-blind and bounded outbound-call posture.
- Pinned `golang.org/x/oauth2/jwt` → `Config.TokenSource`, `jwtSource.Token`; `oauth2.NewClient`: JWT uses `PostForm` without attaching the source context to the request. Context-bound transport is required for actual network cancellation.
- [Protocol of record](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md) → push_wake and Security model: no wire changes and no application payload inspection.
- `Makefile` → check: user requires the full vet/race gate before push.

## Context

Slow authentication currently outlives cancelling sends and shutdown, and failed refreshes are retried serially by waiters. This is one deliverable: a bounded, cancellable send with shared refresh outcomes. No decision record is needed.

Size: approximately 650 written lines including plan, helper and regression tests; zero new exported types, zero consumer signature changes, five acceptance criteria and fewer than ten reject branches. The #132 analogue wrote 535 sender/test lines, with 591 implementation insertions overall; this change reuses its endpoint fixtures. Re-count before implementation commit. Required branch overlap fetch found no overlapping in-flight feature branches for the planned files.

## Design

`FCMSender.Send` creates `context.WithTimeout(ctx, fcmSendTimeout)` at entry. An already-done caller returns `ErrFCMSend` wrapping its context error, preserving the existing classification, without token lookup or outbound requests. This same context governs token waiting and the FCM POST, including earlier caller deadlines. Recheck cancellation before FCM dispatch.

Replace the long-lived `oauth2.TokenSource` with an unexported `fcmTokenCache` in `internal/relay/push_refresh.go`. Its `token(ctx) (*oauth2.Token, error)` owns a cached token and an optional refresh attempt under one mutex. Use `oauth2.Token.Valid` to retain existing pre-expiry refresh semantics. A new attempt uses the existing JWT signer and redirect-refusing HTTP client, with a new source per attempt. The cache, rather than the dependency's mutex, shares both success and failure.

Each attempt has a done channel, waiter count, cancellation function and immutable result published before done closes. Sends join the current attempt and consume that attempt's result exactly once; no waiter loops into another attempt after failure. Later calls can retry. Token failures are sanitised once before publication: only `ErrFCMTokenFetch`, optionally an HTTP status. Cancellation while waiting wraps `ErrFCMTokenFetch` and a safe context sentinel.

An attempt owns an independent context capped at `fcmSendTimeout`, rather than the starter's context/deadline. Copy the HTTP client for that attempt and install an unexported context-binding transport that delegates to the original/default transport with the refresh context attached. Preserve redirect refusal and client timeout. No dependency, wire shape or logging changes.

## Concurrency model

The cache mutex protects cached token, current-attempt identity and waiter counts; it is never held during HTTP I/O or channel waits. One refresh goroutine publishes its result and closes done. Send waits on its own context or done. Departing waiters decrement under the mutex. When the last waiter leaves an unfinished attempt, detach it, cancel its context and join done before that Send returns. Thus `PushWaker.Close`'s existing send wait group also joins refresh cleanup, without a new public shutdown API or production waker change.

A new send can create a new attempt after abandonment; completion of an abandoned attempt may not overwrite the cache or clear a newer attempt. Publication checks pointer identity under the mutex. Completed results stay attached to each old waiter even when a later send starts a fresh attempt. The refresh timeout, endpoint completion or last-waiter cancellation terminates every refresh goroutine. No additional lock order is introduced.

## State transitions and identity reuse

| Event | Contract and race regression |
|---|---|
| Six sends join one pending attempt, success or failure | One authentication request; all consume the same result; `TestFCMSender_SharedRefresh`. |
| Failed attempt followed by another send | Old waiters never retry; later send starts a fresh attempt; `TestFCMSender_SharedRefresh`. |
| Cached token becomes invalid, twice consecutively | Each invalidation refreshes once; `TestFCMSender_RefreshesExpiredToken`. |
| Starter or another waiter cancels/expires while a survivor remains | Return promptly without cancelling authentication; `TestFCMSender_IndependentCancellation`. |
| Every waiter leaves, followed by reuse of sender | Cancel and join old refresh; new attempt succeeds; `TestFCMSender_AllWaitersCancel`. |
| Delayed authentication consumes send budget | FCM gets only remaining time; caller deadline wins; `TestFCMSender_WholeOperationDeadline`. |
| Six wakes cancelled by close; post-close request | Authentication cancelled, six outcomes and refresh cleanup joined within two seconds; no later requests; `TestPushWaker_CloseCancelsAuthentication`. |
| Caller already cancelled, cold or warm cache | No outbound calls; `TestFCMSender_AlreadyCancelledNoRequests`. |

## Error handling

Token transport, status and decode failures retain `ErrFCMTokenFetch` with no raw dependency error/body. FCM errors retain existing `ErrFCMSend` and `ErrFCMUnregistered` behaviour. Cancellation in the token phase is token-fetch failure; pre-cancelled entry remains send failure. No retries, correction or background refresh after abandonment. Existing high-priority payload-free wakes, response-size caps and redirect refusal remain intact.

## Testing strategy

Write regressions before production edits and observe failures against the old sender. Use held-open local OAuth/FCM endpoints, counters and explicit waiter-admission synchronisation (inspect the cache under its mutex once available), never sleep to assume six sends joined. Assert endpoint request cancellation and attempt done, not just prompt Send returns. Exercise six shared successes and sanitised failures, subsequent retry, independent starter/other cancellation and expiry, all-waiter cancellation, repeat expiry and cold/warm pre-cancelled callers. Use the real ten-second bound for the whole-operation regression and an earlier caller deadline separately. Existing shape, redirects, secret-free errors and log allowlist tests remain coverage. Run focused race tests, `make check` as required by user and ticket, and `go build ./cmd/pyrycode-relay`.

## Open questions

None. The pinned JWT implementation's lack of request-context attachment is resolved by the refresh-scoped transport, rather than custom JWT signing.

## Documentation handoff

Pending for the documentation stage:

- `docs/knowledge/features/fcm-push-sender.md`, Sending a message, Error handling — nothing leaks, Concurrency: single whole-send deadline; shared success/failure; cache lifetime; waiter-owned cancellation; replace HTTP-timeout-only/no-shutdown claims.
- `docs/knowledge/features/push-wake-dispatch.md`, The two limits, Concurrency model: whole-send accounting and close joining cancellable authentication plus sends. Distinguish the two-second push-cleanup regression budget from `runServers`' separate ten-second HTTP/WebSocket drain.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — only token endpoint replies are handled in `fcmTokenCache`; routing and opaque inner frames are untouched. Device tokens remain only in the fixed wake shape.
- [Tokens, secrets and credentials] No findings — existing JWT signing and in-memory token caching remain; `fcmTokenCache` publishes only sanitised failures, never endpoint bodies or credential-bearing transport errors.
- [File operations] No findings — no new runtime disk state; credentials remain boot-validated in `parseFCMCredentials`.
- [Subprocesses] No findings — no subprocess introduced.
- [Cryptography] No findings — JWT signing stays in pinned `oauth2/jwt`; no custom cryptography.
- [Network and I/O] No findings — refresh transport attaches an actual cancellable request context, preserves redirect refusal and existing OAuth response cap (1 MiB); FCM retains its 4 KiB cap. Existing waker limits bound admission; one attempt serves all active waiters. No listener changes.
- [Errors, logs and telemetry] No findings — sanitisation occurs before sharing a failure; no new log calls or keys; existing `ErrFCMSend` and enum filtering remain.
- [Concurrency] No findings — cancel the refresh only at zero waiters; last departing Send joins cleanup outside the mutex. Pointer-identity checks prevent abandoned work overwriting newer cache state; old waiters retain their own attempt result.
- [Threat model alignment] No findings — outbound targets, dependencies and single-instance assumptions are unchanged; no new review trigger. The existing global throughput hardening deferral in threat-model Outbound network calls remains outside scope.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11
