package relay

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCheckLoopbackBind_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		addr        string
		wantErr     bool
		wantNonLoop bool
	}{
		{name: "ipv4 loopback default port", addr: "127.0.0.1:9090"},
		{name: "ipv4 loopback /8 high address", addr: "127.0.0.5:1234"},
		{name: "ipv6 loopback bracketed", addr: "[::1]:9090"},

		{name: "ipv4 all-interfaces rejected", addr: "0.0.0.0:9090", wantErr: true, wantNonLoop: true},
		{name: "ipv4 private rejected", addr: "192.168.1.10:9090", wantErr: true, wantNonLoop: true},
		{name: "ipv6 non-loopback rejected", addr: "[2001:db8::1]:9090", wantErr: true, wantNonLoop: true},
		{name: "hostname rejected", addr: "localhost:9090", wantErr: true, wantNonLoop: true},
		{name: "empty host rejected", addr: ":9090", wantErr: true, wantNonLoop: true},

		{name: "port 0 rejected", addr: "127.0.0.1:0", wantErr: true},
		{name: "port out of range rejected", addr: "127.0.0.1:99999", wantErr: true},
		{name: "no port rejected", addr: "127.0.0.1", wantErr: true},
		{name: "empty addr rejected", addr: "", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckLoopbackBind(tc.addr)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("CheckLoopbackBind(%q) returned error: %v", tc.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckLoopbackBind(%q) = nil; want error", tc.addr)
			}
			if tc.wantNonLoop && !errors.Is(err, ErrNonLoopbackBind) {
				t.Errorf("CheckLoopbackBind(%q) error = %v; want errors.Is(_, ErrNonLoopbackBind)", tc.addr, err)
			}
		})
	}
}

func TestNewMetricsServer_Matrix(t *testing.T) {
	t.Parallel()

	t.Run("empty addr returns (nil, nil) opt-out", func(t *testing.T) {
		t.Parallel()
		srv, err := NewMetricsServer("", http.NotFoundHandler())
		if err != nil {
			t.Fatalf("NewMetricsServer(\"\", _) error = %v; want nil", err)
		}
		if srv != nil {
			t.Fatalf("NewMetricsServer(\"\", _) srv = %+v; want nil (opt-out)", srv)
		}
	})

	t.Run("non-loopback addr returns wrapped ErrNonLoopbackBind", func(t *testing.T) {
		t.Parallel()
		srv, err := NewMetricsServer("0.0.0.0:9090", http.NotFoundHandler())
		if err == nil {
			t.Fatalf("NewMetricsServer(\"0.0.0.0:9090\", _) error = nil; want error")
		}
		if !errors.Is(err, ErrNonLoopbackBind) {
			t.Errorf("NewMetricsServer(\"0.0.0.0:9090\", _) error = %v; want errors.Is(_, ErrNonLoopbackBind)", err)
		}
		if srv != nil {
			t.Errorf("NewMetricsServer(\"0.0.0.0:9090\", _) srv = %+v; want nil", srv)
		}
	})

	t.Run("loopback addr returns configured server", func(t *testing.T) {
		t.Parallel()
		srv, err := NewMetricsServer("127.0.0.1:9090", http.NotFoundHandler())
		if err != nil {
			t.Fatalf("NewMetricsServer(\"127.0.0.1:9090\", _) error = %v; want nil", err)
		}
		if srv == nil {
			t.Fatalf("NewMetricsServer(\"127.0.0.1:9090\", _) srv = nil; want non-nil")
		}
		if srv.Addr != "127.0.0.1:9090" {
			t.Errorf("srv.Addr = %q; want %q", srv.Addr, "127.0.0.1:9090")
		}
		// Timeouts pinned to literal values: the spec says the metrics
		// listener and the public listener share a policy today but may
		// drift independently in a future ticket; assert on the values,
		// not on a shared constant.
		if got, want := srv.ReadHeaderTimeout, 5*time.Second; got != want {
			t.Errorf("ReadHeaderTimeout = %v; want %v", got, want)
		}
		if got, want := srv.ReadTimeout, 60*time.Second; got != want {
			t.Errorf("ReadTimeout = %v; want %v", got, want)
		}
		if got, want := srv.WriteTimeout, 60*time.Second; got != want {
			t.Errorf("WriteTimeout = %v; want %v", got, want)
		}
		if got, want := srv.IdleTimeout, 120*time.Second; got != want {
			t.Errorf("IdleTimeout = %v; want %v", got, want)
		}
	})
}

// TestMetricsServer_EndToEnd_HappyPath drives the wired listener over a
// real loopback TCP socket: validator + constructor + net.Listen + actual
// HTTP round-trip. This is the AC (a) anchor for #60 — httptest.NewRecorder
// is already exercised by metrics_test.go (#59); this test catches
// regressions in the path main.go runs at boot.
//
// The server is constructed with the default loopback address (which
// passes CheckLoopbackBind) and then served on an ephemeral port via
// net.Listen("tcp", "127.0.0.1:0"); http.Server.Serve ignores Addr once a
// listener is supplied, so the port-0 rule in ListenerPort does not
// conflict with the test's need for an ephemeral port.
func TestMetricsServer_EndToEnd_HappyPath(t *testing.T) {
	t.Parallel()

	reg := NewMetricsRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "relay_test_listen_counter_total",
		Help: "Sole purpose: ensure /metrics body is non-empty for the end-to-end test.",
	})
	reg.MustRegister(c)
	c.Inc()

	mux := http.NewServeMux()
	mux.Handle("/metrics", NewMetricsHandler(reg))

	srv, err := NewMetricsServer("127.0.0.1:9090", mux)
	if err != nil {
		t.Fatalf("NewMetricsServer: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(l)
	}()

	resp, err := http.Get("http://" + l.Addr().String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, textFormatPrefix) {
		t.Errorf("Content-Type = %q; want prefix %q", got, textFormatPrefix)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "relay_test_listen_counter_total 1") {
		t.Errorf("body missing registered counter; got:\n%s", body)
	}

	if err := srv.Close(); err != nil {
		t.Errorf("srv.Close: %v", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned: %v", err)
	}
}
