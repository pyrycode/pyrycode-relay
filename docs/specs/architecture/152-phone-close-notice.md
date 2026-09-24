# #152 — Tell the binary when a phone's connection ends

## Files read

- `internal/relay/client_endpoint.go` → `ClientHandler` — the phone lifecycle; the notice is sent here once `StartPhoneForwarder` returns.
- `internal/relay/forward.go` → `StartPhoneForwarder` — returns the phone's read error, which carries the phone's close status; `StartBinaryForwarder` — hands every close directive to the phone's `phoneQueue`.
- `internal/relay/phone_outbox.go` → `phoneOutbox.Enqueue`, `deliver` — the only place a binary-requested close (`closeCode != 0`) reaches a production phone.
- `internal/relay/envelope.go` → `Envelope`, `Marshal` — `Envelope.Frame` has no `omitempty`, so marshalling an `Envelope` with no frame emits `"frame":null`, not the spec's frameless shape.
- `internal/relay/registry.go` → `BinaryFor`, `ScheduleReleaseServer`, `handleGraceExpiry`, reclaim eviction (#127) — a binary in grace stays in `binaries` with a dead conn; a reclaim puts a *different* binary under the same server-id and evicts the old phones with 4404.
- `internal/relay/client_endpoint_test.go` → `startClient`, `dialWithClient`, `waitForPhones` — harness the new tests use.
- `internal/relay/forward_test.go` → `fakeBinary` — mutex-safe recording binary.
- `internal/relay/registry_test.go` → `fakeConn` — `Send` appends without its mutex; with the notice, two phones closing at once would race on it under `-race`.
- `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_ReclaimDuringGraceClosesPhoneWith4404` — reads the first frame b2 receives and expects the new phone's probe; a notice for the evicted phone sent to b2 would break it.
- protocol-mobile.md § Routing envelope (binary↔relay leg), "New: a relay→binary close notice" — the wire shape `{conn_id, close_code}` with no `frame`.

In flight: `feature/127` touches `internal/relay/registry_test.go`; my edit there is local to `fakeConn.Send`.

## Context

When a phone's WebSocket ends the relay unregisters it and tells the binary nothing, so the daemon keeps that phone's v2 session until its 15-minute idle sweep, and `pushWaker` skips the device (pyrycode/pyrycode-mobile#955). The protocol spec now defines the relay→binary close notice; this ticket is the relay-side send.

## Design

**Wire shape.** A new unexported helper in `envelope.go`, `marshalCloseNotice(connID string, code uint16) ([]byte, error)`, marshals a two-field struct `{conn_id, close_code}` — exactly the spec's example, no `frame` key. Returns `ErrEmptyConnID` on an empty id. No payload is involved.

**Close code.** `phoneCloseCode(err error) uint16` in `client_endpoint.go`: `websocket.CloseStatus(err)` when it is a valid non-zero uint16, otherwise 1001 (`StatusGoingAway`) — no close frame (abrupt drop, forwarder ended on a nil or non-close error).

**Binary-requested close.** `phoneOutbox` gains `closeRequested atomic.Bool` and an accessor `closedByBinary() bool`. `Enqueue` sets it when `closeCode != 0` and the outbox is still live (after the `done` check) — i.e. the binary asked for this phone to be closed, whether the directive is then delivered or overflows the backlog. Every production close directive goes through `Enqueue`.

**Which binary.** `ClientHandler` snapshots `reg.BinaryFor(serverID)` right after a successful registration. When `StartPhoneForwarder` returns, the handler sends the notice only if:

1. `!phone.closedByBinary()`, and
2. `reg.BinaryFor(serverID)` still returns the *same* `Conn` as the snapshot (interface identity).

Condition 2 is "skip it when the binary is gone": a released server-id returns nothing, and a reclaimed one (#127) returns a new binary that never saw this `conn_id` — sending it there would be noise and would put a stray envelope ahead of the new phone's frames. A binary in grace is still listed; its `Send` fails fast on the closed conn and is logged.

The notice is sent in the handler body after the forwarder returns, **before** the deferred `UnregisterPhone`. It is sent on the same goroutine that forwarded the phone's frames, so it reaches the binary after the phone's last frame. Sending before unregistering also means a test that sees the phone unregistered knows the notice decision has already run.

Send failure logs `phone_close_notice_send_failed` with `server_id`, `conn_id`, `close_code`, `err` — all already allowlisted; no new log key.

## Concurrency model

No new goroutines. `closeRequested` is written by the binary forwarder goroutine (`Enqueue`) and read by the phone handler goroutine, hence `atomic.Bool`. `binary.Send` is already safe for concurrent callers (`WSConn.writeMu`) and carries its own write timeout, the same bound the phone forwarder already accepts on this goroutine.

## Error handling

- Marshal failure (unreachable: `connID` is never empty) — logged under the same event and skipped.
- Send failure — logged, skipped; the phone is gone either way and the daemon's idle sweep remains the fallback.

## Testing strategy

In `client_endpoint_test.go`, with a `fakeBinary` claimed as the server's binary:

- Phone closes with 1000 → binary receives exactly one message, which decodes to `conn_id` = the phone's id, `close_code` = 1000, and has no `frame` key.
- Phone drops without a close frame (`CloseNow`) → one notice with `close_code` 1001.
- Binary-requested close (`Enqueue(nil, 4401)` on the registered phone) → phone observes 4401, phone unregisters, binary received nothing.
- Binary replaced under the server-id before the phone closes → neither binary receives a notice.

`fakeConn.Send` takes its mutex so existing multi-phone tests stay race-free now that phone handlers send to the seeded binary. The #127 e2e test covers the reclaim path end to end.

## Open questions

- Do any existing tests with a real binary reader count frames after a phone closes? Resolve by running the package and e2e suites in Phase B.

## Documentation handoff

Pending for the documentation stage: `docs/architecture.md` (phone lifecycle / routing envelope section) should note that the relay now sends the binary a `{conn_id, close_code}` notice when a phone's connection ends on its own. The protocol spec already documents the shape; no change there.

## Revisions

- **2026-09-24, Phase B — open question resolved, no design change.** No existing test counts binary frames after a phone ends; `go test -race` passes for `./internal/relay/...` and `./cmd/pyrycode-relay/...`, including `TestRun_ReclaimDuringGraceClosesPhoneWith4404`. The new tests wait for the handler's `phone_unregistered` log line (via `captureLogger`) rather than for the registry to drop the phone. That line is the last step of teardown, whereas a reclaim evicts the phone from the registry before its handler has run.
