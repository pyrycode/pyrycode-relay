# Metrics registry — scaffolding

The relay holds a private `*prometheus.Registry` and serves it over a Prometheus-format `/metrics` handler. The registry is constructed per process; sibling tickets register counters / gauges / histograms against it. `prometheus.DefaultRegisterer` is never touched.

This page documents the scaffolding only — what the seam is, why it has the shape it does, and where the rules are recorded. The `/metrics` listener wiring lives in #60; see [Metrics listener (localhost-only)](metrics-listener.md).

The first collector to plug into the seam landed in #61: see [Connection-count gauges](connection-count-gauges.md) for the pull-based `connectionsCollector` exposing `pyrycode_relay_connected_{binaries,phones}` over `Registry.Counts()`. Sibling counter/histogram tickets (#57, #58) follow.

## API

Package `internal/relay` (`metrics.go`):

```go
func NewMetricsRegistry() *prometheus.Registry
func NewMetricsHandler(reg *prometheus.Registry) http.Handler
```

- `NewMetricsRegistry` returns a fresh `*prometheus.Registry`. Per-call construction (not a singleton) — tests instantiate as many as they need. Matches the registry-as-passive-store posture of `Registry` (#3, ADR-0003).
- `NewMetricsHandler` wraps `promhttp.HandlerFor(reg, …)` and returns `http.Handler`. Follows the factory pattern established by `NewHealthzHandler` (#10): no per-request state, no exported fields, safe for concurrent use.

The handler ships text exposition format only — `Content-Type: text/plain; version=0.0.4; charset=utf-8` (newer `prometheus/common` releases append `; escaping=<scheme>`, which is additive). OpenMetrics negotiation is disabled (`HandlerOpts.EnableOpenMetrics: false`, the library default). The on-the-wire response shape is independent of the caller's `Accept` header, matching the no-content-negotiation posture of `/healthz`.

`HandlerOpts.Registry` is set to `reg` so `promhttp`'s own self-instrumentation counters (`promhttp_metric_handler_*`) register against the relay's private registry rather than `DefaultRegisterer` — load-bearing for ADR-0008 § Scope of use even for the library's own bookkeeping.

## Seam shape — per-concern collector struct

Sibling tickets that add metrics define their own file under `internal/relay/` (e.g. `metrics_upgrade.go`) with a private collector type:

```go
type upgradeFloodMetrics struct {
    rejectsTotal *prometheus.CounterVec
}

func newUpgradeFloodMetrics(reg prometheus.Registerer) *upgradeFloodMetrics {
    m := &upgradeFloodMetrics{
        rejectsTotal: prometheus.NewCounterVec(
            prometheus.CounterOpts{Name: "relay_upgrade_rejects_total", Help: "…"},
            []string{"reason"},
        ),
    }
    reg.MustRegister(m.rejectsTotal)
    return m
}
```

The wiring site (`main.go`, #60) holds N such collectors as locals and threads each into the handler that increments it. Adding a new metric type is a greenfield file; no edits to `metrics.go` or to siblings' files.

Rejected alternatives:

- **Package-level `var`s.** Package-init can only register against a package-level singleton, which forces `NewMetricsRegistry` into a singleton. Singletons break per-test isolation that `NewRegistry()` (ADR-0003) already establishes.
- **One mega-struct.** A growing `type Metrics struct { … }` forces every sibling PR to touch the same file → guaranteed merge conflicts between concurrently-architected siblings.

## Scope of use (ADR-0008)

In bounds inside `internal/relay/` and #60's wiring:

- Construction of the private registry, counters, gauges, histograms.
- Handler factory via `promhttp.HandlerFor`.
- `prometheus/testutil` for metric assertions in tests.

Structurally out of bounds:

- `prometheus.DefaultRegisterer` is never touched. `promauto`'s default-registry shortcuts are off-limits. Enforced by `TestMetricsRegistry_NoGlobalRegistrarLeak` (snapshot-delta on `DefaultGatherer`).
- Process / Go-runtime collectors (`collectors.NewGoCollector`, `NewProcessCollector`) are not registered. A future ticket may opt in by registering them on the private registry.
- `slog` integration. `promhttp` collection errors return HTTP 500 with the library's error string; no log adapter is wired.
- Metric labels are an exfiltration channel. Sibling tickets MUST NOT emit token values, IPs (without the rate-limit policy bit), or any value covered by threat-model § Log hygiene's MUST-NOT-emit list.

## Concurrency

`promhttp.HandlerFor` is documented safe for concurrent calls; `prometheus.Registry`'s `Register` / `MustRegister` / `Gather` use internal locking. This package spawns no goroutines and owns no shared state.

## Testing

`internal/relay/metrics_test.go`, three tests, stdlib + `prometheus/common/expfmt` + `client_model/go` (both transitives of `client_golang`):

- `TestMetricsHandler_EmptyRegistry_ResponseShape` — AC gate. Status 200, content-type prefix matches the text-format literal, body decodes via `expfmt.NewDecoder` cleanly to `io.EOF`.
- `TestMetricsHandler_RegisterAndScrape_RoundTrip` — register a trivial counter, increment to 2, assert `relay_test_counter_total 2` appears in the body. Cheap seam-verification against future `client_golang` bumps.
- `TestMetricsRegistry_NoGlobalRegistrarLeak` — snapshot `DefaultGatherer.Gather()` size before and after constructing the registry+handler; the delta MUST be zero. Structural defence for ADR-0008 § Scope of use against a contributor reaching for `promauto`'s default-registry shortcuts.

## What this deliberately does NOT do

- No counters, no histograms here — siblings (#57, #58, future) own those. The first gauge collector (`connectionsCollector`) landed in #61 as a pull-based reader of `Registry.Counts()`; see [Connection-count gauges](connection-count-gauges.md).
- No listener — `/metrics` was not bound to a `mux` in #59. The listener (flag, loopback bind-address validation, `http.Server` timeouts) landed in #60 and is documented at [Metrics listener](metrics-listener.md).
- No process / Go-runtime collectors.
- No `ErrorLog` wiring from `promhttp` into `slog`.
- No content negotiation (OpenMetrics opt-in is a separate decision).

## Related

- [ADR-0008: Adopt `github.com/prometheus/client_golang`](../decisions/0008-prometheus-client-adoption.md) — alternatives, supply-chain notes, scope-of-use rules.
- [`/healthz` JSON endpoint](healthz.md) — sibling factory-shaped handler; pattern reference for `NewMetricsHandler`.
- [Threat model](../../threat-model.md) — log-hygiene posture that extends to metric label values.
