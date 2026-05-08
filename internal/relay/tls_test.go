package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/crypto/acme/autocert"
)

func TestNewAutocertManager_CreatesCacheDirWith0700(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	cache := filepath.Join(parent, "certs")

	m, err := NewAutocertManager("relay.example.com", cache)
	if err != nil {
		t.Fatalf("NewAutocertManager: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("NewAutocertManager: returned nil manager")
	}

	info, err := os.Stat(cache)
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("cache path is not a directory")
	}
	if runtime.GOOS != "windows" {
		if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
			t.Errorf("cache dir mode: got %o, want %o", got, want)
		}
	}
}

func TestNewAutocertManager_ExistingSecureDirIsNoOp(t *testing.T) {
	t.Parallel()

	cache := filepath.Join(t.TempDir(), "certs")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	sentinel := filepath.Join(cache, "sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if _, err := NewAutocertManager("relay.example.com", cache); err != nil {
		t.Fatalf("NewAutocertManager: unexpected error: %v", err)
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel file disappeared: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(cache)
		if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
			t.Errorf("cache dir mode changed: got %o, want %o", got, want)
		}
	}
}

func TestNewAutocertManager_ExistingInsecureDirRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits don't apply on Windows")
	}
	t.Parallel()

	cache := filepath.Join(t.TempDir(), "certs")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}

	_, err := NewAutocertManager("relay.example.com", cache)
	if !errors.Is(err, ErrCacheDirInsecure) {
		t.Fatalf("expected ErrCacheDirInsecure, got %v", err)
	}
}

func TestNewAutocertManager_CacheDirPathNotADirectory(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	cache := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(cache, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup writefile: %v", err)
	}

	_, err := NewAutocertManager("relay.example.com", cache)
	if err == nil {
		t.Fatal("expected error for non-directory cache path, got nil")
	}
}

func TestNewAutocertManager_RequiresDomainAndCacheDir(t *testing.T) {
	t.Parallel()

	if _, err := NewAutocertManager("", t.TempDir()); err == nil {
		t.Error("expected error for empty domain")
	}
	if _, err := NewAutocertManager("relay.example.com", ""); err == nil {
		t.Error("expected error for empty cacheDir")
	}
}

func TestNewAutocertManager_HostPolicyAcceptsConfiguredDomain(t *testing.T) {
	t.Parallel()

	m, err := NewAutocertManager("relay.example.com", filepath.Join(t.TempDir(), "c"))
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	if err := m.HostPolicy(context.Background(), "relay.example.com"); err != nil {
		t.Errorf("HostPolicy rejected configured domain: %v", err)
	}
}

func TestNewAutocertManager_HostPolicyRejectsOtherDomains(t *testing.T) {
	t.Parallel()

	m, err := NewAutocertManager("relay.example.com", filepath.Join(t.TempDir(), "c"))
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}

	cases := []struct {
		name string
		host string
	}{
		{"different domain", "evil.example.com"},
		{"uppercase mismatch", "RELAY.EXAMPLE.COM"},
		{"empty host", ""},
		{"suffix attack", "relay.example.com.evil.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.HostPolicy(context.Background(), tc.host); err == nil {
				t.Errorf("HostPolicy accepted %q; want rejection", tc.host)
			}
		})
	}
}

func TestEnforceHost(t *testing.T) {
	t.Parallel()

	const domain = "relay.example.com"

	cases := []struct {
		name       string
		host       string
		wantStatus int
		wantCalled bool
	}{
		{"match", "relay.example.com", http.StatusOK, true},
		{"match with port", "relay.example.com:8443", http.StatusOK, true},
		{"case-insensitive match", "RELAY.EXAMPLE.COM", http.StatusOK, true},
		{"different host", "evil.com", http.StatusMisdirectedRequest, false},
		{"empty host", "", http.StatusMisdirectedRequest, false},
		{"suffix attack", "relay.example.com.evil.com", http.StatusMisdirectedRequest, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "http://example/", nil)
			req.Host = tc.host
			rr := httptest.NewRecorder()

			EnforceHost(domain, next).ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rr.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Errorf("next called: got %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

func TestTLSConfig_PinsMinVersionToTLS12(t *testing.T) {
	t.Parallel()

	m := &autocert.Manager{}
	cfg := TLSConfig(m)

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion: got %x, want %x", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.GetCertificate == nil {
		t.Error("GetCertificate: nil; expected autocert wiring")
	}
}
