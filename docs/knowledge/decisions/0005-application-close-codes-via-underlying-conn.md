# ADR-0005: Application WS close codes go through the underlying `*websocket.Conn`, not `WSConn`

**Status:** Accepted (#16)
**Date:** 2026-05-08

## Context

`WSConn` ([ADR-0004](0004-ws-library-and-adapter-context-strategy.md), #15) is the registry's `Conn`-interface adapter over `nhooyr.io/websocket.Conn`. Its `Close()` always emits `StatusNormalClosure` — that is the registry's contract: "drop this connection cleanly," with no policy on *why*.

The protocol defines application close codes in the 4000–4999 range:

- `4401` — token invalid (on `/v1/client`, #5).
- `4404` — no server for `serverID` (on `/v1/client`, #5).
- `4409` — `serverID` already claimed (on `/v1/server`, #16).

Plus future frame-forward error codes on both endpoints. The upgrade handlers need to emit these, but `WSConn` does not know *why* a close is happening — that knowledge lives in the handler.

So: where does the code that picks a close code (and the human-readable reason) belong?

## Decision

**Application close codes are emitted by calling `Close(code, reason)` directly on the underlying `*websocket.Conn`, not through `WSConn`.** `WSConn.Close()` keeps a fixed `StatusNormalClosure` body. The handler that holds the policy holds the close-code call site.

Specifically: at the moment a handler decides to refuse a connection (e.g. `4409` on duplicate claim), it calls `c.Close(websocket.StatusCode(code), reason)` on the `*websocket.Conn` it received from `websocket.Accept`, before `wsconn.Close()` would have run. The `WSConn` was constructed but no `Send` was attempted and no goroutine holds `writeMu` — a stillborn WSConn — so the WSConn invariant ("callers must reach the connection only through WSConn methods") is preserved in spirit: the conn was never *used* through the adapter.

Adopted in `internal/relay/server_endpoint.go` (#16) for `4409`. The same pattern applies to `4401` / `4404` in `/v1/client` (#5).

## Rationale

Three options were on the table:

1. **Add `WSConn.CloseWithCode(code websocket.StatusCode, reason string)`.** Inflates the adapter's API surface for one caller per close code. Each new code adds a method or makes the existing one's signature creep. The adapter would also need policy on what to do if `Close` was already called — the `closeOnce` guard exists precisely to make `Close` idempotent, and a new method would have to either share the guard (then which code wins?) or duplicate it. Rejected — the adapter's small-surface property is load-bearing for review (#15's security review hinged on it).

2. **Pass the close code into `WSConn` at construction time, store it, and emit it from `Close`.** Doesn't fit: the close code at construction time is *not* the close code the handler eventually decides on. By the time we know it's `4409`, the WSConn already exists (we constructed it to pass to `ClaimServer`).

3. **Direct `c.Close(code, reason)` from the handler, no method on `WSConn`.** Adopted. Keeps `WSConn` a closed, narrow type. Keeps the close-code knowledge in the handler that holds the policy.

Option 3 has one cost: it documents an exception to WSConn's "all access goes through the adapter" invariant. The exception is bounded by structure — a stillborn WSConn before any `Send`, no concurrent writer to interleave with, no `writeMu` contender — so the spirit of the invariant survives. The handler comments name the exception so a future reader doesn't generalise it into a "you can use `c` directly any time" pattern.

## Consequences

- `WSConn.Close()` stays minimal: `StatusNormalClosure`, idempotent, owns the cancel of `closeCtx`. No close-code variants, no policy parameters.
- Handlers that emit application close codes do so on the `*websocket.Conn` they received from `websocket.Accept`, before the WSConn would have been closed via `defer`. The handler must comment the exception inline so it doesn't read as a violation.
- The pattern only applies to **stillborn WSConn** paths — pre-`Send`, pre-defer. Once a WSConn is in use (registered with `Send` reachable from broadcasters), close goes through `WSConn.Close()` and the code is `StatusNormalClosure`. Application close codes for an *active* WSConn (e.g. binary disconnects mid-stream and we want to surface a forward-error code) need a different solution — likely a small `WSConn.CloseWithCode` added at the time the first such use case lands, justified by an ADR amendment.
- Each new application close code (`4401`, `4404`, future frame-forward codes) follows this pattern at its handler, not by widening `WSConn`.

## Related

- [ADR-0003](0003-connection-registry-passive-store.md) — registry's `Conn` interface and the `Send/Close` contract `WSConn` implements.
- [ADR-0004](0004-ws-library-and-adapter-context-strategy.md) — why `WSConn.Close()` cancels `closeCtx` instead of taking `writeMu`; the property the stillborn-conn case relies on.
- [Server endpoint feature](../features/server-endpoint.md) — first concrete user of this pattern (`4409`).
- [WSConn adapter](../features/ws-conn-adapter.md) — the type whose surface this ADR keeps minimal.
