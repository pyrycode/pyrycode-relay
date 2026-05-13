package relay

import (
	"bufio"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// UpgradeMetrics emits two Prometheus CounterVecs at the WebSocket upgrade
// call sites:
//
//   - pyrycode_relay_ws_upgrade_attempts_total{endpoint,outcome} — one
//     increment per terminal decision in ServerHandler / ClientHandler.
//     endpoint ∈ {server, client}; outcome ∈ {accept, reject_headers,
//     reject_409, reject_404, reject_429, reject_rate_limit}. All 12
//     combinations are pre-bound by NewUpgradeMetrics; three are
//     structurally unreachable (server×{reject_404, reject_429},
//     client×reject_409) but are exposed as zero-valued series so ops
//     dashboards see a stable shape.
//
//   - pyrycode_relay_register_failures_total{kind} — co-incremented with
//     the matching upgrade-outcome series. kind ∈ {no_server,
//     server_in_use, phones_at_cap, ratelimit}.
//
// Label values are HARD-CODED CONSTANTS set per event-named method.
// None are derived from request headers, request paths, or any other
// attacker-influenced input — same load-bearing constraint as
// metrics_forward.go's direction label
// (docs/specs/architecture/58-frame-and-grace-counters.md § Security
// review § Tokens). The closed-label contract is: extending either
// vector requires updating this constructor, the
// TestUpgradeMetrics_AllSixteenSeries_Exposed expected-series list,
// and the doc comment above.
//
// Methods are nil-receiver safe: callers that don't care about metrics
// pass nil to ServerHandler / ClientHandler / the Wrap* helpers, and
// every counter call is a one-line no-op.
type UpgradeMetrics struct {
	serverAccept        prometheus.Counter
	serverHeaderReject  prometheus.Counter
	serverIDConflict    prometheus.Counter
	serverRateLimitDeny prometheus.Counter

	clientAccept        prometheus.Counter
	clientHeaderReject  prometheus.Counter
	clientNoServer      prometheus.Counter
	clientPhonesAtCap   prometheus.Counter
	clientRateLimitDeny prometheus.Counter

	failServerInUse prometheus.Counter
	failNoServer    prometheus.Counter
	failPhonesAtCap prometheus.Counter
	failRateLimit   prometheus.Counter
}

// NewUpgradeMetrics registers the two counter vectors against reg and
// pre-binds all 16 label combinations via WithLabelValues so the entire
// metric surface is visible at scrape time even before the first
// upgrade attempt. The structurally unreachable cells
// (server×{reject_404, reject_429}, client×reject_409) are pre-bound to
// zero and have no method to advance them — a future error variant
// would need both an event-named method and a spec update.
//
// Wiring site: cmd/pyrycode-relay/main.go, alongside NewConnectionsMetrics
// / NewForwardMetrics / NewGraceMetrics. Boot-time only.
func NewUpgradeMetrics(reg prometheus.Registerer) *UpgradeMetrics {
	upgradeVec := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pyrycode_relay_ws_upgrade_attempts_total",
			Help: "WebSocket upgrade attempts by endpoint and outcome. One increment per terminal decision; a single request never increments more than once.",
		},
		[]string{"endpoint", "outcome"},
	)
	reg.MustRegister(upgradeVec)

	failureVec := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pyrycode_relay_register_failures_total",
			Help: "Registry-side registration failures by kind. Co-incremented with the matching pyrycode_relay_ws_upgrade_attempts_total{outcome=reject_*} series; their sum on kind equals the sum on the matching outcome value.",
		},
		[]string{"kind"},
	)
	reg.MustRegister(failureVec)

	// Pre-bind the unreachable cells so the scrape exposes them at 0.
	// They have no stored field because no event-named method points at
	// them — extending either vector requires both a method here and a
	// spec/AC update.
	_ = upgradeVec.WithLabelValues("server", "reject_404")
	_ = upgradeVec.WithLabelValues("server", "reject_429")
	_ = upgradeVec.WithLabelValues("client", "reject_409")

	return &UpgradeMetrics{
		serverAccept:        upgradeVec.WithLabelValues("server", "accept"),
		serverHeaderReject:  upgradeVec.WithLabelValues("server", "reject_headers"),
		serverIDConflict:    upgradeVec.WithLabelValues("server", "reject_409"),
		serverRateLimitDeny: upgradeVec.WithLabelValues("server", "reject_rate_limit"),

		clientAccept:        upgradeVec.WithLabelValues("client", "accept"),
		clientHeaderReject:  upgradeVec.WithLabelValues("client", "reject_headers"),
		clientNoServer:      upgradeVec.WithLabelValues("client", "reject_404"),
		clientPhonesAtCap:   upgradeVec.WithLabelValues("client", "reject_429"),
		clientRateLimitDeny: upgradeVec.WithLabelValues("client", "reject_rate_limit"),

		failServerInUse: failureVec.WithLabelValues("server_in_use"),
		failNoServer:    failureVec.WithLabelValues("no_server"),
		failPhonesAtCap: failureVec.WithLabelValues("phones_at_cap"),
		failRateLimit:   failureVec.WithLabelValues("ratelimit"),
	}
}

// ServerAccept records a successful /v1/server upgrade.
func (m *UpgradeMetrics) ServerAccept() {
	if m == nil {
		return
	}
	m.serverAccept.Inc()
}

// ServerHeaderReject records a /v1/server header-gate rejection (HTTP 400).
func (m *UpgradeMetrics) ServerHeaderReject() {
	if m == nil {
		return
	}
	m.serverHeaderReject.Inc()
}

// ServerIDConflict records a /v1/server upgrade rejected with WS close
// 4409 (server-id already claimed). Composite: increments both the
// endpoint×outcome and the register-failure kind in lockstep.
func (m *UpgradeMetrics) ServerIDConflict() {
	if m == nil {
		return
	}
	m.serverIDConflict.Inc()
	m.failServerInUse.Inc()
}

// ServerRateLimitDeny records a /v1/server attempt denied by the per-IP
// rate-limit middleware (HTTP 429). Composite: bumps both the
// endpoint×outcome and the register-failure kind=ratelimit.
func (m *UpgradeMetrics) ServerRateLimitDeny() {
	if m == nil {
		return
	}
	m.serverRateLimitDeny.Inc()
	m.failRateLimit.Inc()
}

// ClientAccept records a successful /v1/client upgrade.
func (m *UpgradeMetrics) ClientAccept() {
	if m == nil {
		return
	}
	m.clientAccept.Inc()
}

// ClientHeaderReject records a /v1/client header-gate rejection (HTTP 400).
func (m *UpgradeMetrics) ClientHeaderReject() {
	if m == nil {
		return
	}
	m.clientHeaderReject.Inc()
}

// ClientNoServer records a /v1/client upgrade rejected with WS close
// 4404 (no binary for server-id). Composite.
func (m *UpgradeMetrics) ClientNoServer() {
	if m == nil {
		return
	}
	m.clientNoServer.Inc()
	m.failNoServer.Inc()
}

// ClientPhonesAtCap records a /v1/client upgrade rejected with WS close
// 4429 (phones-per-server-id cap exceeded, per #30). Composite.
func (m *UpgradeMetrics) ClientPhonesAtCap() {
	if m == nil {
		return
	}
	m.clientPhonesAtCap.Inc()
	m.failPhonesAtCap.Inc()
}

// ClientRateLimitDeny records a /v1/client attempt denied by the per-IP
// rate-limit middleware (HTTP 429). Composite.
func (m *UpgradeMetrics) ClientRateLimitDeny() {
	if m == nil {
		return
	}
	m.clientRateLimitDeny.Inc()
	m.failRateLimit.Inc()
}

// WrapServerRateLimitDeny returns an http.Handler that wraps next with a
// status-code observer. If the wrapped pipe writes
// http.StatusTooManyRequests, ServerRateLimitDeny is invoked once per
// request. The rate-limit middleware is the only HTTP-429 source in the
// /v1/server pipe — ServerHandler writes HTTP 400 on header-gate, HTTP
// 101 on success, and WS application close codes (4409) travel inside
// the upgraded WebSocket, not as HTTP status. Nil-receiver safe; a nil
// receiver returns next unchanged.
func (m *UpgradeMetrics) WrapServerRateLimitDeny(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs := &statusObserverResponseWriter{ResponseWriter: w}
		next.ServeHTTP(obs, r)
		if obs.status == http.StatusTooManyRequests {
			m.ServerRateLimitDeny()
		}
	})
}

// WrapClientRateLimitDeny is the /v1/client counterpart of
// WrapServerRateLimitDeny.
func (m *UpgradeMetrics) WrapClientRateLimitDeny(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs := &statusObserverResponseWriter{ResponseWriter: w}
		next.ServeHTTP(obs, r)
		if obs.status == http.StatusTooManyRequests {
			m.ClientRateLimitDeny()
		}
	})
}

// statusObserverResponseWriter records the HTTP status code passed to
// WriteHeader and forwards every other ResponseWriter method to the
// underlying writer. Request-scoped; one instance per ServeHTTP call.
// Implements http.Hijacker so the downstream WS-upgrade path can still
// hijack the connection on the success branch — websocket.Accept type-
// asserts the ResponseWriter for http.Hijacker.
type statusObserverResponseWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusObserverResponseWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusObserverResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
