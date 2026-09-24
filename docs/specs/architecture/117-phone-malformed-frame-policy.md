# #117 — Phone forwarder malformed-frame policy: document and pin the teardown

Short plan: comments plus one pinning test. No behaviour change.

## Files read

- `internal/relay/forward.go` → `StartPhoneForwarder`, `StartBinaryForwarder` — the two doc comments that gain the policy statement; the `Marshal` error branch (`phone_forwarder_marshal_err`, `return err`) and the `Unmarshal` error branch (`binary_forwarder_unmarshal_err`, `continue`) they describe.
- `internal/relay/envelope.go` → `Marshal`, `ErrInvalidFrameJSON` — the structural `json.Valid` check that is the only thing the phone side inspects.
- `internal/relay/forward_test.go` → `fakePhone`, `fakeBinary`, `registryConn`, `runForwarder`, `waitForSent`, `TestStartBinaryForwarder_MalformedEnvelope_DropsAndContinues` — the harness and the binary-side analogue the new test mirrors.
- `docs/knowledge/features/phone-forwarder.md` § Adversarial framing — already states the teardown mechanics, not the reasons.
- `docs/knowledge/features/binary-forwarder.md` § *Error policy divergence from `StartPhoneForwarder`* — the table already has an "Envelope (un)marshal error" row (return / continue), so no row is missing.

No in-flight `feature/*` branch touches `forward.go` or `forward_test.go`.

## Change

`StartPhoneForwarder`'s doc comment gains a paragraph stating that a frame `Marshal` rejects (`ErrInvalidFrameJSON`) ends the loop and so the connection, and why: the frame came from this phone so only the sender is dropped; teardown bounds `phone_forwarder_marshal_err` to one warn line per connection, with each reconnect re-passing the upgrade rate limiter; and no real client has been observed dropped by an encoding glitch. `StartBinaryForwarder`'s existing "Does NOT return on per-frame errors" paragraph gains the other half of the reason for the asymmetry: closing a binary on a bad envelope would drop every phone it serves. No code moves.

## Testing strategy

New `TestStartPhoneForwarder_NonJSONFrame_TearsDown` in `forward_test.go`, beside the other phone-forwarder tests:

- Claim a binary, register the phone, start the forwarder; send one valid frame and wait for it at the binary (proves the pipe works, so the later absence is meaningful).
- Queue a non-JSON frame followed by a valid frame.
- The forwarder returns an error that `errors.Is(ErrInvalidFrameJSON)`.
- The binary holds exactly the one pre-malformed frame; the trailing valid frame is still unread in the phone's channel — neither the bad frame nor any later one reached the binary.

The behaviour already exists, so the test is green on first run. To show it can go red, temporarily flip the branch's `return err` to `continue` and watch the test fail, then restore.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/phone-forwarder.md` § Adversarial framing, "Non-JSON frame from phone": record that teardown is the chosen posture, with the three reasons (who pays, log volume bounded to one line per connection plus the upgrade rate limiter on reconnect, no observed failure).
- `docs/knowledge/features/binary-forwarder.md` § *Error policy divergence from `StartPhoneForwarder`*: the table already has the "Envelope (un)marshal error" row, so no new row is needed; optionally add the who-pays / log-volume reasoning to the prose below it.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — no code path changes. The phone frame remains opaque bytes; `Marshal`'s `json.Valid` in `StartPhoneForwarder` stays the only inspection, and the test asserts on the sentinel, not on frame content.
- [Tokens, secrets, credentials] No findings — no header or credential handling is touched.
- [File operations] No findings — no file I/O.
- [Subprocess] No findings — none introduced.
- [Cryptographic primitives] No findings — none used.
- [Network & I/O] No findings — size caps, timeouts and the upgrade rate limiter are unchanged. The posture being documented is itself a DoS consideration: teardown keeps per-connection warn-log volume from a hostile phone at one line, and each reconnect is metered by the upgrade limiter. Tolerating malformed frames (the rejected alternative) would have needed a per-connection log throttle.
- [Error messages, logs] No findings — no new log keys; `phone_forwarder_marshal_err` keeps its allowlisted keys (`server_id`, `conn_id`, `err`), and `err` is the fixed sentinel text, never frame bytes.
- [Concurrency] No findings — the test uses the existing buffered-channel fakes and the existing `runForwarder` goroutine, which exits on the returned error; its `cancel` is deferred.
- [Threat model alignment] No findings — no new endpoint, dependency or deploy change; no re-review trigger is tripped.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
