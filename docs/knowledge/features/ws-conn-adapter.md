# WSConn — WebSocket adapter for the registry

`WSConn` is the single adapter that wraps `github.com/coder/websocket.Conn` so it satisfies the registry's `Conn` interface ([connection-registry.md](connection-registry.md)). The relay's WS upgrade handlers (`/v1/server` in #4/#16, `/v1/client` in #5) hand the registry a `*WSConn` rather than each inventing its own wrapper. Frame forwarding, broadcasts, and the future heartbeat ticket all reach the wire through this type.

The adapter is small on purpose: one file, one mutex, one one-shot guard, no goroutines, no queues. Everything else (handshake, header validation, close-code semantics, ping/pong, read-side frame loop) lives in adjacent tickets.

## API

Package `internal/relay` (`ws_conn.go`):

```go
type WSConn struct { /* *websocket.Conn + connID + writeMu + closeOnce + closeCtx */ }

func NewWSConn(c *websocket.Conn, connID string, maxFrameBytes int64) *WSConn

func (w *WSConn) ConnID() string
func (w *WSConn) Send(msg []byte) error
func (w *WSConn) Read(ctx context.Context) ([]byte, error)
func (w *WSConn) Close()
func (w *WSConn) CloseWithCode(code websocket.StatusCode, reason string)  // #7
func (w *WSConn) Ping(ctx context.Context) error                           // #7
```

Plus two package-level constants:

```go
const writeTimeout = 10 * time.Second
const readMessageTimeout = 30 * time.Second
```

`writeTimeout` bounds a single `Send`. A slow peer cannot stall a caller past this deadline.

`readMessageTimeout` bounds receipt of one complete inbound message, counted from the arrival of its first data-frame header, not from when `Read` is called (#111). It never bounds the idle gap before a message starts — the heartbeat covers idle liveness — so a peer may sit silent between messages for as long as it likes, but once a message begins arriving (including a fragmented message's continuation frames) it must complete within 30s or the connection is closed. 30s over the 256 KiB `maxFrameBytes` read cap is a floor of ~8.7 KB/s for a max-size message; real frames are far smaller, so a slow mobile link is not at risk. Stored per-connection as the unexported `readTimeout` field (set to `readMessageTimeout` in `NewWSConn`) rather than read directly from the constant, so same-package tests can shorten the deadline on one connection without mutating package state that would race across parallel tests; production never writes the field after construction.

`Send` writes each frame with the WebSocket **text** opcode (`websocket.MessageText`), never binary. The wire is line-delimited JSON over text frames per `pyrycode/pyrycode/docs/protocol-mobile.md` § *Encoding* (UTF-8); the mobile client closes the connection on any binary frame, so the opcode is a load-bearing cross-repo contract, not a cosmetic detail. Pinned by `TestWSConn_Send_UsesTextOpcode`, which captures the raw observed `websocket.MessageType` at the `Send` boundary — the all-text fakes (`fakerelay`/`fakephone`) structurally cannot guard this (#108, see [codebase/108.md](../codebase/108.md)).

`CloseWithCode` and `Ping` were added in #7 (heartbeat). `Close()` now delegates to `CloseWithCode(StatusNormalClosure, "")` — both share the `closeOnce` funnel, so whichever close-family call fires first wins. `Ping` is a pure forwarder over `*websocket.Conn.Ping`; it does not take `writeMu` because `github.com/coder/websocket` serialises control frames against data writes internally. See [ADR-0007](../decisions/0007-wsconn-closewithcode-for-active-conn.md) for why active-conn application close codes go on `WSConn` while stillborn-WSConn close codes (`4409`/`4404`/`4401`, [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md)) keep the underlying-`*websocket.Conn` pattern.

## Concurrency model

| Method | Lock | Context | Concurrent-safe with |
|---|---|---|---|
| `ConnID` | none | none | every other method |
| `Send` | `writeMu` (held across one frame write) | `WithTimeout(closeCtx, writeTimeout)` | other `Send` (serialised), `Close` (cancellation breaks it), `ConnID`, `Read` |
| `Read` | none | `context.WithCancel(caller ctx)` (NOT joined with `closeCtx`); a `time.AfterFunc(readTimeout, cancel)` is armed once the library's `Reader` returns (first data-frame header received) and stopped when `Read` returns (#111) | `Send`, `Close` (underlying `Close` aborts in-flight `Read`), `ConnID`. **NOT** safe with another `Read` — single-caller only. |
| `Close` | `closeOnce` (one shot) | cancels `closeCtx` | `Send` (in-flight write is cancelled), `Read` (underlying `*websocket.Conn.Close` aborts the read), other `Close` (no-op), `ConnID` |

One mutex, one one-shot. The lock graph is a single node. There are no callbacks, no channels, no goroutines spawned by the adapter.

`Close` deliberately does **not** take `writeMu`. Acquiring it would deadlock against a slow peer holding the mutex inside `Send`: the whole point of cancelling `closeCtx` is to abort the in-flight `Write` so it releases the mutex on its own. `github.com/coder/websocket.Conn.Close` is documented to be safe with an in-flight `Write` — that property is why this library was chosen.

`Read` takes only the caller-supplied `ctx`; it does **not** join `closeCtx`. Rationale (added in #25): when `Close` cancels `closeCtx` *and* closes the underlying `*websocket.Conn`, the in-flight library `Read` returns immediately with the close error. Plumbing `closeCtx` through the read path would be redundant. The single-caller contract makes the lock-free shape safe — only the per-WSConn forwarder goroutine (`internal/relay/forward.go`) calls `Read`.

`Read` derives its own `msgCtx` from the caller's `ctx` rather than using `ctx` directly, because the message deadline (#111) needs a cancellation it can trigger independently of the caller: `w.conn.Reader(msgCtx)` bounds the wait for the first data-frame header by `ctx` alone (idle connections are untouched, exactly as before #111), and only once `Reader` returns does `Read` arm `time.AfterFunc(w.readTimeout, cancel)` against `msgCtx`. `github.com/coder/websocket`'s `Conn.Reader` binds the whole message — including continuation frames of a fragmented message — to the context passed to it and cannot have that context swapped mid-message, so a timer that cancels `msgCtx` after the header arrives is the library's own documented pattern for a body deadline. A cancel that fires after `io.ReadAll` has already finished is inert (the library clears its internal `AfterFunc` when each read finishes); `deadline.Stop()` on return covers the case where the message completed before the timer fired.

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
- **Slow-loris on the read side — a peer that starts a message and dribbles or stalls it.** The heartbeat proves the peer is alive, not that it is making progress: a peer that answers every ping while sending a message one byte at a time (or one non-final fragment, then silence) would, before #111, hold `Read` open indefinitely. Mitigated by `readMessageTimeout = 30s`, armed only once the first data-frame header of a message arrives — never during the idle wait before a message starts, so idle phones and daemons are untouched. `docs/threat-model.md` previously credited this gap to "per-frame deadlines (#15)", which in fact only ever covered the write side (`writeTimeout` on `Send`); #111 is the read-side counterpart. Tests: `TestWSConn_Read_StalledFragmentedMessage_ClosesWithinDeadline` (peer answers pings throughout the stall) and `TestWSConn_Read_DribbledFrame_ClosesWithinDeadline` (hand-rolled peer dribbles one payload byte at a time, faster than the deadline but never finishing).
- **Concurrent `Send` corrupting frames.** `github.com/coder/websocket.Conn.Write` is *not* safe to call from multiple goroutines at once — concurrent calls would interleave bytes mid-frame. `writeMu` serialises every `Send`. Race-detector test under `-race` is the verification, not a smoke test.
- **`Close` while many goroutines are blocked on `writeMu`.** `Close` cancels `closeCtx` without taking `writeMu`. The currently-writing goroutine's `ctx` is cancelled and `Write` returns. Each next-in-line goroutine acquires the mutex, derives a `WithTimeout(closeCtx, …)` from an already-cancelled parent, and `Write` short-circuits with the cancellation error. No goroutine is stuck.
- **`Close` racing with mid-`Write` `Send`.** Library is documented to handle this; relying on that property is explicit. If the property regresses in a future library version, the race test surfaces a `DATA RACE`.
- **Same `*websocket.Conn` wrapped by two `WSConn` constructors.** Each gets its own `writeMu`; serial-write guarantee is broken; the wire interleaves. Caller-invariant; not enforced.
- **Adversarial `connID` bytes.** Stored and returned as-is; no interpretation. Charset/length is the conn-id-scheme ticket's job.
- **Per-frame read cap.** `NewWSConn` applies `*websocket.Conn.SetReadLimit(maxFrameBytes)` before returning. Production wires **256 KiB** as a `const` in `cmd/pyrycode-relay/main.go` (derivation: `pyrycode/pyrycode/docs/protocol-mobile.md` § *Backfill semantics* — `message_chunk` envelopes are bounded at ≤50 messages with text-only payloads, worst case comfortably under 256 KiB once routing-envelope overhead is added; see `docs/specs/architecture/29-wsconn-read-limit.md`). Setting the cap at the constructor — the single chokepoint both forwarders reach — discharges the "cover every reader" guarantee without per-handler code. On an over-cap frame the library closes with `StatusMessageTooBig` (1009); subsequent `Read` calls surface a non-nil error. The relay does not emit its own close code on this path.

- **Non-UTF-8 bytes in a text frame.** RFC 6455 text frames are expected to carry valid UTF-8, whereas binary frames carry arbitrary bytes. Could a peer make the relay forward non-UTF-8 bytes as a text frame (#108)? Not reachable: both `Send` inputs are relay-validated UTF-8 JSON (the binary leg forwards a relay-`Marshal`ed envelope; the phone leg forwards `env.Frame`, which passed the envelope boundary's JSON well-formedness check, and JSON is UTF-8 by spec). No path forwards arbitrary attacker bytes through `Send`. Even if the library rejected a malformed text write, `Send`'s contract is "non-nil ⇒ drop this one connection" — a single contained drop on a stateless relay, never relay-wide.

The `security-sensitive` label was applied because `Send` is on every routed-frame path. Verdict from review: PASS — one new code-level slow-loris mitigation, no widening of documented threat surface beyond the supply-chain cost the threat model already names for adding any WS library. The #108 opcode flip (binary → text) was independently re-reviewed PASS and *reduces* divergence from the wire spec; the only opcode-specific risk (UTF-8 above) is unreachable.

## Testing

`internal/relay/ws_conn_test.go`, `package relay`. End-to-end against a real `*websocket.Conn` via `httptest.NewServer` — no library mocks. The `startEcho` helper spins up a server whose handler runs a tiny read-loop pushing received frames onto a buffered channel *and* echoing them back to the client; the test gets back a connected `*WSConn` (client side, capped at the caller-supplied `maxFrameBytes`) and the channel. The echo path lets cap tests round-trip a frame so the client-side cap surfaces on `Read`.

Tests (1:1 with the AC):

- `TestWSConn_ConnID_ReturnsConstructorValue` — locks the contract.
- `TestWSConn_ConcurrentSend_ProducesIntactFrames` — N=16 goroutines each `Send` a unique tagged payload; the test asserts N intact, distinct frames. Race detector under `-race` is the primary signal; interleaved frames would surface as malformed messages or corrupted writes.
- `TestWSConn_DoubleClose_DoesNotPanic`.
- `TestWSConn_SendAfterClose_ReturnsError` — non-nil error is the contract; the specific error type is library-dependent (any of `context.Canceled`, a closed-connection error, or a wrapped variant) and not asserted on.
- `TestWSConn_Read_FrameExceedingCap_ReturnsError` — round-trips an oversize frame and asserts the first AND a subsequent `Read` both return non-nil. The error type is not asserted on (library-dependent close-error wrapping).
- `TestWSConn_Read_FrameAtCap_DeliveredIntact` — round-trips a frame whose payload is exactly at the cap; asserts the server-side channel receives the bytes intact and the client-side `Read` of the echoed frame returns the same bytes with `err == nil`.
- `TestWSConn_Read_StalledFragmentedMessage_ClosesWithinDeadline` (#111) — peer opens a `Conn.Writer`, writes one non-final data frame (header plus part of the payload), never closes the writer, and keeps a read loop running so it answers the relay's pings; asserts `Ping` still succeeds during the stall (the heartbeat would not catch this peer) and `Read` returns a non-nil error within `readTimeout` plus slack, followed by a `Send` failure (connection closed).
- `TestWSConn_Read_DribbledFrame_ClosesWithinDeadline` (#111) — a hand-rolled peer over a hijacked conn declares a frame length then dribbles one payload byte at a time, faster than the deadline but never finishing; asserts `Read` errors within `readTimeout` plus slack. Peer is hand-rolled because `coder/websocket` cannot itself emit a partial frame, and a control frame cannot interrupt a frame's payload mid-flight.
- `TestWSConn_Read_IdleLongerThanDeadline_ThenMessage_DeliveredIntact` (#111) — peer stays silent for 3× the deadline, then writes one complete message; asserts `Read` returns it intact, pinning that the deadline never bounds the idle gap before a message starts.

Tests shorten the deadline via the unexported `testReadTimeout` (500ms) rather than the 30s production value. One non-obvious detail the fragmented-message test relies on: `coder/websocket` buffers a non-final data frame and flushes it only on the connection's next final-frame or control-frame write, so the stalled fragment actually reaches the relay's `Read` when the peer answers its first ping — not at the moment the peer calls `Writer.Write`.

What we deliberately do not test: the library's behaviour itself (we trust `Write` to honour `ctx` and `Close` to be safe with in-flight `Write`); unit-level mocks of the library; performance.

## Dependency

`github.com/coder/websocket`. First WS library in the project; second non-stdlib direct dep (after `golang.org/x/crypto` for autocert). Migrated from the deprecated `nhooyr.io/websocket` vanity path in #98 ([ADR-0010](../decisions/0010-coder-websocket-migration.md)) — same upstream, same public API, maintained under the non-vanity path. `make lint` (`govulncheck`) covers known CVEs; the residual supply-chain risk (a malicious release tagged by an authentic maintainer would see every routed frame in cleartext) is named in `docs/threat-model.md` § "Supply chain — Go dependencies."

## Related

- [ADR-0010: Migrate to `github.com/coder/websocket`](../decisions/0010-coder-websocket-migration.md) — supersedes ADR-0004; the library identity in use today.
- [ADR-0004: WS library choice and adapter context strategy](../decisions/0004-ws-library-and-adapter-context-strategy.md) — superseded; retained as historical context for the original choice and the adapter context-strategy rationale (still in force).
- [Connection registry](connection-registry.md) — the `Conn` interface this implements.
- [Phone-side frame forwarder](phone-forwarder.md) — sole caller of `Read`; satisfies the local `phoneSource` interface.
- [Threat model](../../threat-model.md) — slow-loris and supply-chain framings.
