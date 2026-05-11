# ADR-0008: Adopt `github.com/prometheus/client_golang` for relay metrics

**Status:** Accepted (#59)
**Date:** 2026-05-11

## Context

`/healthz` (#10) is a point-in-time snapshot: it answers "is the relay alive
and how many things are connected right now." Production diagnosis of the
adversarial / capacity regimes the relay actually faces needs cumulative
counters — upgrade-attempt floods, header-validation rejects, register-failure
rates, frame throughput, grace-expiry rates. Those are not derivable from
snapshots without a scrape-and-diff harness; they want a real time-series
surface.

The downstream consumer is a Prometheus scraper. The chosen exposition format
must be one a stock Prometheus server accepts on a `/metrics` endpoint, with
no shim component between relay and scraper.

This is the relay's first non-stdlib direct dep since #15
(`nhooyr.io/websocket`). `docs/PROJECT-MEMORY.md` line 37 fixes the rule:
"Stdlib + `golang.org/x/crypto` + `nhooyr.io/websocket`. Direct deps arrived
in #9 and #15. Keep the dependency surface deliberate; new deps need a
justification (the ADR is the justification)." This file is that
justification.

## Alternatives considered

### 1. Hand-rolled Prometheus text exposition

The text format is documented and small. Counters are easy; gauges are easy.
Histograms and summaries (`_bucket{le=…}`, `_count`, `_sum` triplet with
ordered buckets and `+Inf` terminator) are not — escapability, label sorting,
quantile encoding, and OpenMetrics interop are each their own footgun. The
relay does not need histograms today, but the metrics surface will grow into
them (frame size distributions, grace-window latencies). Hand-rolling now
means hand-rolling the histogram code later, in a security-sensitive package,
to save one dep.

Rejected. The maintenance cost lands on the relay forever; the dep cost
is paid once and is bounded.

### 2. `github.com/VictoriaMetrics/metrics`

Drop-in alternative to `client_golang`. Smaller and fewer transitive deps —
genuinely attractive on the supply-chain axis. Rejected because:

- The ecosystem around `client_golang` (`testutil`, `promhttp`,
  `expfmt`, `collectors`) is the lingua franca; rejecting it means
  every future contributor reaches for the wrong package by reflex.
- VictoriaMetrics' library does not target full Prometheus parity on
  exposition format edge cases (it ships a deliberate subset). The
  relay does not gain anything from the subset.

### 3. `expvar` (stdlib) + exporter sidecar

`expvar` ships JSON, not text format. To feed a Prometheus scraper you bridge
through an `expvar` exporter sidecar. That is one more process to deploy and
one more failure mode to monitor (the sidecar's health is now also a
production concern). The relay is supposed to be a single binary in a
distroless container; adding an exposition sidecar undermines the deployment
posture (#32).

Rejected.

## Decision

Adopt `github.com/prometheus/client_golang` v1.x (the current stable line) as
a direct dep. Use:

- `prometheus.NewRegistry()` for the per-process registry.
- `prometheus.NewCounter` / `NewCounterVec` / `NewGauge` / `NewGaugeVec` /
  `NewHistogramVec` for metric types as sibling tickets need them.
- `promhttp.HandlerFor(reg, opts)` for the exposition handler.
- `prometheus/testutil` if and when a sibling ticket needs to assert
  counter increments under test.

## Scope of use

In bounds (`internal/relay/` and the listener's wiring in #60):

- Construction of the relay's private registry (`internal/relay/metrics.go`).
- Construction of counters / gauges / histograms by sibling tickets.
- Handler factory wrapping `promhttp.HandlerFor`.
- Test-time use of `prometheus/testutil` for metric assertions.

Out of bounds, structurally:

- **`prometheus.DefaultRegisterer` is never touched.** The relay's registry
  is constructed with `prometheus.NewRegistry()` and held privately. A
  developer who reaches for `promauto`'s default-registry shortcuts is
  reaching outside the design; code review and the spec's docstring on
  `NewMetricsRegistry` are the gates.
- **Process / Go-runtime collectors** (`collectors.NewGoCollector`,
  `collectors.NewProcessCollector`) are **not** registered in this ticket.
  They are useful but they expand the exposed surface (memory layout,
  GC pacing, file descriptor counts, command line via `process_*`)
  and the listener in #60 will be reachable from a more sensitive
  scrape position than `/healthz`. If a future ticket adds them, it
  registers them on the same private registry — never on the global default.
- **Structured logging.** The relay continues to use `slog` for logs. No
  log adapter from `client_golang` is wired in. The "Log hygiene"
  threat-model rule (#36's allowlist) does not extend to metric labels;
  metric labels are an independent low-cardinality channel and are
  governed by the threat-model § *Log hygiene* MUST-NOT-emit list
  applied to label values when sibling tickets land.
- **HTTP routing.** stdlib `net/http`, not any Prometheus router.
- **Config parsing.** stdlib `flag`, not any Prometheus config helper.

## Supply-chain notes

Direct dep: `github.com/prometheus/client_golang` v1.x.

Transitive deps the module graph will pick up (verified against
`go.sum` of comparable projects on 2026-05-11; the developer's `go mod tidy`
output is the authoritative version of this list at commit time):

- `github.com/beorn7/perks` — histogram quantile estimation.
- `github.com/cespare/xxhash/v2` — fast non-cryptographic hash for
  label-set keying.
- `github.com/munnerz/goautoneg` — content-type negotiation for
  `Accept` headers on `/metrics`.
- `github.com/prometheus/client_model` — generated protobuf for the
  Prometheus exposition model.
- `github.com/prometheus/common` — shared types across the Prometheus
  Go ecosystem; pulls `expfmt` (text/OpenMetrics encoders).
- `github.com/prometheus/procfs` — only used by process / runtime
  collectors. We do not register those (see *Scope of use*), but the
  module is transitively present.
- `google.golang.org/protobuf` — required by `client_model`.

Maintenance status: actively maintained, hosted under the Prometheus
organisation, deployed at production scale across the CNCF graduated
ecosystem. The release cadence (multiple minor releases per year) is on
the slow-and-steady side compared to web frameworks.

Vetting posture: `go.sum` digests pin each transitive at the resolved
version. Renovate (or whatever bumper lands) is responsible for keeping
the line current. `govulncheck` (already in `make lint`) covers the
expanded surface.

## Consequences

- **Dependency surface grows.** The `go.mod` `require` block gains one
  direct entry; transitives (~7 modules) are recorded in `go.sum`. This is
  a known, deliberate expansion, signed off in this ADR.
- **`govulncheck` and `gosec` now scan a wider tree.** Failures in
  `prometheus/*` modules land as `make lint` failures in CI; the
  remediation is the same as for any dep — bump within the line, or
  open an upstream issue if the bump is unavailable.
- **`make build` artifact size grows.** Indicative figure (from comparable
  Go services adopting `client_golang` for the first time): the static
  binary gains roughly 1–2 MB. Acceptable for the relay's distroless
  deployment posture (#32); the artifact is still well under the
  registry's practical ceiling.
- **The "stdlib + `x/crypto` + `nhooyr.io/websocket`" line in
  `docs/PROJECT-MEMORY.md` is now stale.** Updating that line is a
  documentation-phase concern, not the architect's; the documentation
  agent will edit it when the codebase summary for #59 lands.
- **The pattern this establishes for any next dep:** an ADR file, before
  the `go.mod` edit. The dispatcher's documentation agent then
  cross-links it from `docs/knowledge/INDEX.md` under *Decisions*. No
  silent first-import.
