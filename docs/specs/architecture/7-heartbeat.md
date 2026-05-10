# Spec: WebSocket heartbeat — RFC 6455 ping/pong (#7)

## Files to read first

- `internal/relay/server_endpoint.go` (full, 122 lines) — handler structure, `defer { ScheduleReleaseServer; wsconn.Close; log }` pattern, `c.CloseRead(r.Context()) ; <-readCtx.Done()` placeholder. The new heartbeat goroutine launches inside this handler, after `ClaimServer` succeeds and after the existing release defer is registered.
- `internal/relay/client_endpoint.go` (full, 88 lines) — symmetric structure for `/v1/client`. Heartbeat wiring is identical in shape; the only difference is which register/unregister functions the handler defers around.
- `internal/relay/ws_conn.go` (full, 83 lines) — `WSConn` adapter, `closeOnce`, `closeCtx`, `writeMu`. The new `CloseWithCode` and `Ping` methods land here; `Close()` is refactored to delegate to `CloseWithCode`.
- `docs/knowledge/decisions/0005-application-close-codes-via-underlying-conn.md` — explicitly anticipates *this* ticket: "Application close codes for an *active* WSConn ... need a different solution — likely a small `WSConn.CloseWithCode` added at the time the first such use case lands, justified by an ADR amendment." Heartbeat is the first such use case. ADR-0005 still applies to *stillborn* WSConn paths (4404/4409); the new method extends the surface for *active* WSConn closes.
- `internal/relay/server_endpoint_test.go:21-43` — `startServer` / `dialWith` / `validHeaders` test helpers. The new heartbeat tests use the same `httptest.NewServer` + raw `nhooyr.io/websocket` pair pattern.
- `internal/relay/client_endpoint_test.go:21-93` — analogous helpers for `/v1/client`.
- `pyrycode/pyrycode/docs/protocol-mobile.md` § Heartbeat (linked from the ticket body) — authoritative behaviour: 30s ping interval, 30s pong timeout, 60s worst-case dead-connection detection, both endpoints.
- `docs/PROJECT-MEMORY.md` § Patterns established — esp. "Application WS close codes are emitted on the underlying `*websocket.Conn`, not via `WSConn`" (which this ticket *amends* for the active-conn case) and "Adapters bridge interface↔library API mismatches by owning policy locally" (the WSConn extension follows the same shape).

## Context

Both endpoints (`/v1/server` and `/v1/client`) currently park on `<-readCtx.Done()` after a successful register. `CloseRead` drains control frames in the background — including pongs — so the connection survives ping/pong handshakes initiated by the *peer*, but the relay never **initiates** any. A peer whose TCP socket dies silently (NAT timeout, mobile interface flap, kernel panic, severed cable) leaves the relay holding a registered `Conn` indefinitely, blocking the server-id slot and consuming a phone slot.

RFC 6455 ping/pong at the WebSocket layer is the only signal the relay has to detect this. Per protocol-mobile.md § Heartbeat: send a ping every 30 seconds; if a pong does not arrive within 30 seconds of the ping, treat the connection as dead. Worst-case detection: 60 seconds.

This ticket adds the heartbeat. It is intentionally symmetric across both endpoints — same interval, same timeout, same goroutine shape — because the relay treats both peers as untrusted internet-exposed sockets with identical liveness requirements.

## Design

### Module layout

Add `internal/relay/heartbeat.go` containing two file-level constants and one unexported function:

```go
const heartbeatInterval = 30 * time.Second
const heartbeatTimeout  = 30 * time.Second

func runHeartbeat(ctx context.Context, w *WSConn, interval, timeout time.Duration)
```

The function is unexported; both upgrade handlers live in `package relay` and call it directly. The constants are file-level — not exported, not configurable from `cmd/pyrycode-relay/main.go`. Tests in `heartbeat_test.go` call `runHeartbeat` directly with shorter values; the constants are used only at the production wiring sites.

Why constants in the file (rather than a literal at the wiring site, which is the established `grace` pattern from #21): there are *two* wiring entry points (server + client) using the same value. Naming the value once and referring to it from both sites is the threshold the existing pattern memo names ("Promote to a package-level constant only when a second wiring entry point needs the same value"). The AC also asks for "file-level constants" explicitly.

### `WSConn` extension

`WSConn` gains two methods. Both are minimal forwarders; together they let heartbeat operate on a `*WSConn` without exposing the underlying `*websocket.Conn`.

```go
// CloseWithCode cancels in-flight writes and closes the WebSocket with
// code/reason. Idempotent under closeOnce: only the first Close-family
// call reaches the underlying conn. Safe to call concurrently with Send.
//
// Used by paths that need to signal a non-normal close on an *active*
// WSConn (heartbeat timeout → 1011 "heartbeat timeout"). See ADR-0005
// for why this lives on WSConn rather than at the call site.
func (w *WSConn) CloseWithCode(code websocket.StatusCode, reason string) {
    w.closeOnce.Do(func() {
        w.cancel()
        _ = w.conn.Close(code, reason)
    })
}

// Ping sends an RFC 6455 ping and blocks until the pong returns or ctx
// expires. Pure forwarder over the library's c.Ping; does not take
// writeMu (the library serialises control frames against writes
// internally). Used by runHeartbeat.
func (w *WSConn) Ping(ctx context.Context) error {
    return w.conn.Ping(ctx)
}

// Close is preserved as a normal-closure convenience. Existing call
// sites (handler defers, registry-side teardown) keep their current
// signature.
func (w *WSConn) Close() {
    w.CloseWithCode(websocket.StatusNormalClosure, "")
}
```

Idempotency note: `closeOnce` ensures whichever `CloseWithCode`-family call arrives first wins. If heartbeat fires `CloseWithCode(1011, "heartbeat timeout")` and the handler defer subsequently calls `wsconn.Close()` (`StatusNormalClosure`), the second call is a `closeOnce` no-op — the wire saw 1011. Conversely, on the clean-shutdown path, the defer's `Close()` runs first and any later `CloseWithCode` from a goroutine racing with the cancel becomes a no-op. This is the single-funnel guarantee `closeOnce` was built for.

### Heartbeat function

```go
// runHeartbeat sends RFC 6455 pings every interval and closes w with
// 1011 "heartbeat timeout" if a pong does not arrive within timeout.
// Returns when ctx is cancelled (clean-shutdown path) without calling
// CloseWithCode itself: the handler's existing defer owns the
// clean-close path. runHeartbeat owns ONLY the timeout-driven close.
func runHeartbeat(ctx context.Context, w *WSConn, interval, timeout time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            pingCtx, cancel := context.WithTimeout(ctx, timeout)
            err := w.Ping(pingCtx)
            cancel()
            if err == nil {
                continue
            }
            // Cancellation race: if ctx fired while Ping was in flight,
            // Ping returns ctx.Err(). Bail without closing — the handler
            // defer owns the clean-close path.
            if ctx.Err() != nil {
                return
            }
            w.CloseWithCode(websocket.StatusInternalError, "heartbeat timeout")
            return
        }
    }
}
```

Two invariants encoded:

1. **Only one path calls `CloseWithCode`.** The ctx-cancelled paths (`<-ctx.Done()` and `ctx.Err() != nil` after a Ping that returned ctx-cancellation) both `return` without touching the conn. Acceptance criterion (c) — "exits cleanly when the connection's context is cancelled, without attempting a close call" — falls out structurally.
2. **`time.NewTicker` is used, not `time.Tick`.** `Tick` leaks; `NewTicker` + `defer Stop` is the only correct shape inside a goroutine that exits.

### Handler wiring

`/v1/server` (`server_endpoint.go`): immediately after the existing release-defer is registered and before `c.CloseRead`, launch the heartbeat with a cancellable context derived from `r.Context()`. The cancel is registered as a defer so it runs **before** the release defer (LIFO).

```go
logger.Info("server_claimed", ...)

defer func() {                          // EXISTING — release defer
    reg.ScheduleReleaseServer(serverID, grace)
    wsconn.Close()
    logger.Info("server_released", "server_id", serverID)
}()

hbCtx, cancelHB := context.WithCancel(r.Context())   // NEW
defer cancelHB()                                     // NEW — runs first (LIFO)
go runHeartbeat(hbCtx, wsconn, heartbeatInterval, heartbeatTimeout)  // NEW

readCtx := c.CloseRead(r.Context())     // EXISTING
<-readCtx.Done()                        // EXISTING
```

`/v1/client` (`client_endpoint.go`): identical insertion at the analogous point (after the unregister defer is registered, before `c.CloseRead`).

Defer-order rationale: when the handler returns, deferred funcs run LIFO. `cancelHB` fires first → heartbeat goroutine observes `ctx.Done()` and returns without closing. Then the release defer runs `wsconn.Close()` (StatusNormalClosure). Net wire effect on the clean-shutdown path: a single normal close. On the heartbeat-timeout path: heartbeat has already emitted `CloseWithCode(1011, "heartbeat timeout")` before the handler unwinds — the subsequent `wsconn.Close()` is a `closeOnce` no-op.

### Concurrency model

Per accepted upgrade:

- 1 handler goroutine (the HTTP handler itself, runs `<-readCtx.Done()`).
- 1 `CloseRead` drain goroutine (existing, owned by the library after `c.CloseRead`).
- **1 heartbeat goroutine (new).** Spawned via `go runHeartbeat(...)` after register success.

Goroutine count per connection: 3 (was 2). Bounded by `Registry`-imposed slot limits (1 binary per server-id; phone count cap is #19's responsibility).

Lifecycle:

- Heartbeat exits via `ctx.Done()` when the handler returns (peer-close path, conflict path is unreachable because heartbeat hasn't started yet on early-return).
- Heartbeat exits via the timeout path when `Ping` returns a non-ctx error after `timeout` elapsed; it emits `CloseWithCode(1011, ...)` first.
- No leak path: `defer cancelHB()` is unconditional once heartbeat has been launched, and heartbeat is launched only after the release defer is in place, so any panic between them still cancels.

`Ping` (control frame) and `Send` (binary frame) at the underlying-library level: nhooyr.io/websocket serialises pings against writes via its own internal mutex. WSConn's `writeMu` guards *concurrent Send callers* (broadcasters), not Send-vs-Ping. Heartbeat does not take `writeMu`. This is correct: control frames and data frames have separate ordering requirements, and the library's internal serialisation is the right granularity for this case.

### Error handling

- `Ping` returns `nil` → loop continues to next tick.
- `Ping` returns non-nil error AND `ctx.Err() == nil` → real timeout / dead conn → emit 1011 and return.
- `Ping` returns non-nil error AND `ctx.Err() != nil` → handler is shutting down → return without close.
- `CloseWithCode` returns no error (it ignores the underlying close error via `_ = ...`); this matches `Close`'s existing pattern.
- The handler defer is unchanged: it still calls `wsconn.Close()` and `ScheduleReleaseServer` regardless of how readCtx ended (peer close, heartbeat-induced close, server shutdown).

The "ping returning a non-nil error (timeout, connection-closed, etc.)" wording in the AC encompasses both the timeout case and the case where Ping fails because the conn is *already* closed (e.g., peer hung up between two ticks; Ping fails on the next tick before `CloseRead` has surfaced the close to readCtx). Both branches converge on the same path: `CloseWithCode(1011, "heartbeat timeout")`. If the conn is already closed, the underlying lib's `Close` no-ops and the wire effect is whatever the original close emitted; that's fine — the goroutine exits either way.

### Testing strategy

`internal/relay/heartbeat_test.go` (new). Each test sets up a websocket pair via `httptest.NewServer` running a small handler that calls `runHeartbeat` directly (NOT through the upgrade handlers). The function-direct shape lets tests pass tiny intervals (e.g., 50ms) without reaching for the production constants.

Test fixture (single helper at the top of the file):

```go
// startHeartbeatPair upgrades a server-side connection, wraps it in a
// WSConn, runs runHeartbeat in a goroutine with the supplied
// interval/timeout, and returns:
//   - the *WSConn (server side; for assertions)
//   - the *websocket.Conn (client side; for driving pings/pongs)
//   - a done channel that closes when runHeartbeat returns
//   - a cancel func that fires the heartbeat ctx
//   - a teardown that closes everything
func startHeartbeatPair(t *testing.T, interval, timeout time.Duration) (...)
```

Three tests, one per AC bullet (a/b/c):

**(a) Healthy peer keeps the conn open across cycles.** Client side runs a `c.Read(ctx)` loop in a goroutine — this drives the library's control-frame processing, which auto-pongs incoming pings. Sleep through 3 intervals (e.g. 150ms with interval=50ms). Assert: `done` channel is NOT closed; client's last Read has not seen a CloseError. Tear down.

**(b) Unresponsive peer triggers 1011 close.** Client side does NOT read — pings sit in the conn unobserved, no pong is sent. After (interval + timeout + slack), the heartbeat must have emitted the 1011 close. Drive a single client Read with a generous deadline; assert it returns a `*websocket.CloseError` with `Code == 1011` and `Reason == "heartbeat timeout"`. Assert `done` channel closed. (Guard against an autoflush window: read deadline of ~1s on top of (interval + timeout) is plenty for the close frame to land.)

**(c) Cancelling the parent ctx exits cleanly without closing.** Cancel the ctx immediately after launching. Wait for `done` to close. Assert: client's `Read(ctx)` does NOT return a `CloseError` from the heartbeat path — instead, when the test tears down (which closes everything), the close code is `StatusNormalClosure` (the test's teardown call, not the heartbeat). The cleanest assertion: after cancel + done, the underlying conn is still open enough for the *test* to send a `c.Ping(ctx)` from the client side and receive a pong. If that succeeds, the heartbeat did not close the conn.

Production constants are exercised by an existing handler test (e.g. `TestServerEndpoint_PeerClose_ReleasesSlot`) implicitly — those tests already pass and a 30s heartbeat does not interfere with their sub-second flows. Adding a slow integration test that waits 30s is out of scope; the constants are validated by inspection of the wiring in `server_endpoint.go` / `client_endpoint.go`.

### What does not change

- `WSConn.Send`, `WSConn.ConnID`, `closeCtx` semantics.
- `Registry` API. Heartbeat operates entirely above the registry layer; from the registry's perspective, a heartbeat-induced close is indistinguishable from a peer-initiated close.
- `cmd/pyrycode-relay/main.go`. Heartbeat constants are not flag-controlled in v1.
- `CloseRead` placeholder in both handlers. Frame forwarding (#6) replaces it later; heartbeat is independent of that work.

### Knowledge artefacts

The developer should add (alongside the implementation):

1. A new feature doc `docs/knowledge/features/heartbeat.md` (one page) — what runHeartbeat does, why 30s/30s, the cancellation invariant, the test approach. Mirrors the shape of existing feature docs.
2. An ADR amendment OR a new ADR-0007 covering the `WSConn.CloseWithCode` extension. ADR-0005 already names this case as "an ADR amendment"; either form is acceptable. If a new ADR, keep it short (<50 lines) and link from ADR-0005 as the realised follow-up.
3. The `INDEX.md` entry under `docs/knowledge/`.

These are routine for this repo — the spec calls them out so they don't get missed.

## Open questions

- **Logging on heartbeat-induced close.** Should heartbeat log `heartbeat_timeout` with `conn_id` before calling `CloseWithCode`? The handler's existing `server_released` / `phone_unregistered` log fires regardless (defer path), but it doesn't distinguish "peer hung up cleanly" from "we 1011'd them after timeout." For mobile-flapping diagnosability this distinction matters. Recommendation: pass `*slog.Logger` and `connID` into `runHeartbeat` and log `heartbeat_timeout` at INFO before the `CloseWithCode` call. Cost: 2 extra parameters; benefit: ops visibility into a failure mode that's otherwise invisible. Decide during implementation; this spec does not gate on it.
- **Unit tests for the WSConn additions.** `Ping` is a pure forwarder; `CloseWithCode` is mostly tested transitively by the heartbeat tests. A small direct `TestWSConn_CloseWithCode_IsIdempotent` may be worth adding to `ws_conn_test.go`, but it is not strictly required by the AC.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings. Heartbeat operates on an already-authorised connection (post-`ClaimServer` / `RegisterPhone`). Ping is a control frame; the relay neither reads nor writes application data here. There is no new untrusted input to validate.
- **[Tokens, secrets, credentials]** Not applicable. Heartbeat does not read or emit credentials. Close reason `"heartbeat timeout"` is a fixed literal — no credential, header, or peer-supplied value enters the close frame.
- **[File operations]** Not applicable. Heartbeat is purely in-memory + network.
- **[Subprocess / external command execution]** Not applicable.
- **[Cryptographic primitives]** Not applicable. RFC 6455 ping payload is unspecified by this design (library default — empty); pong is the library's automatic response. No keys, no comparisons.
- **[Network & I/O]**
  - Bounded concurrency: one heartbeat goroutine per accepted upgrade. Bounded above by `Registry` slot limits (1 binary per server-id; phone-count cap is #19's responsibility — explicitly out of scope).
  - Bounded write rate: one ping per 30 seconds per connection. A hostile peer cannot amplify or accelerate this — the interval is server-driven.
  - Bounded write size: control frame, library-default empty payload. No attacker-controlled byte enters the wire.
  - Slow-loris resistance: a peer that slow-pongs (responds to ping at 29s) keeps the conn alive but consumes no extra resources beyond a single heartbeat goroutine. A peer that withholds pongs is detected within 60s and the slot is reclaimed via the existing release path. This is *strictly an improvement* over the pre-#7 state, where dead peers wedged forever.
- **[Error messages, logs, telemetry]** No tokens, headers, or peer payloads enter logs. The proposed `heartbeat_timeout` log (open question) carries `conn_id` only — `conn_id` is a relay-assigned random hex8 + server-id, contains no peer secret. Close reason `"heartbeat timeout"` is fixed.
- **[Concurrency]**
  - Goroutine lifecycle: heartbeat exits via `ctx.Done()` (handler unwind) or via the timeout-then-close path. No third exit; no leak. `defer cancelHB()` is unconditional.
  - Lock ordering: heartbeat holds no locks. `WSConn.CloseWithCode` is `closeOnce`-guarded, same as `Close()`; no new lock-order interaction.
  - Race with concurrent `Send`: nhooyr.io/websocket's internal write serialisation handles ping-vs-write ordering. WSConn's `writeMu` is for multiple Send callers, not Send-vs-Ping; heartbeat correctly does not take it.
  - Race on close: heartbeat-emitted 1011 vs handler-defer-emitted normal close — `closeOnce` ensures whichever fires first wins. The wire shows exactly one close frame.
- **[Threat model alignment]** protocol-mobile.md § Heartbeat is the canonical source; this design implements it directly. The threat addressed: half-open TCP from disappeared peers wedging the routing tables forever (a DoS-by-leak vector against an internet-exposed relay). No threat is moved or expanded by this change. Future-ticket scope (per-IP connection limits, application-level keepalive) is named explicitly out of scope by the ticket body.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-09
