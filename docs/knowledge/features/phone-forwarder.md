# Phone-side frame forwarder

The phone-side data path. After `/v1/client` registers a phone in the registry, the handler hands the connection to `StartPhoneForwarder`, which reads frames from the phone, wraps each in the routing envelope keyed by the phone's relay-assigned `conn_id`, and writes the wrapped envelope to the binary holding the requested `serverID`. Inner frames are opaque bytes — the relay never parses the inner protocol.

Mirror image of the (separate) binary-side forwarder. Replaces the placeholder `<-readCtx.Done()` block that #5 left in place pending the read loop.

Authoritative wire spec: [`pyrycode/pyrycode/docs/protocol-mobile.md` § Routing envelope](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#routing-envelope).

## API

Package `internal/relay` (`forward.go`):

```go
type phoneSource interface {
    ConnID() string
    Read(ctx context.Context) ([]byte, error)
}

func StartPhoneForwarder(
    ctx context.Context,
    reg *Registry,
    serverID string,
    phone phoneSource,
    logger *slog.Logger,
) error
```

Despite the `Start` verb (carried from the AC), the call is **synchronous**: it blocks until a terminating condition is hit. The returned error is for observability only — the `/v1/client` handler discards it (`_ = StartPhoneForwarder(...)`); the lifecycle is closed out by the handler's existing `defer { UnregisterPhone; Close; log phone_unregistered }`.

`phoneSource` is package-private and defined at the consumer (this file), not on `WSConn`. Production passes `*WSConn`, which satisfies it via `Read` ([ws-conn-adapter.md](ws-conn-adapter.md)). Tests substitute a fake without touching `WSConn`'s shape.

## Loop body

Per iteration:

1. `phone.Read(ctx)` for the next frame. Any error → log `phone_forwarder_read_end` (info) and return.
2. `Marshal(phone.ConnID(), frame)` to wrap. Marshal failure (only `ErrInvalidFrameJSON` is reachable in practice — the `WSConn` constructor guarantees a non-empty `ConnID`) → log `phone_forwarder_marshal_err` (warn) and return; the handler's `defer` closes the conn.
3. `reg.BinaryFor(serverID)`. Missing → log `phone_forwarder_no_binary` (info) and return `nil` (the only non-error return path).
4. `binary.Send(wrapped)`. Error → log `phone_forwarder_send_failed` (info) and return.

No retries, no buffering, no backpressure handling beyond `WSConn.Send`'s own 10s write deadline. Backpressure-as-blocking is accepted for v1.

## Termination paths

| Cause | Mechanism | Cleanup owner |
|---|---|---|
| Phone closes WS | `c.Read` returns close error → loop returns | handler's `defer` |
| Server shutdown / request cancel | `ctx` cancel → `c.Read` returns `ctx.Err()` → loop returns | handler's `defer` |
| Binary disconnect + grace expiry | registry's `handleGraceExpiry` calls `wsconn.Close()` → underlying `*websocket.Conn.Close` aborts in-flight `Read` → loop returns | registry tore down state; handler's `defer` is idempotent |
| Binary disconnect during grace | next-frame `BinaryFor` still returns the dead binary → `Send` fails → loop returns | handler's `defer`; phone unregisters; on grace expiry the registry's fan-out-Close is a no-op for this phone |
| Adversarial non-JSON frame from phone | `Marshal` returns `ErrInvalidFrameJSON` → loop returns | handler's `defer` (warn-logged) |

All four production paths terminate the single goroutine; the handler's defer cleans up registry state. **No goroutine leaks.**

## Concurrency model

- One forwarder per phone — runs on the HTTP handler goroutine itself; no extra goroutine spawned.
- `phone.Read` (and `WSConn.Read`) is **single-caller**: the forwarder is the sole reader by contract. Concurrent `Read` is not supported by the underlying library.
- `binary.Send` is multi-caller; `WSConn.writeMu` (#15) serialises across all phone forwarders writing to the same binary. Frames go on the wire whole and non-interleaved.
- `reg.BinaryFor` takes an RLock; cheap, contention-free under typical load.

## Edits to neighbouring code

- `WSConn.Read(ctx) ([]byte, error)` was added in this ticket (`ws_conn.go`). It does **not** join `closeCtx`: when `Close` cancels `closeCtx` *and* closes the underlying `*websocket.Conn`, the in-flight `Read` returns immediately with the library's close error. No need to plumb `closeCtx` through the read path. The type-level doc lists `Read` as the single exception to "concurrent-safe with every other method." See [ws-conn-adapter.md](ws-conn-adapter.md).
- `/v1/client` handler now ends with `_ = StartPhoneForwarder(r.Context(), reg, serverID, wsconn, logger)` instead of `c.CloseRead(...) + <-readCtx.Done()`. The drain-and-discard goroutine `CloseRead` would have spawned is gone — the new read loop processes control frames inline with data reads, and retaining `CloseRead` would race the forwarder for the sole-reader role (see `lessons.md` § "A long-lived WS handler that does not read frames will never observe peer close" for why `CloseRead` was needed before this ticket; it is no longer needed once a real reader exists).

## Logging

Field set is fixed; nothing else (frame bytes, headers, tokens) appears.

| event | level | fields |
|---|---|---|
| `phone_forwarder_read_end` | info | `server_id`, `conn_id`, `err` |
| `phone_forwarder_marshal_err` | warn | `server_id`, `conn_id`, `err` |
| `phone_forwarder_no_binary` | info | `server_id`, `conn_id` |
| `phone_forwarder_send_failed` | info | `server_id`, `conn_id`, `err` |

`err` carries library errors (close codes, ctx cancellation, write deadlines), never user payloads. The handler's `phone_unregistered` log line bookends the lifecycle.

## What this forwarder deliberately does NOT do

- **No `UnregisterPhone` and no `wsconn.Close()`.** The handler's `defer` owns both. Doing them here would either double-close (idempotent, but muddies the lifecycle) or unregister twice (no-op the second time but signals a confused contract).
- **No inner-frame parsing.** The relay's role per `protocol-mobile.md` § Routing envelope is "wrap, address, forward; never inspect." `Marshal`'s `json.Valid` check is structural, not semantic.
- **No buffering, no bounded channels.** A slow downstream `Send` blocks this goroutine; `WSConn.Send`'s 10s deadline bounds the worst case to ~10s before the loop returns via `Send` error.
- **No per-frame size cap.** Inherited from `WSConn`'s underlying `*websocket.Conn` (nhooyr's default 32 MiB read limit). A deliberate per-message cap is a follow-up against `WSConn` so it covers both forwarders. Not a regression introduced by this ticket; named explicitly in the architect's security review.
- **No retries on `Send` failure.** Returns and lets the handler's defer run; the phone reconnects.
- **No heartbeat / ping-pong.** Separate ticket.

## Adversarial framing

- **Non-JSON frame from phone.** `Marshal`'s `json.Valid` returns false → forwarder returns with a warn log → handler closes the conn. Loud but not panicky.
- **Frame larger than the binary's tolerance.** Forwarder doesn't know or care; the binary owns inner-frame validation. Per-message size on the inbound side is the inherited residual.
- **Phone races a binary disconnect.** The `BinaryFor`-then-`Send` pattern is best-effort; during grace `BinaryFor` returns the dead binary. The forwarder logs and exits cleanly when `Send` fails. No double-free, no double-unregister — the handler's defer is idempotent against the registry's grace-expiry fan-out (the `WSConn.Close` `closeOnce` swallows the second close; `UnregisterPhone` no-ops on an already-removed entry).
- **Slow binary.** Bounded by `WSConn.Send`'s 10s deadline (#15). A wedged binary causes the forwarder to return via `Send` error within ~10s rather than hanging indefinitely.
- **Crafted log content.** `err` strings come from the WS library (close codes, ctx errors, write deadline errors); no user-supplied bytes enter logs.

Verdict from the architect's security review: **PASS**. No new credentials handled, no new logging surface, no new locks. The single residual (per-message size cap) is inherited from #15 and tracked separately.

## Testing

`internal/relay/forward_test.go`, `package relay`. All tests wire mocks to a real `Registry`; no httptest server (the handler-level integration is covered by `/v1/client`'s own tests).

Two package-private fakes:

- `fakePhone` — implements `phoneSource`. Holds a `frames chan []byte`. `Read` selects on `ctx.Done()` and on `frames`; closed channel → `io.EOF`. Tests close the chan to signal "phone disconnected".
- `fakeBinary` — implements registry `Conn`. Holds a mutex-protected `sent [][]byte` (the forwarder writes from a goroutine while the test reads, so the existing `registry_test.go` `fakeConn` shape needed mu-protection). `snapshot()` copies under the lock; `waitForSent(t, want, timeout)` polls.

Tests (1:1 with AC):

- **Forwards 3 frames bytewise.** Push 3 frames containing nested JSON; assert each wrapped envelope at the binary side carries the registered phone's `conn_id` and an inner frame byte-equal **modulo whitespace** via `json.Compact` per the lesson on `json.RawMessage` round-trips.
- **Phone disconnects mid-stream.** Push 1 frame, close the frames chan → forwarder returns; mimic the handler-level `UnregisterPhone`; assert `Registry.PhonesFor(serverID)` is nil.
- **No binary registered.** Don't claim a binary; push 1 frame; forwarder logs and returns `nil`. No panic.
- **Context cancellation.** Cancel parent ctx; forwarder returns within 100ms (generous bound under `-race`).

`make test` clean with `-race`. The package-level doc comment in `forward_test.go` documents the manual stress invocation `go test -race -count=20 ./internal/relay/` per the [race-count lesson](../../lessons.md).

## Related

- [`/v1/client`](client-endpoint.md) — sole production caller; owns the cleanup defer the forwarder must not touch.
- [Routing envelope](routing-envelope.md) — `Marshal` is the per-frame wrap.
- [WSConn adapter](ws-conn-adapter.md) — `Read` was added here to satisfy `phoneSource`; sole-reader contract documented on the type.
- [Connection registry](connection-registry.md) — `BinaryFor` is the per-frame lookup.
- [ADR-0006](../decisions/0006-grace-period-as-reclaim-path.md) — explains why `BinaryFor` may return a dead binary during grace, and why a `Send` failure is the forwarder's signal to exit.
- [Protocol spec § Routing envelope](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#routing-envelope) — authoritative wire shape.
