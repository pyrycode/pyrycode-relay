// Command pyrycode-relay routes WebSocket traffic between mobile clients and
// pyrycode binaries. It is intentionally minimal: routing by the
// x-pyrycode-server header, no payload inspection, no per-user state.
//
// See https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md
// for the wire protocol.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/pyrycode/pyrycode-relay/internal/relay"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

func main() {
	var (
		domain         = flag.String("domain", "", "Public domain for Let's Encrypt cert issuance (required unless --insecure-listen is set).")
		certCache      = flag.String("cert-cache", defaultCertCache(), "Directory for autocert's TLS certificate cache.")
		insecureListen = flag.String("insecure-listen", "", "Listen address for plain HTTP (e.g. :8080). Disables autocert; use only when fronted by a reverse proxy.")
		metricsListen  = flag.String("metrics-listen", "127.0.0.1:9090", "Listen address for the /metrics endpoint. Must be a loopback IP literal (e.g. 127.0.0.1:9090, [::1]:9090). Empty disables.")
		showVersion    = flag.Bool("version", false, "Print version and exit.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if *insecureListen == "" && *domain == "" {
		logger.Error("either --domain (for autocert) or --insecure-listen (for behind-proxy mode) must be set")
		os.Exit(2)
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
		os.Exit(2)
	}

	if err := relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv); err != nil {
		logger.Error("refusing to start: production-mode misconfiguration",
			"err", err,
			"env_var", "PYRYCODE_RELAY_PRODUCTION",
			"fix", "remove --insecure-listen and set --domain, or unset PYRYCODE_RELAY_PRODUCTION")
		os.Exit(2)
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
		os.Exit(2)
	}

	if err := relay.CheckCapabilities(); err != nil {
		logger.Error("refusing to start: unexpected Linux capabilities",
			"err", err,
			"fix", "drop extra capabilities (e.g. --cap-drop=ALL --cap-add=NET_BIND_SERVICE on docker, or securityContext.capabilities on kubernetes)")
		os.Exit(2)
	}

	startedAt := time.Now()
	reg := relay.NewRegistry()

	metricsReg := relay.NewMetricsRegistry()
	relay.NewConnectionsMetrics(metricsReg, reg)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", relay.NewMetricsHandler(metricsReg))

	metricsSrv, err := relay.NewMetricsServer(*metricsListen, metricsMux)
	if err != nil {
		logger.Error("refusing to start: invalid --metrics-listen address",
			"err", err,
			"value", *metricsListen,
			"fix", "use a loopback IP literal such as 127.0.0.1:9090 or [::1]:9090, or pass --metrics-listen= to disable")
		os.Exit(2)
	}

	// maxFrameBytes: 256 KiB per-frame read cap. Derivation:
	// docs/specs/architecture/29-wsconn-read-limit.md (≤50-message
	// message_chunk envelope + routing wrapper, headroom for outliers,
	// four orders of magnitude below nhooyr's 32 MiB default).
	const maxFrameBytes int64 = 256 * 1024

	mux := http.NewServeMux()
	mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))
	mux.Handle("/v1/server", relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes))
	// maxPhones=16 caps phones per server-id; over-cap registrations are
	// rejected with WS close 4429. Per #30 architect spec.
	mux.Handle("/v1/client", relay.ClientHandler(reg, logger, maxFrameBytes, 16))

	if *insecureListen != "" {
		logger.Info("starting", "version", Version, "mode", "insecure", "listen", *insecureListen)
		srv := &http.Server{
			Addr:              *insecureListen,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		port, err := relay.ListenerPort(srv.Addr)
		if err != nil {
			logger.Error("refusing to start: invalid listener address",
				"err", err, "addr", srv.Addr)
			os.Exit(2)
		}
		expected := map[uint16]struct{}{port: {}}
		actual := map[uint16]struct{}{port: {}}
		if metricsSrv != nil {
			mp, err := relay.ListenerPort(metricsSrv.Addr)
			if err != nil {
				logger.Error("refusing to start: invalid listener address",
					"err", err, "addr", metricsSrv.Addr)
				os.Exit(2)
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
			os.Exit(2)
		}
		if metricsSrv != nil {
			logger.Info("starting metrics listener", "listen", metricsSrv.Addr)
			go func() {
				if err := metricsSrv.ListenAndServe(); err != nil {
					logger.Error("metrics listener failed", "err", err)
					os.Exit(1)
				}
			}()
		}
		if err := srv.ListenAndServe(); err != nil {
			logger.Error("listen failed", "err", err)
			os.Exit(1)
		}
		return
	}

	mgr, err := relay.NewAutocertManager(*domain, *certCache)
	if err != nil {
		logger.Error("autocert setup failed", "err", err)
		os.Exit(1)
	}

	httpsSrv := &http.Server{
		Addr:              ":443",
		Handler:           relay.EnforceHost(*domain, mux),
		TLSConfig:         relay.TLSConfig(mgr),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	httpSrv := &http.Server{
		Addr: ":80",
		// NotFoundHandler — NOT nil. autocert.Manager.HTTPHandler(nil)
		// would 302 GET/HEAD to HTTPS; the AC requires explicit 404 for
		// non-challenge traffic.
		Handler:           mgr.HTTPHandler(http.NotFoundHandler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	httpsPort, err := relay.ListenerPort(httpsSrv.Addr)
	if err != nil {
		logger.Error("refusing to start: invalid listener address",
			"err", err, "addr", httpsSrv.Addr)
		os.Exit(2)
	}
	httpPort, err := relay.ListenerPort(httpSrv.Addr)
	if err != nil {
		logger.Error("refusing to start: invalid listener address",
			"err", err, "addr", httpSrv.Addr)
		os.Exit(2)
	}
	expected := map[uint16]struct{}{443: {}, 80: {}}
	actual := map[uint16]struct{}{httpsPort: {}, httpPort: {}}
	if metricsSrv != nil {
		mp, err := relay.ListenerPort(metricsSrv.Addr)
		if err != nil {
			logger.Error("refusing to start: invalid listener address",
				"err", err, "addr", metricsSrv.Addr)
			os.Exit(2)
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
		os.Exit(2)
	}

	logger.Info("starting", "version", Version, "mode", "autocert",
		"domain", *domain, "cert_cache", *certCache)

	if metricsSrv != nil {
		logger.Info("starting metrics listener", "listen", metricsSrv.Addr)
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil {
				logger.Error("metrics listener failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil {
			logger.Error("http-01 listener failed", "err", err)
			os.Exit(1)
		}
	}()

	if err := httpsSrv.ListenAndServeTLS("", ""); err != nil {
		logger.Error("https listener failed", "err", err)
		os.Exit(1)
	}
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
