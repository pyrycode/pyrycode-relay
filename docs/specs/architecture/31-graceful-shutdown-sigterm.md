# Spec — graceful shutdown on SIGTERM: drain WS connections before exit (#31)

## Files to read first

- `cmd/pyrycode-relay/main.go:26-326` — full `main`. Two things to extract:
  1. The two listener branches (insecure: `:8080`-style `ListenAndServe` at
     lines 161-211; autocert: `:443` `ListenAndServeTLS` + `:80` ACME at
     lines 213-297, plus the loopback metrics listener at lines 198-205 /
     277-285). Both branches currently call the last `ListenAndServe[TLS]`
     synchronously and `os.Exit(1)` on any goroutine listener error — this
     spec replaces both shapes with a goroutine-per-listener + signal-wait
     pattern.
  2. The boot-refusal flow above the listener block (lines 48-100). All
     existing `os.Exit(2)` exits stay; the shutdown path attaches only to
     post-boot, post-listener-start runtime.
- `internal/relay/registry.go:62-103,275-339` — the registry's lock shape and
  the `PhonesFor` (lines 306-324) / `Counts` (lines 326-339) accessors. The
  new accessor in this spec mirrors `PhonesFor` exactly: snapshot under
  `RLock`, return a freshly-allocated slice, do all slow work (Close
  fan-out) outside the registry lock.
- `internal/relay/ws_conn.go:96-120` — `Close` (delegates to
  `CloseWithCode(StatusNormalClosure, "")`) and `CloseWithCode` semantics:
  `closeOnce` makes both safe under concurrent unwind from a handler
  defer, so the shutdown-path fan-out cannot collide with a handler
  unwinding at the same instant (e.g. via heartbeat 1011).
- `internal/relay/server_endpoint.go:41-110` and
  `internal/relay/client_endpoint.go:40-91` — the WS handlers. The
  `websocket.Accept` call hijacks the underlying TCP conn; the handler
  goroutine then runs the forwarder loop inline. This is what makes the
  WS handlers invisible to `http.Server.Shutdown` (which by Go's contract
  "does not attempt to close nor wait for hijacked connections such as
  WebSockets" — `net/http` docs). The shutdown helper must enumerate them
  itself from the registry.
- `docs/lessons.md` § *"nhooyr.io/websocket.Conn.Close performs a 5s
  close-handshake, gating goroutine exit"* (lines 77-79) — every
  `CloseWithCode` call can block for up to 5s waiting for the peer's
  reciprocal close. Implication: the close-fan-out MUST be concurrent
  (one goroutine per conn) so the wall-clock cost stays bounded at ~5s
  regardless of conn count. The default drain deadline (10s) leaves ~5s
  of headroom beyond that worst case.
- `docs/specs/architecture/3-connection-registry.md` (skim) — the
  passive-registry pattern: registry never invokes `Send` or `Close` on a
  `Conn` under its own lock. The new accessor preserves this — the lock
  scope is the slice copy; the caller iterates and closes outside.
- `docs/specs/architecture/60-metrics-listener.md` (skim) — established
  policy-values-at-wiring-site convention; the 10s drain deadline is one
  more such literal.

## Context

The relay binary has no signal handling today. `grep -rn 'signal.Notify'`
returns zero hits in `cmd/` or `internal/`. SIGTERM (fly machine update,
container stop, systemd restart, k8s pod evict) kills in-flight WS frames
mid-write; peers reconnect after seeing a TCP reset.

The fix is best-effort clean close: stop accepting new connections, send a
1001 (`StatusGoingAway`) close frame on every live WS conn, give peers a
bounded window to acknowledge, then exit. Phones and binaries observe the
close code on the wire and can run their existing reconnect path
(documented out-of-scope in `pyrycode/pyrycode/docs/protocol-mobile.md`).

The shape is constrained by three pre-existing facts in this repo:

- `http.Server.Shutdown(ctx)` is blind to hijacked WS conns (Go's
  documented contract). Listener-close happens promptly inside `Shutdown`,
  but the WS handlers won't unwind until we close their underlying conns.
  The registry is the only authoritative list.
- `WSConn.CloseWithCode` (#7) already exists and is idempotent under
  `closeOnce`, so the shutdown-path and a concurrent handler-defer Close
  cannot double-fire.
- `nhooyr.io/websocket.Conn.Close` waits up to 5s for the peer's
  reciprocal close. The shutdown helper has to fan closes out concurrently
  or the wall-clock cost scales linearly in conn count.

## Design

Three pieces, in dependency order:

### 1. Registry accessor: `Snapshot() []Conn`

Add one method to `*Registry` in `internal/relay/registry.go`:

```go
// Snapshot returns a freshly-allocated slice of every Conn currently
// registered as a binary or a phone. The caller may iterate, append, or
// otherwise mutate the slice without affecting the registry's internal
// state or holding any registry lock. Returns nil when no conns are
// registered. Used by the graceful-shutdown path (#31) to fan close
// frames out without exposing internal maps.
//
// The Conn handles are the same references the registry holds; calling
// Close on them affects the live connection.
func (r *Registry) Snapshot() []Conn
```

Lock shape: `RLock` for the duration of the copy; return outside the
lock. Same pattern as `PhonesFor`. Estimated count: snapshot capacity is
`len(r.binaries) + sum(len(s) for s in r.phones)` — compute under the
lock, single allocation, no resize churn. Order is unspecified
(map-iteration order); callers don't care.

Why not `Snapshot() (binaries, phones []Conn)`: the only consumer
(shutdown helper) treats them uniformly — close them all with the same
code. Returning one flat slice keeps the caller boring. Future consumers
that care about role can read the registry through the existing
role-typed accessors.

### 2. Shutdown helper: `Shutdown(ctx, logger, reg, servers...)`

New file `internal/relay/shutdown.go`. Function signature:

```go
// Shutdown initiates a graceful drain: it concurrently invokes
// http.Server.Shutdown on each server (which closes their listeners and
// waits for non-hijacked handlers), snapshots every WS conn from reg and
// emits a 1001 StatusGoingAway close frame on each, and returns once
// both fan-outs complete or ctx expires. On ctx expiry it force-closes
// each server via http.Server.Close before returning.
//
// Safe to call exactly once per process. Concurrent / repeat invocations
// are undefined.
func Shutdown(ctx context.Context, logger *slog.Logger, reg *Registry, servers ...*http.Server) error
```

Behavior (described, not pre-written):

- Start one goroutine per server invoking `srv.Shutdown(ctx)`. Listener
  close happens synchronously at the start of `Shutdown`, so new
  `/v1/server` and `/v1/client` upgrade attempts will fail the handshake
  the moment we enter this function.
- Snapshot via `reg.Snapshot()`; start one goroutine per snapshotted
  conn invoking `c.CloseWithCode(websocket.StatusGoingAway, "shutting down")`.
- A `sync.WaitGroup` tracks the union of (a) per-server `Shutdown`
  calls and (b) per-conn close calls. A single done-channel closes when
  the WaitGroup hits zero.
- `select { case <-done: ... case <-ctx.Done(): ... }`. On ctx-first,
  iterate `servers` and call `srv.Close()` to force-close any non-WS
  conns that the per-server `Shutdown` was still waiting on; return
  `ctx.Err()`. On done-first, return `nil`.
- Errors from `srv.Shutdown` are logged but do not abort the drain —
  every server gets a Shutdown call, every conn gets a CloseWithCode
  call. The function's return code reflects only deadline outcome.

Error contract: returns `nil` on clean drain within deadline, `ctx.Err()`
on deadline expiry. Test for these two outcomes.

### 3. `main.go` wiring

Replace the two listener tails (lines 161-211 for `--insecure-listen`
mode, lines 213-297 for `--domain` mode) with a uniform pattern. The
boot-refusal flow above stays untouched.

Sequence after `CheckListenerPorts` passes:

1. Build the `[]*http.Server` slice for the active mode (insecure: one
   public + optional metrics; autocert: HTTPS + HTTP-01 + optional
   metrics).
2. `signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)`
   — captures the signal as a context-cancel.
3. For each server, launch a listener goroutine: `srv.ListenAndServe()`
   (or `ListenAndServeTLS("", "")` for the HTTPS server). On
   non-`http.ErrServerClosed` return, send the error to a buffered
   `listenerErr chan error` (one slot is enough — first error wins).
4. `select { case <-sigCtx.Done(): ... case err := <-listenerErr: ... }`.
   On either branch, proceed to shutdown.
5. `drainCtx, cancel := context.WithTimeout(context.Background(), drainDeadline)`
   (default `10*time.Second`, declared as a `const drainDeadline` at the
   top of `main` per the wiring-site-policy convention).
6. `relay.Shutdown(drainCtx, logger, reg, servers...)`. Log the outcome.
7. Return from `main` with the appropriate exit code (`0` on signal or
   `Shutdown` clean return, `1` if a listener-goroutine error was the
   trigger).

The existing `defer limiter.Close()` (main.go:147) now runs on the clean
return path. The comment at lines 142-146 referencing #47 as the
out-of-scope graceful-shutdown ticket can be updated to point to this
ticket — but the architect leaves the wording to the developer.

The `os.Exit(1)` calls currently sitting inside the listener goroutines
(main.go:202, 207, 281, 289, 295) are replaced by the error-channel send
described above. None of the `os.Exit(2)` boot-refusal exits change.

## Concurrency model

Goroutines that exist during a drain:

- **Listener goroutines** (1 per `*http.Server`): unchanged from today
  except for the error sink. Each blocks in `ListenAndServe[TLS]` until
  `http.Server.Shutdown` is invoked, at which point it returns
  `http.ErrServerClosed`.
- **Per-server `Shutdown` goroutines** (1 per `*http.Server`, inside the
  helper): synchronous from the helper's WaitGroup perspective.
- **Per-conn close goroutines** (1 per snapshotted `Conn`, inside the
  helper): each may block up to 5s on the close handshake; running them
  in parallel keeps wall-clock bounded.
- **Handler goroutines** (1 per live WS): not joined. Process exit reaps
  them. The `CloseWithCode` from the close-fan-out is what unblocks the
  handler's `Read` and lets it unwind through its existing
  `defer reg.ScheduleReleaseServer / wsconn.Close` chain.

Shutdown ordering is enforced by the helper, not by main: the moment the
helper is entered, listener-close is in flight; the moment the snapshot
is taken, every WS conn alive at that instant is targeted. Conns
registered after the snapshot starts cannot exist — listener is closed
before the snapshot fires (sequential inside the helper's first two
steps), so no new WS handshake can complete.

## Error handling and edge cases

- **Listener error during normal serving (TCP bind drop, autocert
  failure, etc.)**: surfaced via `listenerErr` channel → triggers drain
  → exit code 1.
- **Listener returns `http.ErrServerClosed`**: expected during shutdown.
  Goroutine returns silently; no error-channel send.
- **Second SIGTERM during drain**: ignored. `signal.NotifyContext` only
  cancels the context once; the shutdown helper proceeds on its
  `drainCtx`. Sending a separate hard-kill signal (SIGKILL) bypasses Go
  entirely and the kernel reaps the process — there is nothing for the
  relay to do.
- **Drain deadline elapses with some conns still mid-handshake**: the
  helper calls `srv.Close()` on each server (force-close of any non-WS
  conn still around) and returns `ctx.Err()`. The hijacked WS conns'
  underlying TCP sockets close when the process exits; their peers see
  a TCP reset on those specific conns. Acceptable per the AC ("a peer
  reading the conn observes the close code on the wire" applies to
  every conn alive at drain start — those that DON'T finish the close
  handshake within 10s degrade to today's behavior, no regression).
- **Empty registry at drain start**: `Snapshot()` returns nil; the close
  fan-out is a no-op; the per-server `Shutdown` calls return promptly;
  function returns `nil`.
- **Boot fails before listener launch**: existing `os.Exit(2)` paths are
  unchanged. The shutdown helper is only reached after at least one
  listener is running.

## Testing strategy

Tests split between the registry accessor, the shutdown helper, and a
single end-to-end smoke through `main`-equivalent wiring.

**Registry — `internal/relay/registry_test.go`** (new test function):

- `Snapshot` on an empty registry returns nil (or a zero-length slice;
  pick one and stick to it — match `PhonesFor`'s nil-on-empty).
- `Snapshot` returns one entry per registered binary plus one per
  registered phone (verify by ConnID set membership, not by order).
- `Snapshot` returns a fresh slice: mutating the returned slice does
  not affect a subsequent `Snapshot` or `BinaryFor` / `PhonesFor`.
- Concurrent `Snapshot` + `ClaimServer` + `RegisterPhone` under
  `-race`: no data race, no panic, internally consistent (each
  individual `Snapshot` reflects some valid state — not a torn read).

**Shutdown helper — `internal/relay/shutdown_test.go`** (new file):

- Clean drain: pass a registry holding two fake `Conn`s (use the
  existing test `mockConn` pattern from `registry_test.go:48-onwards`,
  extended with a `closeCode` field if needed) and two pre-`Listen`'d
  `httptest.NewServer().Config` instances. Assert both conns observed
  `CloseWithCode(StatusGoingAway, …)` and both servers' listeners are
  closed; helper returns `nil`.
- Deadline expiry: use a fake `Conn` whose `Close` blocks past the
  deadline; pass a ctx with 50ms deadline; helper returns `ctx.Err()`;
  `srv.Close` was invoked on each server (assert via a wrapper or by
  observing the listener's `Accept` returns).
- Empty registry: helper returns `nil`, no panic, server-Shutdown still
  ran.
- Idempotency of close: re-Close on the same WSConn does not panic and
  does not double-emit (already covered by `closeOnce` in `ws_conn`,
  but worth one assertion in the shutdown helper test).

**End-to-end — `cmd/pyrycode-relay/main_e2e_test.go`** (new file, or
add to existing `deps_test.go`'s package if package-internal access is
useful):

- Boot the relay on `--insecure-listen=127.0.0.1:0` via a test entry
  point that returns the chosen port and a "trigger shutdown" hook.
  Open a `/v1/server` WS, read once to consume the handshake response,
  invoke the shutdown hook, assert the next read returns a close error
  whose `websocket.CloseStatus` equals `StatusGoingAway`. Assert the
  test entry point returns within the drain deadline.

  The "test entry point" can be as small as exporting `run(args []string,
  signal <-chan os.Signal) int` from `cmd/pyrycode-relay` and having
  `main` call `os.Exit(run(os.Args[1:], handlerCtxFor(SIGTERM/SIGINT)))`.
  The exact shape is a developer choice; the spec asserts only that a
  test can drive the shutdown path without forking a real subprocess.

## Open questions

- **Default drain deadline (10s)**: matches the ticket body and leaves
  ~5s headroom beyond a single close-handshake. If operational
  experience shows phones routinely take >5s to ack a close, this is
  the knob to raise. Architect proposes 10s; PO / ops can override at
  the wiring site without touching the helper.
- **Should `listenerErr` triggering a drain still emit exit code 0?**
  Architect's proposal: exit 1 when shutdown was triggered by a
  listener error (so process supervisors restart), exit 0 when
  triggered by signal (clean operator action). The signal path is the
  ticket's headline; the listener-error path is a side-effect of the
  refactor and should not regress today's `os.Exit(1)` semantics.
