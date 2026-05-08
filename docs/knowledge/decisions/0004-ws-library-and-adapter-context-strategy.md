# ADR-0004: WS library choice and adapter context strategy

**Status:** Accepted (#15)
**Date:** 2026-05-08

## Context

The registry's `Conn` interface from #3 is implementation-agnostic, but everything that actually moves bytes in the relay needs a concrete WebSocket library. Two interlocking choices had to be made before the upgrade handlers (#4/#16, #5) could land:

1. **Which Go WS library?**
2. **How does the `Send([]byte) error` ↔ library `Write(ctx, …)` API mismatch get bridged?** The registry's `Conn.Send` deliberately has no `context.Context` parameter (set in ADR-0003 / #3); the library requires one for every write.

Both were decided in #15 along with the `WSConn` adapter that embodies them.

## Decision

**Library:** `nhooyr.io/websocket` (v1.8.x), pulled via the vanity import path. No alternative library is wrapped or kept compatible.

**Context strategy in `WSConn`:** the adapter owns its own context. `NewWSConn` derives `closeCtx`/`cancel` from `context.Background()`. Each `Send` uses `context.WithTimeout(closeCtx, writeTimeout)` — `writeTimeout = 10 * time.Second`. `Close` cancels `closeCtx` first, then calls the library's `Close`.

## Rationale

### Library: `nhooyr.io/websocket`

- **Context-aware API on every blocking method** (`Read`, `Write`, `Close`). Fits the relay's existing `context.Context` discipline; the adapter's per-call timeout strategy is only possible because of this.
- **`Conn.Close` is documented as safe with an in-flight `Write`.** This is the property the adapter relies on for non-deadlocking close — `Close` cancels `closeCtx` without taking `writeMu`, the in-flight `Write` aborts, and the lock unwinds.
- **Maintained.** `gorilla/websocket` was unmaintained as of 2022 (last meaningful release predates Go 1.18); `gobwas/ws` is a frame-level toolkit that would force the adapter to own framing details.

The vanity path `nhooyr.io/websocket` resolves to v1.8.x; upstream development moved to `github.com/coder/websocket` but the published line continues. Use the vanity path; revisit when v2 (or the coder fork) introduces a meaningful behavioural change.

### Context strategy: adapter-owned, per-call deadline

Three options were on the table:

1. **`context.Background()` only.** A slow peer hangs `Send` indefinitely. Rejected — the threat model already treats slow-loris on the read side as a known DoS vector; the same shape applies on the write side, and a broadcast loop fanning out to many phones cannot afford one slow phone parking a goroutine forever.
2. **Caller-supplied context.** Would require changing `relay.Conn.Send`'s signature, which is fixed by ADR-0003 and consumed by callers (broadcast loops iterating a `PhonesFor` snapshot) that don't naturally have a context to thread. Rejected — would force a registry-API change to fix an adapter-local concern.
3. **Adapter-owned: `closeCtx` cancelled by `Close`, plus per-call `WithTimeout`.** Bounds individual writes (slow peer); cancels in-flight writes on close (clean shutdown); does not require a registry API change. Adopted.

`writeTimeout = 10s` is conservative — long enough that a momentarily-laggy mobile client on a poor network is not killed mid-frame, short enough that one hung `Send` cannot park a goroutine indefinitely. One constant, one place to change. Heartbeat (#7) will give real RTT signal to recalibrate against.

### Why `Close` does not take `writeMu`

Locking `writeMu` inside `Close` would deadlock against a slow peer holding the mutex inside `Send`. Cancelling `closeCtx` instead aborts the in-flight `Write`, which releases the mutex on its own. This is the crux of the design and the reason the library's "Close-safe-with-in-flight-Write" property mattered when picking it.

## Consequences

- The relay has exactly one WS library and one adapter. Future routing-layer code (#4/#16, #5, #6, #7) hands the registry a `*WSConn`, never a hand-rolled adapter.
- The `Conn.Send` signature stays context-free. Callers (broadcast loops, the upgrade handler) pass `[]byte`, get `error`, and the adapter handles deadline policy internally.
- `writeTimeout = 10s` is the relay's first code-level slow-loris mitigation on the write side. A peer that accepts but never reads costs one goroutine for at most 10 seconds; connection-count caps remain the upgrade layer's job.
- `govulncheck` now has a wider scan surface. If it flags a v1.8.x release, bump within the line. Do not pin to a non-stable line; do not switch to `github.com/coder/websocket` opportunistically.
- The adapter relies on a documented library property (`Close` safe with in-flight `Write`). If a future library version regresses it, the race test surfaces a `DATA RACE` and the lint pass breaks; that is the early-warning signal.
- `WSConn` owns its `*websocket.Conn` after construction. Calling the library directly on the same conn is documented as a caller invariant; there is no defensive code, and there cannot be without a global registry of wrapped conns.
- `Conn.Close` returns no error. Idempotency is `sync.Once`-guarded; the underlying `conn.Close`'s error is discarded by design — the registry's contract has no return value, and there is no useful action on a "close-after-close" or "close-after-network-error" report.
