# WSConn — WebSocket adapter for the registry

`WSConn` is the single adapter that wraps `nhooyr.io/websocket.Conn` so it satisfies the registry's `Conn` interface ([connection-registry.md](connection-registry.md)). The relay's WS upgrade handlers (`/v1/server` in #4/#16, `/v1/client` in #5) hand the registry a `*WSConn` rather than each inventing its own wrapper. Frame forwarding, broadcasts, and the future heartbeat ticket all reach the wire through this type.

The adapter is small on purpose: one file, one mutex, one one-shot guard, no goroutines, no queues. Everything else (handshake, header validation, close-code semantics, ping/pong, read-side frame loop) lives in adjacent tickets.

## API

Package `internal/relay` (`ws_conn.go`):

```go
type WSConn struct { /* *websocket.Conn + connID + writeMu + closeOnce + closeCtx */ }

func NewWSConn(c *websocket.Conn, connID string) *WSConn

func (w *WSConn) ConnID() string
func (w *WSConn) Send(msg []byte) error
func (w *WSConn) Read(ctx context.Context) ([]byte, error)
func (w *WSConn) Close()
func (w *WSConn) CloseWithCode(code websocket.StatusCode, reason string)  // #7
func (w *WSConn) Ping(ctx context.Context) error                           // #7
```

Plus one package-level constant:

```go
const writeTimeout = 10 * time.Second
```

`writeTimeout` bounds a single `Send`. A slow peer cannot stall a caller past this deadline.

`CloseWithCode` and `Ping` were added in #7 (heartbeat). `Close()` now delegates to `CloseWithCode(StatusNormalClosure, "")` — both share the `closeOnce` funnel, so whichever close-family call fires first wins. `Ping` is a pure forwarder over `*websocket.Conn.Ping`; it does not take `writeMu` because `nhooyr.io/websocket` serialises control frames against data writes internally. See [ADR-0007](../decisions/0007-wsconn-closewithcode-for-active-conn.md) for why active-conn application close codes go on `WSConn` while stillborn-WSConn close codes (`4409`/`4404`/`4401`, [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md)) keep the underlying-`*websocket.Conn` pattern.

## Concurrency model

| Method | Lock | Context | Concurrent-safe with |
|---|---|---|---|
| `ConnID` | none | none | every other method |
| `Send` | `writeMu` (held across one frame write) | `WithTimeout(closeCtx, writeTimeout)` | other `Send` (serialised), `Close` (cancellation breaks it), `ConnID`, `Read` |
| `Read` | none | caller-supplied `ctx` (NOT joined with `closeCtx`) | `Send`, `Close` (underlying `Close` aborts in-flight `Read`), `ConnID`. **NOT** safe with another `Read` — single-caller only. |
| `Close` | `closeOnce` (one shot) | cancels `closeCtx` | `Send` (in-flight write is cancelled), `Read` (underlying `*websocket.Conn.Close` aborts the read), other `Close` (no-op), `ConnID` |

One mutex, one one-shot. The lock graph is a single node. There are no callbacks, no channels, no goroutines spawned by the adapter.

`Close` deliberately does **not** take `writeMu`. Acquiring it would deadlock against a slow peer holding the mutex inside `Send`: the whole point of cancelling `closeCtx` is to abort the in-flight `Write` so it releases the mutex on its own. `nhooyr.io/websocket.Conn.Close` is documented to be safe with an in-flight `Write` — that property is why this library was chosen.

`Read` takes only the caller-supplied `ctx`; it does **not** join `closeCtx`. Rationale (added in #25): when `Close` cancels `closeCtx` *and* closes the underlying `*websocket.Conn`, the in-flight library `Read` returns immediately with the close error. Plumbing `closeCtx` through the read path would be redundant. The single-caller contract makes the lock-free shape safe — only the per-WSConn forwarder goroutine (`internal/relay/forward.go`) calls `Read`.

## Context strategy

The non-trivial design problem is the `Send([]byte) error` ↔ `Conn.Write(ctx, ...)` API mismatch: the registry's contract has no context, but the library requires one for every write. The adapter owns its own context:

- `NewWSConn` derives a `closeCtx`/`cancel` pair from `context.Background()`. The context's only purpose is to be cancelled by `Close`.
- Each `Send` derives a per-call `context.WithTimeout(closeCtx, writeTimeout)`, calls `Write`, cancels the timer.
- `Close` cancels `closeCtx` first, then calls the library's `Close`. After that, every queued or future `Send` finds an already-cancelled context and `Write` short-circuits before touching the socket — no partial frame goes out.

`writeTimeout = 10s` is conservative — long enough that a momentarily-laggy mobile client on a poor network is not killed mid-frame, short enough that one hung `Send` cannot park a goroutine indefinitely. Same order of magnitude as `http.Server.WriteTimeout: 60s` (which bounds a whole upgrade response) but tighter because it bounds a single forwarded frame. Constant lives in one place; revisit when the heartbeat ticket (#7) gives real RTT signal.

See [ADR-0004](../decisions/0004-ws-library-and-adapter-context-strategy.md) for the alternatives considered (`context.Background()` only; caller-supplied context).

## What the adapter deliberately does NOT do

These are documented so the next contributor doesn't add defensive code that doesn't belong:

- **No `connID` validation.** Length, charset, uniqueness are owned by the conn-id-scheme ticket. The adapter treats the id as opaque.
- **No handshake / header validation / subprotocol selection.** Lives at the upgrade boundary (#4/#16, #5).
- **No heartbeat policy.** The adapter exposes `Ping(ctx)` as a pure forwarder; the *policy* (interval, timeout, what to do on failure) lives in `runHeartbeat` in `internal/relay/heartbeat.go` (#7). The adapter does not own the heartbeat goroutine, the ticker, or the close decision — it only provides the pinging primitive.
- **No read-side frame loop or envelope wrap/unwrap.** `Read` is a single-frame primitive; the loop and envelope wrapping live in `internal/relay/forward.go` ([phone-forwarder.md](phone-forwarder.md)).
- **No per-conn send queue / backpressure / rate limit.** `Send` writes synchronously and returns. None of those are in the registry's `Conn` contract.
- **No close-on-`Send`-error.** Caller observes the error and chooses to call `Close`.
- **No close-code semantics beyond `StatusNormalClosure`.** The adapter doesn't know why the registry asked it to close. Close-code mapping (`4401`/`4404`/`4409`) is the upgrade handler's job.
- **No nil-checks on `c`.** Passing a nil `*websocket.Conn` is a programmer error.

## Caller invariant: WSConn owns the underlying conn

Documented on `NewWSConn`: after construction, callers must reach the connection only through `WSConn` methods. Calling `c.Write` or `c.Close` directly defeats the adapter's serialisation and cancellation guarantees. There is no defensive code to enforce this — the adapter cannot keep a global registry of wrapped `*websocket.Conn` — so it lives as a doc-comment contract on the constructor.

## Adversarial framing

The adapter is on the hot path: every routed frame passes through `WSConn.Send`. Threats considered:

- **Slow-loris on the write side.** A peer that completes the handshake and stops reading would, with `context.Background()` writes, hang every `Send` forever. Mitigated by `writeTimeout = 10s` — the first code-level slow-loris defence in the relay. A flood of such peers consumes one goroutine each for at most 10 seconds; connection-count caps belong to the WS upgrade ticket.
- **Concurrent `Send` corrupting frames.** `nhooyr.io/websocket.Conn.Write` is *not* safe to call from multiple goroutines at once — concurrent calls would interleave bytes mid-frame. `writeMu` serialises every `Send`. Race-detector test under `-race` is the verification, not a smoke test.
- **`Close` while many goroutines are blocked on `writeMu`.** `Close` cancels `closeCtx` without taking `writeMu`. The currently-writing goroutine's `ctx` is cancelled and `Write` returns. Each next-in-line goroutine acquires the mutex, derives a `WithTimeout(closeCtx, …)` from an already-cancelled parent, and `Write` short-circuits with the cancellation error. No goroutine is stuck.
- **`Close` racing with mid-`Write` `Send`.** Library is documented to handle this; relying on that property is explicit. If the property regresses in a future library version, the race test surfaces a `DATA RACE`.
- **Same `*websocket.Conn` wrapped by two `WSConn` constructors.** Each gets its own `writeMu`; serial-write guarantee is broken; the wire interleaves. Caller-invariant; not enforced.
- **Adversarial `connID` bytes.** Stored and returned as-is; no interpretation. Charset/length is the conn-id-scheme ticket's job.
- **Per-frame read cap.** `NewWSConn` applies `*websocket.Conn.SetReadLimit(maxFrameBytes)` before returning. Production wires **256 KiB** as a `const` in `cmd/pyrycode-relay/main.go` (derivation: `pyrycode/pyrycode/docs/protocol-mobile.md` § *Backfill semantics* — `message_chunk` envelopes are bounded at ≤50 messages with text-only payloads, worst case comfortably under 256 KiB once routing-envelope overhead is added; see `docs/specs/architecture/29-wsconn-read-limit.md`). Setting the cap at the constructor — the single chokepoint both forwarders reach — discharges the "cover every reader" guarantee without per-handler code. On an over-cap frame the library closes with `StatusMessageTooBig` (1009); subsequent `Read` calls surface a non-nil error. The relay does not emit its own close code on this path.

The `security-sensitive` label was applied because `Send` is on every routed-frame path. Verdict from review: PASS — one new code-level slow-loris mitigation, no widening of documented threat surface beyond the supply-chain cost the threat model already names for adding any WS library.

## Testing

`internal/relay/ws_conn_test.go`, `package relay`. End-to-end against a real `*websocket.Conn` via `httptest.NewServer` — no library mocks. The `startEcho` helper spins up a server whose handler runs a tiny read-loop pushing received frames onto a buffered channel *and* echoing them back to the client; the test gets back a connected `*WSConn` (client side, capped at the caller-supplied `maxFrameBytes`) and the channel. The echo path lets cap tests round-trip a frame so the client-side cap surfaces on `Read`.

Tests (1:1 with the AC):

- `TestWSConn_ConnID_ReturnsConstructorValue` — locks the contract.
- `TestWSConn_ConcurrentSend_ProducesIntactFrames` — N=16 goroutines each `Send` a unique tagged payload; the test asserts N intact, distinct frames. Race detector under `-race` is the primary signal; interleaved frames would surface as malformed messages or corrupted writes.
- `TestWSConn_DoubleClose_DoesNotPanic`.
- `TestWSConn_SendAfterClose_ReturnsError` — non-nil error is the contract; the specific error type is library-dependent (any of `context.Canceled`, a closed-connection error, or a wrapped variant) and not asserted on.
- `TestWSConn_Read_FrameExceedingCap_ReturnsError` — round-trips an oversize frame and asserts the first AND a subsequent `Read` both return non-nil. The error type is not asserted on (library-dependent close-error wrapping).
- `TestWSConn_Read_FrameAtCap_DeliveredIntact` — round-trips a frame whose payload is exactly at the cap; asserts the server-side channel receives the bytes intact and the client-side `Read` of the echoed frame returns the same bytes with `err == nil`.

What we deliberately do not test: the library's behaviour itself (we trust `Write` to honour `ctx` and `Close` to be safe with in-flight `Write`); unit-level mocks of the library; performance.

## Dependency

`nhooyr.io/websocket` (v1.8.x line). First WS library in the project; second non-stdlib direct dep (after `golang.org/x/crypto` for autocert). The vanity import path remains `nhooyr.io/websocket` even though the upstream module home moved to `github.com/coder/websocket`. `make lint` (`govulncheck`) covers known CVEs; the residual supply-chain risk (a malicious release tagged by an authentic maintainer would see every routed frame in cleartext) is named in `docs/threat-model.md` § "Supply chain — Go dependencies."

## Related

- [ADR-0004: WS library choice and adapter context strategy](../decisions/0004-ws-library-and-adapter-context-strategy.md)
- [Connection registry](connection-registry.md) — the `Conn` interface this implements.
- [Phone-side frame forwarder](phone-forwarder.md) — sole caller of `Read`; satisfies the local `phoneSource` interface.
- [Threat model](../../threat-model.md) — slow-loris and supply-chain framings.
