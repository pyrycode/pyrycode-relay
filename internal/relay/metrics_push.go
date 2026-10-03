package relay

import "github.com/prometheus/client_golang/prometheus"

// pushWakeOutcomeLabels holds the hard-coded outcome label value for each
// pushWakeOutcome. No label is derived from a server id, a device token or
// Firebase's reply, so the series count is fixed at five.
var pushWakeOutcomeLabels = [pushWakeOutcomeCount]string{
	pushWakeSent:             "sent",
	pushWakeUnregistered:     "unregistered",
	pushWakeSendFailed:       "send_failed",
	pushWakeTokenFetchFailed: "token_fetch_failed",
	pushWakeDropped:          "dropped",
}

// NewPushMetrics registers pyrycode_relay_push_wakes_total{outcome} against
// reg, pre-binds all five outcomes so they scrape at 0 from boot, and
// installs the increment hook on w. Boot-time only, before w serves any
// Request.
func NewPushMetrics(reg prometheus.Registerer, w *PushWaker) {
	vec := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pyrycode_relay_push_wakes_total",
			Help: "Push wakes by outcome. dropped counts rate-limit and in-flight-cap refusals; every admitted wake counts once as sent, unregistered, send_failed or token_fetch_failed when its send returns.",
		},
		[]string{"outcome"},
	)
	reg.MustRegister(vec)

	var counters [pushWakeOutcomeCount]prometheus.Counter
	for o, label := range pushWakeOutcomeLabels {
		counters[o] = vec.WithLabelValues(label)
	}
	w.setOutcomeHook(func(o pushWakeOutcome) { counters[o].Inc() })
}
