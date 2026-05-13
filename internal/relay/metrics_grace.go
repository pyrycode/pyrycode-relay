package relay

import "github.com/prometheus/client_golang/prometheus"

// graceMetrics emits pyrycode_relay_grace_expiries_total as a scalar
// Prometheus Counter. Push-shaped: the hook fires from
// Registry.handleGraceExpiry's success branch, structurally ensuring
// stale-fire no-ops never increment. The pointer-identity guard in
// handleGraceExpiry is the load-bearing defence for the no-double-count
// invariant; the hook only runs after the guard passes.
//
// No labels. A {server="<id>"} shape would carry the attacker-influenced
// x-pyrycode-server header value onto the metrics surface, which the
// threat model § Log hygiene forbids — see
// docs/specs/architecture/58-frame-and-grace-counters.md § Security review.
type graceMetrics struct {
	counter prometheus.Counter
}

// NewGraceMetrics registers the grace-expiry counter against reg and wires
// the eviction-increment hook on src. Boot-time only;
// SetGraceExpiryHook is not safe to call after ScheduleReleaseServer has
// been called.
func NewGraceMetrics(reg prometheus.Registerer, src *Registry) {
	m := &graceMetrics{
		counter: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pyrycode_relay_grace_expiries_total",
			Help: "Number of grace-window timers that actually evicted a server-id slot. Stale fires (cancel-and-replace races) do not increment.",
		}),
	}
	reg.MustRegister(m.counter)
	src.SetGraceExpiryHook(m.counter.Inc)
}
