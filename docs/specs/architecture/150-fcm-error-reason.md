# #150 — Log FCM's error reason when a wake send fails

Short plan: one guarded decode on an existing error branch, no new type
exported, no new state, no new log key.

## Files read

- `internal/relay/push.go` → `FCMSender.Send`, `ErrFCMSend`, `fcmMaxDrainBytes` — the non-2xx branch that today returns `ErrFCMSend: status N` after a bounded discard-drain.
- `internal/relay/push_test.go` → `fakeFCM`, `assertNoSecrets`, `TestFCMSender_NonSuccessReplyNamesOnlyStatus` — the harness the new case mirrors; `fakeFCM`'s body is non-JSON (marker + device token + bearer), so the existing test already covers the "not JSON" fallback.
- `internal/relay/push_wake.go` → `PushWaker.send` — logs the error under the allowlisted `err` key as `push_wake_send_failed`; its comment says the error carries "at most an HTTP status", which this ticket makes stale.
- `docs/knowledge/features/fcm-push-sender.md` § "Error handling — nothing leaks" — the table row for FCM non-2xx says "HTTP status only".

## Change

In `FCMSender.Send`, replace the discard-drain with a read of at most
`fcmMaxDrainBytes` into memory (still bounded, same cap). On a non-2xx reply,
decode that buffer into an unexported struct that declares only
`error.status` and `error.details[].{@type, errorCode}` — `error.message` is
not a field, so free text is never even held in a typed value. Take
`errorCode` only from a detail whose `@type` is
`type.googleapis.com/google.firebase.fcm.v1.FcmError`. Each of the two values
is kept only when it looks like an enum: 1–64 bytes of `[A-Z0-9_]`; anything
else is dropped. This makes "nothing else from the body" deterministic even
if the upstream reply were hostile or a proxy echoed request data into those
fields. The error becomes `ErrFCMSend: status 404 (NOT_FOUND, UNREGISTERED)`
when both are present, with only the present one in the parentheses when one
is, and exactly today's `ErrFCMSend: status N` when the body is empty, not
JSON, truncated past the cap, or lacks both fields. A decode failure is never
wrapped or reported. The 2xx path still reads and discards up to the cap.

Also update `PushWaker.send`'s comment to say the error carries the HTTP
status plus FCM's enum reason codes, never the token or free text. No log key
is added: the reason travels inside the already-allowlisted `err` value.

Not a content-blindness question: the decoded body is Google's reply to the
relay's own request, not a routed payload.

## Testing strategy

In `push_test.go`, beside `TestFCMSender_NonSuccessReplyNamesOnlyStatus`:

- A fake FCM replying 404 with a standard error body whose `message` carries
  the marker and device token → error is exactly
  `…: status 404 (NOT_FOUND, UNREGISTERED)`, `assertNoSecrets` passes.
- Table of fallbacks, each asserting the error ends in `status N` with no
  parenthesised suffix: empty body, non-JSON, JSON without the fields, an
  `errorCode` under a non-FcmError `@type`, and enum fields carrying non-enum
  text (the marker/token) — the last also runs `assertNoSecrets`.
- One case with only `error.status` present → `status 400 (INVALID_ARGUMENT)`.
- The existing non-JSON test stays green unchanged.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/fcm-push-sender.md` § "Error handling — nothing
  leaks": the FCM non-2xx row now carries `status N` plus FCM's
  `error.status` and FcmError `errorCode` when they are enum-shaped; and
  § "Sending a message": the reply body is now read (bounded) and decoded on
  non-2xx, not only drained.
- `docs/threat-model.md` § "Outbound network calls — FCM push wake", v1
  mitigation paragraph: "`FCMSender`'s errors carry at most an HTTP status"
  becomes "an HTTP status plus FCM's enum-shaped `error.status` and
  `errorCode`, never free text or the token".
