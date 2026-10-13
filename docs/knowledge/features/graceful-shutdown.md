# Graceful shutdown on SIGTERM

On `SIGTERM` or `SIGINT`, the relay drains in-flight WebSocket connections to a clean `1001 StatusGoingAway` close before exiting. Connected phones and binaries observe the close code on the wire and run their existing reconnect path instead of seeing a TCP reset mid-frame.

Best-effort by design: the HTTP/WebSocket drain has a bounded deadline
(10s), followed by deferred push and limiter cleanup before process exit.
WebSocket closes run concurrently and use the underlying
`github.com/coder/websocket` library's close-handshake bounds. At deadline,
shutdown force-closes HTTP connections via `http.Server.Close()` and returns;
hijacked WebSockets are outside that call's reach, and process exit closes
any remaining sockets. Handler goroutines are not joined.

## Contract

- **Signals handled**: `SIGTERM`, `SIGINT`. Captured via `signal.NotifyContext` at program entry. A second signal during drain is ignored — `NotifyContext` cancels its context once. `SIGKILL` is unhandled (the kernel reaps the process and no Go code runs).
- **Exit codes**:
  - `0` — drain triggered by a signal (clean operator action). Returned whether `Shutdown` completed cleanly or hit the deadline.
  - `1` — drain triggered by a listener error (`ListenAndServe[TLS]` returned a non-`http.ErrServerClosed` error). Process supervisors should restart.
  - `2` — boot-time configuration refusal (unchanged from before this ticket).
- **Admission boundary**: `Shutdown` first calls `reg.beginDrain()`, which permanently marks the registry draining and snapshots registered handles under the same write lock used for admission. `ClaimServer`, `TakeoverServer` and `RegisterPhoneCapped` then return `ErrRegistryDraining` before conflict, grace-reclaim, missing-server or phone-cap checks; `RegisterPhone` delegates to the guarded method. Rejected admission leaves registry identities and grace timers unchanged.
- **Late upgrades**: HTTP shutdown runs asynchronously after the boundary and closes listeners, but neither it nor `http.Server.Close()` waits for or closes hijacked WebSockets. An already-upgraded binary or phone reaching admission after the boundary closes itself with `1001 StatusGoingAway`, reason `"shutting down"`, before starting forwarders, heartbeats or success cleanup. A failed conflict probe also reaches the guard through `TakeoverServer`.
- **Upgrade versus admission**: a successful WebSocket dial proves the HTTP upgrade, not registry admission. On these endpoints a native ping/pong round trip with a reader active proves the handler reached its post-admission read loop. This needs no relay-originated application acknowledgement; registration follows the [Binary → relay contract](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#binary--relay).
- **Close code on the wire**: snapshotted connections receive concurrent `CloseWithCode(StatusGoingAway, "shutting down")` calls; late rejected upgrades receive the same call from their own handler. `closeOnce` prevents double emission if another close races it (ADR-0007); only the first close-family call chooses the code.
- **Drain deadline**: 10 seconds by default, declared as `const drainDeadline = 10 * time.Second` at the wiring site in `cmd/pyrycode-relay/main.go`. Expiry returns `ctx.Err()` after calling `http.Server.Close()` on each server. WebSocket close goroutines and late handlers may outlive that return within the library's close bounds; process exit reaps anything remaining.
- **Complete exit**: after `runServers` returns, `run` first closes the
  upgrade `IPRateLimiter`, then `PushWaker` when enabled. The waker refuses
  new wakes, cancels sends, joins their final outcomes and unfinished shared
  authentication, then closes its own limiter. Only after these defers does
  `main` call `os.Exit`; the drain deadline does not cover this cleanup.
- **Configured process budget**: #200 reserves 25s = the full 10s drain +
  10s conservative subsequent push cancellation/authentication join + 5s
  for local limiter joins, scheduling and exit. Push sends and shared refresh
  cleanup run concurrently, so six wakes do not multiply the allowance.
  Both limiter joins stop local eviction workers without network I/O. The
  existing send/refresh deadlines inform the budget; no cleanup deadline was
  added. Runtime or transport stalls can exceed it. Fly's SIGTERM/30s
  [best-effort allowance](../../deploy.md#flytoml-gotchas) adds five seconds;
  fast responsive-peer tests and the two-second push unit assertion are
  evidence, not worst-case bounds or platform guarantees.

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
- Registry admission stays closed after return, including deadline expiry. Reusing a released source-cap reservation does not reopen it; a fresh registry is needed for a new serving lifetime.

Package `internal/relay` (`registry.go`):

```go
func (r *Registry) Snapshot() []Conn
```

- Freshly-allocated slice of every `Conn` (binaries and phones) registered at the moment of the call.
- `nil` on empty registry.
- Order unspecified.
- Snapshotted under `RLock`; the caller iterates and closes outside the lock — preserves the passive-registry pattern (ADR-0003).

`Snapshot()` remains read-only and does not stop admission. Shutdown uses the
unexported `beginDrain() []Conn` instead: it sets the permanent `draining` flag
and calls the shared snapshot helper under `Lock`, then releases the lock
before any socket I/O.

## Sequence

1. `main` calls `signalContextFor(syscall.SIGTERM, syscall.SIGINT)`, which wraps `signal.NotifyContext`, and threads the resulting context into `run`.
2. `runServers` launches one goroutine per `*http.Server` (insecure: public + optional metrics; autocert: HTTPS + HTTP-01 + optional metrics) through its `listen` closure. Each goroutine forwards non-`http.ErrServerClosed` returns to a 1-slot buffered `listenerErr` channel (first error wins, additional errors dropped).
3. `runServers` `select`s on `<-sigCtx.Done()` (operator-triggered) or `<-listenerErr` (listener-triggered).
4. Either branch builds `drainCtx, cancel := context.WithTimeout(context.Background(), drainDeadline)` and calls `relay.Shutdown(drainCtx, logger, reg, servers...)`.
5. Inside `Shutdown`:
   - Call `conns := reg.beginDrain()` to close admission and snapshot registered handles atomically, before launching HTTP shutdown.
   - For each server: launch a goroutine running `srv.Shutdown(drainCtx)` — closes listeners and waits for non-hijacked handlers.
   - For each snapshotted conn: launch a goroutine running `c.CloseWithCode(StatusGoingAway, "shutting down")` (or plain `c.Close()` if the conn doesn't implement the unexported `gracefulCloser` interface). The fan-out unblocks WS handlers' reads so they unwind through their existing defers. Late upgraded handlers rejected by the registry close their own sockets outside this fan-out.
   - Wait on `done` (WaitGroup zero) or `drainCtx.Done()`. On ctx-first, iterate `servers` and call `srv.Close()` to force-close any non-WS conn or listener still pending; return `ctx.Err()`. On done-first, return `nil`.
6. `runServers` returns 0 or 1 based on the trigger. `run` executes its
   deferred upgrade-limiter close and push cleanup before returning that code;
   `main` then calls `os.Exit(run(...))`.

## Concurrency model

During a drain, the relay holds:

- **Listener goroutines** (1 per `*http.Server`) — block in `ListenAndServe[TLS]` until `srv.Shutdown` is called, at which point they return `http.ErrServerClosed` and exit.
- **Per-server `Shutdown` goroutines** (1 per server, inside the helper) — wait for non-hijacked handlers and the listener-close. Tracked by the helper's `WaitGroup`.
- **Per-conn close goroutines** (1 per snapshotted conn, inside the helper) — run in parallel within the WebSocket library's close bounds, so a stalled peer does not delay another peer's close. Tracked by the same `WaitGroup`; the context limits how long shutdown waits.
- **Handler goroutines** (1 per live WS) — not joined by the helper. Admitted handlers unwind through existing defers after their reads unblock; binary release uses `ScheduleReleaseServerIfHeld`. Late upgraded handlers close with 1001 themselves, outside the helper's `WaitGroup` and registry lock. A handler paused past shutdown completion still takes this rejection path when it resumes, without extending shutdown's deadline. Process exit reaps any remaining handlers.

**Admission and drain share one mutex boundary.** Admission that wins `Registry.mu` before `beginDrain` is included in the snapshot unless independently removed. Admission after it fails, even for grace reclaim or a takeover probe begun before draining. Launching HTTP shutdown cannot establish this ordering: an upgrade may already have succeeded, and HTTP shutdown does not wait for hijacked sockets. No registry lock covers ping, socket I/O or a close handshake.

**Why concurrent fan-out is load-bearing**: a WebSocket close can block for 5s waiting for the peer's reciprocal close (see [historical close-handshake constraint](../../lessons.md)). Serial iteration would scale linearly in conn count. Independent close goroutines let reading peers receive 1001 while another peer's close is stalled; the context bounds how long shutdown waits for the group.

## Force-close vs graceful close

`http.Server.Shutdown(ctx)` closes listeners and idle HTTP connections, then
waits for active HTTP connections. On context expiry it returns without
force-closing active connections. The helper's explicit `srv.Close()` loop
handles those HTTP connections before returning `ctx.Err()`. Neither HTTP
method closes hijacked WebSockets; their concurrent close calls have separate
library bounds. `TestShutdown_DeadlineExpiryReturnsCtxErr` asserts a return
within 300ms for a 50ms deadline, and `TestShutdown_UnresponsivePeerDeadline`
pins deadline expiry with a real unread WebSocket peer.

## Wiring (`cmd/pyrycode-relay/main.go`)

Serving entry points:

1. **Process entry split**: `main` is one line — `os.Exit(run(os.Args[1:], signalContextFor(syscall.SIGTERM, syscall.SIGINT)))`. End-to-end tests can drive `run` with synthetic cancellation; the complete-process regression re-executes the test executable and calls the same `run` and signal helper with real SIGTERM.
2. **`run(args []string, sigCtx context.Context) int`**: builds the servers slice for the active mode (insecure: 1 + optional metrics; autocert: 2 + optional metrics) and delegates to `runServers`.
3. **`runServers(sigCtx, logger, reg, servers, listen)`**: single sink for both modes. The `listen func(*http.Server) error` closure abstracts the per-server choice between `ListenAndServe` and `ListenAndServeTLS("", "")`.

The `defer limiter.Close()` introduced by #47 (per-IP rate limiter eviction
goroutine) runs before `defer pushWaker.Close()` in LIFO order on serving
shutdown paths. Previously, `os.Exit(1)` calls inside listener goroutines
skipped deferred cleanup. `PushWaker.Close` joins authentication through the
last send's refresh-waiter cleanup; see [push concurrency](push-wake-dispatch.md#concurrency-model).

Two grouped boot factories retain `relay.NewPushWakerFromEnv` and
`relay.NewMetricsRegistry` as production defaults. Only the isolated process
regression wraps them, delegating construction unchanged and retaining the
waker/metrics for assertions after `run` returns. `PushWaker.RefreshState`
snapshots the FCM refresh's joined-waiter count and completion channel under
the cache mutex, without I/O, credentials or mutation. Retaining that channel
allows the child to prove completion after the cache removes the attempt.
These observations add no public endpoint or protocol acknowledgement and
leave production deadlines unchanged.

## Threat model alignment

- **DoS resistance**: the HTTP/WebSocket drain's wait is context-bounded;
  deferred push cleanup follows outside that deadline. Per-conn close
  goroutines are independent; one stuck peer doesn't block another's close.
  At deadline, `srv.Close()` force-closes HTTP connections; process exit
  closes remaining hijacked sockets. Late handlers cannot reopen registry
  admission or extend the drain with an unbounded join.
- **Information disclosure**: the close reason `"shutting down"` is hardcoded and operator-visible only on the wire to already-connected peers; no env/host/process info leaks.
- **Log hygiene**: the shutdown-path log lines emit only `err` (the wrapped `ctx.Err()` for deadline expiry) and the static "shutdown complete" / "shutdown signal received; draining" / "listener error; draining" / "drain incomplete" messages. No attacker-influenced values.
- **Sibling-defenses preserved**: the rate limiter, header gates and connection caps still run before endpoint admission. Once draining begins, `ErrRegistryDraining` takes precedence at registry admission, including over the phone cap. Rejected handlers release middleware reservations as they return, without installing connections or changing incumbent identities.

## Failure modes

| Failure | Surface | Recovery |
| --- | --- | --- |
| `SIGTERM` / `SIGINT` arrives during steady state | Drain runs; exit 0 | Operator restarts process or supervisor moves on |
| `ListenAndServe[TLS]` returns a non-`ErrServerClosed` error | Drain runs (best-effort close of any live conns); exit 1 | Process supervisor restarts |
| Drain deadline expires before all close handshakes complete | `srv.Close()` force-closes HTTP connections; helper returns `context.DeadlineExceeded`; exit 0 (signal trigger) or 1 (listener-error trigger) | WebSocket cleanup remains best-effort; process exit closes remaining sockets |
| Second `SIGTERM` arrives during drain | Ignored (`NotifyContext` cancels its context once) | Send `SIGKILL` if drain is wedged; kernel reaps the process |
| Empty registry at drain start | `beginDrain()` returns nil and permanently closes admission; close fan-out is a no-op | HTTP shutdown still runs within the context |
| Successful upgrade reaches admission after drain starts | Handler closes with 1001 without installing the connection, even after shutdown returns | Peer reconnects to a serving relay |

## What this deliberately does NOT do

- **Reconnect / resume protocol coordination with peers** — the relay sends `1001` and exits; phones and binaries handle reconnect per the [wire protocol](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md).
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

- `TestRun_SigtermClosesWSConnsWithGoingAway` — full boot via `run`, real WS dial against `/v1/server`, start the three-second read and complete a native `Ping` round trip before cancelling the synthetic sigCtx. The same read deadline must observe wire code 1001, and `run` must exit 0 within `drainDeadline + 2s`. Cancelling on dial success alone races admission; sleeps or longer timeouts would not prove the ordering.
- `TestRun_SigtermWithNoConns` — empty-registry happy path; also asserts listener is no longer dialable after `run` returns.
- `TestRun_ProcessSIGTERMCompleteShutdown` — hermetic re-exec child using
  production `run` and the real signal helper. Native ping/pong and opaque
  round trips prove binary/phone admission; traffic continues in both
  directions while six cold-cache wakes join one held local OAuth request.
  A final guarded snapshot proves all six remain joined and refresh is
  unfinished; signal occurs within five seconds of the first wake, before
  natural ten-second deadlines. The test requires zero exit within the 25s
  process budget, both wire closes 1001, one cancelled OAuth/no FCM request,
  and all six `token_fetch_failed` outcomes with refresh completion checked
  inside the child after `run` returns. It logs signal-to-exit and
  signal-to-authentication-cancellation durations. Synthetic credentials,
  an exact-loopback-only outbound transport, bounded waits and unconditional
  peer/process cleanup keep it local.
- `TestFlyCompleteShutdownAllowance` — pins top-level SIGTERM/30s settings,
  the five-second process-budget margin and Fly's 300s maximum. Full offline
  TOML parsing/baseline comparison evidence is retained in [PR #203](https://github.com/pyrycode/pyrycode-relay/pull/203).

**Cancellation is not cleanup completion.** A server-side socket cancellation
could still pass if the last send stopped joining authentication. The process
test holds the cancelled transport's return for a 100ms negative assertion:
`run` must not return until released, and the retained refresh completion
channel must then be closed inside the child. Also allow the existing
frameless phone-close notice with code 1001 at the still-closing binary;
concurrent drain can deliver it before the binary close handshake finishes.
Ordinary forwarded inner frames remain byte-exact; the [close-notice contract](routing-envelope.md#close-notice-relay-to-binary-152)
is unchanged.

`internal/relay/shutdown_admission_test.go`:

- `TestShutdown_LateUpgradeGoingAway` — a test-only `http.Hijacker` wrapper pauses after the successful 101 upgrade, before `Accept` returns to admission. Shutdown completes before resumption; fresh claims, grace reclaims and phone registrations (including no-server and at-cap states) must receive wire 1001 and leave registry identities unchanged. Synchronizing only on admission would miss the production gap.
- `TestShutdown_ConflictProbeDuringDrain` — gates an incumbent's `Ping` so its failure reaches takeover after shutdown completes; the replacement receives 1001 and cannot install itself.
- `TestRegistry_DrainRejectsAdmission` — repeated claims, grace reclaim, takeover with held/empty/changed incumbent, capped and uncapped phone registration all return `ErrRegistryDraining` without installing or closing the rejected connection or changing a grace timer's identity.
- `TestShutdown_AdmittedPeersGoingAway` — native ping/pong confirms binary and phone admission; both real peers receive 1001 while a separate close is blocked.
- `TestShutdown_UnresponsivePeerDeadline` — a real unread peer forces `context.DeadlineExceeded`; reading after expiry still receives the already-sent 1001 frame.

`TestSourceConnCap_FailureAndTeardown/shutdown` in
`internal/relay/source_conncap_test.go` checks that source reservations are
released and reusable, while a new upgrade against the drained registry is
closed with 1001 and never claimed. Capacity cleanup and registry reopening
are separate contracts.

Focused race stress command:

```sh
go test -race -run '^TestRun_SigtermClosesWSConnsWithGoingAway$' -count=100 ./cmd/pyrycode-relay
```

## Related

- [Codebase: #31](../codebase/31.md) — implementation notes for this ticket.
- [ADR-0003: Connection registry as a passive store](../decisions/0003-connection-registry-passive-store.md) — the pattern `Snapshot` preserves.
- [ADR-0007: `WSConn.CloseWithCode` for active-conn application close codes](../decisions/0007-wsconn-closewithcode-for-active-conn.md) — the `closeOnce` idempotency the shutdown path inherits.
- [Connection registry](connection-registry.md) — admission guards and the permanent drain boundary.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) — `defer limiter.Close()` now runs on every shutdown path.
- [Metrics listener (localhost-only)](metrics-listener.md) — the third `*http.Server` that joins the drain.
- [Lessons § nhooyr Close 5s handshake](../../lessons.md) — the constraint that forces concurrent fan-out.
