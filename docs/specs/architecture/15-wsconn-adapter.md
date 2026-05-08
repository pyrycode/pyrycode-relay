# Spec — `WSConn` adapter for `nhooyr.io/websocket` (#15)

## Files to read first

- `internal/relay/registry.go:22-46` — the `Conn` interface this adapter must implement, with the doc-comment contract for each of `ConnID`, `Send`, `Close`. The source of truth.
- `internal/relay/registry.go:117-145` — `UnregisterPhone` is the one site that calls `ConnID()` under the registry write lock; that is why `ConnID` must be a non-blocking pure getter.
- `internal/relay/registry_test.go:14-22` — the `fakeConn` shape established for the registry's own tests. Useful as a reference for "how `Conn` implementations are framed in this package," not as a thing this ticket touches.
- `internal/relay/envelope.go:1-19` — sentinel-error idiom and doc-comment style. New code matches it.
- `internal/relay/doc.go` — `package relay` doc string. The new file lives in the same package.
- `docs/PROJECT-MEMORY.md:22-29` — established patterns: stdlib-default, `errors.Is`, "loud failure over silent correction," interface-methods-called-under-lock are documented as non-blocking, package-internal tests.
- `docs/architecture.md:7-15` — relay is the TLS terminus and routes opaque frames; the WS library sees plaintext, hence the supply-chain framing of the new dependency.
- `docs/threat-model.md` § "Supply chain — Go dependencies" and § "DoS resistance — connection floods, slow-loris, fork-bomb retry" — `nhooyr.io/websocket` joins the dependency surface for `govulncheck`; `Send` must not block indefinitely (slow-loris on the write side).
- `Makefile:14-24` — `make test` runs `go test -race ./...`. `make lint` runs `gosec` and `govulncheck`. The race detector is the default mode for every PR.
- `go.mod` — current dep set: stdlib + `golang.org/x/crypto`. This ticket adds the second non-stdlib direct dep.
- `docs/specs/architecture/3-connection-registry.md:34-62` — historical context for why `Conn` is defined where it is; do not re-derive.

## Context

The registry from #3 takes any `Conn` (interface in `internal/relay/registry.go:22-46`); its contract requires `ConnID()` non-blocking, `Send` serialised per connection, and `Close` idempotent and safe concurrently with `Send`. Both endpoint handlers (`/v1/server` in #4/#16, `/v1/client` in #5) need a wrapper around the chosen WebSocket library. Extracting the adapter into one ticket prevents two handlers from inventing two adapters that drift, and isolates the new dependency introduction in a single, small, reviewable PR.

`nhooyr.io/websocket` is the chosen library (modern, context-aware, idiomatic Go, actively maintained — `gorilla/websocket` was unmaintained as of 2022; `gobwas/ws` is lower-level than required). The vanity import path remains `nhooyr.io/websocket`; the upstream module home moved to `github.com/coder/websocket` but the `nhooyr.io/websocket` import path is still the published v1.8.x line. The developer pulls it with `go get nhooyr.io/websocket@latest` and verifies the resolved version is in the v1.8.x line.

The non-trivial design problem is the `Send([]byte) error` ↔ `Conn.Write(ctx, ...)` API mismatch: the registry's contract has no context, but the library requires one for every write. The adapter must own a context strategy that (a) bounds write duration so a slow peer cannot stall callers, and (b) cancels in-flight writes when `Close` runs.

## Design

### Package & files

- New file: `internal/relay/ws_conn.go` (`package relay`).
- New test file: `internal/relay/ws_conn_test.go` (`package relay` — same convention as `envelope_test.go` and `registry_test.go`).
- `go.mod` and `go.sum` updated for `nhooyr.io/websocket` v1.8.x.

No edits to `registry.go`, `envelope.go`, `doc.go`, `cmd/pyrycode-relay/main.go`, or `docs/`. Wiring the adapter into HTTP handlers lands in #4/#16 and #5.

### Types & API

```go
package relay

import (
    "context"
    "sync"
    "time"

    "nhooyr.io/websocket"
)

// writeTimeout bounds a single Send. A slow peer cannot stall a caller
// past this deadline; Send returns context.DeadlineExceeded (wrapped by
// the library) and the caller decides whether to drop the connection.
const writeTimeout = 10 * time.Second

// WSConn adapts a *websocket.Conn from nhooyr.io/websocket to the
// registry's Conn interface. It owns the per-connection write mutex
// (the underlying library forbids concurrent Write) and a per-connection
// cancellation context that Close trips to abort in-flight writes.
//
// All exported methods are safe for concurrent use. Send serialises;
// ConnID is a pure getter; Close is idempotent and may run concurrently
// with Send.
type WSConn struct {
    conn   *websocket.Conn
    connID string

    writeMu sync.Mutex // serialises Conn.Write; held only across one frame.

    closeOnce sync.Once
    closeCtx  context.Context    // cancelled by Close.
    cancel    context.CancelFunc // called once via closeOnce.
}

// NewWSConn wraps c with the relay-assigned connection id. The caller
// retains responsibility for the WebSocket handshake and for choosing
// connID; this constructor neither validates connID nor inspects c.
//
// After construction, the WSConn owns c: callers must reach the
// connection only through WSConn methods. Calling c.Write or c.Close
// directly defeats the adapter's serialisation and cancellation
// guarantees.
func NewWSConn(c *websocket.Conn, connID string) *WSConn

// ConnID returns the constructor-supplied id. Pure getter; safe to call
// from any goroutine and never blocks. The registry calls this under
// its write lock — see registry.go:117-145.
func (w *WSConn) ConnID() string

// Send writes msg as a single binary WebSocket frame. Concurrent Send
// callers are serialised on a per-WSConn mutex; the wire receives whole,
// non-interleaved frames in some order. Each call has a fixed write
// deadline (writeTimeout); Send after Close returns a non-nil error
// (the library reports the cancelled context; the caller treats any
// non-nil return as "drop the connection").
func (w *WSConn) Send(msg []byte) error

// Close cancels in-flight writes and closes the WebSocket with
// StatusNormalClosure. Idempotent: only the first call reaches the
// underlying *websocket.Conn; subsequent calls are no-ops. Safe to call
// concurrently with Send. Returns no value, matching the registry's
// Conn.Close contract.
func (w *WSConn) Close()
```

### Algorithms

**`NewWSConn`** — capture the conn and id, derive a `closeCtx`/`cancel` from `context.Background()`. The context's only purpose is to be cancelled by `Close`; it has no parent because the WSConn outlives the request scope (the registry holds the reference until `Close` runs).

**`Send`** — take `writeMu`, derive a per-call `context.WithTimeout(closeCtx, writeTimeout)`, call `w.conn.Write(ctx, websocket.MessageBinary, msg)`, release the mutex.

```go
func (w *WSConn) Send(msg []byte) error {
    w.writeMu.Lock()
    defer w.writeMu.Unlock()
    ctx, cancel := context.WithTimeout(w.closeCtx, writeTimeout)
    defer cancel()
    return w.conn.Write(ctx, websocket.MessageBinary, msg)
}
```

If `Close` has already run, `closeCtx` is already cancelled and `context.WithTimeout` returns an immediately-cancelled context; `Write` short-circuits and returns the cancellation error. No partial frame goes out. (`nhooyr.io/websocket.Conn.Write` honours `ctx.Done()` before touching the socket.)

**`Close`** — `closeOnce.Do` of "cancel `closeCtx`; call `w.conn.Close(websocket.StatusNormalClosure, "")`; discard its returned error". The library's `Close` is safe to call concurrently with an in-flight `Write` — that's a key reason for choosing `nhooyr.io/websocket` over alternatives.

```go
func (w *WSConn) Close() {
    w.closeOnce.Do(func() {
        w.cancel()
        _ = w.conn.Close(websocket.StatusNormalClosure, "")
    })
}
```

`Close` does **not** take `writeMu`. Acquiring it would deadlock against a slow peer holding the mutex inside `Send`: the whole point of cancelling `closeCtx` is to abort the in-flight `Write` so it releases the mutex on its own. Once the cancelled `Write` returns, the second `Close` step (`conn.Close`) finalises the WebSocket; if it races with the still-unwinding `Send`, `nhooyr.io/websocket` is documented to handle that race internally.

The returned error from `conn.Close` is discarded by design: `Conn.Close` in the registry's contract returns nothing, and there is no useful action the adapter can take on a "close-after-close" or "close-after-network-error" report. Logging the close code is the WS upgrade handler's responsibility (#4/#16, #5), not the adapter's.

**`ConnID`** — `return w.connID`. No lock, no allocation.

### Concurrency model

| Method | Lock | Context | Concurrent-safe with |
|---|---|---|---|
| `ConnID` | none | none | every other method |
| `Send` | `writeMu` (held across one frame write) | `WithTimeout(closeCtx, writeTimeout)` | other `Send` (serialised), `Close` (cancellation breaks it), `ConnID` |
| `Close` | `closeOnce` (one shot) | cancels `closeCtx` | `Send` (in-flight write is cancelled), other `Close` (no-op), `ConnID` |

There is exactly one mutex (`writeMu`) and one one-shot guard (`closeOnce`). The lock graph is a single node; there are no callbacks, no channels, no goroutines spawned by the adapter.

### Error handling

- `Send` returns the underlying library error verbatim; no wrapping. The registry never inspects it; the WS upgrade handler decides whether to log and whether to drop the connection. (No new sentinel errors — there is no protocol-level distinction this adapter needs to expose.)
- `Close` returns nothing, matching the registry's `Conn.Close` contract.
- `NewWSConn` returns no error: every input is trusted by the caller (the WS upgrade handler chose to upgrade and chose the `connID`).
- No panics. No nil checks on `c` — passing a nil `*websocket.Conn` is a programmer error, not a runtime case.

### Lifecycle expectations the adapter does NOT enforce

These are listed so the developer doesn't add defensive code:

- The adapter does not validate `connID` (length, charset, uniqueness). Generation rules belong to the conn-id-scheme ticket.
- The adapter does not perform the WebSocket handshake, header validation, subprotocol selection, ping/pong, or read-side frame loop. Those live in #4/#16, #5, #6, #7.
- The adapter does not bound the per-connection send queue. There is no queue: `Send` writes synchronously and returns. Throttling and backpressure are caller decisions.
- The adapter does not close on `Send` errors. The caller observes the error and chooses to call `Close`.

### Why this context strategy

Three options were considered:

1. **`context.Background()` only.** A slow peer hangs `Send` indefinitely. Rejected — the threat model already treats slow-loris on the read side as a known DoS vector; the same shape applies on the write side.
2. **Caller-supplied context.** Would require changing `relay.Conn.Send`'s signature, which is fixed by #3 and consumed by code that does not naturally have a context to thread (e.g. broadcast loops iterating a `PhonesFor` snapshot). Rejected — would force a registry-API change to fix an adapter-local concern.
3. **Adapter-owned context: `closeCtx` cancelled by `Close`, plus a per-call `WithTimeout`.** Bounds individual writes (slow peer); cancels in-flight writes on close (clean shutdown); does not require a registry API change. Adopted.

`writeTimeout = 10s` is conservative — long enough that a momentarily-laggy mobile client on a poor network is not killed mid-frame, short enough that a single hung `Send` cannot park a goroutine indefinitely. This is the same order of magnitude as the existing `http.Server.WriteTimeout: 60s` (which bounds the whole upgrade response) but tighter because it bounds a single forwarded frame, not a full HTTP exchange. If a future ticket finds this wrong, it changes one constant.

### Why the library is `nhooyr.io/websocket`

Choice fixed by the ticket; this spec records the rationale so the developer doesn't relitigate it:

- Context-aware API on every blocking method (`Read`, `Write`, `Close`) — fits the relay's existing `context.Context` discipline.
- `Conn.Close` is documented as safe-with-an-in-flight-`Write`, which is the property the adapter relies on for non-deadlocking close.
- Maintained: `gorilla/websocket` is in maintenance-only mode, last meaningful release predates Go 1.18; `gobwas/ws` is a frame-level toolkit that would force the adapter to own framing details that aren't relevant to the relay.

The vanity import path `nhooyr.io/websocket` resolves to the v1.8.x module (the upstream maintainer moved development to `github.com/coder/websocket`, but the `nhooyr.io/websocket` line continues to be published). Use the vanity path; do not switch to `github.com/coder/websocket` in this ticket.

## Testing strategy

`internal/relay/ws_conn_test.go`, `package relay`. End-to-end against a real `*websocket.Conn` via `httptest.NewServer` — no mocks of the library, per the ticket.

### Test harness

A single helper in the test file spins up an `httptest.NewServer` whose handler accepts a WebSocket and runs a tiny read-loop that pushes received messages onto a channel:

```go
// startEcho returns a connected *WSConn (client side) and a channel that
// receives every frame the test server reads. Caller defers the cleanup.
func startEcho(t *testing.T) (*WSConn, <-chan []byte, func()) {
    t.Helper()
    received := make(chan []byte, 1024)
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        c, err := websocket.Accept(w, r, nil)
        if err != nil { t.Errorf("accept: %v", err); return }
        defer c.Close(websocket.StatusInternalError, "test ended")
        for {
            _, data, err := c.Read(r.Context())
            if err != nil { return }
            received <- data
        }
    }))

    wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
    client, _, err := websocket.Dial(context.Background(), wsURL, nil)
    if err != nil { t.Fatalf("dial: %v", err) }

    wc := NewWSConn(client, "test-conn-id")
    cleanup := func() {
        wc.Close()
        srv.Close()
    }
    return wc, received, cleanup
}
```

The test server's handler reads with `r.Context()` so closing the test server tears down the read loop cleanly. The receive channel is buffered generously so the producer side is never blocked by an inattentive test.

### Tests (1:1 with AC bullets)

1. **`TestWSConn_ConnID_ReturnsConstructorValue`** — call `NewWSConn(c, "abc")`, assert `ConnID() == "abc"`. Trivial; included for AC coverage and to lock the contract.

2. **`TestWSConn_ConcurrentSend_ProducesIntactFrames`** — start the harness; spawn N goroutines (e.g. N=16), each calling `wc.Send([]byte(fmt.Sprintf("g%d", id)))` once (or M times with a goroutine-tagged payload); `wg.Wait`; drain `received` and assert N (or N×M) intact, distinct frames. The race detector is the primary signal — interleaved frames would produce malformed messages on the receiver, and corrupted writes would surface as `Read` errors. Use `-race` (the Makefile already does).

3. **`TestWSConn_DoubleClose_DoesNotPanic`** — `wc.Close(); wc.Close()`. The test passes by not panicking.

4. **`TestWSConn_SendAfterClose_ReturnsError`** — `wc.Close()`; assert `wc.Send([]byte("late")) != nil`. Do not assert on the specific error type; the library may return any of `context.Canceled`, a closed-connection error, or a wrapped variant. The contract is "non-nil," not a specific value.

5. **(Optional, free coverage)** **`TestWSConn_SlowPeer_TimesOut`** — sketch only; deferred. Building a "peer that accepts but never reads" requires either hijacking the TCP socket in the handler or filling the OS send buffer, both of which are flaky in CI. Not in the ticket's AC. Listed here so the developer doesn't add it on a hunch.

### What we deliberately do not test

- **The library's behaviour itself.** We trust `nhooyr.io/websocket.Conn.Write` to honour `ctx`, and `Conn.Close` to be safe with in-flight `Write`. Wrapping the library to test the library is out of scope.
- **Unit-level mocks of the library.** The ticket explicitly asks for a real `*websocket.Conn` end-to-end. The race-and-correctness signal from `httptest` + `-race` is stronger than what a mock could prove.
- **Performance.** No throughput or latency assertions. Capacity work belongs to a profiling ticket if and when it's needed.

### Lint expectations

`make vet`, `make test`, `make build` all clean. `gosec` and `govulncheck` clean. Adding the dependency widens `govulncheck`'s scan surface; if it flags a known-issue version, bump within v1.8.x to a clean release before merging. Do not pin to a non-stable line.

## Open questions

1. **`writeTimeout = 10s` — right number?** This is a guess informed by typical mobile-network frame budgets (a single forwarded text/voice frame should not take seconds; if it does, the connection is lost). No real traffic to calibrate against in v1. Leave as `const writeTimeout = 10 * time.Second` for now; revisit when the heartbeat ticket (#7) lands and we have a sense of in-the-wild RTT distributions.
2. **Should `Close` set a `StatusNormalClosure` reason string?** No, leave it empty. Close-code semantics for protocol errors (`4401`, `4404`, `4409`) are owned by the WS upgrade handlers; the adapter sees only "the registry asked us to close" and cannot distinguish reasons. Adding placeholder text would mislead operators reading captures.
3. **Is the per-call `context.WithTimeout` allocation acceptable on every `Send`?** Yes — the relay's hot path is human-mediated frame forwarding (chat, command), not microsecond-bound RPC. The allocation is a few-hundred-nanosecond `context.WithDeadline` plus a goroutine-cancel timer; insignificant against the WebSocket frame cost. If profiling later disagrees, swap to a long-lived context with manual deadline management; the public API does not change.

## Out of scope (re-stated, for the developer)

- No edits to `internal/relay/registry.go` or its test. The `Conn` interface is fixed.
- No HTTP handler, no upgrade logic. That is #4/#16 (`/v1/server`) and #5 (`/v1/client`).
- No header validation (`x-pyrycode-server`, `x-pyrycode-token`). Header checks live at the upgrade boundary; they have no presence in the adapter.
- No close-code semantics beyond `StatusNormalClosure`. `4401`/`4404`/`4409` mapping is the upgrade handler's job (#4/#5/#16).
- No heartbeat / ping-pong (#7).
- No frame loop, no envelope wrap/unwrap (#6).
- No conn-id generation. `connID` is supplied by the caller.
- No throttling, queueing, or per-conn rate limit. None of those are in the registry's contract.

## Done means

- `internal/relay/ws_conn.go` exists with `WSConn`, `NewWSConn`, `ConnID`, `Send`, `Close`, plus the `writeTimeout` constant. Every exported symbol carries a doc comment that names its concurrency contract — what is locked, what is cancelled, what is safe to call concurrently with what.
- `internal/relay/ws_conn_test.go` covers the four functional tests above.
- `go.mod` lists `nhooyr.io/websocket` (v1.8.x) as a direct dependency; `go.sum` is updated.
- `make vet`, `make test` (under `-race`), `make build` all clean. `gosec ./...` and `govulncheck ./...` clean.
- One commit on `feature/15`: `feat(relay): WSConn adapter for nhooyr.io/websocket (#15)`.

---

## Security review (security-sensitive label)

### Threat surface for THIS ticket

The adapter sits directly on the path between the network and the registry: every byte from a binary or a phone passes through `WSConn.Send` (in the reverse direction it is a future ticket's read-loop). Because the relay is the TLS terminus, the WSConn's underlying `*websocket.Conn` sees plaintext. The adapter itself does not interpret payloads, but it must not introduce footguns that a downstream caller's mistake turns into a vulnerability.

### Categories walked

- **Trust boundaries.** The adapter trusts (a) `connID` is whatever the WS upgrade handler assigned (validation is the conn-id-scheme ticket's job — `WSConn` treats it as opaque); (b) the caller, which holds the only `*WSConn` reference for that connection, will not reach around it to call `c.Write` or `c.Close` directly; (c) `nhooyr.io/websocket` honours its documented contract (context cancellation aborts `Write`; `Close` is safe with in-flight `Write`). Each trust is named in the doc comment of `NewWSConn` or in the spec's "lifecycle expectations." **Finding:** none introduced; trust shape matches what #3 already documented for the `Conn` interface.

- **Resource exhaustion / DoS.** The threat-model document calls out slow-loris and connection-flood vectors as known v1 gaps. This adapter is the right place to defend against the **write-side** slow-peer case — a peer that completes the WS handshake and then stops reading would, with `context.Background()` writes, hang every `Send` to that peer indefinitely, parking goroutines and (more importantly) blocking broadcast loops that fan out to many phones. **Finding:** mitigated by `writeTimeout = 10s` on every `Send`. A single slow peer cannot stall a broadcast goroutine for more than 10 seconds before it errors out and the upper layer drops the connection. This is the first code-level slow-loris mitigation in the relay; document the constant clearly.

- **Resource exhaustion / memory.** No internal queue, no background goroutine, no buffer pool. `Send` is synchronous; the only allocation per call is the `context.WithTimeout` triplet. **Finding:** the adapter contributes nothing to per-connection memory growth.

- **Race conditions / data corruption on the wire.** `nhooyr.io/websocket.Conn.Write` is **not** safe to call from multiple goroutines at once; concurrent calls would interleave bytes mid-frame. The adapter's `writeMu` serialises every `Send`. The race-detector test under `-race` is the verification mechanism, not just a smoke test. **Finding:** mitigated; tested.

- **Lock-order / deadlock.** Single mutex (`writeMu`); one one-shot guard (`closeOnce`). `Close` deliberately does **not** take `writeMu` — instead it cancels `closeCtx`, which causes the in-flight `Write` to abort and release the mutex on its own. **Finding:** no deadlock path; the design rationale is named in the spec so a future contributor doesn't "tidy up" by adding `Close`-takes-`writeMu` and reintroduce the deadlock.

- **Use-after-close / double-close.** `Close` is `sync.Once`-guarded; the underlying `conn.Close` runs at most once. `Send` after `Close` finds `closeCtx` already cancelled and `Write` returns the cancellation error without touching the socket. **Finding:** safe; tested.

- **Information disclosure.** The adapter sends what it is given; it does not log payloads, headers, or `connID`. (No `slog` calls anywhere in the adapter.) **Finding:** matches `docs/threat-model.md` § "Log hygiene" — no leakage path through this file.

- **Supply chain.** Adds `nhooyr.io/websocket` to `go.mod`. The threat model already names this as a known cost when the WS library lands (`docs/threat-model.md` § "Supply chain — Go dependencies": *"A compromised future WebSocket library would see every routed frame in cleartext"*). The mitigation lives in `make lint` (`govulncheck`) and the project's "new dependencies need a justification" rule (this spec is the justification). **Finding:** acceptable; documented.

- **Panics / nil-deref.** No `nil` checks; the adapter trusts the caller to pass a valid `*websocket.Conn`. No slice indexing, no map access. **Finding:** none observed.

- **Error leakage to the wire.** `Send` returns library errors verbatim. The error is consumed by registry callers (broadcast loops, the upgrade handler), not sent to a peer. `Close` discards `conn.Close`'s error — no leakage path. **Finding:** none.

### Adversarial framings considered

- *"What if a peer completes the handshake, then never reads?"* — `Send` blocks for up to `writeTimeout` (10s), then returns `context.DeadlineExceeded`. The caller drops the connection. A flood of such peers consumes one goroutine each for 10 seconds, capped. Connection-count caps belong to the WS upgrade ticket; this is the per-connection bound.

- *"What if many goroutines call `Send` concurrently with one slow peer?"* — `writeMu` serialises them. Each waits in turn; each that does eventually win the mutex hits the same 10-second timeout. The serial-tail is bounded — N callers wait at most N×10s in the worst case. Acceptable for v1; if it becomes a problem, the upgrade layer adds connection caps before the adapter changes.

- *"What if `Close` is called while ten goroutines are blocked on `writeMu`?"* — `Close` cancels `closeCtx` without taking `writeMu`. The currently-writing goroutine's `ctx` is cancelled and `Write` returns. The mutex is released. The next-in-line goroutine acquires `writeMu`, derives its own `WithTimeout(closeCtx, ...)` — `closeCtx` is already cancelled, so `Write` short-circuits. Each queued `Send` returns in turn with a cancellation error. No goroutine is stuck.

- *"What if `Close` runs concurrently with a `Send` that is mid-`Write`?"* — `Close` cancels `closeCtx`, then calls `conn.Close`. `nhooyr.io/websocket` is documented to handle `Close`-with-in-flight-`Write` cleanly: the `Write` returns an error; the `Close` finalises. The adapter relies on this property — it's why this library was chosen. If the property regresses in a future library version, `govulncheck` won't catch it (it's a behaviour change, not a CVE), but the race test would surface a `DATA RACE` and the lint pass would break.

- *"What if the caller passes the same `*websocket.Conn` to two `WSConn` constructors?"* — Each gets its own `writeMu`; serial-write guarantee is broken; the wire interleaves. **The doc comment on `NewWSConn` says the WSConn owns `c`.** This is a caller-invariant; the adapter does not (and cannot, without keeping a global registry of wrapped `*websocket.Conn`) defend against it. Documented; not coded.

- *"What if `connID` contains adversarial bytes (NULs, very long, control characters)?"* — `WSConn` stores it and returns it from `ConnID()`. The registry uses it as a map key (slice-scan key). No interpretation. The conn-id-scheme ticket is responsible for charset/length; the adapter's contract is "opaque string." This matches #3's stance on `serverID`.

- *"What if `nhooyr.io/websocket` v1.8.x is compromised?"* — Plaintext exposure of every routed frame, as `docs/threat-model.md` § "Supply chain" already names. `govulncheck` catches known CVEs; it does not catch a malicious release tagged by an authentic maintainer. No new mitigation in this ticket; the residual is the same risk the threat model already accepts for adding any WS library.

- *"Does the per-call `context.WithTimeout` create a goroutine that leaks?"* — No. `context.WithTimeout` schedules a single timer that is cancelled by the `defer cancel()` in `Send`. No goroutine is spawned by the cancel-context machinery; the underlying timer is reclaimed.

### Verdict: PASS

The adapter introduces one new code-level slow-loris mitigation (`writeTimeout`), preserves the registry's lock-discipline assumptions, and does not widen any documented threat surface beyond the supply-chain cost the threat model already names for adding a WS library. The doc-comment-encoded caller invariants ("WSConn owns `c`") are clearly out of the adapter's hands and are explicitly noted; no defensive code can be added against them without changing the API. The `security-sensitive` label is justified — `Send` is on every routed-frame path — and the review is correspondingly thorough.
