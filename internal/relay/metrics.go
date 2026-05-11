package relay

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewMetricsRegistry constructs a fresh *prometheus.Registry private to the
// relay. Each call returns an independent instance — tests construct as many
// as they need without process-wide collisions. The relay's wiring site
// (cmd/pyrycode-relay/main.go, landing in #60) holds exactly one registry for
// the process lifetime.
//
// Counters and gauges added by sibling tickets MUST register against the
// registry returned here (or a prometheus.Registerer derived from it), NEVER
// against prometheus.DefaultRegisterer. ADR-0008 § Scope of use fixes that
// rule; metrics_test.go's TestMetricsRegistry_NoGlobalRegistrarLeak enforces
// it structurally.
func NewMetricsRegistry() *prometheus.Registry {
	return prometheus.NewRegistry()
}

// NewMetricsHandler returns an http.Handler that serves reg in the standard
// Prometheus text exposition format ("text/plain; version=0.0.4;
// charset=utf-8"). Pattern follows NewHealthzHandler (#10): factory-shaped,
// no per-request state, safe for concurrent use.
//
// The handler does not negotiate the OpenMetrics content type
// (EnableOpenMetrics defaults to false in promhttp.HandlerOpts). Holding the
// text format stable keeps the on-the-wire response shape independent of the
// caller's Accept header, matching the relay's /healthz posture (no content
// negotiation).
//
// HandlerOpts.Registry is set to reg so promhttp's own self-instrumentation
// counters (promhttp_metric_handler_*) register against the relay's private
// registry rather than prometheus.DefaultRegisterer — that bookkeeping is
// load-bearing for the ADR-0008 § Scope of use rule "DefaultRegisterer is
// never touched" even for the library's internals.
//
// Errors during collection are propagated to the response with HTTP 500
// (promhttp's default ErrorHandling = HTTPErrorOnError); the relay does not
// currently inject a logger into the handler — if a future ticket wants
// collection errors in slog, it threads them via ErrorLog at the wiring
// site, not here.
func NewMetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
	})
}
