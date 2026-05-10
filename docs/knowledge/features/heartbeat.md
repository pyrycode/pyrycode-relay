# WebSocket heartbeat — RFC 6455 ping/pong

Every accepted WS connection (binary on `/v1/server`, phone on `/v1/client`) gets a per-connection heartbeat goroutine that sends an RFC 6455 ping every 30 seconds. If the matching pong does not arrive within 30 seconds of the most recent ping, the goroutine closes the conn with code `1011 server error` and reason `"heartbeat timeout"`. The handler's existing release/unregister defer then runs naturally (peer-close path, via `CloseRead` surfacing the close to `readCtx`).

This is the only mechanism the relay has to detect a half-open TCP connection — a peer whose socket dies silently (NAT timeout, mobile interface flap, kernel panic, severed cable) leaves no application-layer signal otherwise. Worst-case dead-connection detection: **60 seconds** (interval + timeout).

## Wire shape

Standard RFC 6455 control frames. The library (`nhooyr.io/websocket`) handles ping/pong framing internally:

- Server sends a `Ping` opcode with library-default empty payload.
- Peer's library auto-responds with a matching `Pong`.
- A peer that does not process incoming pings (no `Read` / `CloseRead` on its side) cannot pong.

No application-level keepalive messages. No protocol-spec changes. Per-endpoint behaviour is **identical** for binaries and phones.

## API

Package `internal/relay` (`heartbeat.go`):

```go
const (
    heartbeatInterval = 30 * time.Second
    heartbeatTimeout  = 30 * time.Second
)

func runHeartbeat(ctx context.Context, w *WSConn, interval, timeout time.Duration)
```

One unexported function and two file-level constants. `runHeartbeat` is called from both upgrade handlers as `go runHeartbeat(hbCtx, wsconn, heartbeatInterval, heartbeatTimeout)` after the registry claim/register succeeds.

Uses two methods on `WSConn` added in this ticket (see [ADR-0007](../decisions/0007-wsconn-closewithcode-for-active-conn.md)):

- `WSConn.Ping(ctx) error` — pure forwarder over `*websocket.Conn.Ping`.
- `WSConn.CloseWithCode(code, reason)` — `closeOnce`-guarded close with custom code/reason; `Close()` now delegates to it as `CloseWithCode(StatusNormalClosure, "")`.

## Constants are file-level, not flags

`heartbeatInterval` and `heartbeatTimeout` are file-level constants — not `cmd/pyrycode-relay/main.go` flags, not constructor parameters. Two reasons:

1. **Two wiring entry points share the same value.** `/v1/server` and `/v1/client` both call `runHeartbeat(..., heartbeatInterval, heartbeatTimeout)`. Naming the value once and referring to it from both sites is the threshold the project pattern names ("Promote to a package-level constant only when a second wiring entry point needs the same value"). The grace duration in `/v1/server` (#21) had only one wiring entry point and stayed inline; heartbeat has two and gets a constant.
2. **The AC explicitly requires file-level constants.** Surfacing them as flags would invite operator-tweaking against the protocol spec. v1 deliberately does not configure these.

Tests pass shorter values (50ms / 100ms) directly to `runHeartbeat` so production constants are not coupled to test wall-clock cost.

## Handler wiring

Both handlers, immediately after the successful register and after the existing release/unregister defer is in place:

```go
hbCtx, cancelHB := context.WithCancel(r.Context())
defer cancelHB()
go runHeartbeat(hbCtx, wsconn, heartbeatInterval, heartbeatTimeout)
```

`cancelHB` is registered as a defer **after** the release defer, so it runs **first** under LIFO unwind. The order matters:

1. `cancelHB()` fires → heartbeat goroutine observes `ctx.Done()` and returns without touching the conn.
2. Then the release defer runs `wsconn.Close()` (`StatusNormalClosure`).

On the clean-shutdown / peer-close path the wire sees exactly one normal-close. On the heartbeat-timeout path, heartbeat has already emitted `CloseWithCode(1011, ...)` before unwind; the subsequent `wsconn.Close()` is a `closeOnce` no-op (the wire saw 1011).

## The cancellation invariant

`runHeartbeat`'s body has two `return` paths that **do not** call `CloseWithCode`:

```go
case <-ctx.Done():
    return                          // (1) ctx fired between ticks
case <-ticker.C:
    pingCtx, cancel := context.WithTimeout(ctx, timeout)
    err := w.Ping(pingCtx)
    cancel()
    if err == nil { continue }
    if ctx.Err() != nil {
        return                      // (2) ctx fired while Ping was in flight
    }
    w.CloseWithCode(websocket.StatusInternalError, "heartbeat timeout")
    return
```

Both ctx-cancellation cases — (1) and (2) — exit cleanly. The handler defer owns the clean-close path; `runHeartbeat` owns **only** the timeout-driven close. This is what AC bullet (c) — "exits cleanly when the connection's context is cancelled, without attempting a close call" — encodes. The structural property: there is exactly one `CloseWithCode` call site, gated by `ctx.Err() == nil`.

`time.NewTicker` (with `defer ticker.Stop()`) is used rather than `time.Tick`; `Tick` leaks its underlying ticker for the lifetime of the program, which is wrong inside any goroutine that exits.

## Concurrency

Per accepted upgrade: **3 goroutines** (was 2 pre-#7).

| Goroutine | Source | Lifecycle |
|---|---|---|
| handler | http.Server | request-scoped; blocks on `<-readCtx.Done()` |
| `CloseRead` drain | `c.CloseRead(r.Context())` | exits on conn close / r.Context cancel |
| heartbeat | `go runHeartbeat(...)` (this ticket) | exits via ctx.Done OR timeout-then-close |

No leak path: `defer cancelHB()` is unconditional once heartbeat has been launched, and heartbeat is launched only after the release defer is in place. Any panic between the two still cancels.

`Ping` (a control frame) and `Send` (a binary frame) at the underlying-library level: `nhooyr.io/websocket` serialises pings against writes via its own internal mutex. `WSConn.writeMu` guards *concurrent `Send` callers* (broadcasters), not Send-vs-Ping. `runHeartbeat` correctly does not take `writeMu` — control frames and data frames have separate ordering requirements, and the library's internal serialisation is the right granularity.

`CloseWithCode` and `Close` share the same `closeOnce`. Whichever close-family call fires first wins; the wire sees exactly one close frame.

## Close-frame timing — what the test had to reckon with

`nhooyr.io/websocket.Conn.Close` performs a close handshake: it writes the close frame (5s write timeout) then waits up to 5s for the peer's reciprocal close frame before tearing down the TCP. For a peer that has stopped reading entirely (the heartbeat-target case), the reciprocal close never arrives and the handshake's 5s grace bounds how long `runHeartbeat` blocks inside `CloseWithCode`.

Operationally this adds at most ~5s onto the 60s detection window before the handler unwinds and the registry slot is released. The protocol spec's "60s worst case" is about **detection**; teardown is bounded by the library's handshake timeout. No additional configuration is needed.

For tests, the implication is that `done` (the channel that closes when `runHeartbeat` returns) lags the close-frame-on-the-wire by up to ~5s if the test client never reads. The test gives `done` a 7s deadline; the close-frame assertion is independent of `done` and uses a shorter window.

## Testing

`internal/relay/heartbeat_test.go`, `package relay`. The `startHeartbeatPair(t, interval, timeout)` helper sets up an `httptest.NewServer` whose handler accepts the upgrade, wraps in `WSConn`, calls `c.CloseRead(r.Context())` to mirror the production handler shape (so the server side processes incoming pongs and pings), and runs `runHeartbeat` in a goroutine. Returns the `*WSConn` (server side), the client `*websocket.Conn`, a `done` channel, the heartbeat `cancel` func, and a `teardown`.

Three tests, 1:1 with AC bullets (a)/(b)/(c):

- **`TestHeartbeat_HealthyPeer_KeepsConnOpen`** — client side runs `client.CloseRead(...)` so the lib auto-pongs server's pings. Sleep through ~3 ping cycles (interval=50ms, 3 cycles + slack). Assert `done` is **not** closed.
- **`TestHeartbeat_UnresponsivePeer_TriggersClose`** — client side does **not** read until after the heartbeat has had time to fire and emit its close. Reading earlier would auto-pong and keep the heartbeat satisfied (the inverse of what's wanted). After `interval + timeout + 200ms` the test does a single `client.Read` with a 5s deadline; assert it returns `*websocket.CloseError{Code: 1011, Reason: "heartbeat timeout"}`. The Read also triggers the client lib's automatic reciprocal close, which unblocks the server's Close handshake and lets `done` fire; assert `done` closes within 7s.
- **`TestHeartbeat_ContextCancelled_ExitsCleanly`** — cancel the heartbeat ctx immediately; assert `done` closes within 1s; assert a `client.Read` with a short (200ms) deadline returns `context.DeadlineExceeded` rather than a `CloseError` (proving the heartbeat did **not** emit a close on the cancel path).

Production constants (30s/30s) are exercised implicitly by existing handler tests that already pass at sub-second flows; an explicit slow integration test that waits 30s is out of scope.

## What this ticket deliberately does NOT do

- **No application-level keepalive messages.** Would be a protocol change.
- **No per-endpoint interval differences (phone vs binary).** Both endpoints get the same regime; the relay treats both as untrusted internet sockets with identical liveness requirements.
- **No adaptive intervals based on connection quality.** v1.
- **No configurability of interval / timeout.** Revisit only if mobile-flapping evidence emerges.
- **No new logging on heartbeat-induced close.** The handler's existing `server_released` / `phone_unregistered` log fires on the defer path regardless of whether the close was peer-initiated or heartbeat-initiated. Distinguishing the two would help mobile-flapping diagnosability and the spec named it as an open question; not adopted in this ticket because no operational signal currently demands it. Revisit when ops evidence motivates it.

## Adversarial framing

The heartbeat operates on an already-authorised connection (post-`ClaimServer` / `RegisterPhone`). It introduces no new untrusted input.

- **Bounded write rate.** One ping per 30s per connection — server-driven, not amplifiable by a hostile peer.
- **Bounded write size.** Library-default empty ping payload. No attacker-controlled byte enters the wire.
- **Slow-loris resistance — improvement.** A peer that withholds pongs is now detected within 60s and its slot reclaimed. Pre-#7, dead peers wedged the routing tables forever. A peer that slow-pongs (responds at 29s) keeps the conn alive but consumes nothing beyond the existing per-conn footprint.
- **Goroutine count.** +1 per accepted upgrade (3 total per conn). Bounded above by `Registry` slot limits.
- **No new log content.** The `1011` close reason is a fixed literal; no peer-supplied value enters logs or wire frames.
- **`Ping`-vs-`Send` interleaving.** Library serialises control frames against data writes internally; `WSConn.writeMu` guards Send-vs-Send. No new lock-order interaction.

Verdict from the architect's pre-implementation security review (in the spec): **PASS**. No new defences invented; this ticket is a strict improvement over the pre-#7 dead-peer-detection state.

## Related

- [WSConn adapter](ws-conn-adapter.md) — `Ping` and `CloseWithCode` were added here for this ticket.
- [`/v1/server` WS upgrade](server-endpoint.md) — first heartbeat wiring site.
- [`/v1/client` WS upgrade](client-endpoint.md) — second heartbeat wiring site.
- [ADR-0007](../decisions/0007-wsconn-closewithcode-for-active-conn.md) — the WSConn-surface widening that this feature required.
- [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md) — still applies for stillborn-WSConn close codes (`4409`/`4404`/`4401`).
- [Protocol spec § Heartbeat](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#heartbeat) — authoritative source for 30s interval / 30s timeout / 60s detection.
