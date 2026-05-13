package relay

import "github.com/prometheus/client_golang/prometheus"

// forwardMetrics emits pyrycode_relay_frames_forwarded_total as a
// Prometheus CounterVec labelled by direction
// (phone_to_binary | binary_to_phone). Push-shaped: the forwarder loop is
// the source of truth, not a snapshot of registry state (contrast
// metrics_connections.go's pull-based gauges). The hook indirection
// through *Registry keeps the prometheus dep out of forward.go.
//
// The direction label values are HARD-CODED CONSTANTS — see
// docs/specs/architecture/58-frame-and-grace-counters.md § Security review
// § Tokens: cardinality is exactly 2 and neither value is
// attacker-influenced. Do not "fix" the hard-coding by reading the
// direction from request state.
type forwardMetrics struct {
	phoneToBinary prometheus.Counter
	binaryToPhone prometheus.Counter
}

// NewForwardMetrics registers the frames-forwarded counter vector against
// reg and wires the per-direction increment hooks on src. The two counters
// are pre-bound from WithLabelValues so the hook body is a single atomic
// Inc — avoids re-resolving the label set on every forwarded frame.
//
// Wiring site: cmd/pyrycode-relay/main.go, alongside NewConnectionsMetrics
// and NewGraceMetrics. Boot-time only; SetForwarderHooks is not safe to
// call after either listener starts serving.
func NewForwardMetrics(reg prometheus.Registerer, src *Registry) {
	vec := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pyrycode_relay_frames_forwarded_total",
			Help: "Number of frames forwarded by the relay, labelled by direction. Increments once per successful sink Send; per-frame error and skip paths do not increment.",
		},
		[]string{"direction"},
	)
	reg.MustRegister(vec)
	m := &forwardMetrics{
		phoneToBinary: vec.WithLabelValues("phone_to_binary"),
		binaryToPhone: vec.WithLabelValues("binary_to_phone"),
	}
	src.SetForwarderHooks(m.phoneToBinary.Inc, m.binaryToPhone.Inc)
}
