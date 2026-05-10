# Phone-side frame forwarder (#25)

Per-phone read pump that wraps each inbound phone frame in the routing
envelope and writes it to the binary registered for the phone's
server-id. Replaces the placeholder `<-readCtx.Done()` block in
`internal/relay/client_endpoint.go`. Mirror-image of the binary-side
forwarder (separate ticket).

## Files to read first

- `internal/relay/client_endpoint.go:26-88` — current `/v1/client`
  handler. Lines 81-86 hold the `CloseRead` + `<-readCtx.Done()` block
  this spec replaces. Note the existing
  `defer { UnregisterPhone; Close; log }` at lines 73-79 — the
  forwarder must NOT touch either.
- `internal/relay/ws_conn.go` (whole file, 83 lines) — adapter shape,
  `closeCtx` cancellation discipline, the "reach the conn only through
  WSConn methods" contract (lines 39-42), and the existing `Send`
  method as the template for the new `Read`.
- `internal/relay/envelope.go:40-55` — `Marshal(connID, frame)`
  signature, the JSON-validity precondition, and `ErrInvalidFrameJSON`
  / `ErrEmptyConnID` sentinels. Forwarder is the sole caller in this
  ticket.
- `internal/relay/registry.go:32-47` — `Conn` interface (`ConnID`,
  `Send`, `Close`); `BinaryFor` at lines 230-236 — the lookup the
  forwarder calls per frame.
- `internal/relay/registry.go:125-184` — grace-window semantics
  (`ScheduleReleaseServer` doc + `handleGraceExpiry`). Read these
  before deciding what the forwarder does when a `Send` fails or
  `BinaryFor` returns false. Critical: during grace, `BinaryFor`
  returns the dead binary; on expiry, registry calls `phone.Close()`
  on every registered phone.
- `internal/relay/registry_test.go:11-45` — `fakeConn` shape (id,
  `sent` slice, `closed` flag, optional `closeCh`). `forward_test.go`
  needs a similar binary-side fake but with mu-protected `sent` (the
  forwarder writes from a goroutine while the test reads).
- `internal/relay/envelope_test.go:10-38` — the canonical pattern for
  asserting bytewise opacity via `json.Compact`. `forward_test.go`
  reuses this exactly.
- `internal/relay/client_endpoint_test.go:21-65` — the
  `startClient` / `seedBinary` / `waitForPhones` helpers. The
  forwarder tests don't need an httptest server (they wire fakes to
  a real `Registry` directly), but the polling shape of
  `waitForPhones` is the model for `waitForSent`.
- `docs/lessons.md` § "json.RawMessage round-trips are byte-stable
  modulo whitespace" (line 37-39); § "Race-test count is a CI-runner
  knob" (line 45-47); § "A long-lived WS handler that does not read
  frames will never observe peer close" (lines 13-15) — explains why
  the existing `CloseRead` call must be deleted, not retained
  alongside the new reader.
- `pyrycode/pyrycode/docs/protocol-mobile.md` § Routing envelope
  (lines 100-120 in that file) — the wire shape `{conn_id, frame}`,
  and the rule that `conn_id` is relay↔binary only (the phone never
  sees it).

## Context

After `/v1/client` (#5) registers a phone, the handler currently parks
on `c.CloseRead(r.Context())` + `<-readCtx.Done()` so the WS
processes peer-side control frames but discards data frames. This
ticket lands the data path: read each inbound frame, wrap it in the
routing envelope keyed by the phone's relay-assigned `conn_id`, and
write the wrapped envelope to the binary's `Conn`.

Stateless relay; opaque inner frames; per-frame `BinaryFor` lookup
picks up grace-window reclaim transparently.

## Design

### New file: `internal/relay/forward.go`

Two pieces:

1. A small package-private interface `phoneSource` for the read side.
   Defined at the consumer (this file), not exported, not on
   `WSConn`. Tests pass a fake; production passes a `*WSConn` (which
   gains a `Read` method — see § "Edit: `internal/relay/ws_conn.go`"
   below).

   ```go
   type phoneSource interface {
       ConnID() string
       Read(ctx context.Context) ([]byte, error)
   }
   ```

2. The exported `StartPhoneForwarder` function — synchronous (despite
   the `Start` verb; the AC fixes the name). It blocks until a
   terminating condition is hit, returning the underlying cause for
   observability. The caller (the `/v1/client` handler) discards the
   return; its `defer` runs regardless.

   ```go
   func StartPhoneForwarder(
       ctx context.Context,
       reg *Registry,
       serverID string,
       phone phoneSource,
       logger *slog.Logger,
   ) error
   ```

### Loop body

```
for {
    frame, err := phone.Read(ctx)
    if err != nil {
        // ctx cancellation, peer close, library error — all funnel
        // here. Log at info; the caller's phone_unregistered log
        // closes out the lifecycle.
        logger.Info("phone_forwarder_read_end",
            "server_id", serverID,
            "conn_id", phone.ConnID(),
            "err", err)
        return err
    }

    wrapped, err := Marshal(phone.ConnID(), frame)
    if err != nil {
        // Adversarial input: phone sent non-JSON. Loud failure —
        // close the conn (handler's defer does this on return).
        logger.Warn("phone_forwarder_marshal_err",
            "server_id", serverID,
            "conn_id", phone.ConnID(),
            "err", err)
        return err
    }

    binary, ok := reg.BinaryFor(serverID)
    if !ok {
        // No binary: either grace already expired (registry will
        // have called phone.Close so we'd normally exit via Read
        // err), or the test seam where the handler skipped
        // RegisterPhone. Either way, return.
        logger.Info("phone_forwarder_no_binary",
            "server_id", serverID,
            "conn_id", phone.ConnID())
        return nil
    }

    if err := binary.Send(wrapped); err != nil {
        // During grace, BinaryFor returns the dead binary; its
        // Send fails. Log + return, handler's defer cleans up.
        logger.Info("phone_forwarder_send_failed",
            "server_id", serverID,
            "conn_id", phone.ConnID(),
            "err", err)
        return err
    }
}
```

### Edit: `internal/relay/ws_conn.go`

Add a `Read` method that delegates to the wrapped `*websocket.Conn`.
Mirror of `Send`'s shape, but no read mutex — the forwarder is the
sole reader by contract; concurrent `Read` is not supported (matches
nhooyr's library posture).

```go
// Read returns the next inbound message as opaque bytes. ctx bounds
// the wait; cancellation aborts the read with the library's wrapped
// error. The message type (binary vs text) is discarded — the relay
// treats inner frames as opaque bytes. Concurrent Read callers are
// NOT supported; the per-WSConn forwarder goroutine is the sole
// reader. After Close, in-flight Reads return with a close error
// from the underlying *websocket.Conn.
func (w *WSConn) Read(ctx context.Context) ([]byte, error) {
    _, data, err := w.conn.Read(ctx)
    return data, err
}
```

Update the type-level doc comment (lines 16-23 of `ws_conn.go`) to
note that `Read` is single-caller while `Send`, `ConnID`, `Close`
remain concurrency-safe.

`Read` does NOT take the `closeCtx` and does NOT join it with `ctx`.
Rationale: when `Close` cancels `closeCtx` AND closes the underlying
`*websocket.Conn`, the in-flight `c.Read(ctx)` returns immediately
with the library's close error. No need to plumb `closeCtx` through
the read path — the underlying close already aborts the read.

### Edit: `internal/relay/client_endpoint.go`

Replace the placeholder block (lines 81-86) with a
`StartPhoneForwarder` call:

```go
// Before:
readCtx := c.CloseRead(r.Context())
<-readCtx.Done()

// After:
_ = StartPhoneForwarder(r.Context(), reg, serverID, wsconn, logger)
```

Drop the `CloseRead` call entirely. The new read loop processes
control frames inline with data reads, so the drain-and-discard
goroutine is no longer needed (and would race the forwarder if
retained — see `lessons.md` line 13-15 and the `WSConn` "sole reader"
contract).

The existing `defer { UnregisterPhone; Close; log }` (lines 73-79)
runs unchanged when the forwarder returns. The forwarder MUST NOT
call `UnregisterPhone` or `wsconn.Close()` — that's the handler's
job, and doing it here would either double-close (idempotent, but
muddies the lifecycle) or unregister twice (no-op the second time
but signals a confused contract).

The return value is intentionally discarded (`_ =`). The handler's
`phone_unregistered` log already terminates the lifecycle from the
HTTP side; the forwarder's own logs cover the data path.

## Concurrency model

- One forwarder goroutine per phone (the HTTP handler goroutine
  itself; no extra goroutine spawned).
- `phone.Read` is single-caller (forwarder).
- `binary.Send` is multi-caller — `WSConn.writeMu` serialises across
  all phone forwarders writing to the same binary. Existing
  guarantee from #15.
- `reg.BinaryFor` takes RLock; cheap, contention-free under typical
  load.
- Shutdown paths:
  1. **Phone closes WS:** `c.Read` returns close error → loop
     returns → handler defer runs.
  2. **Server shutdown / request cancel:** `ctx` cancels → `c.Read`
     returns ctx error → loop returns.
  3. **Binary disconnect + grace expiry:** registry's
     `handleGraceExpiry` calls `wsconn.Close()` →
     `*websocket.Conn.Close` aborts in-flight `Read` → loop returns.
  4. **Binary disconnect during grace, before expiry:** `Send` to
     the (closed) binary fails → loop logs + returns. Phone's
     handler defer fires; phone gets unregistered; on grace expiry
     the phone is no longer in the snapshot so registry's
     fan-out-Close is a no-op for it.
- No goroutine leaks: all four paths terminate the single
  goroutine, and the handler's defer cleans up registry state.

## Error handling

| Cause                                | Action                                  | Log level |
|--------------------------------------|-----------------------------------------|-----------|
| `phone.Read` error (any)             | log, return err                         | info      |
| `Marshal` error (`ErrInvalidFrameJSON`) | log, return err — closes the phone   | warn      |
| `BinaryFor` returns false            | log, return nil                         | info      |
| `binary.Send` error                  | log, return err                         | info      |

`Marshal` returning `ErrEmptyConnID` is unreachable: the WSConn
guarantees a non-empty `ConnID` (set in `NewWSConn`, the handler
constructs it as `"client-" + serverID + "-" + randHex8()`). The
spec does not add a defensive check; if the invariant is ever
broken upstream, the loop returns the marshal error like any other
malformed-input case.

## Testing strategy

`internal/relay/forward_test.go`. All tests use mocks against a real
`Registry`; no httptest server.

Two test fakes (defined in this file, package-private):

- `fakePhone`: implements `phoneSource`. Holds an `id` and a
  `frames chan []byte`. `Read` selects on `ctx.Done()` and on
  `frames`; on closed frames-chan, returns `io.EOF`. (Tests close
  the chan to signal "phone disconnected".)
- `fakeBinary`: implements `Conn`. Holds an `id`, `mu sync.Mutex`,
  and `sent [][]byte`. `Send` appends under the mutex (the existing
  registry-test `fakeConn` does not, and the forwarder writes from
  a goroutine while the test reads — so we need the mutex). Add a
  `snapshot()` helper that copies under the lock for assertion.
  Provide a `waitForSent(t, binary, want, timeout)` helper that
  polls `len(snapshot()) == want`.

Test cases:

1. **Forwards 3 frames bytewise.** Claim a `fakeBinary` for
   server-id `"s1"` via `reg.ClaimServer`. Construct a `fakePhone`
   with id `"client-s1-aa00bb11"`; register it via
   `reg.RegisterPhone`. Run `StartPhoneForwarder` in a goroutine.
   Push 3 frames containing nested JSON (e.g.
   `{"type":"hello","nested":{"a":[1,2,3],"b":null}}`). Wait for
   `fakeBinary.sent` to have 3 entries. For each: `Unmarshal` the
   wrapped bytes, assert `ConnID == phone.ConnID()`, and assert the
   inner frame is byte-equal modulo whitespace via the `json.Compact`
   shape from `envelope_test.go`. Close the frames chan to terminate
   the forwarder; wait on its done chan.

2. **Phone disconnects mid-stream.** Same setup. Push 1 frame
   (assert it arrives). Close the frames chan → forwarder's `Read`
   returns `io.EOF` → forwarder returns. From the test goroutine,
   call `reg.UnregisterPhone(serverID, phone.ConnID())` (mimicking
   the handler defer) and assert `reg.PhonesFor(serverID)` is nil.

3. **No binary registered.** Construct a `fakePhone`. Do NOT call
   `ClaimServer`; do NOT call `RegisterPhone` (we're testing the
   forwarder's own resilience to a missing binary, not the
   handler's). Run forwarder, push 1 frame. Forwarder's `BinaryFor`
   lookup returns false → logs + returns nil. Assert via the
   forwarder's done-chan that it returned, and that no panic
   occurred. (Use `slog.New(slog.NewTextHandler(io.Discard, nil))`
   so the warn/info logs don't pollute test output.)

4. **Context cancellation.** Same setup as test 1 but DON'T push
   frames. Cancel the parent ctx. Forwarder's `Read` returns
   ctx.Err → loop returns. Assert return within 100 ms (generous
   bound under `-race`).

`make test` clean with `-race`. The package-level doc comment for
`forward_test.go` documents the manual stress invocation per the
race-count lesson:

```go
// Manual stress: go test -race -count=20 ./internal/relay/
// (count belongs on the command line, not as a loop in the test;
// see docs/lessons.md "Race-test count is a CI-runner knob".)
```

A `WSConn.Read` test isn't required by the AC, but a single
round-trip test in `ws_conn_test.go` (write a frame from the test
side, `Read` it through the adapter, assert byte-equal) prevents the
new method from regressing silently. ~15 LOC; counts toward the
production-side line budget but keeps `WSConn`'s contract test
parity with `Send`. Optional — the developer can defer if the budget
is tight.

## Open questions

None blocking. Two judgement calls the developer will hit:

1. **Return value plumbing.** `error` return is for observability
   only; the handler discards it. If the developer prefers a
   `void` return for clarity, that's acceptable — the AC says
   "exact signature shape is the architect's call". I've specified
   `error` because it preserves diagnostic signal at zero cost.

2. **`phoneSource` interface placement.** Defined in `forward.go`
   per the "interface at the consumer" pattern. If the binary-side
   forwarder ticket lands and wants the same shape mirrored for its
   read side, we can promote it later. No need to anticipate now.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings — phone WS bytes are untrusted;
  the forwarder treats them as opaque (`json.RawMessage` via
  `Marshal`) and the binary owns inner-frame validation. The single
  trust transition is `Marshal`'s `json.Valid` check at
  `envelope.go:44`; on failure the forwarder closes the phone (warn
  log + return → handler defer). Downstream binary code already
  knows it receives untrusted-via-relay bytes.
- **[Tokens, secrets, credentials]** No findings — the forwarder
  never reads or constructs `X-Pyrycode-Token`. The token already
  went out of scope unread in the `/v1/client` handler (#5). No
  log call site in this spec includes any header value; the
  enumerated log fields are `server_id`, `conn_id`, and `err` (a
  library error, not request-derived).
- **[File operations]** N/A — no filesystem access.
- **[Subprocess execution]** N/A — no subprocess.
- **[Cryptographic primitives]** N/A — no cryptographic operations
  in this ticket; the existing `randHex8`-derived `conn_id` is
  passed through unchanged.
- **[Network & I/O]** SHOULD FIX (deferred) — `c.Read(ctx)` has no
  per-message size cap. A malicious phone can send arbitrarily
  large frames to exhaust memory in `Marshal` and on the binary's
  buffers. **Out of scope for #25** — the per-frame size cap belongs
  on the WS upgrade configuration (`websocket.AcceptOptions` does
  not expose a max-message-size knob in nhooyr v1.8.x; the
  `*websocket.Conn` has `SetReadLimit` which is the right hook).
  Should be a follow-up ticket against `WSConn` so it covers the
  binary-side forwarder too. Tracking note: this is the same
  unbounded-read posture inherited from #15; not a regression.
  Default nhooyr read limit is 32 MiB which provides a soft floor
  but isn't the deliberate policy choice the threat model warrants.
- **[Network & I/O]** No finding on backpressure — a slow binary
  blocks the phone's forwarder goroutine. AC explicitly accepts
  this for v1; the `WSConn.Send` 10 s deadline (#15) bounds any
  single write, so a wedged binary causes the forwarder to return
  via `Send` error within 10 s rather than hanging indefinitely.
- **[Error messages, logs, telemetry]** No findings — the four log
  call sites enumerate fields explicitly. No frame bytes, no
  headers, no tokens enter logs. `err` carries library errors
  (close codes, ctx cancellation, write deadlines), not user
  payloads. No telemetry / metrics added.
- **[Concurrency]** No findings — single goroutine per phone (the
  HTTP handler's own); no new locks; per-frame `BinaryFor` lookup
  is RLock-only and does not nest with any caller-held lock.
  Goroutine lifecycle: the four termination paths in §
  "Concurrency model" cover ctx cancel, phone close, binary close,
  grace expiry. No leaks.
- **[Threat model alignment]** No findings — the relay's role per
  `protocol-mobile.md` § Routing envelope is "wrap, address,
  forward; never inspect". This spec preserves that: `json.Valid`
  is structural, not semantic; the binary owns token verification
  and message-kind dispatch. Out-of-scope items (heartbeat, binary
  disconnect grace, binary-side forwarder) are named in the
  ticket's Technical Notes section.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-09
