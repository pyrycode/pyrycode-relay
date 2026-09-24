# Binary-side frame forwarder

The binary-side data path. After `/v1/server` claims the slot for `serverID` and the heartbeat goroutine has been launched, the handler hands the connection to `StartBinaryForwarder`, which reads frames from the binary, unwraps each routing envelope, looks up the phone registered under `serverID` whose `ConnID()` equals `env.ConnID`, and writes `env.Frame` verbatim to that phone. Inner frames are opaque bytes — the relay never parses the inner protocol.

Mirror image of the [phone-side forwarder](phone-forwarder.md). Same overall shape — synchronous despite the `Start` verb, consumer-defined source interface, return value is observability-only — with a divergent **error policy**: a single misaddressed envelope, malformed envelope, or failing `phone.Send` does NOT tear down the binary connection.

Replaces the `c.CloseRead(r.Context())` + `<-readCtx.Done()` placeholder #16 left in place pending the read loop.

Authoritative wire spec: [`pyrycode/pyrycode/docs/protocol-mobile.md` § Routing envelope](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#routing-envelope).

## API

Package `internal/relay` (`forward.go`):

```go
type binarySource interface {
    ConnID() string
    Read(ctx context.Context) ([]byte, error)
}

func StartBinaryForwarder(
    ctx context.Context,
    reg *Registry,
    serverID string,
    binary binarySource,
    logger *slog.Logger,
) error
```

Despite the `Start` verb (carried from the AC), the call is **synchronous**: it blocks until `binary.Read` errors or `ctx` is cancelled. The returned error is for observability only — the `/v1/server` handler discards it (`_ = StartBinaryForwarder(...)`); lifecycle is closed by the handler's existing `defer { ScheduleReleaseServer; Close; log server_released }` (#16/#21) and `defer cancelHB()` (#7).

`binarySource` is package-private and defined at the consumer (this file), structurally identical to `phoneSource` but distinct so the two `Start*Forwarder` signatures read self-describingly at call sites. Production passes `*WSConn`, which satisfies it via `Read`. Tests substitute a fake without touching `WSConn`'s shape.

## Loop body

Per iteration:

1. `binary.Read(ctx)` for the next wire-encoded envelope. Any error → log `binary_forwarder_read_end` (info) and **return**.
2. `Unmarshal(wrapped)`. Any sentinel (`ErrMalformedEnvelope`, `ErrMissingConnID`, `ErrMissingFrame`, defensive catch-all) → log `binary_forwarder_unmarshal_err` (warn) and **continue**.
3. `env.ConnID == ""` (a [`push_wake` envelope](routing-envelope.md#push_wake-shape-relay-addressed), the only way `Unmarshal` returns an empty `ConnID` without erroring) → hand off to the registry's `PushWaker` (see [§ push_wake handling](#push_wake-handling) below) and **continue**.
4. `reg.PhoneFor(serverID, env.ConnID)` (#116, zero-allocation — scans the live phones slice under the registry's read lock rather than copying a `PhonesFor` snapshot). Miss → log `binary_forwarder_unknown_conn_id` (warn) and **continue**.
5. `phone.Send(env.Frame)`. Error → log `binary_forwarder_phone_send_failed` (info) and **continue**.

The relay neither parses nor canonicalises the inner bytes; `env.Frame` is forwarded verbatim. `json.RawMessage` makes accidental inspection hard.

## `push_wake` handling

A `push_wake` envelope addresses the relay itself, not a phone, so it never reaches the `PhonesFor` lookup. `StartBinaryForwarder` hands `env.PushWake` straight to `reg.pushWaker.Request(serverID, env.PushWake)` (`internal/relay/registry.go`'s `pushWaker` field, set once at boot via `SetPushWaker` — nil means push is off, same "optional collaborator" shape as `SetForwarderHooks`). See [Push wake dispatch](push-wake-dispatch.md) for what `Request` does: structural validation, a per-server-id rate limit, a relay-wide in-flight cap, and an async send off this loop so a blocking `FCMSender.Send` never stalls frame forwarding.

Every refusal — bad platform, empty token, malformed object, over either limit, or push off — comes back from `Request` as an error and is logged as a single line, same as any other per-frame drop:

```go
if err := reg.pushWaker.Request(serverID, env.PushWake); err != nil {
    logger.Warn("binary_forwarder_push_wake_dropped", "server_id", …, "binary_conn_id", …, "err", err)
}
continue
```

The binary connection is never affected by a refused wake — same "one bad frame doesn't tear down the loop" contract the rest of this forwarder follows. No token or credential value ever reaches this log line; `err` is one of `Request`'s sentinels, none of which carry the token (see [Push wake dispatch § Error handling](push-wake-dispatch.md#error-handling)).

## Error policy divergence from `StartPhoneForwarder`

| Cause | `StartPhoneForwarder` | `StartBinaryForwarder` |
|---|---|---|
| Source `Read` error | return | return |
| Envelope (un)marshal error | return | **continue** |
| Sink missing (no binary / unknown conn_id) | return `nil` | **continue** |
| Sink `Send` error | return | **continue** |

The phone-side forwarder has one sink (the binary); a failing sink means the conn is dead, so the loop ends. The binary-side forwarder has N sinks (phones); a single failing sink does not justify dropping every other phone served by this binary. The phone-side semantics encode "frame loop owns the lifecycle of its single pair"; the binary-side semantics encode "frame loop is decoupled from the lifecycle of its sinks." A bad frame from the binary MUST NOT tear the binary connection down — phones come and go, and an envelope addressing a just-disconnected phone is a normal race.

The forwarder does not branch on specific `Unmarshal` sentinels — all four get the same warn-and-continue treatment. Branching would add no behaviour and risks divergence if envelope errors are added later.

## Termination paths

| Cause | Mechanism | Cleanup owner |
|---|---|---|
| Binary closes WS | `c.Read` returns close error → loop returns | handler's `defer` |
| Server shutdown / request cancel | `ctx` cancel → `c.Read` returns `ctx.Err()` → loop returns | handler's `defer` |
| Heartbeat fires (no pong) | `wsconn.CloseWithCode(1011, "heartbeat timeout")` → underlying `*websocket.Conn` aborts in-flight `Read` → loop returns | handler's `defer` (`closeOnce` swallows the second close) |
| Per-frame error | `continue` — loop does **not** terminate | n/a |

All termination paths terminate the single goroutine; the handler's defer cleans up registry state. **No goroutine leaks.**

## Concurrency model

- One forwarder per binary — runs on the HTTP handler goroutine itself; no extra goroutine spawned. Runs in parallel with the heartbeat goroutine on a sibling goroutine; the two never share state.
- `binary.Read` (and `WSConn.Read`) is **single-caller**: the forwarder is the sole reader by contract. Concurrent `Read` is not supported by the underlying library.
- `phone.Send` is multi-caller across the system: every binary's forwarder writing to a given phone, plus any future server-side signal path. `WSConn.writeMu` (#15) serialises them. No new locks added by this ticket.
- `reg.PhoneFor` takes `RLock`, scans the live phones slice for `serverID`, and returns before this loop touches the result — allocating nothing per frame (#116; previously `reg.PhonesFor` copied the whole slice every frame). The scan itself runs under the lock rather than outside it, but stays cheap: phones per server-id are capped at `maxPhones` (16), so the critical section is bounded and short, matching the established passive-store contract.
- Backpressure: since #113, delivery to a phone goes through its own bounded [`phoneOutbox`](phone-outbox.md) instead of a direct `phone.Send` — the forwarder only enqueues (`Enqueue`, never blocks) and the phone's own writer goroutine drains it. A slow or stalled phone therefore no longer blocks this loop, its siblings, or the binary's own pong processing at all; it only blocks its own writer, bounded first by the outbox's 16-frame depth and then by `WSConn.Send`'s 10 s deadline once a write is actually in flight. A phone whose backlog fills or whose write fails is closed with `1011` and its pending frames dropped. `env.CloseCode != 0` close directives go through the same queue, ordered behind every frame enqueued before them. The direct-`Send` / direct-`closePhone` code below this loop is the fallback for a `Conn` that does not implement the outbox's `phoneQueue` capability — in production that's every registered phone, so the fallback serves only this package's unit-test fakes.

## Edits to neighbouring code

- `/v1/server` handler now ends with `_ = StartBinaryForwarder(r.Context(), reg, serverID, wsconn, logger)` instead of `c.CloseRead(...)` + `<-readCtx.Done()`. The `CloseRead` call is **deleted entirely** — the new read loop processes control frames inline with data reads, and retaining `CloseRead` would race the forwarder for the `*websocket.Conn` sole-reader role (see `lessons.md` § "A long-lived WS handler that does not read frames will never observe peer close").
- No changes to `WSConn`: `Read` was added in #25 and already satisfies `binarySource` via the same method.

## Logging

Field set is fixed; nothing else (envelope bytes, frame bytes, headers, tokens) appears.

| event | level | fields |
|---|---|---|
| `binary_forwarder_read_end` | info | `server_id`, `binary_conn_id`, `err` |
| `binary_forwarder_unmarshal_err` | warn | `server_id`, `binary_conn_id`, `err` |
| `binary_forwarder_push_wake_dropped` | warn | `server_id`, `binary_conn_id`, `err` |
| `binary_forwarder_unknown_conn_id` | warn | `server_id`, `conn_id` |
| `binary_forwarder_phone_enqueue_failed` | info | `server_id`, `conn_id`, `err` |
| `binary_forwarder_phone_send_failed` | info | `server_id`, `conn_id`, `err` |

`err` carries library errors (close codes, ctx cancellation, write deadlines) and `Unmarshal` sentinels — none of which embed user payloads (the `Unmarshal` wrap in `envelope.go:70` includes the JSON decoder error, which can name a byte offset but not payload contents). The handler's `server_released` log bookends the lifecycle.

## What this forwarder deliberately does NOT do

- **No `ScheduleReleaseServer`, no `wsconn.Close()`, no `cancelHB`.** The handler's defers own all three. Doing them here would either double-close or arm a stray grace timer.
- **No inner-frame parsing.** The relay's role per `protocol-mobile.md` § Routing envelope is "wrap, address, forward; never inspect." `Unmarshal`'s structural checks inspect envelope shape only, never `env.Frame`.
- **No retries.** Drop the offending frame and keep serving.
- **No direct phone writes.** Since #113, the forwarder hands every frame and close directive to the addressed phone's [`phoneOutbox`](phone-outbox.md) via `Enqueue`, which never blocks; the outbox's own writer goroutine does the actual `Send`. This loop is never the one waiting on a phone's socket.
- **No per-frame size cap.** Inherited from `WSConn`'s underlying `*websocket.Conn` (nhooyr's default 32 MiB read limit). A deliberate per-message cap is a follow-up against `WSConn` so it covers both forwarders. Not a regression introduced by this ticket; named explicitly in the architect's security review.
- **No branching on specific `Unmarshal` sentinels.** All sentinels funnel through one warn-and-continue branch — `errors.Is` is not needed when every case is the same.
- **No heartbeat / ping-pong.** Separate goroutine (#7).

## Adversarial framing

- **Cross-server addressing.** `PhoneFor(serverID, connID)` scopes the lookup to the binary's own slot. A binary cannot address phones registered under a different server-id even if it forges an `env.ConnID` collision — the scan only considers phones it owns. Structural defence, not a runtime check; follows from the registry's per-server-id map shape (#3).
- **Malformed envelope.** `Unmarshal` returns a sentinel; loop logs + continues. The binary owns its own protocol health; the relay does not punish it for one bad frame.
- **Unknown `conn_id`.** Phone disconnected between the binary's last observation and this envelope, or the binary addressed a phone it shouldn't know about. Either way, a normal race or a binary bug — drop, continue.
- **Adversarial `env.Frame` contents.** Forwarded verbatim as opaque bytes. The downstream phone's protocol layer already knows it receives untrusted-via-relay bytes (the binary is trusted relative to the phone, not relative to the relay).
- **Slow phone.** Since #113, bounded by the phone's own [`phoneOutbox`](phone-outbox.md) rather than this loop: a slow or stalled phone never blocks the read pump, other phones on the same server-id, or the binary's pong processing. Its backlog fills at 16 frames, or a write times out at `WSConn.Send`'s 10 s deadline, and either way the phone is closed with `1011` and its pending frames dropped.
- **Frame larger than any phone's tolerance.** Forwarder doesn't know or care; per-message size on the inbound side is the inherited residual on `WSConn`.
- **Crafted log content.** `err` strings come from the WS library or `Unmarshal` sentinels; no envelope/frame bytes enter logs.

Verdict from the architect's security review: **PASS**. No new credentials handled, no new locks, no logging surface beyond the four enumerated events. The single residual (per-message size cap on `WSConn`) is shared with #25 and tracked separately.

## Testing

`internal/relay/forward_test.go` (extended from #25), `package relay`. All tests wire mocks to a real `Registry`; no httptest server (handler-level integration is covered by `/v1/server`'s own tests).

Two new test fakes added alongside `fakePhone` / `fakeBinary` from #25:

- `fakeBinarySource` — implements `binarySource`. Holds a `frames chan []byte`. `Read` selects on `ctx.Done()` and `frames`; closed chan → `io.EOF`. Mirrors `fakePhone`'s Read-side shape.
- `fakePhone` was extended (not duplicated) to satisfy `Conn` directly via a mu-protected `sent [][]byte` and a `sendErr` knob. Existing #25 tests use `&registryConn{phone}` and are unaffected (they never inspect `fakePhone.sent`). New tests register `fakePhone` directly via `reg.RegisterPhone(serverID, phone)` and inspect `phone.snapshotSent()`.

Tests (1:1 with AC):

- **Routes to addressed phone.** Envelope addressed to P1 lands at P1; P2 receives nothing; inner bytes byte-equal modulo whitespace via `json.Compact`.
- **Multiple phones.** One envelope per phone; each receives exactly its own.
- **Unknown conn_id drops and continues.** Bogus `client-s1-deadbeef` dropped; subsequent envelope to P1 still lands; closing the binary frames chan returns `io.EOF` (proves the forwarder is still running).
- **Malformed envelope drops and continues.** `[]byte("not-json")` dropped; subsequent valid envelope lands.
- **Phone Send error drops and continues.** `p1.sendErr` non-nil; envelope to P1 dropped (`len(p1.sent) == 0`); envelope to P2 still lands. **Encodes the divergence** from `StartPhoneForwarder`'s return-on-Send-error.
- **Binary disconnect returns.** Source `Read` returns `io.EOF` → forwarder returns; test mimics handler defer via `reg.ScheduleReleaseServer("s1", 0)` and polls `BinaryFor` to confirm the slot clears.
- **Context cancellation returns.** `cancel()` → forwarder returns `context.Canceled` within 100 ms.

`make test` clean with `-race`. Package-level doc comment in `forward_test.go` already documents `go test -race -count=20 ./internal/relay/` per the race-count lesson.

## Related

- [`/v1/server`](server-endpoint.md) — sole production caller; owns the cleanup defers the forwarder must not touch.
- [Phone-side forwarder](phone-forwarder.md) — mirror image; explains why the two have distinct named source interfaces despite identical shape.
- [Routing envelope](routing-envelope.md) — `Unmarshal` is the per-frame structural check; all four sentinels funnel through one warn-and-continue branch.
- [Push wake dispatch](push-wake-dispatch.md) — what happens to a `push_wake` envelope after this forwarder hands it off: validation, rate limit, in-flight cap, async send.
- [Per-phone delivery queue](phone-outbox.md) — where a routed frame or close directive actually goes since #113: the addressed phone's bounded outbox, not a direct `Send` from this loop.
- [WSConn adapter](ws-conn-adapter.md) — `Read` (added in #25) satisfies `binarySource` in production.
- [Connection registry](connection-registry.md) — `PhoneFor` is the per-frame lookup (#116); it allocates nothing, scanning the live phones slice under the read lock instead of copying a `PhonesFor` snapshot.
- [Heartbeat](heartbeat.md) — sibling per-conn goroutine; closes the conn with `1011` on pong timeout, which aborts the forwarder's in-flight `Read`.
- [ADR-0001](../decisions/0001-routing-envelope-shape-and-opacity.md) — opacity-by-type and sentinel errors.
- [ADR-0006](../decisions/0006-grace-period-as-reclaim-path.md) — grace-window semantics; the forwarder is dead before grace expiry and never sees expiry-time state.
- [Protocol spec § Routing envelope](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#routing-envelope) — authoritative wire shape.
