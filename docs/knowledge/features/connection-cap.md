# Global connection cap (`/v1/server` + `/v1/client`)

A relay-wide cap on *live* WebSocket connections, shared by `/v1/server` and `/v1/client`. The per-IP rate limiter ([rate-limit-middleware.md](rate-limit-middleware.md)) bounds how fast a source can attempt upgrades, but not how many connections it can hold open at once — an attacker staying under the refill rate could add connections without bound until the 256 MB Fly machine OOMs. This closes that path: once the cap is reached, a new upgrade gets `503` before `websocket.Accept` runs.

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
- **No slot available** — `http.Error(w, "", http.StatusServiceUnavailable)`, one `logger.Warn("conn_cap_reached", "path", r.URL.Path, "remote", remoteHost(r))` line, return. `next` never runs, so `websocket.Accept` never runs and no header is read on this path. The try-acquire never blocks, so an over-cap request never queues.

One `*ConnCap` instance wraps both `ServerHandler` and `ClientHandler`, so binaries and phones draw from a single shared pool — a flood of one kind can starve the other.

### Why every close path releases its slot

`ServerHandler` and `ClientHandler` stay inside `ServeHTTP` for the whole life of the connection and return only after their own deferred cleanup runs (the outbox writer joined, the heartbeat goroutine cancelled). A slot taken on entry and released by `defer` on return therefore outlives every per-connection goroutine and covers every close reason without the wrapper needing to know what they are: peer close, read or write error, heartbeat timeout, a phone torn down when its binary's grace period expires, graceful shutdown, and the reject-after-accept closes (`4404`, `4409`, `4429`).

### What is not counted

Connections still in the TLS or HTTP-header phase are not counted — `ReadHeaderTimeout` (5s) and the per-IP rate limiter bound that window instead. A connection only takes a slot once its handler starts running, immediately before `websocket.Accept`.

## Wiring

`cmd/pyrycode-relay/main.go`:

```go
const defaultMaxConnections = 20 // see the doc comment for the derivation

maxConnections = fs.Int("max-connections", defaultMaxConnections, ...)
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

`connCap.Wrap` sits **inside** `rateLimit`: a rate-limited attempt never takes a slot, and `rateLimit` sits inside the `WrapServerRateLimitDeny` / `WrapClientRateLimitDeny` HTTP-429-only observers (#57) — nesting the cap inside them keeps their "counts only 429" contract true without teaching them a second status code. `/healthz` stays unwrapped (must remain pollable from monitoring); `/metrics` runs on a separate `*http.Server` and mux entirely, so it is structurally ungated.

`NewConnCap` is constructed right after the `--insecure-listen`/`--domain` mutual-exclusion guard, before any goroutine-starting setup — a non-positive `--max-connections` is a boot-time refusal (exit 2), not a runtime condition.

### Default derivation

`defaultMaxConnections = 20`, sized for the 256 MB Fly machine, charging every connection at the phone worst case (a binary costs less — it has no outbox):

- handler, heartbeat and phone-outbox goroutines: 3 stacks, ~8 KiB each once grown ≈ 24 KiB;
- websocket/bufio/TLS record buffers ≈ 64 KiB;
- `phoneOutboxDepth` × `maxFrameBytes` = 16 × 256 KiB = 4 MiB of queued frames — reachable by anyone who owns both a binary and a non-reading phone ([phone-outbox.md](phone-outbox.md));
- one `maxFrameBytes` frame in flight on the read side = 256 KiB.

≈ 4.4 MiB per connection. Reserving ~56 MiB of the 256 MB machine for the binary, Go runtime, autocert and kernel socket buffers leaves ~200 MiB; with no `GOMEMLIMIT` set, `GOGC=100` lets the heap reach ~2× live before collecting, so ~100 MiB of live connection state ≈ 22 connections, rounded down to 20. An operator on a bigger machine raises it with `--max-connections`. At the default, the production `fly.toml` command needs no change.

## Concurrency model

No new goroutines. The slot channel is the only shared state; buffered-channel send/receive is safe under concurrent handlers without extra locking. Acquire happens on the request goroutine before `next` runs; release happens on the same goroutine when `next` returns. Graceful shutdown closes every live connection, which makes every handler return, which releases every slot — there is nothing in `ConnCap` itself to shut down.

## Out of scope

- **Per-IP concurrent connection cap.** This cap is relay-wide, not per source. The rate limiter's burst (20) equals the default global cap (20), so a single IP can still fill the entire cap in one burst and lock out new upgrades relay-wide while its own connections keep working. Established connections are unaffected, and the relay no longer risks OOM — but the lockout itself is a residual risk. Deferred in [`security-followups.md`](../../security-followups.md) "Per-IP concurrent connection cap".
- **TLS/header-phase connections.** Not counted against the cap; bounded only by `ReadHeaderTimeout` (5s) and the rate limiter.
- **Kernel socket buffers.** Outside the Go heap, covered only by the derivation's ~56 MiB reserve; a stalled phone's autotuned send buffer can exceed that share. An operator can lower `--max-connections` if this is observed.

## Related

- [Codebase: #114](../codebase/114.md) — implementation notes for this ticket.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) — the attempt-rate cap this cap complements; `connCap.Wrap` composes inside it.
- [Per-phone delivery queue](phone-outbox.md) — the per-connection worst case the default derivation charges against.
- [Threat model § DoS resistance](../../threat-model.md) — the threat-model entry this ticket updates.
- [Push wake dispatch](push-wake-dispatch.md) — `pushWakeMaxInFlight`, the prior instance of the same non-blocking buffered-channel try-acquire shape.
