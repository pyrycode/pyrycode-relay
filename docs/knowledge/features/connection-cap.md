# Connection caps: upgraded sessions and public listeners

Two limits on live WebSocket handlers are shared by `/v1/server` and
`/v1/client`: `--max-connections` (default 20) relay-wide, and
`--max-connections-per-source` (default 8) for each source (#180). They
complement the [per-IP attempt-rate limiter](rate-limit-middleware.md):
the global cap bounds resident upgraded state, while the source allowance
keeps one source from occupying every upgraded slot. Either refusal returns
`503` before `websocket.Accept`. Independently, `--max-listener-connections`
(default 32) limits accepted sockets on each public listener before PROXY,
TLS or HTTP parsing (#182).

## API

Package `internal/relay`, `internal/relay/conncap.go`:

```go
var ErrInvalidConnCap = errors.New("relay: connection cap must be positive")

func NewConnCap(max int, logger *slog.Logger) (*ConnCap, error)
func (c *ConnCap) Wrap(next http.Handler) http.Handler
```

`NewConnCap` returns an error wrapping `ErrInvalidConnCap` for `max <= 0` — the relay refuses to run uncapped. `Wrap` has the conventional `func(http.Handler) http.Handler` composition shape, matching `rateLimit` and `EnforceHost`.

Package `internal/relay`, `internal/relay/source_conncap.go`:

```go
var ErrInvalidSourceConnCap = errors.New("relay: source connection allowance must be positive")

func NewSourceConnCap(max int, logger *slog.Logger, trustForwardedFor bool) (*SourceConnCap, error)
func (c *SourceConnCap) Wrap(next http.Handler) http.Handler
```

The source constructor rejects `max <= 0` with an error wrapping
`ErrInvalidSourceConnCap`. `run` additionally requires the source allowance
to be below the global cap; the constructor has no global-cap argument.

## Behaviour

`ConnCap.Wrap` does a non-blocking try-acquire on a buffered channel (`slots`, capacity `max`) before `next` runs:

- **Slot acquired** — `defer` the release, then `next.ServeHTTP(w, r)`. The `defer` fires on every exit from `next`, including a panic recovered by `net/http`, so the slot never leaks.
- **No slot available** — `http.Error(w, "", http.StatusServiceUnavailable)`, one `logger.Warn("conn_cap_reached", "path", r.URL.Path, "remote", remoteHost(r))` line, return. `next` never runs, so `websocket.Accept` never runs and the upgrade handler never examines routing headers. The try-acquire never blocks, so an over-cap request never queues.

One `*ConnCap` and one `*SourceConnCap` wrap both endpoint handlers. A source's
binaries and phones share one allowance regardless of endpoint or server-id;
many sources can still fill the global pool.

`SourceConnCap.Wrap` atomically checks and increments a mutex-protected count
before global admission. At its allowance it returns
`http.Error(w, "", http.StatusServiceUnavailable)` (body only `"\n"`) and
exactly one `Warn("source_conn_cap_reached", "remote", strconv.Quote(source))`.
It takes no global slot, adds no reservation or count entry, and never invokes
the endpoint. Neither routing headers nor tokens are examined or logged by
this gate; it adds no source metric labels. With global and raw capacity
available, a different source can still upgrade.

The key is the exact `ClientIP(r, trustForwardedFor)` result used by rate
limiting, with no canonicalization. Source ports are stripped from
`RemoteAddr`. XFF is ignored by default; when trusted, its trimmed left-most
entry is used, falling back to the `RemoteAddr` host if that entry is empty
or absent. On PROXY HTTPS, `RemoteAddr` already carries the declared PROXY v2
source; with XFF trust off, distinct declared sources sharing a socket peer
have separate allowances. See the [source trust model](rate-limit-middleware.md#trust-model----trust-x-forwarded-for).

### Why every close path releases its slot

`ServerHandler` and `ClientHandler` stay inside `ServeHTTP` for the whole life of the connection and return only after their own deferred cleanup runs (the outbox writer joined, the heartbeat goroutine cancelled). A slot taken on entry and released by `defer` on return therefore outlives every per-connection goroutine and covers every close reason without the wrapper needing to know what they are: peer close, read or write error, heartbeat timeout, a phone torn down when its binary's grace period expires, graceful shutdown, and the reject-after-accept closes (`4404`, `4409`, `4429`).

Both upgraded reservations last through handler cleanup, including header
or handshake failure and panic unwinding. Source release deletes the count
entry when the last admitted handler returns, so no source history remains.
A global refusal immediately returns its temporary source reservation.
Heartbeat timeout starts a close handshake; it does not itself free either
reservation before handler cleanup finishes.

### What is not counted

The upgraded caps count requests reaching the upgrade routes before the
endpoint validates headers or calls `websocket.Accept`, including requests
that subsequently fail those checks. They exclude PROXY, TLS and HTTP-header
phases, other HTTP routes and keep-alive connections between requests. Those
sockets are counted by the independent raw per-listener bound instead. Upgraded
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
const defaultMaxConnectionsPerSource = 8
const defaultMaxListenerConnections = 32 // combined derivation below

maxConnections = fs.Int("max-connections", defaultMaxConnections, ...)
maxConnectionsPerSource = fs.Int("max-connections-per-source", defaultMaxConnectionsPerSource, ...)
maxListenerConnections = fs.Int("max-listener-connections", defaultMaxListenerConnections, ...)
...
connCap, err := relay.NewConnCap(*maxConnections, logger)
if err != nil {
    logger.Error("refusing to start: invalid --max-connections", "err", err, "value", *maxConnections,
        "fix", "pass a positive connection count, or omit the flag for the default")
    return 2
}
sourceCap, err := relay.NewSourceConnCap(*maxConnectionsPerSource, logger, *trustXFF)
if err != nil || *maxConnectionsPerSource >= *maxConnections {
    logger.Error("refusing to start: invalid --max-connections-per-source",
        "value", *maxConnectionsPerSource,
        "fix", "pass a positive source allowance below --max-connections; reducing the global cap may require reducing the source allowance")
    return 2
}
...
mux.Handle("/v1/server", upgradeMetrics.WrapServerRateLimitDeny(rateLimit(sourceCap.Wrap(connCap.Wrap(relay.ServerHandler(...))))))
mux.Handle("/v1/client", upgradeMetrics.WrapClientRateLimitDeny(rateLimit(sourceCap.Wrap(connCap.Wrap(relay.ClientHandler(...))))))
```

Admission order is metrics observer → rate limiter → source allowance →
global cap → endpoint. Empty-source and rate-limited attempts receive 429
without reserving source or global capacity. Source denials take no global
slot. The outer `WrapServerRateLimitDeny` / `WrapClientRateLimitDeny`
observers count only HTTP 429; neither cap's 503 is a rate-limit denial.
`/healthz` bypasses these handler limits but consumes raw listener capacity;
it can wait during public-listener saturation. `/metrics` uses a separate
loopback server and mux, outside all connection caps, so public saturation
leaves it accessible.

All three flags must be positive, with `0 < max-connections-per-source <
max-connections`. After the serving-mode mutual-exclusion guard, `run`
validates the raw cap, then the global cap and source allowance, before
listeners or background workers start. Invalid values return exit 2,
including when the source flag was omitted (default 8) and the global cap
is 8 or below. Such operators must explicitly lower the source allowance;
a global cap of 1 permits no valid positive allowance. For example,
`--max-connections=4 --max-connections-per-source=2` is valid. The raw cap
is independent: a positive raw cap below the global cap is valid, though it
then limits how many upgrades that listener can admit.

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

On Fly, both raw-TCP services configure `type = "connections"` and
`hard_limit = 32`, matching this flag's default independently for
8080/external 80 and 8443/external 443 (#183). Fly's
[forwarding ceiling](https://docs.fly.io/reference/configuration#servicesconcurrency)
stops new traffic to a saturated Machine; the relay cap separately bounds
accepted sockets, including connections that bypass the public edge.
Keep the manifest ceilings aligned when changing the raw listener setting.

### Default derivation

`defaultMaxConnectionsPerSource = 8` allows one daemon, six phones and one
desktop behind one NAT, leaving twelve of the default twenty global slots
for other sources. It bounds occupancy by source without reserving slots
for a particular client kind. The registry still permits one live binary per
server-id. Clients behind a shared NAT compete for that same allowance.

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
allocation vary. Raising the global or raw cap changes the combined budget;
an operator must validate memory and saturation on an isolated non-production Fly app
before production deployment ([deploy verification](../../deploy.md#verify-a-deploy)).
The defaults require no `fly.toml` argv change.

## Concurrency model

None of the caps adds production goroutines. For `ConnCap`, the slot channel
is the only shared state; buffered-channel send/receive is safe under
concurrent handlers without extra locking. `SourceConnCap` holds its mutex
only for checking and updating counts, never while logging, writing a
response or calling `next`, so it does not nest with the global semaphore or
registry locks. Concurrent attempts across both routes cannot exceed the
source allowance. Acquire and deferred release run on the request goroutine.
Existing shutdown closes live connections and lets their handlers return;
neither upgraded cap needs a shutdown worker.

For raw admission, each existing serving goroutine waits on its own listener
semaphore. `net/http` owns admitted sockets until hijack; the WebSocket
handler then owns closing them. Closing a limited listener wakes a
capacity-blocked `Accept`. The existing shutdown path drains servers and
registered WebSockets with `1001` within `drainDeadline` (10s), returning
exit 0 for a signal or exit 1 for a listener failure, including at saturation.

## Testing

`internal/relay/source_conncap_test.go` covers concurrent shared admission,
quoted denial logs and newline-only bodies, cleanup lifetime, panic release,
map deletion and exact `ClientIP` identity. It also checks that global denial
returns the source reservation and that outer rate/empty-source denials take
neither reservation. Header and handshake failures return capacity; real
WebSockets cover abrupt close, heartbeat, 4409/4429 and shutdown. The existing
connection-cap harness also checks source-map cleanup for peer close, 4404
and grace-expiry teardown.
Recovery assertions must wait for cleanup rather than treating a timeout or
client-side close as immediate capacity release. In heartbeat tests, let the
timeout initiate close before reading, then complete the peer's close
handshake; reading early can answer the ping and prevent the intended timeout.

`cmd/pyrycode-relay/source_conncap_test.go` checks coupled startup validation
and actual HTTPS/PROXY wiring with the default eight. Declared sources share
one loopback socket peer: one reaches its allowance on both routes, another
still upgrades, and health/metrics remain available with 429 counters
unchanged. After a phone closes, consume its close notice at the binary
before checking the next routed recovery frame; otherwise the notice can
look like a forwarding failure. Global-saturation tests use distinct sources
and a valid source/global configuration so they reach the global gate.

`cmd/pyrycode-relay/listener_cap_test.go` exercises actual `run` wiring in
insecure, HTTPS and PROXY HTTPS modes, with independent HTTP-01 and metrics
checks. Local certificates and a cached challenge avoid external ACME calls.

`startCappedRelay` uses `reserveCapAddrs` to bind one complete endpoint set
on `127.0.0.1:0`: public and metrics in insecure mode, plus HTTP-01 in HTTPS
and PROXY HTTPS. Holding all listeners through certificate, transport and
argument setup keeps the endpoints pairwise distinct; selecting and releasing
them one at a time can reuse a port and cause a false admission failure.
Reservations are released immediately before the `run` goroutine starts,
without startup retries. Cleanup is registered before allocation, so setup
failure also closes any reservations already obtained. A brief window for
another process to claim a released endpoint remains because `run` binds
its own listeners (#206).

`TestReserveCapAddrs/2` and `/3` check distinct addresses and require bind
attempts against every held endpoint to fail with `EADDRINUSE`. After release,
the test rebinds all endpoints together, releases the old reservations again,
and verifies the new listeners still hold them. Uniqueness alone misses
reservations released too early; the held-address bind check detects that
broken lifetime deterministically, without depending on accidental OS port
reuse or retrying failed startups.

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

Offline config assertions and local socket tests cannot prove Fly's live
forwarding policy. An absent HTTP/TLS response can mean either Fly withheld
forwarding or the relay has not accepted a queued peer; TCP dial success
proves neither. The operator must observe forwarding at saturation and
recovery for each service on an isolated Fly app before production deployment,
as described in [Verify a deploy](../../deploy.md#verify-a-deploy).

## Out of scope

- **Per-source raw listener allowances.** One source can fill either raw
  listener before headers reach the upgraded source gate. Continuously
  replaced pre-header peers can keep capacity occupied despite phase
  timeouts. Deferred in [`security-followups.md`](../../security-followups.md)
  under "Per-IP concurrent connection cap". Multiple sources, including
  one operator's daemons on different addresses, can still fill the global
  upgraded pool. XFF/PROXY spoofing under an untrusted upstream remains a
  [source-trust risk](../../threat-model.md#dos-resistance--connection-floods-slow-loris-fork-bomb-retry).
- **Edge resources and queued TCP peers.** The configured Fly ceilings
  stop new forwarding, without guaranteeing immediate client drops or
  bounding edge resources and the kernel backlog. Raw admission remains
  the separate process boundary.
- **Kernel socket buffers.** Outside the Go heap and covered only by the
  estimate's reserve; autotuned buffers can exceed it. Lower either cap if
  isolated operational validation requires it.

## Related

- [Codebase: #114](../codebase/114.md) — implementation notes for this ticket.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) — the attempt-rate cap the upgraded caps complement; source and global admission compose inside it.
- [Per-phone delivery queue](phone-outbox.md) — the per-connection worst case the default derivation charges against.
- [Threat model § DoS resistance](../../threat-model.md) — the threat-model entry this ticket updates.
- [Push wake dispatch](push-wake-dispatch.md) — `pushWakeMaxInFlight`, the prior instance of the same non-blocking buffered-channel try-acquire shape.
