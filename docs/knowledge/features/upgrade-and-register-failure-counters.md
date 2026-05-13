# Upgrade-attempt and register-failure counters

Two Prometheus counter vectors at the `/v1/server` and `/v1/client` WebSocket-upgrade call sites, so an operator can distinguish "flood of malformed headers" from "flood of `4409` conflicts" from "flood of `4429` phone-cap rejections" from "flood of rate-limited clients" without log-grepping:

- `pyrycode_relay_ws_upgrade_attempts_total{endpoint, outcome}` — one increment per terminal decision in `ServerHandler` / `ClientHandler`. A single request never increments more than once.
  - `endpoint` ∈ `{server, client}`.
  - `outcome` ∈ `{accept, reject_headers, reject_409, reject_404, reject_429, reject_rate_limit}`.
- `pyrycode_relay_register_failures_total{kind}` — co-incremented in lockstep with the matching `outcome=reject_*` cell.
  - `kind` ∈ `{no_server, server_in_use, phones_at_cap, ratelimit}`.

All 16 series (12 endpoint×outcome cells + 4 kind cells) are pre-bound via `WithLabelValues` in the constructor and exposed at the scrape — including the three structurally unreachable cells (`server×reject_404`, `server×reject_429`, `client×reject_409`), which stay at 0 by construction. Ops dashboards see a stable shape rather than series flickering into existence on first occurrence.

## API

Package `internal/relay`:

```go
// metrics_upgrade.go
func NewUpgradeMetrics(reg prometheus.Registerer) *UpgradeMetrics

func (m *UpgradeMetrics) ServerAccept()
func (m *UpgradeMetrics) ServerHeaderReject()
func (m *UpgradeMetrics) ServerIDConflict()       // bumps server×reject_409 AND kind=server_in_use
func (m *UpgradeMetrics) ServerRateLimitDeny()    // bumps server×reject_rate_limit AND kind=ratelimit
func (m *UpgradeMetrics) ClientAccept()
func (m *UpgradeMetrics) ClientHeaderReject()
func (m *UpgradeMetrics) ClientNoServer()         // bumps client×reject_404 AND kind=no_server
func (m *UpgradeMetrics) ClientPhonesAtCap()      // bumps client×reject_429 AND kind=phones_at_cap
func (m *UpgradeMetrics) ClientRateLimitDeny()    // bumps client×reject_rate_limit AND kind=ratelimit

func (m *UpgradeMetrics) WrapServerRateLimitDeny(next http.Handler) http.Handler
func (m *UpgradeMetrics) WrapClientRateLimitDeny(next http.Handler) http.Handler
```

Every method is nil-receiver safe. `ServerHandler` / `ClientHandler` accept `*UpgradeMetrics` as their final parameter; existing tests pass `nil` and write nothing to either vector.

## Design — two load-bearing choices

### Why a per-handler nil-safe parameter, not hooks-on-Registry

Three of the five per-endpoint terminal sites (header-reject, success, rate-limit-deny) have **no Registry call to hook through** — the #58 hooks-on-`Registry` pattern doesn't fit. The next-best seam is an exported `*UpgradeMetrics` parameter on the two handler constructors, threaded once from `cmd/pyrycode-relay/main.go`. The parameter is nil-safe: every existing test gets a mechanical `, nil` append (7 sites total, under the 10-call-site fan-out red line).

### Why a status-code observer wrapper around the rate-limit middleware

The rate-limit middleware (#47) short-circuits before the upgrade handler runs, so the handler cannot observe its deny. Adding a callback parameter to `NewRateLimitMiddleware` would cascade across **11 call sites** (1 prod + 10 tests) — at the edit-fan-out red line. Instead, `WrapServerRateLimitDeny` / `WrapClientRateLimitDeny` stack a thin `statusObserverResponseWriter` OUTSIDE the rate-limit middleware and inspect the status code after the inner pipe returns. HTTP 429 in either pipe **is** the rate-limit deny signal:

- Header-gate rejects write 400.
- Successful upgrades write 101 (regardless of subsequent WS application close codes).
- WS close codes 4404 / 4409 / 4429 travel **inside** the upgraded WebSocket, not as HTTP status.
- The rate-limit middleware is structurally the only HTTP-429 source on either pipe.

If a future ticket introduces a second HTTP-429 source on the same pipe (e.g. capacity-based admission), the observer's filter must grow to disambiguate, or the increment must move closer to the source.

The two wrap methods are named-per-endpoint (`WrapServerRateLimitDeny` vs. `WrapClientRateLimitDeny`) rather than a single `WrapRateLimitDeny(endpoint string, next)` because the `endpoint` label value MUST be a compile-time constant — same constraint as `metrics_forward.go`'s hard-coded `direction` values.

### Composite methods as the lockstep contract

The five composite methods (`ServerIDConflict`, `ClientNoServer`, `ClientPhonesAtCap`, `ServerRateLimitDeny`, `ClientRateLimitDeny`) bump both the endpoint×outcome cell AND the matching `kind` cell in a single call. The "their sum on `kind` equals the sum on the matching `outcome` value" invariant from the help string is a property of the method, not of every caller — tests cannot accidentally advance one without the other.

## Increment placement — structurally tight to the deciding branch

Each handler increment fires **immediately after** the deciding branch is taken and **before** any error return / WS close, so "did this code path execute?" is one statement away from "did the counter increment?".

`server_endpoint.go`:

- Header-gate reject → `metrics.ServerHeaderReject()` between `http.Error` and `return`.
- `ClaimServer` `ErrServerIDConflict` (WS 4409) → `metrics.ServerIDConflict()` between the close-code write and the `logger.Info`.
- Success → `metrics.ServerAccept()` before the success log fires.
- The defensive `wsconn.Close()` branch on a non-conflict `ClaimServer` error is structurally unreachable from the current registry and **does not** increment — a future variant would be a new outcome and belongs in a follow-up ticket.

`client_endpoint.go`:

- Header-gate reject → `metrics.ClientHeaderReject()`.
- `ErrNoServer` (WS 4404) → `metrics.ClientNoServer()` (composite: also kind=`no_server`).
- `ErrPhonesAtCap` (WS 4429) → `metrics.ClientPhonesAtCap()` (composite: also kind=`phones_at_cap`).
- Success → `metrics.ClientAccept()` before the success log.
- The defensive `wsconn.Close()` non-`Is`-matched branch stays MUST-NOT-increment.

`cmd/pyrycode-relay/main.go` wires the observer OUTSIDE the rate-limit middleware:

```go
upgradeMetrics := relay.NewUpgradeMetrics(metricsReg)

mux.Handle("/v1/server",
    upgradeMetrics.WrapServerRateLimitDeny(
        rateLimit(relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes, upgradeMetrics))))
mux.Handle("/v1/client",
    upgradeMetrics.WrapClientRateLimitDeny(
        rateLimit(relay.ClientHandler(reg, logger, maxFrameBytes, 16, upgradeMetrics))))
```

## `statusObserverResponseWriter` — Hijack forwarding is load-bearing

The 30-line observer records the first `WriteHeader` code and forwards every other call to the embedded `http.ResponseWriter`. **It MUST implement `http.Hijacker`** because the wrapped pipe terminates in `websocket.Accept`, which type-asserts the writer for `http.Hijacker` to hand off the TCP connection. Without the forwarding method, the upgrade fails with HTTP 500. Any future middleware wrapper that surrounds a WS upgrade handler must also forward `Hijack`. (`http.ResponseController` in newer stdlib provides an alternative, but `nhooyr.io/websocket` at v1.8 type-asserts directly.)

The wrapper does not buffer the response body — it records one `int` per request. No new memory pressure or DoS vector.

## No attacker-influenced labels

All three label spaces (`endpoint`, `outcome`, `kind`) are hard-coded string constants set at the call site by the event-named methods. None are derived from request headers, request paths, or any other attacker-influenced input. Cardinality is exactly 16 series regardless of traffic — same load-bearing constraint as `metrics_forward.go`'s `direction` label, with the same rationale (a label whose value comes from a user-controlled header explodes cardinality and exposes the header value to anyone scraping `/metrics`). Extending either vector requires updating `NewUpgradeMetrics`, the `TestUpgradeMetrics_AllSixteenSeries_Exposed` expected-series list, and the doc comment on the type.

## Concurrency

- **No new goroutines.** Counter increments run on whichever goroutine the caller is on — the per-request `ServeHTTP` goroutine for handler increments, the same goroutine for the wrapper's post-pipe inspection.
- **No new locks.** `prometheus.Counter.Inc` is internally atomic.
- **Per-request observer state.** `statusObserverResponseWriter` is allocated per `ServeHTTP` call — no shared state across goroutines.
- **CounterVec children are pre-bound** in the constructor; each increment is a single atomic Inc on a pre-resolved child.

## Registration discipline

`NewUpgradeMetrics` accepts a `prometheus.Registerer` and `MustRegister`s both vectors against it. Per ADR-0008 § *Scope of use*, registration against `prometheus.DefaultRegisterer` is forbidden; the package-level `TestMetricsRegistry_NoGlobalRegistrarLeak` plus a local sibling `TestUpgradeMetrics_NoGlobalRegistrarLeak` enforce the rule structurally (before/after snapshot of `prometheus.DefaultGatherer.Gather()` is unchanged).

## Out of scope

- **Latency histograms** for upgrade handshake duration — not covered by this slice. A future ticket would weigh the hot-path cost.
- **A second HTTP-429 source on the upgrade pipes** — would invalidate the observer's "429 ⇒ rate-limit deny" filter. None exist today; a future contributor adding one must extend the observer or relocate the increment.
- **Connection-attempt rate metrics by source IP** — would carry attacker-influenced label values, ruled out by threat model § Log hygiene.

## Related

- [Metrics registry (scaffolding)](metrics-registry.md) — the seam these counters plug into (#59).
- [Metrics listener (localhost-only)](metrics-listener.md) — the `/metrics` surface that serves them (#60).
- [Connection-count gauges](connection-count-gauges.md) — pull-based sibling collector (#61).
- [Frame-forwarded and grace-expiry counters](frame-and-grace-counters.md) — push-shaped sibling using hooks-on-`Registry` instead of a handler parameter (#58); same pre-bind pattern, different seam.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) — the deny path observed by `WrapXxxRateLimitDeny` (#47).
- [`/v1/server` WS upgrade](server-endpoint.md) — host of the server-side increments (#26, #57).
- [`/v1/client` WS upgrade](client-endpoint.md) — host of the client-side increments, including the `4429` phones-at-cap branch from #30.
- [ADR-0008: Prometheus client adoption](../decisions/0008-prometheus-client-adoption.md) — scope-of-use rules; closed-label cardinality budget; `DefaultRegisterer` ban.
- [Threat model](../../threat-model.md) — log-hygiene rule extended to metric labels.
