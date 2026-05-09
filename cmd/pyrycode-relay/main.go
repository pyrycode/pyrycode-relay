// Command pyrycode-relay routes WebSocket traffic between mobile clients and
// pyrycode binaries. It is intentionally minimal: routing by the
// x-pyrycode-server header, no payload inspection, no per-user state.
//
// See https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md
// for the wire protocol.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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

	startedAt := time.Now()
	reg := relay.NewRegistry()

	mux := http.NewServeMux()
	mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))
	mux.Handle("/v1/server", relay.ServerHandler(reg, logger, 30*time.Second))
	mux.Handle("/v1/client", relay.ClientHandler(reg, logger))

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

	logger.Info("starting", "version", Version, "mode", "autocert",
		"domain", *domain, "cert_cache", *certCache)

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

func defaultCertCache() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.pyrycode-relay/certs"
	}
	return ".pyrycode-relay/certs"
}
