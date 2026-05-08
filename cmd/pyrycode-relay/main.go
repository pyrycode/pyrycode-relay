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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

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

	// TLS path (autocert) is intentionally not implemented yet — first
	// real ticket wires it up. Refuse to start so misconfiguration is loud.
	logger.Error("autocert TLS path not yet implemented; use --insecure-listen for now",
		"domain", *domain, "cert_cache", *certCache)
	os.Exit(2)
}

func defaultCertCache() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.pyrycode-relay/certs"
	}
	return ".pyrycode-relay/certs"
}
