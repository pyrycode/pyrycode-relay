# Graceful shutdown on SIGTERM

On `SIGTERM` or `SIGINT`, the relay drains in-flight WebSocket connections to a clean `1001 StatusGoingAway` close before exiting. Connected phones and binaries observe the close code on the wire and run their existing reconnect path instead of seeing a TCP reset mid-frame.

Best-effort by design: the drain has a bounded deadline (default 10s), and conns still alive at expiry are force-closed. Phones / binaries that have stopped reading from their socket will hit the 5s close-handshake gate (per the underlying `nhooyr.io/websocket` library) — the deadline budgets for one such stuck peer per fan-out goroutine and force-closes the rest.

## Contract

- **Signals handled**: `SIGTERM`, `SIGINT`. Captured via `signal.NotifyContext` at program entry. A second signal during drain is ignored — `NotifyContext` cancels its context once. `SIGKILL` is unhandled (the kernel reaps the process and no Go code runs).
- **Exit codes**:
  - `0` — drain triggered by a signal (clean operator action). Returned whether `Shutdown` completed cleanly or hit the deadline.
  - `1` — drain triggered by a listener error (`ListenAndServe[TLS]` returned a non-`http.ErrServerClosed` error). Process supervisors should restart.
  - `2` — boot-time configuration refusal (unchanged from before this ticket).
- **New-connection refusal**: the listener is closed synchronously at the start of the drain (`http.Server.Shutdown(ctx)` shuts down the listener before waiting on handlers). Dials arriving after the drain begins see `ECONNREFUSED`; in-progress upgrades that haven't completed their handshake fail.
- **Close code on the wire**: every WS conn alive at the moment of drain receives a WebSocket close frame with code 1001 (`StatusGoingAway`) and reason `"shutting down"`. Idempotent: a handler-side `Close` racing the shutdown-side `CloseWithCode` does not double-emit (ADR-0007, `closeOnce`).
- **Drain deadline**: 10 seconds by default, declared as `const drainDeadline = 10 * time.Second` at the wiring site in `cmd/pyrycode-relay/main.go`. Conns whose close handshake has not completed by the deadline are force-closed via `http.Server.Close()`; their underlying TCP sockets close when the process exits.

## API

Package `internal/relay` (`shutdown.go`):

```go
func Shutdown(
    ctx context.Context,
    logger *slog.Logger,
    reg *Registry,
    servers ...*http.Server,
) error
```

- Returns `nil` on clean drain within `ctx`'s deadline.
- Returns `ctx.Err()` (`context.DeadlineExceeded`) on deadline expiry; force-closes each `*http.Server` via `srv.Close()` before returning.
- Errors from `srv.Shutdown` are logged via `logger` but do NOT abort the drain — every server gets a `Shutdown` call, every conn gets a close call. The function's return value reflects only the deadline outcome.
- Safe to call exactly once per process. Concurrent or repeat invocations are undefined.

Package `internal/relay` (`registry.go`):

```go
func (r *Registry) Snapshot() []Conn
```

- Freshly-allocated slice of every `Conn` (binaries and phones) registered at the moment of the call.
- `nil` on empty registry.
- Order unspecified.
- Snapshotted under `RLock`; the caller iterates and closes outside the lock — preserves the passive-registry pattern (ADR-0003).

## Sequence

1. `main` calls `signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)` and threads the resulting context into `run`.
2. `run` launches one goroutine per `*http.Server` (insecure: public + optional metrics; autocert: HTTPS + HTTP-01 + optional metrics) invoking `ListenAndServe` or `ListenAndServeTLS("", "")`. Each goroutine forwards non-`http.ErrServerClosed` returns to a 1-slot buffered `listenerErr` channel (first error wins, additional errors dropped).
3. `run` `select`s on `<-sigCtx.Done()` (operator-triggered) or `<-listenerErr` (listener-triggered).
4. Either branch builds `drainCtx, cancel := context.WithTimeout(context.Background(), drainDeadline)` and calls `relay.Shutdown(drainCtx, logger, reg, servers...)`.
5. Inside `Shutdown`:
   - For each server: launch a goroutine running `srv.Shutdown(drainCtx)` — closes the listener synchronously, waits for non-hijacked handlers.
   - Snapshot `reg.Snapshot()`. For each conn: launch a goroutine running `c.CloseWithCode(StatusGoingAway, "shutting down")` (or plain `c.Close()` if the conn doesn't implement the unexported `gracefulCloser` interface). The fan-out unblocks every WS handler's `Read`, which lets each handler unwind through its existing defers.
   - Wait on `done` (WaitGroup zero) or `drainCtx.Done()`. On ctx-first, iterate `servers` and call `srv.Close()` to force-close any non-WS conn or listener still pending; return `ctx.Err()`. On done-first, return `nil`.
6. `run` returns 0 or 1 based on the trigger; `main` calls `os.Exit(run(...))`.

## Concurrency model

During a drain, the relay holds:

- **Listener goroutines** (1 per `*http.Server`) — block in `ListenAndServe[TLS]` until `srv.Shutdown` is called, at which point they return `http.ErrServerClosed` and exit.
- **Per-server `Shutdown` goroutines** (1 per server, inside the helper) — wait for non-hijacked handlers and the listener-close. Tracked by the helper's `WaitGroup`.
- **Per-conn close goroutines** (1 per snapshotted conn, inside the helper) — each may block up to 5s on the WS close handshake. Run in parallel so wall-clock stays bounded at ~5s regardless of conn count. Tracked by the same `WaitGroup`.
- **Handler goroutines** (1 per live WS) — NOT joined by the helper. `CloseWithCode` unblocks the handler's `Read`; the handler unwinds through its existing `defer reg.ScheduleReleaseServer / wsconn.Close` chain. Process exit reaps anything still alive at the deadline.

**Ordering inside the helper is structural, not enforced by a barrier.** The listener-close goroutine and the close-fan-out are launched in sequence on the helper's goroutine; the snapshot is taken after the per-server `Shutdown` calls are in flight, so no new WS handshake can complete between snapshot and close (the listener is already closed). A conn that registered *just before* the snapshot is covered; a conn that tried to register *just after* the snapshot was started never got its handshake accepted.

**Why concurrent fan-out is load-bearing**: `nhooyr.io/websocket.Conn.Close` blocks up to 5s waiting for the peer's reciprocal close (see [Lessons § nhooyr Close 5s handshake](../../lessons.md)). Serial iteration would scale linearly in conn count — two stuck peers would already blow past the 10s deadline. The drain deadline budgets for one such 5s handshake plus ~5s of headroom; concurrency keeps that budget achievable.

## Force-close vs graceful close

`http.Server.Shutdown(ctx)` does NOT call `srv.Close()` internally when `ctx` expires — its listeners stay bound, idle keep-alives stay open. The helper's deadline-path explicit `srv.Close()` loop is what makes the helper return promptly after the deadline. Without it, the helper would still hold listeners open until process exit.

`Close()` on the listeners is the difference between "deadline expired, listeners still bound for ~100ms while goroutines exit" and "deadline expired, immediate force-close, prompt return". The unit test `TestShutdown_DeadlineExpiryReturnsCtxErr` asserts wall-clock < 300ms after a 50ms deadline specifically to catch a regression that drops the force-close.

## Wiring (`cmd/pyrycode-relay/main.go`)

Three structural seams:

1. **Process entry split**: `main` is one line — `os.Exit(run(os.Args[1:], signalContextFor(syscall.SIGTERM, syscall.SIGINT)))`. The split exists so end-to-end tests in `main_e2e_test.go` can drive `run` with a synthetic `context.Context` instead of forking a subprocess to send real signals.
2. **`run(args []string, sigCtx context.Context) int`**: builds the servers slice for the active mode (insecure: 1 + optional metrics; autocert: 2 + optional metrics) and delegates to `runServers`.
3. **`runServers(sigCtx, logger, reg, servers, listen)`**: single sink for both modes. The `listen func(*http.Server) error` closure abstracts the per-server choice between `ListenAndServe` and `ListenAndServeTLS("", "")`.

The `defer limiter.Close()` introduced by #47 (per-IP rate limiter eviction goroutine) now runs on every shutdown path — previously the `os.Exit(1)` calls inside listener goroutines skipped it.

## Threat model alignment

- **DoS resistance**: an attacker cannot stall the relay's shutdown beyond `drainDeadline`. Per-conn close goroutines are independent; one stuck peer doesn't block another's close. At deadline, `srv.Close()` force-closes regardless of peer state.
- **Information disclosure**: the close reason `"shutting down"` is hardcoded and operator-visible only on the wire to already-connected peers; no env/host/process info leaks.
- **Log hygiene**: the shutdown-path log lines emit only `err` (the wrapped `ctx.Err()` for deadline expiry) and the static "shutdown complete" / "shutdown signal received; draining" / "listener error; draining" / "drain incomplete" messages. No attacker-influenced values.
- **Sibling-defenses preserved**: the rate-limiter, header gate, registry caps all stay in force during drain — the listener is closed before the snapshot, so no new connection can be accepted; existing handlers continue to enforce their invariants until they unwind.

## Failure modes

| Failure | Surface | Recovery |
| --- | --- | --- |
| `SIGTERM` / `SIGINT` arrives during steady state | Drain runs; exit 0 | Operator restarts process or supervisor moves on |
| `ListenAndServe[TLS]` returns a non-`ErrServerClosed` error | Drain runs (best-effort close of any live conns); exit 1 | Process supervisor restarts |
| Drain deadline expires before all close handshakes complete | `srv.Close()` force-closes listeners; helper returns `context.DeadlineExceeded`; exit 0 (signal trigger) or 1 (listener-error trigger) | None — peers whose handshakes didn't complete see a TCP reset on those specific conns (no regression from pre-#31 behaviour) |
| Second `SIGTERM` arrives during drain | Ignored (`NotifyContext` cancels its context once) | Send `SIGKILL` if drain is wedged; kernel reaps the process |
| Empty registry at drain start | `Snapshot()` returns nil; close fan-out is a no-op; per-server `Shutdown` calls return promptly; helper returns nil | n/a |

## What this deliberately does NOT do

- **Reconnect / resume protocol coordination with peers** — the relay sends `1001` and exits; phones and binaries handle reconnect per their own logic. Resume semantics live in `pyrycode/pyrycode/docs/protocol-mobile.md` and are out of scope here.
- **Wait for handler goroutines to finish** — the helper unblocks handlers via `CloseWithCode` but does not join them. The handler unwind path is fast (existing defers) and any goroutine still alive at process exit is reaped by the kernel.
- **Persist in-flight frames** — the relay is stateless. Frames mid-write at drain time are lost; the peer's reconnect path re-establishes state.
- **Special-case the metrics listener** — `metricsSrv` joins the same `servers` slice as the public listener(s) and goes through the same `srv.Shutdown` / `srv.Close` path. Loopback scrapers see the same `ECONNREFUSED` after drain as any other client.
- **Configurable deadline via CLI flag** — 10s is a wiring-site constant. A future ticket can promote it to a flag if operational experience demands it; the helper takes `context.Context` so the change is localised.
- **Drain on `SIGHUP`** — `SIGHUP` is typically a config-reload signal; the relay has no runtime-reloadable config (everything is flags or build-time), so handling it would have no caller. Add when needed.

## Testing

`internal/relay/shutdown_test.go`:

- `TestShutdown_EmptyRegistry` — no-args and one-server-no-conns; both return nil; `Serve` returns `http.ErrServerClosed`.
- `TestShutdown_ClosesAllConnsWithGoingAway` — 1 binary + 2 phones; all three observe `CloseWithCode(StatusGoingAway, …)`; server's listener closes.
- `TestShutdown_DeadlineExpiryReturnsCtxErr` — stuck conn + 50ms deadline; `errors.Is(err, context.DeadlineExceeded)`; wall-clock < 300ms; `Serve` returns `http.ErrServerClosed` (force-close path).
- `TestShutdown_CloseIdempotentOnRealWSConn` — races `CloseWithCode(StatusGoingAway)` + `Close()` on the same real `*WSConn`; both return.

`internal/relay/registry_test.go`:

- `TestSnapshot_EmptyRegistry` — pins `nil` on empty.
- `TestSnapshot_IncludesBinariesAndPhones` — set-membership over ConnIDs (no order).
- `TestSnapshot_FreshSliceIsolation` — mutating the returned slice does not affect the registry.
- `TestSnapshot_RaceFreedom` — concurrent `Snapshot` + `ClaimServer` / `ReleaseServer` / `RegisterPhone` under `-race`.

`cmd/pyrycode-relay/main_e2e_test.go`:

- `TestRun_SigtermClosesWSConnsWithGoingAway` — full boot via `run`, real WS dial against `/v1/server`, cancel the synthetic sigCtx, assert the next `Read` returns a close error with `websocket.CloseStatus(err) == StatusGoingAway`, assert exit code 0 within `drainDeadline + 2s`.
- `TestRun_SigtermWithNoConns` — empty-registry happy path; also asserts listener is no longer dialable after `run` returns.

## Related

- [Codebase: #31](../codebase/31.md) — implementation notes for this ticket.
- [ADR-0003: Connection registry as a passive store](../decisions/0003-connection-registry-passive-store.md) — the pattern `Snapshot` preserves.
- [ADR-0007: `WSConn.CloseWithCode` for active-conn application close codes](../decisions/0007-wsconn-closewithcode-for-active-conn.md) — the `closeOnce` idempotency the shutdown path inherits.
- [Connection registry](connection-registry.md) — the registry whose `Snapshot()` accessor this ticket adds.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) — `defer limiter.Close()` now runs on every shutdown path.
- [Metrics listener (localhost-only)](metrics-listener.md) — the third `*http.Server` that joins the drain.
- [Lessons § nhooyr Close 5s handshake](../../lessons.md) — the constraint that forces concurrent fan-out.
