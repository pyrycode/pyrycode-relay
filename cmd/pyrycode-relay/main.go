// Command pyrycode-relay routes WebSocket traffic between mobile clients and
// pyrycode binaries. It is intentionally minimal: routing by the
// x-pyrycode-server header, no payload inspection, no per-user state.
//
// See https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md
// for the wire protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/pyrycode/pyrycode-relay/internal/relay"
)

// drainDeadline bounds how long Shutdown will wait for in-flight WS
// close handshakes before force-closing. github.com/coder/websocket Conn.Close
// waits up to 5s per conn for the peer's reciprocal close (lessons.md);
// 10s leaves ~5s of headroom while keeping a stuck drain from delaying
// a fly machine update indefinitely. Lives at the wiring site per the
// established policy-values-in-main convention (#21, #60).
const drainDeadline = 10 * time.Second

// proxyHeaderTimeout bounds how long an accepted HTTPS connection may take
// to deliver its PROXY protocol header under --https-proxy-protocol. Fly's
// edge writes the header as soon as it opens the connection, so a real
// header never approaches this; it matches the listeners' 5s
// ReadHeaderTimeout. See
// docs/specs/architecture/110-proxy-protocol-https-listener.md.
const proxyHeaderTimeout = 5 * time.Second

// defaultMaxConnections: global cap on live /v1/server + /v1/client
// connections, sized for the 256 MB Fly machine. Derivation, charging
// every connection at the phone worst case (a binary has no outbox):
//
//   - handler, heartbeat and phone-outbox goroutines: 3 stacks, ~8 KiB
//     each once grown ≈ 24 KiB;
//   - websocket, bufio and TLS record buffers ≈ 64 KiB;
//   - phoneOutboxDepth × maxFrameBytes = 16 × 256 KiB = 4 MiB of queued
//     frames, reachable by anyone who owns both a binary and a
//     non-reading phone;
//   - one maxFrameBytes frame in flight on the read side = 256 KiB.
//
// ≈ 4.4 MiB per connection. Reserving ~56 MiB for the binary, Go runtime,
// autocert and kernel socket buffers leaves ~200 MiB; with no GOMEMLIMIT
// set, GOGC=100 lets the heap reach ~2× live before collecting, so
// ~100 MiB of live connection state ≈ 22 connections, rounded down to 20.
// Raise it with --max-connections on a bigger machine. See
// docs/specs/architecture/114-global-connection-cap.md.
const defaultMaxConnections = 20

// Version is overridden at build time via -ldflags.
var Version = "dev"

func main() {
	os.Exit(run(os.Args[1:], signalContextFor(syscall.SIGTERM, syscall.SIGINT)))
}

// signalContextFor returns a context that is cancelled when any of the
// listed signals is received. Equivalent to signal.NotifyContext; broken
// out so tests can drive the shutdown path with a synthetic cancellation
// instead of the real signal handler.
func signalContextFor(sigs ...os.Signal) context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), sigs...)
	return ctx
}

func run(args []string, sigCtx context.Context) int {
	fs := flag.NewFlagSet("pyrycode-relay", flag.ExitOnError)
	var (
		domain         = fs.String("domain", "", "Public domain for Let's Encrypt cert issuance (required unless --insecure-listen is set).")
		certCache      = fs.String("cert-cache", defaultCertCache(), "Directory for autocert's TLS certificate cache.")
		insecureListen = fs.String("insecure-listen", "", "Listen address for plain HTTP (e.g. :8080). Disables autocert; use only when fronted by a reverse proxy.")
		httpsListen    = fs.String("https-listen", ":443",
			"Listen address for the autocert TLS terminator (host:port). "+
				"Used only when --domain is set. Pass a high port like :8443 "+
				"when a substrate forwards external 443 to a high internal port "+
				"(e.g. distroless/nonroot containers without CAP_NET_BIND_SERVICE).")
		httpListen = fs.String("http-listen", ":80",
			"Listen address for the ACME HTTP-01 challenge listener (host:port). "+
				"Used only when --domain is set. Pass :8080 when a substrate "+
				"forwards external 80 to a high internal port.")
		metricsListen = fs.String("metrics-listen", "127.0.0.1:9090", "Listen address for the /metrics endpoint. Must be a loopback IP literal (e.g. 127.0.0.1:9090, [::1]:9090). Empty disables.")
		trustXFF      = fs.Bool("trust-x-forwarded-for", false,
			"Trust the X-Forwarded-For header as the source IP for per-IP rate limiting. "+
				"WARNING: enabling this without a trusted reverse proxy in front of the relay "+
				"allows clients to spoof their source IP and bypass per-IP rate limits.")
		httpsProxyProtocol = fs.Bool("https-proxy-protocol", false,
			"Require a PROXY protocol v2 header before the TLS handshake on every "+
				"--https-listen connection and use its source address as the client IP. "+
				"Pair with Fly's proxy_proto handler on the 443 service; with this set, "+
				"any connection without a valid header is closed unserved.")
		maxConnections = fs.Int("max-connections", defaultMaxConnections,
			"Global cap on live WebSocket connections across /v1/server and /v1/client. "+
				"Upgrades beyond it get HTTP 503 before the handshake. Must be positive.")
		showVersion = fs.Bool("version", false, "Print version and exit.")
	)
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Println(Version)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if *insecureListen == "" && *domain == "" {
		logger.Error("either --domain (for autocert) or --insecure-listen (for behind-proxy mode) must be set")
		return 2
	}

	// --insecure-listen short-circuits the autocert branch entirely, so
	// --http-listen / --https-listen / --https-proxy-protocol would silently
	// no-op alongside it.
	// fs.Visit walks only flags set explicitly on argv: an operator who
	// passes --http-listen=:80 (the default) alongside --insecure-listen
	// is still confused and deserves the fast-fail. See
	// docs/specs/architecture/96-autocert-configurable-listener-addrs.md
	// § Mutual-exclusion guard.
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if *insecureListen != "" && (setFlags["http-listen"] || setFlags["https-listen"] || setFlags["https-proxy-protocol"]) {
		logger.Error("refusing to start: --insecure-listen is mutually exclusive with --http-listen / --https-listen / --https-proxy-protocol",
			"fix", "use --insecure-listen alone (proxy-fronted plaintext mode), "+
				"OR use --domain with optional --http-listen / --https-listen (autocert mode); "+
				"the new flags configure the autocert listeners and have no effect in insecure mode")
		return 2
	}

	connCap, err := relay.NewConnCap(*maxConnections, logger)
	if err != nil {
		logger.Error("refusing to start: invalid --max-connections",
			"err", err,
			"value", *maxConnections,
			"fix", "pass a positive connection count, or omit the flag for the default")
		return 2
	}

	// Boot-time env-var validation runs BEFORE CheckInsecureListenInProduction
	// so that a typo like PYRYCODE_RELAY_PRODUCTION=true cannot slip through
	// IsProductionMode's silent-non-production fallback and reach the
	// insecure-listen guard with an unvalidated value. See
	// docs/specs/architecture/80-validate-env-vars-at-boot.md § Wiring order.
	if err := relay.CheckEnvConfig(os.LookupEnv); err != nil {
		var cfgErr *relay.ErrInvalidConfig
		if errors.As(err, &cfgErr) {
			logger.Error("refusing to start: invalid env-var config",
				"err", err,
				"env_var", cfgErr.Key,
				"reason", cfgErr.Reason)
		} else {
			logger.Error("refusing to start: invalid env-var config", "err", err)
		}
		return 2
	}

	// CheckSingleInstance is the deterministic backstop to the
	// docs/architecture.md § Single-instance constraint prose half (#64).
	// Refuses to start on multi-instance-capable platforms (today: Fly.io,
	// signalled by FLY_APP_NAME) unless the operator asserts single-instance
	// intent via PYRYCODE_RELAY_SINGLE_INSTANCE=1. Runs after CheckEnvConfig
	// so a typo'd bypass fails with the structured config error rather than
	// being silently treated as unset; runs before CheckInsecureListenInProduction
	// because a deploy-shape misconfiguration is more fundamental than a
	// production-mode flag misconfiguration. See
	// docs/specs/architecture/65-startup-multi-instance-check.md § Wiring.
	if err := relay.CheckSingleInstance(os.Getenv); err != nil {
		logger.Error("refusing to start: multi-instance deploy detected",
			"err", err,
			"bypass_env_var", "PYRYCODE_RELAY_SINGLE_INSTANCE",
			"fix", "set PYRYCODE_RELAY_SINGLE_INSTANCE=1 in the deploy manifest (e.g. fly.toml [env]) to assert single-instance intent; see docs/architecture.md § Single-instance constraint")
		return 2
	}

	if err := relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv); err != nil {
		logger.Error("refusing to start: production-mode misconfiguration",
			"err", err,
			"env_var", "PYRYCODE_RELAY_PRODUCTION",
			"fix", "remove --insecure-listen and set --domain, or unset PYRYCODE_RELAY_PRODUCTION")
		return 2
	}

	// CheckRunningAsRoot is the in-process backstop for the CI non-root-build
	// contract: docker run --user 0 or a missing/overridden USER directive at
	// deploy time escapes CI and would otherwise silently run the
	// internet-facing process as root. Runs before CheckCapabilities because
	// production-mode misconfiguration is a deploy-shape concern that should
	// be reported before Linux-specific runtime concerns. See
	// docs/specs/architecture/78-refuse-boot-as-root-in-production.md § Wiring.
	if err := relay.CheckRunningAsRoot(syscall.Geteuid, os.Getenv); err != nil {
		logger.Error("refusing to start: production-mode misconfiguration",
			"err", err,
			"env_var", "PYRYCODE_RELAY_PRODUCTION",
			"effective_uid", syscall.Geteuid(),
			"fix", "drop privileges before exec (e.g. Dockerfile USER directive or --user <non-zero>, kubernetes securityContext.runAsUser), or unset PYRYCODE_RELAY_PRODUCTION if the deploy is truly dev")
		return 2
	}

	if err := relay.CheckCapabilities(); err != nil {
		logger.Error("refusing to start: unexpected Linux capabilities",
			"err", err,
			"fix", "drop extra capabilities (e.g. --cap-drop=ALL --cap-add=NET_BIND_SERVICE on docker, or securityContext.capabilities on kubernetes)")
		return 2
	}

	startedAt := time.Now()
	reg := relay.NewRegistry()

	// Push is optional: unset PYRYCODE_RELAY_FCM_CREDENTIALS means the relay
	// drops every push_wake. CheckEnvConfig has already rejected an unusable
	// value, so an error here is unexpected and fails loud.
	pushWaker, err := relay.NewPushWakerFromEnv(os.LookupEnv, logger)
	if err != nil {
		logger.Error("refusing to start: cannot build the FCM push sender", "err", err)
		return 2
	}
	if pushWaker == nil {
		logger.Info("push off: PYRYCODE_RELAY_FCM_CREDENTIALS unset")
	} else {
		reg.SetPushWaker(pushWaker)
		defer pushWaker.Close()
	}

	metricsReg := relay.NewMetricsRegistry()
	relay.NewConnectionsMetrics(metricsReg, reg)
	relay.NewForwardMetrics(metricsReg, reg)
	relay.NewGraceMetrics(metricsReg, reg)
	upgradeMetrics := relay.NewUpgradeMetrics(metricsReg)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", relay.NewMetricsHandler(metricsReg))

	metricsSrv, err := relay.NewMetricsServer(*metricsListen, metricsMux)
	if err != nil {
		logger.Error("refusing to start: invalid --metrics-listen address",
			"err", err,
			"value", *metricsListen,
			"fix", "use a loopback IP literal such as 127.0.0.1:9090 or [::1]:9090, or pass --metrics-listen= to disable")
		return 2
	}

	// maxFrameBytes: 256 KiB per-frame read cap. Derivation:
	// docs/specs/architecture/29-wsconn-read-limit.md (≤50-message
	// message_chunk envelope + routing wrapper, headroom for outliers,
	// four orders of magnitude below the library's 32 MiB default).
	const maxFrameBytes int64 = 256 * 1024

	// Per-IP rate-limit policy: ~10 attempts/IP/minute steady-state, burst
	// 20. Derivation: docs/threat-model.md § DoS resistance future-hardening
	// line names ~10/min/IP and burst headroom for retry storms; the 5-min
	// eviction sweep keeps the bucket map's resident size bounded under
	// address-space scanning (docs/specs/architecture/50-ip-rate-limiter.md
	// § Adversarial walk). All three values must be positive — NewIPRateLimiter
	// panics on a zero/negative evictionInterval via time.NewTicker.
	const (
		rateLimitRefillEvery      = 6 * time.Second
		rateLimitBurst            = 20
		rateLimitEvictionInterval = 5 * time.Minute
	)
	limiter := relay.NewIPRateLimiter(rateLimitRefillEvery, rateLimitBurst, rateLimitEvictionInterval)
	// Graceful shutdown (#31) reaches limiter.Close on the clean return
	// path: a signal or a listener-goroutine error triggers relay.Shutdown
	// before run returns.
	defer limiter.Close()
	rateLimit := relay.NewRateLimitMiddleware(limiter, logger, *trustXFF)

	mux := http.NewServeMux()
	mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))
	// upgradeMetrics.WrapXxxRateLimitDeny sits OUTSIDE the rate-limit
	// middleware so it observes the middleware's HTTP 429 (the only
	// HTTP-429 source in either pipe — header-gate writes 400, success
	// is 101, WS application close codes travel inside the upgrade).
	// connCap sits INSIDE the rate limiter: a rate-limited attempt never
	// takes a slot, and its 503 never reaches the 429 observers. One
	// connCap wraps both endpoints so binaries and phones share the cap.
	mux.Handle("/v1/server", upgradeMetrics.WrapServerRateLimitDeny(rateLimit(connCap.Wrap(relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes, upgradeMetrics)))))
	// maxPhones=16 caps phones per server-id; over-cap registrations are
	// rejected with WS close 4429. Per #30 architect spec.
	mux.Handle("/v1/client", upgradeMetrics.WrapClientRateLimitDeny(rateLimit(connCap.Wrap(relay.ClientHandler(reg, logger, maxFrameBytes, 16, upgradeMetrics)))))

	if *insecureListen != "" {
		logger.Info("starting", "version", Version, "mode", "insecure", "listen", *insecureListen)
		// ReadHeaderTimeout (5s) bounds the pre-upgrade WebSocket
		// handshake window — the gap between TCP accept and full HTTP
		// request-header receipt. After header parse,
		// github.com/coder/websocket Accept hijacks the connection and
		// ReadTimeout/WriteTimeout no longer apply; post-upgrade slow
		// peers are covered by per-frame deadlines (#15) and heartbeat
		// ping/pong (#7). Caps slow-loris exposure on the internet-
		// exposed surface (docs/threat-model.md § DoS resistance).
		// Lives at each wiring site per the policy-values-in-main
		// convention (docs/PROJECT-MEMORY.md "Project-level conventions").
		srv := &http.Server{
			Addr:              *insecureListen,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		port, err := relay.ListenerPort(srv.Addr)
		if err != nil {
			logger.Error("refusing to start: invalid listener address",
				"err", err, "addr", srv.Addr)
			return 2
		}
		expected := map[uint16]struct{}{port: {}}
		actual := map[uint16]struct{}{port: {}}
		if metricsSrv != nil {
			mp, err := relay.ListenerPort(metricsSrv.Addr)
			if err != nil {
				logger.Error("refusing to start: invalid listener address",
					"err", err, "addr", metricsSrv.Addr)
				return 2
			}
			expected[mp] = struct{}{}
			actual[mp] = struct{}{}
		}
		if err := relay.CheckListenerPorts(expected, actual); err != nil {
			surplus, expectedList := listenerPortLists(expected, actual)
			logger.Error("refusing to start: unexpected listener",
				"err", err,
				"unexpected_ports", surplus,
				"expected_ports", expectedList)
			return 2
		}
		servers := []*http.Server{srv}
		if metricsSrv != nil {
			servers = append(servers, metricsSrv)
			logger.Info("starting metrics listener", "listen", metricsSrv.Addr)
		}
		return runServers(sigCtx, logger, reg, servers, func(s *http.Server) error {
			return s.ListenAndServe()
		})
	}

	mgr, err := relay.NewAutocertManager(*domain, *certCache)
	if err != nil {
		logger.Error("autocert setup failed", "err", err)
		return 1
	}

	httpsSrv := &http.Server{
		Addr:              *httpsListen,
		Handler:           relay.EnforceHost(*domain, mux),
		TLSConfig:         relay.TLSConfig(mgr),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	httpSrv := &http.Server{
		Addr: *httpListen,
		// NotFoundHandler — NOT nil. autocert.Manager.HTTPHandler(nil)
		// would 302 GET/HEAD to HTTPS; the AC requires explicit 404 for
		// non-challenge traffic.
		Handler:           mgr.HTTPHandler(http.NotFoundHandler()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	httpsPort, err := relay.ListenerPort(httpsSrv.Addr)
	if err != nil {
		logger.Error("refusing to start: invalid listener address",
			"err", err, "addr", httpsSrv.Addr)
		return 2
	}
	httpPort, err := relay.ListenerPort(httpSrv.Addr)
	if err != nil {
		logger.Error("refusing to start: invalid listener address",
			"err", err, "addr", httpSrv.Addr)
		return 2
	}
	expected := map[uint16]struct{}{httpsPort: {}, httpPort: {}}
	actual := map[uint16]struct{}{httpsPort: {}, httpPort: {}}
	if metricsSrv != nil {
		mp, err := relay.ListenerPort(metricsSrv.Addr)
		if err != nil {
			logger.Error("refusing to start: invalid listener address",
				"err", err, "addr", metricsSrv.Addr)
			return 2
		}
		expected[mp] = struct{}{}
		actual[mp] = struct{}{}
	}
	if err := relay.CheckListenerPorts(expected, actual); err != nil {
		surplus, expectedList := listenerPortLists(expected, actual)
		logger.Error("refusing to start: unexpected listener",
			"err", err,
			"unexpected_ports", surplus,
			"expected_ports", expectedList)
		return 2
	}

	logger.Info("starting", "version", Version, "mode", "autocert",
		"domain", *domain, "cert_cache", *certCache,
		"https_proxy_protocol", *httpsProxyProtocol)

	servers := []*http.Server{httpsSrv, httpSrv}
	if metricsSrv != nil {
		servers = append(servers, metricsSrv)
		logger.Info("starting metrics listener", "listen", metricsSrv.Addr)
	}
	return runServers(sigCtx, logger, reg, servers, func(s *http.Server) error {
		if s == httpsSrv {
			if !*httpsProxyProtocol {
				return s.ListenAndServeTLS("", "")
			}
			// The PROXY header precedes the TLS ClientHello, so the wrapper
			// sits under ServeTLS's tls.NewListener rather than on top.
			ln, err := net.Listen("tcp", s.Addr)
			if err != nil {
				return err
			}
			pln, err := relay.NewProxyProtoListener(ln, proxyHeaderTimeout)
			if err != nil {
				ln.Close()
				return err
			}
			return s.ServeTLS(pln, "", "")
		}
		return s.ListenAndServe()
	})
}

// runServers launches one goroutine per *http.Server invoking listen(s)
// and blocks until either sigCtx is cancelled (operator signal) or one
// of the listeners returns a non-ErrServerClosed error. Either way it
// runs relay.Shutdown(drainCtx, …) and returns:
//
//   - 0 on signal-triggered drain (clean operator action),
//   - 1 on listener-error-triggered drain (process supervisor restart).
//
// Errors are logged with their source listener's Addr; only the first
// listener error wins (buffered chan).
func runServers(sigCtx context.Context, logger *slog.Logger, reg *relay.Registry, servers []*http.Server, listen func(*http.Server) error) int {
	listenerErr := make(chan error, 1)
	for _, s := range servers {
		s := s
		go func() {
			if err := listen(s); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("listener failed", "addr", s.Addr, "err", err)
				select {
				case listenerErr <- err:
				default:
				}
			}
		}()
	}

	var triggeredByErr bool
	select {
	case <-sigCtx.Done():
		logger.Info("shutdown signal received; draining")
	case <-listenerErr:
		triggeredByErr = true
		logger.Error("listener error; draining")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), drainDeadline)
	defer cancel()
	if err := relay.Shutdown(drainCtx, logger, reg, servers...); err != nil {
		logger.Warn("drain incomplete", "err", err)
	}

	if triggeredByErr {
		return 1
	}
	return 0
}

// listenerPortLists returns the ascending-sorted surplus (actual\expected)
// and expected port slices, suitable for emission as []uint16 fields on
// the boot-refusal log line. Computed in main (not in relay) so the
// CheckListenerPorts API stays a single error return — the duplicate
// pass is cheap at boot and runs at most once.
func listenerPortLists(expected, actual map[uint16]struct{}) (surplus, expectedSorted []uint16) {
	for p := range actual {
		if _, ok := expected[p]; !ok {
			surplus = append(surplus, p)
		}
	}
	slices.Sort(surplus)
	expectedSorted = make([]uint16, 0, len(expected))
	for p := range expected {
		expectedSorted = append(expectedSorted, p)
	}
	slices.Sort(expectedSorted)
	return surplus, expectedSorted
}

func defaultCertCache() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.pyrycode-relay/certs"
	}
	return ".pyrycode-relay/certs"
}
