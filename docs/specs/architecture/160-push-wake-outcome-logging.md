# #160 — Log every push wake outcome, with Firebase's message id on success

## Files read

- `internal/relay/push_wake.go` → `pushSender`, `PushWaker.send`, `PushWaker.Close`: the only send call site and the only place outcome logging can live (it holds the logger and server id).
- `internal/relay/push.go` → `FCMSender.Send`, `fcmErrorReply`, `fcmErrorReason`, `isFCMEnum`: the 2xx reply is already drained (capped by `fcmMaxDrainBytes`) and discarded; the id parser mirrors `fcmErrorReply` / `isFCMEnum`.
- `internal/relay/log_allowlist.go` → `allowedLogKeys`: three new keys land here.
- `internal/relay/log_keys_test.go` → `TestLogKeysAreAllowlisted`: keys must be string literals, so the optional message id needs two literal call shapes, not a built-up args slice.
- `internal/relay/push_wake_test.go` → `fakePushSender`, `captureLogger`, `TestPushWaker_SendFailureLoggedWithoutToken`: harness the new log tests extend.
- `internal/relay/push_test.go` → `fakeOAuth`, `fakeFCM`, `assertNoSecrets`, `TestFCMSender_NonSuccessReplyNamesFCMReason`: harness for the 2xx reply table and the end-to-end log test.
- `docs/threat-model.md` § Log hygiene, `docs/knowledge/features/push-wake-dispatch.md`, `docs/knowledge/features/fcm-push-sender.md`: what may be logged today; the Documentation handoff targets.
- `docs/specs/architecture/150-fcm-error-reason.md`: the analogue; the enum gate pattern this follows.

No in-flight branch touches these files (only `feature/127`, registry-only).

## Context

pyrycode-mobile#1573: a daemon logged `push_wake.sent`, the phone never woke, and the relay log for that moment was empty because a successful send logs nothing. One outcome line per admitted wake, with Firebase's message id on success and a token fingerprint on both, lets the operator say whether the wake died before the relay, at Firebase, or on the phone. No decision record needed.

## Design

**`pushSender`** becomes `Send(ctx context.Context, deviceToken string) (string, error)`. The string is a validated FCM message id, or `""`. Implementations: `*FCMSender` and the test fake.

**`FCMSender.Send`** — on a 2xx reply returns `(fcmMessageID(reply), nil)`; every error path returns `("", err)` unchanged.

- `fcmSendReply` declares only `Name string \`json:"name"\``.
- `fcmMessageID(reply []byte) string` unmarshals into `fcmSendReply` and returns `Name` only if `isFCMMessageID(Name)`, else `""`. A bad id never fails the send.
- `isFCMMessageID(s string) bool`: 1..`fcmMaxMessageIDLen` (256) bytes of `[A-Za-z0-9/:%_-]`. A real id is ~70 bytes.

**`tokenFingerprint(token string) string`** — first 8 hex chars of `sha256.Sum256(token)` (`hex.EncodeToString(sum[:4])`). Deterministic, one-way, 32 bits: enough to tell a handful of paired devices apart in logs.

**`PushWaker.send`** — times the `Send` call (`time.Now` / `time.Since`) and logs exactly one line:

- failure: `Warn("push_wake_send_failed", "server_id", …, "token_fp", …, "duration", …, "err", err)`.
- success with id: `Info("push_wake_sent", "server_id", …, "token_fp", …, "duration", …, "fcm_message_id", id)`.
- success without id: the same `Info` line without `fcm_message_id`.

`duration` is a `time.Duration` value (slog renders it, e.g. `152.3ms`). A send cancelled by `Close` returns an error from `Send` (the context error), so it takes the failure branch — no special case.

**New allowlist keys** (with the comment saying why each is safe):

- `token_fp` — 8 hex chars of SHA-256 of the device token; truncated one-way hash of a high-entropy token, never the token.
- `fcm_message_id` — FCM's message name, accepted only as `[A-Za-z0-9/:%_-]{1,256}`; identifies a message at Google, carries no user content.
- `duration` — elapsed time of a send.

## Concurrency model

Unchanged. One goroutine per admitted wake (`PushWaker.send`), exiting when `Send` returns; logging happens on that goroutine before `wg.Done`, so `Close` returning means every admitted wake's outcome line is written.

## Error handling

No new error values. Error text from `FCMSender` is unchanged (status + enum codes). An unparseable, missing, over-long or off-charset `name` on a 2xx is silently dropped to `""` — the send is still a success.

## Testing strategy

In `push_test.go`:
- Table `TestFCMSender_SuccessReplyMessageID` beside `TestFCMSender_NonSuccessReplyNamesFCMReason`: valid real-shaped id returned; id with extra fields alongside (marker, device token) returns only the id; missing `name`, empty body, not JSON, over-cap (257 bytes), hostile `name` (contains space / marker+token / `"`, `=` ) all return `""` and nil error. `assertNoSecrets` on the returned id.
- Existing tests updated for the two-value `Send`.
- `TestTokenFingerprint`: 8 lowercase hex chars, stable across calls, two different tokens differ, never contains the token.

In `push_wake_test.go`:
- `fakePushSender` gains an `id` field returned on success.
- `TestPushWaker_LogsOneOutcomePerWake`: success-with-id, success-without-id, failure, and cancelled-by-`Close`; each asserts exactly one `push_wake_sent`/`push_wake_send_failed` line, `server_id`, `token_fp=<fingerprint>`, `duration=`, the id present only when given, and no raw token.
- `TestPushWaker_FCMEndToEndLogsNoSecrets`: real `FCMSender` against `fakeOAuth` + an httptest FCM, through `PushWaker`; cases: 2xx with valid `name` plus extra fields echoing the marker and tokens, 2xx with a hostile `name`, 404 error body echoing them. Asserts the captured log contains the expected outcome line and none of device token, access token or marker.

## Open questions

None.

## Documentation handoff

Pending for the documentation stage:

- `docs/threat-model.md` § Log hygiene: add `token_fp` (truncated one-way SHA-256 fingerprint of the device token, first 8 hex chars; the raw device token is never logged) and `fcm_message_id` (charset/length-validated FCM message name) to the *MAY be logged* list.
- `docs/knowledge/features/push-wake-dispatch.md`, Error handling / logging: every admitted wake logs exactly one outcome line — `push_wake_sent` (Info: `server_id`, `token_fp`, `duration`, `fcm_message_id` when valid) or `push_wake_send_failed` (Warn: `server_id`, `token_fp`, `duration`, `err`), including sends cancelled by `Close`; `pushSender.Send` now returns `(string, error)`.
- `docs/knowledge/features/fcm-push-sender.md`: `Send` returns the validated message id (`name` from the 2xx reply, `[A-Za-z0-9/:%_-]{1,256}`) on success, `""` when missing or invalid.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the 2xx FCM reply body is untrusted; it crosses into the process in `FCMSender.Send` (already capped by `fcmMaxDrainBytes`), is decoded only into `fcmSendReply` (one field), and `name` passes `isFCMMessageID` before leaving `FCMSender`. Callers only ever hold a validated id or `""`. No envelope payload is read; the device token was already parsed by `parsePushWake` before this ticket.
- [Tokens & credentials] SHOULD FIX — the device token now feeds a logged value. Only `tokenFingerprint` (32 bits of SHA-256) is logged; the raw token never reaches a logger call or an error. Tests assert the raw device token and access token are absent from every captured line, including the end-to-end FCM path. Verifier checks the assertion covers success, failure and cancelled sends.
- [Tokens & credentials] No findings — a 32-bit truncated hash of an FCM registration token (~150+ chars, high entropy) cannot be inverted or brute-forced back to a usable token; it is a linkable per-device identifier, which is its purpose, and logs are operator-owned.
- [File operations] No findings — no files.
- [Subprocesses] No findings — none.
- [Cryptography] No findings — `crypto/sha256` from the standard library; used as a fingerprint, not as a MAC or secret comparison, so no constant-time requirement.
- [Network & I/O] No findings — no new reads; the 2xx body was already read under `fcmMaxDrainBytes`, and a body truncated by the cap fails to parse and yields `""`. No change to timeouts, rate limits or the in-flight cap.
- [Errors, logs & telemetry] SHOULD FIX — a hostile or compromised upstream could put free text, newlines or `key=value` lookalikes in `name` to forge log content; `isFCMMessageID`'s charset excludes space, `=`, `"`, newline and control bytes. Tests pin a hostile `name` is dropped and the log carries none of it. Three new keys go into `allowedLogKeys` in the same commit with their justification.
- [Concurrency] No findings — logging stays on the existing send goroutine before `wg.Done`; no new goroutine, lock or shared state.
- [Threat model alignment] No findings — no new dependency, endpoint or deploy change. The log-hygiene MAY list grows by two keys, recorded in the Documentation handoff. Per-outcome counters and `UNREGISTERED` feedback to the daemon are OUT OF SCOPE per the ticket (separate ticket / protocol change).

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-03
