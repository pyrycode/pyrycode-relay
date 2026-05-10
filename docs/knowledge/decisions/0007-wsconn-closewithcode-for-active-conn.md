# ADR-0007: `WSConn.CloseWithCode` for active-conn application close codes

**Status:** Accepted (#7)
**Date:** 2026-05-10

## Context

[ADR-0005](0005-application-close-codes-via-underlying-conn.md) decided that application WS close codes (`4409`, `4404`, `4401`) are emitted by calling `Close(code, reason)` directly on the underlying `*websocket.Conn`, **not** through `WSConn`. The decision held the adapter's surface narrow: one `Close()` whose body is fixed to `StatusNormalClosure`. The justification rested on the close-codes-so-far being emitted from a *stillborn-WSConn* window — after construction, before any `Send`, before any defer wires the conn into the registry — where the WSConn's `closeOnce`/`writeMu` invariants are not in play.

ADR-0005 anticipated *this* ticket explicitly:

> The pattern only applies to **stillborn WSConn** paths — pre-`Send`, pre-defer. Once a WSConn is in use (registered with `Send` reachable from broadcasters), close goes through `WSConn.Close()` and the code is `StatusNormalClosure`. Application close codes for an *active* WSConn (e.g. binary disconnects mid-stream and we want to surface a forward-error code) need a different solution — likely a small `WSConn.CloseWithCode` added at the time the first such use case lands, justified by an ADR amendment.

The heartbeat ticket (#7) is that first use case. A goroutine running alongside the registered conn must, on a peer that stops responding to RFC 6455 pings, close the WS with `1011 server error` + reason `"heartbeat timeout"`. By the time heartbeat decides to close, the WSConn has been live in the registry — concurrent broadcasters may be in `Send`, the close defer is wired up, `closeOnce` is the only safe single-funnel for the close.

So: where does the `1011 + "heartbeat timeout"` close-code call site live?

## Decision

**Add `WSConn.CloseWithCode(code websocket.StatusCode, reason string)`.** Refactor `WSConn.Close()` to delegate (`CloseWithCode(StatusNormalClosure, "")`). Both share the same `closeOnce` guard and both cancel `closeCtx` before calling the underlying `Close`.

Concretely:

```go
func (w *WSConn) CloseWithCode(code websocket.StatusCode, reason string) {
    w.closeOnce.Do(func() {
        w.cancel()
        _ = w.conn.Close(code, reason)
    })
}

func (w *WSConn) Close() {
    w.CloseWithCode(websocket.StatusNormalClosure, "")
}
```

`closeOnce` ensures that whichever Close-family call fires first wins. If heartbeat fires `CloseWithCode(1011, "heartbeat timeout")` and the handler defer subsequently calls `wsconn.Close()`, the second call is a no-op — the wire saw 1011. Conversely, on the clean-shutdown path the defer's `Close()` runs first and any later `CloseWithCode` from a goroutine racing with the cancel becomes a no-op. Single-funnel guarantee preserved.

Also added: `WSConn.Ping(ctx context.Context) error` — a pure forwarder over `c.Ping`. Heartbeat uses `Ping` rather than reaching for the underlying `*websocket.Conn`, so `runHeartbeat` operates entirely above the `WSConn` boundary.

ADR-0005 still applies for *stillborn* WSConn paths. The pattern there (handler calls `c.Close(websocket.StatusCode(4409), reason)` directly on the underlying conn before any `Send` could have run) remains the right shape for `4409`/`4404`/`4401` — those close codes are decided pre-claim, where the WSConn is provably stillborn. This ADR extends the surface specifically for *post-claim* close-code emission, which is structurally distinct.

## Rationale

The three options ADR-0005 weighed (add a method; embed the code in the constructor; emit on the underlying conn from the call site) get re-evaluated against the active-conn case:

1. **Add `WSConn.CloseWithCode`.** Adopted. Two callers (`Close`, future post-claim closes) share the `closeOnce` funnel through one body. Adapter surface grows by one method but keeps its single-mutex / single-once shape intact. No new `closeOnce` (rejected in ADR-0005 as "which close wins?") because `Close` now *delegates* to `CloseWithCode` — the body is a single function, the guard a single `sync.Once`, so the question of which close wins is settled by `Once.Do` semantics, exactly as before.
2. **Pass the close code into `WSConn` at construction.** Same reason it failed in ADR-0005: the close code at construction time is *not* the code the caller eventually decides on. Heartbeat decides "1011 because timeout" only after a Ping fails; the WSConn was constructed at `RegisterPhone`/`ClaimServer` time long before.
3. **Emit on the underlying `*websocket.Conn` from the heartbeat call site.** Rejected for the post-claim case. Reaching for `c` from a goroutine that holds neither `writeMu` nor `closeOnce` would race the adapter's invariants: a concurrent `Send` could be holding `writeMu` and have a `Write` mid-flight; a concurrent `Close` (handler defer) could be running `c.Close` independently. The library is documented to handle concurrent `Close` calls safely, but our `closeOnce` guarantee — "exactly one close-frame leaves this WSConn" — would be violated. The stillborn-conn carve-out from ADR-0005 cannot be extended here because the conn is no longer stillborn.

The cost of (1) is one method's worth of API surface widening, kept minimal: same shape as `Close`, same idempotency guarantee, no new state field, no new mutex. The benefit is that any future "active-conn close with custom code" (frame-forward error codes from #6, e.g.) plugs into the same call site without a fresh ADR amendment.

## Consequences

- `WSConn.Close()` is now defined as `CloseWithCode(StatusNormalClosure, "")`. Behaviour is unchanged — same close code, same idempotency, same `closeCtx` cancel — but the implementation funnels through `CloseWithCode`. Existing call sites (handler defers, registry-side teardown) require no change.
- `WSConn` now exposes `Ping(ctx)` as a pure forwarder. Used by `runHeartbeat`; safe to use for any future caller that needs RFC 6455 ping/pong from above the adapter boundary.
- Application close codes for **active** WSConns are emitted via `wsconn.CloseWithCode(code, reason)`. The first user is `runHeartbeat` (`1011`/`"heartbeat timeout"`). Future frame-forward error codes on `/v1/server` and `/v1/client` (per #6) follow the same call site.
- ADR-0005 remains in force for **stillborn** WSConns: handlers continue to call `c.Close(websocket.StatusCode(4409), reason)` directly for `4409`, `4404`, `4401`, with the inline comment naming the exception. The two patterns coexist — they describe structurally distinct windows of a WSConn's lifecycle.

## Related

- [ADR-0005: Application WS close codes go through the underlying `*websocket.Conn`](0005-application-close-codes-via-underlying-conn.md) — the parent decision; this ADR is the explicit amendment ADR-0005 named.
- [ADR-0004: WS library choice and adapter context strategy](0004-ws-library-and-adapter-context-strategy.md) — `closeCtx` cancellation is what `CloseWithCode` cancels, same as `Close`.
- [Heartbeat feature](../features/heartbeat.md) — first user of `CloseWithCode` and `Ping`.
- [WSConn adapter](../features/ws-conn-adapter.md) — the type whose surface this ADR widens by exactly two methods.
