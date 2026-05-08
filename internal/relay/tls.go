package relay

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/acme/autocert"
)

// ErrCacheDirInsecure is returned by NewAutocertManager when CacheDir
// already exists with permissions broader than 0700. TLS private keys
// live there; the relay refuses to start with a world- or group-readable
// cache rather than silently weakening the deployment.
var ErrCacheDirInsecure = errors.New("relay: cert cache dir has insecure permissions (must be 0700)")

// NewAutocertManager returns an autocert.Manager bound to the single
// domain via HostWhitelist. The cache dir is created with mode 0700 if
// missing. If it already exists with permissions broader than 0700, the
// function returns ErrCacheDirInsecure (wrapped with the dir path).
//
// The returned manager terminates ACME http-01 challenges via its
// HTTPHandler (mount on :80) and serves certificates via TLSConfig
// (mount on :443).
func NewAutocertManager(domain, cacheDir string) (*autocert.Manager, error) {
	if domain == "" || cacheDir == "" {
		return nil, errors.New("relay: domain and cacheDir required")
	}

	info, err := os.Stat(cacheDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return nil, fmt.Errorf("relay: creating cert cache dir %s: %w", cacheDir, err)
		}
	case err != nil:
		return nil, fmt.Errorf("relay: stat cert cache dir %s: %w", cacheDir, err)
	default:
		if !info.IsDir() {
			return nil, fmt.Errorf("relay: cert cache path is not a directory: %s", cacheDir)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf("%w: %s (mode %o)", ErrCacheDirInsecure, cacheDir, mode)
		}
	}

	return &autocert.Manager{
		Cache:      autocert.DirCache(cacheDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domain),
	}, nil
}

// EnforceHost wraps next so that any request whose Host header does not
// match domain (case-insensitive, port-tolerant) receives 421
// Misdirected Request with no body. Used on the :443 handler chain so a
// client that resolves the cert via SNI but sends an unrelated Host
// header is rejected per the protocol spec's TLS section.
//
// Port-80 traffic does NOT use this wrapper: the autocert HTTPHandler
// owns its own host policy via the manager's HostPolicy.
func EnforceHost(domain string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !strings.EqualFold(host, domain) {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TLSConfig returns m.TLSConfig() with MinVersion forced to TLS 1.2.
// autocert's default config does not set MinVersion explicitly, which
// trips gosec G402; this helper centralises the override so callers
// never see an un-pinned config.
func TLSConfig(m *autocert.Manager) *tls.Config {
	cfg := m.TLSConfig()
	cfg.MinVersion = tls.VersionTLS12
	return cfg
}
