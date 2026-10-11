# Connection caps: upgraded sessions and public listeners

A relay-wide cap on *live* WebSocket connections, shared by `/v1/server` and `/v1/client`. The per-IP rate limiter ([rate-limit-middleware.md](rate-limit-middleware.md)) bounds how fast a source can attempt upgrades, but not how many connections it can hold open at once — an attacker staying under the refill rate could add connections without bound until the 256 MB Fly machine OOMs. Once the upgraded cap is reached, a new upgrade gets `503` before `websocket.Accept` runs. A second bound, `--max-listener-connections` (default 32), independently limits accepted sockets on each public listener before PROXY, TLS or HTTP parsing (#182).

## API

Package `internal/relay`, `internal/relay/conncap.go`:

```go
var ErrInvalidConnCap = errors.New("relay: connection cap must be positive")

func NewConnCap(max int, logger *slog.Logger) (*ConnCap, error)
func (c *ConnCap) Wrap(next http.Handler) http.Handler
```

`NewConnCap` returns an error wrapping `ErrInvalidConnCap` for `max <= 0` — the relay refuses to run uncapped. `Wrap` has the conventional `func(http.Handler) http.Handler` composition shape, matching `rateLimit` and `EnforceHost`.

## Behaviour

`Wrap` does a non-blocking try-acquire on a buffered channel (`slots`, capacity `max`) before `next` runs:

- **Slot acquired** — `defer` the release, then `next.ServeHTTP(w, r)`. The `defer` fires on every exit from `next`, including a panic recovered by `net/http`, so the slot never leaks.
- **No slot available** — `http.Error(w, "", http.StatusServiceUnavailable)`, one `logger.Warn("conn_cap_reached", "path", r.URL.Path, "remote", remoteHost(r))` line, return. `next` never runs, so `websocket.Accept` never runs and the upgrade handler never examines routing headers. The try-acquire never blocks, so an over-cap request never queues.

One `*ConnCap` instance wraps both `ServerHandler` and `ClientHandler`, so binaries and phones draw from a single shared pool — a flood of one kind can starve the other.

### Why every close path releases its slot

`ServerHandler` and `ClientHandler` stay inside `ServeHTTP` for the whole life of the connection and return only after their own deferred cleanup runs (the outbox writer joined, the heartbeat goroutine cancelled). A slot taken on entry and released by `defer` on return therefore outlives every per-connection goroutine and covers every close reason without the wrapper needing to know what they are: peer close, read or write error, heartbeat timeout, a phone torn down when its binary's grace period expires, graceful shutdown, and the reject-after-accept closes (`4404`, `4409`, `4429`).

### What is not counted

`ConnCap` counts only when an upgrade handler starts, immediately before
`websocket.Accept`. It excludes PROXY, TLS and HTTP-header phases, ordinary
HTTP requests and keep-alive connections between requests. Those sockets
are counted by the independent raw per-listener bound instead. Upgraded
WebSockets occupy both kinds of slot; they are not additional accepted
connections.

Public servers retain a 16 KiB header allowance with Go's buffer slack and
5s `ReadHeaderTimeout` ([HTTP limits](autocert-tls.md#server-timeouts)).
PROXY headers and TLS handshakes also have 5s deadlines. These timeouts
bound stalled phases; the raw cap bounds their concurrent accepted sockets.
Upgrade-rate limiting runs after complete HTTP headers, so it cannot protect
any earlier phase.

## Wiring

`cmd/pyrycode-relay/main.go`:

```go
const defaultMaxConnections = 20
const defaultMaxListenerConnections = 32 // combined derivation below

maxConnections = fs.Int("max-connections", defaultMaxConnections, ...)
maxListenerConnections = fs.Int("max-listener-connections", defaultMaxListenerConnections, ...)
...
connCap, err := relay.NewConnCap(*maxConnections, logger)
if err != nil {
    logger.Error("refusing to start: invalid --max-connections", "err", err, "value", *maxConnections,
        "fix", "pass a positive connection count, or omit the flag for the default")
    return 2
}
...
mux.Handle("/v1/server", upgradeMetrics.WrapServerRateLimitDeny(rateLimit(connCap.Wrap(relay.ServerHandler(...)))))
mux.Handle("/v1/client", upgradeMetrics.WrapClientRateLimitDeny(rateLimit(connCap.Wrap(relay.ClientHandler(...)))))
```

`connCap.Wrap` sits **inside** `rateLimit`: a rate-limited attempt never
takes an upgraded slot. The outer `WrapServerRateLimitDeny` /
`WrapClientRateLimitDeny` observers still count only HTTP 429 responses.
`/healthz` bypasses these handler limits but consumes raw listener capacity;
it can wait during public-listener saturation. `/metrics` uses a separate
loopback server and mux, outside both caps, so public saturation leaves it
accessible.

Both flags must be positive. After the serving-mode mutual-exclusion guard,
`run` validates `--max-listener-connections`, then constructs `ConnCap`,
before listeners or background workers start. Invalid values return exit 2.
The values are independent: a positive raw cap below `--max-connections` is
valid, though it then limits how many upgrades that listener can admit.

Each public listen callback opens `net.Listen` and immediately applies
`netutil.LimitListener(ln, maxListenerConnections)`:

- Insecure HTTP and HTTP-01 pass the limited listener to `http.Server.Serve`.
- HTTPS passes it through the optional `NewProxyProtoListener`, then to
  `ServeTLS`, whose TLS wrapper sits above admission and PROXY parsing.
- HTTPS and HTTP-01 have distinct limiters, each with the full configured
  capacity. Saturating HTTP-01 does not consume HTTPS slots. Metrics uses
  its existing `ListenAndServe` path without this wrapper.

`LimitListener` acquires before the underlying `Accept`. At capacity, peers
wait outside relay acceptance; no parser or handler runs for them. A TCP
dial can succeed in the kernel backlog without admission. A raw slot lasts
until socket `Close`, through HTTP keep-alive and a hijacked WebSocket;
handler return and `ConnState` are not release signals. Repeated or concurrent
`Close` calls release exactly once. See [ADR-0014](../decisions/0014-netutil-limitlistener-for-public-admission.md).

The HTTPS callback retains a deferred listener `Close`: `ServeTLS` can fail
during setup before `Serve` takes ownership, so replacing `ListenAndServeTLS`
without preserving its cleanup would leak the listener. PROXY setup failure
also closes the newly opened listener.

### Default derivation

`defaultMaxConnections = 20`, sized for the 256 MB Fly machine, charging every connection at the phone worst case (a binary costs less — it has no outbox):

- handler, heartbeat and phone-outbox goroutines: 3 stacks, ~8 KiB each once grown ≈ 24 KiB;
- websocket/bufio/TLS record buffers ≈ 64 KiB;
- `phoneOutboxBudget` = 4 MiB (16 × `maxFrameBytes` worth of budget, charged in bytes rather than counted in frames since [#154](../codebase/154.md)) of queued frames — reachable by anyone who owns both a binary and a non-reading phone ([phone-outbox.md](phone-outbox.md));
- one `maxFrameBytes` frame in flight on the read side = 256 KiB.

≈ 4.4 MiB per upgraded connection. The combined default estimate charges
both public listeners at capacity on the 256 MB Fly machine:

| Allocation | Estimate |
|---|---:|
| 2 listeners × 32 raw slots × ~128 KiB | 8 MiB |
| 20 upgraded sessions × ~4.4 MiB | 88 MiB |
| Live connection state | 96 MiB |
| ~2× headroom for `GOGC=100`, with no `GOMEMLIMIT` | 192 MiB |
| Binary, runtime, autocert and kernel socket reserve | 56 MiB |
| Combined estimate | 248 MiB |

The ~128 KiB raw allowance includes 16 KiB headers plus Go's 4 KiB slack,
TLS/PROXY/bufio buffers and grown goroutine stacks. The 20 upgraded sockets
are already among the 64 raw slots. Charging raw storage even for those
slots conservatively overlaps some transport storage, not connection count.
The 32-slot default leaves admission headroom above the 20 upgraded slots;
insecure mode has only one public listener.

This is a sizing estimate, not an OOM guarantee. Kernel buffers and runtime
allocation vary. Raising either flag changes the combined budget; an operator
must validate memory and saturation on an isolated non-production Fly app
before production deployment ([deploy verification](../../deploy.md#verify-a-deploy)).
The defaults require no `fly.toml` argv change.

## Concurrency model

Neither cap adds production goroutines. For `ConnCap`, the slot channel is the only shared state; buffered-channel send/receive is safe under concurrent handlers without extra locking. Acquire happens on the request goroutine before `next` runs; release happens on the same goroutine when `next` returns. Graceful shutdown closes every live connection, which makes every handler return, which releases every slot — there is nothing in `ConnCap` itself to shut down.

For raw admission, each existing serving goroutine waits on its own listener
semaphore. `net/http` owns admitted sockets until hijack; the WebSocket
handler then owns closing them. Closing a limited listener wakes a
capacity-blocked `Accept`. The existing shutdown path drains servers and
registered WebSockets with `1001` within `drainDeadline` (10s), returning
exit 0 for a signal or exit 1 for a listener failure, including at saturation.

## Testing

`cmd/pyrycode-relay/listener_cap_test.go` exercises actual `run` wiring in
insecure, HTTPS and PROXY HTTPS modes, with independent HTTP-01 and metrics
checks. Local certificates and a cached challenge avoid external ACME calls.
A successful HTTP response establishes admission; the queued peer must get
no response until a slot frees. Dial success alone would let a broken cap
pass. Keep-alive and WebSocket tests prove slots survive request completion
and hijacking. Release coverage includes abrupt close, failed/stalled TLS,
incomplete HTTP headers and rejected/stalled PROXY headers.

Direct `Accept` observation tests concurrent/repeated `Close`, then closes
the old connection again after slot reuse to catch an erroneous release of
the new owner's slot. Saturated signal and listener-failure tests check
`1001` and bounded exit. Invalid-cap tests use otherwise-valid startup with
a cancelled signal context: an unrelated configuration refusal also returns
2 and could mask a missing cap validator.

## Out of scope

- **Per-source concurrent allowances.** One IP can fill either raw listener
  without reaching the upgrade limiter, or fill all 20 upgraded slots in
  one rate-limit burst. Established connections continue, but new admission
  can be locked out. Deferred in [`security-followups.md`](../../security-followups.md)
  under "Per-IP concurrent connection cap".
- **Fly edge ceilings and queued TCP peers.** The raw cap bounds relay
  acceptance, not the substrate or kernel backlog. Edge concurrency policy
  remains operator-owned, separate work.
- **Kernel socket buffers.** Outside the Go heap and covered only by the
  estimate's reserve; autotuned buffers can exceed it. Lower either cap if
  isolated operational validation requires it.

## Related

- [Codebase: #114](../codebase/114.md) — implementation notes for this ticket.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) — the attempt-rate cap this cap complements; `connCap.Wrap` composes inside it.
- [Per-phone delivery queue](phone-outbox.md) — the per-connection worst case the default derivation charges against.
- [Threat model § DoS resistance](../../threat-model.md) — the threat-model entry this ticket updates.
- [Push wake dispatch](push-wake-dispatch.md) — `pushWakeMaxInFlight`, the prior instance of the same non-blocking buffered-channel try-acquire shape.
