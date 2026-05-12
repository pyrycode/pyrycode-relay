package relay

import "github.com/prometheus/client_golang/prometheus"

// connectionsCollector emits pyrycode_relay_connected_binaries and
// pyrycode_relay_connected_phones as Prometheus gauges. The values come
// from Registry.Counts() on every scrape (pull-based); the collector
// never mutates the registry and never holds the registry lock itself.
//
// Pull-based design — rationale recorded in
// docs/specs/architecture/61-connected-gauges.md § Why pull-based:
//   - registry.go is untouched (audit surface unchanged),
//   - the grace-expiry stale-fire guard (registry.go handleGraceExpiry)
//     keeps the maps unchanged on a stale fire, so the gauge is
//     structurally unaffected — no second source of truth to keep in sync,
//   - Counts() already holds the right lock and returns a
//     snapshot-consistent (binaries, phones) pair.
type connectionsCollector struct {
	binaries *prometheus.Desc
	phones   *prometheus.Desc
	src      *Registry
}

// NewConnectionsMetrics registers the pair of connection-count gauges
// against reg, reading their values from src on each scrape. Returns
// nothing — the collector is held alive by the registerer.
//
// Wiring site: cmd/pyrycode-relay/main.go (the call lands with #60's
// listener wiring; until then the constructor is exercised only by
// metrics_connections_test.go).
//
// The gauges are label-less by design — see
// docs/specs/architecture/61-connected-gauges.md § Security review § Tokens:
// a {server="..."} label would carry the attacker-influenced
// x-pyrycode-server header value onto the metrics surface, which the
// threat model § Log hygiene forbids. Do not add labels.
func NewConnectionsMetrics(reg prometheus.Registerer, src *Registry) {
	reg.MustRegister(&connectionsCollector{
		binaries: prometheus.NewDesc(
			"pyrycode_relay_connected_binaries",
			"Number of pyrycode binary connections currently held by the relay registry.",
			nil, nil,
		),
		phones: prometheus.NewDesc(
			"pyrycode_relay_connected_phones",
			"Number of mobile client connections currently held by the relay registry, summed across all server-ids.",
			nil, nil,
		),
		src: src,
	})
}

func (c *connectionsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.binaries
	ch <- c.phones
}

func (c *connectionsCollector) Collect(ch chan<- prometheus.Metric) {
	b, p := c.src.Counts()
	ch <- prometheus.MustNewConstMetric(c.binaries, prometheus.GaugeValue, float64(b))
	ch <- prometheus.MustNewConstMetric(c.phones, prometheus.GaugeValue, float64(p))
}
