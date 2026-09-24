# Per-IP rate-limit middleware (`/v1/server` + `/v1/client`)

HTTP middleware that throttles WebSocket upgrade attempts per source IP. Excess attempts from a single IP receive `429 Too Many Requests` **before** `websocket.Accept` runs, before the registry is touched, before the mobile/binary token header is read. Combined with the per-frame size cap (#29) and the per-server-id phone cap (#30), this closes the third leg of `docs/threat-model.md` § *DoS resistance* (connection floods, fork-bomb retry).

`/healthz` is intentionally NOT wrapped — it must remain pollable from monitoring without throttling.

## API

Package `internal/relay`:

```go
func NewRateLimitMiddleware(
    limiter *IPRateLimiter,
    logger *slog.Logger,
    trustForwardedFor bool,
) func(http.Handler) http.Handler
```

The factory returns the conventional `func(http.Handler) http.Handler` composition shape (matches `EnforceHost` in `cmd/pyrycode-relay/main.go`).

## Behaviour

Per request:

1. Extract the source IP via `relay.ClientIP(r, trustForwardedFor)` (#51).
2. If the result is empty, deny: write `429`, emit the deny log line, return. **`limiter.Allow` is never called on the empty key** — refusing un-attributable traffic rather than admitting it un-throttled (the loud-failure rule).
3. Otherwise call `limiter.Allow(ip)`. On `false`, deny the same way.
4. On `true`, `next.ServeHTTP(w, r)`.

### 429 response shape

`http.Error(w, "", http.StatusTooManyRequests)` — empty body, matching the existing `http.Error(w, "", http.StatusBadRequest)` shape in `server_endpoint.go` / `client_endpoint.go`. `Content-Type: text/plain; charset=utf-8`, `X-Content-Type-Options: nosniff`, body `"\n"`. No internal state leaked.

### Deny log shape

```go
logger.Warn("rate_limited", "remote", strconv.Quote(ip))
```

Exactly one line per deny, same shape on both deny branches.

- Level `Warn`: operational signal, not noisy enough for `Info`, not severe enough for `Error`.
- Message `"rate_limited"`: event-type-as-msg convention (`"server_claimed"`, `"server_id_conflict"`, …).
- Single key `"remote"`: already in `allowedLogKeys` (`internal/relay/log_allowlist.go`); no allowlist edit.
- Value `strconv.Quote(ip)`: defends against control-byte log injection per `client_ip.go`'s contract. The text handler escapes inner quotes, so a denied `1.2.3.4` renders as `remote="\"1.2.3.4\""`; an empty-IP deny renders as `remote="\"\""` — operators can disambiguate the two deny causes by the value.
- Mobile/binary `X-Pyrycode-Token` is never read on this path. Structural: no `r.Header.Get("X-Pyrycode-Token")` anywhere in the file.

The `TestLogKeysAreAllowlisted` AST walker enforces the literal-string-key requirement; both keys are literals.

## Trust model — `--trust-x-forwarded-for`

A boolean CLI flag controls whether `X-Forwarded-For` is honoured. Default `false`. The flag's `Usage` carries the operator-facing warning that enabling it without a trusted reverse proxy in front allows clients to spoof their source IP and bypass per-IP rate limits.

- `--trust-x-forwarded-for=false` (default): the bucket key is the host portion of `r.RemoteAddr`. An attacker setting `X-Forwarded-For: 127.0.0.1` keys against their real `RemoteAddr`.
- `--trust-x-forwarded-for=true`: the bucket key is the left-most `X-Forwarded-For` entry, falling back to `RemoteAddr`. Trust is binary (all-or-nothing); CIDR-aware proxy chains are out of scope.

The flag's value is captured at process start and threaded once into `NewRateLimitMiddleware`. No per-request mode-flipping.

### Where `RemoteAddr` comes from in production

`ClientIP` reads `r.RemoteAddr`, but on Fly's raw-TCP services the socket
peer is Fly's edge proxy, not the client — `X-Forwarded-For` is not
involved either way. Since [#110](../codebase/110.md), the production
`443` port runs `--https-proxy-protocol`
([feature doc](proxy-protocol-listener.md)): the autocert HTTPS listener
requires and parses a PROXY protocol v2 header before the TLS handshake and
rewrites `RemoteAddr` to the header's source address, so this middleware
(unchanged) keys on the real client IP without ever reading a forwarding
header. `--trust-x-forwarded-for` and `--https-proxy-protocol` are
independent flags addressing different substrates — a deploy that trusts
neither falls back to the raw socket peer, which is Fly's proxy address on
Fly and the real peer on a directly-exposed host.

## Wiring

`cmd/pyrycode-relay/main.go`:

```go
const (
    rateLimitRefillEvery      = 6 * time.Second
    rateLimitBurst            = 20
    rateLimitEvictionInterval = 5 * time.Minute
)
limiter := relay.NewIPRateLimiter(rateLimitRefillEvery, rateLimitBurst, rateLimitEvictionInterval)
defer limiter.Close()
rateLimit := relay.NewRateLimitMiddleware(limiter, logger, *trustXFF)

mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))
mux.Handle("/v1/server", rateLimit(relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes)))
mux.Handle("/v1/client", rateLimit(relay.ClientHandler(reg, logger, maxFrameBytes, 16)))
```

Policy values live at the wiring site, matching the existing `maxFrameBytes` pattern. Derivation: `refillEvery=6s` × `burst=20` ≈ 10 sustained attempts/IP/minute with 20-attempt burst headroom for legitimate retry storms; `evictionInterval=5min` is well above `burst*refillEvery = 120s` (the bucket's at-capacity threshold), keeping the bucket map's resident size bounded under address-space scanning.

All three values must be positive — `NewIPRateLimiter` panics on a zero/negative `evictionInterval` via `time.NewTicker`, the right loud failure for a wiring bug.

### Shared limiter across endpoints

One `*IPRateLimiter`, one middleware, applied to both `mux.Handle` registrations. A misbehaving IP retrying against either endpoint shares one bucket — splitting per endpoint would just halve the attacker's per-IP cost.

### `defer limiter.Close()` is best-effort

`defer limiter.Close()` now runs on every shutdown path: #31's `runServers` collapses the per-listener `os.Exit(1)` calls into a buffered-error channel, so `main` returns normally on signal or listener error and the deferred limiter stop fires.

## Why middleware, not constructor injection

`ServerHandler` and `ClientHandler` know nothing about admission control; the limiter knows nothing about HTTP. Composition at the root keeps each layer single-purpose, makes `/healthz`'s exemption fall out for free (just don't wrap it), and matches the existing `EnforceHost(*domain, mux)` shape.

## Concurrency

The middleware is stateless aside from the shared `*IPRateLimiter`. Concurrency safety is fully delegated to the limiter (race-tested under `-race` per #50's model). No new locks, no new goroutines.

## Out of scope

- **Connection-count cap** (per-IP and global): different threat surface — this ticket caps *attempt rate*, not *resident connection count*. The global cap shipped in [#114](../codebase/114.md) — see [Global connection cap](connection-cap.md). The per-IP concurrency cap is still deferred (`docs/security-followups.md`).
- **CIDR-aware trusted-proxy chain**: today's flag is flat all-or-nothing (#51's design choice).
- **Multi-instance shared-state rate limiting**: v1 is single-instance; multi-instance would need Redis or equivalent.
- **Rate-limit metrics counter**: a future ticket, parallel to #58's frame-forward / grace-expiry counters.
- **Adaptive (load-aware) policy**: fixed token-bucket is sufficient for v1.

## Related

- [Codebase: #47 wiring](../codebase/47.md) — implementation notes for this ticket.
- [Codebase: #50 IP rate-limiter primitive](../codebase/50.md) — the limiter consumed here.
- [Codebase: #51 client-IP extraction helper](../codebase/51.md) — the IP source consumed here.
- [Feature: PROXY protocol v2 on the HTTPS listener](proxy-protocol-listener.md) / [Codebase: #110](../codebase/110.md) — the production source of the real client IP on Fly.
- [Threat model § DoS resistance](../../threat-model.md) — the threat-model entry this ticket converts from future hardening to v1 mitigation.
- [Log-key allowlist](../../../internal/relay/log_allowlist.go) — the closed set the deny log line conforms to.
