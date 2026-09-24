package relay

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

func v2Header(src string) *proxyproto.Header {
	return &proxyproto.Header{
		Version:           2,
		Command:           proxyproto.PROXY,
		TransportProtocol: proxyproto.TCPv4,
		SourceAddr:        &net.TCPAddr{IP: net.ParseIP(src), Port: 40000},
		DestinationAddr:   &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8443},
	}
}

// startProxyProtoTLSServer serves h over TLS on a listener wrapped by
// NewProxyProtoListener, so the PROXY header is consumed before the TLS
// handshake exactly as in the autocert wiring.
func startProxyProtoTLSServer(t *testing.T, h http.Handler, headerTimeout time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	ln, err := NewProxyProtoListener(srv.Listener, headerTimeout)
	if err != nil {
		t.Fatalf("NewProxyProtoListener: %v", err)
	}
	srv.Listener = ln
	// Handshake failures are the expected outcome of the negative tests;
	// keep them out of the test output.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// proxyClient returns a client whose every connection (keep-alives off)
// opens with prefix(conn) before the TLS handshake.
func proxyClient(t *testing.T, srv *httptest.Server, prefix func(net.Conn) error) *http.Client {
	t.Helper()
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if err := prefix(c); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func withHeader(src string) func(net.Conn) error {
	return func(c net.Conn) error {
		_, err := v2Header(src).WriteTo(c)
		return err
	}
}

func getStatus(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestValidateProxyHeader(t *testing.T) {
	t.Parallel()
	v6 := v2Header("203.0.113.1")
	v6.TransportProtocol = proxyproto.TCPv6
	v6.SourceAddr = &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 40000}
	v6.DestinationAddr = &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 8443}

	v1 := v2Header("203.0.113.1")
	v1.Version = 1

	local := v2Header("203.0.113.1")
	local.Command = proxyproto.LOCAL

	unspec := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.UNSPEC}

	unix := &proxyproto.Header{
		Version:           2,
		Command:           proxyproto.PROXY,
		TransportProtocol: proxyproto.UnixStream,
		SourceAddr:        &net.UnixAddr{Name: "/tmp/src", Net: "unix"},
		DestinationAddr:   &net.UnixAddr{Name: "/tmp/dst", Net: "unix"},
	}

	cases := []struct {
		name    string
		h       *proxyproto.Header
		wantErr bool
	}{
		{"v2-proxy-tcp4", v2Header("203.0.113.1"), false},
		{"v2-proxy-tcp6", v6, false},
		{"v1", v1, true},
		{"local-command", local, true},
		{"unspec", unspec, true},
		{"unix-stream", unix, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProxyHeader(tc.h)
			if tc.wantErr {
				if !errors.Is(err, ErrProxyHeaderRejected) {
					t.Fatalf("err = %v, want ErrProxyHeaderRejected", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

func TestNewProxyProtoListener_RejectsNonPositiveTimeout(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := NewProxyProtoListener(ln, d); err == nil {
			t.Errorf("timeout %v: err = nil, want error", d)
		}
	}
}

// TestProxyProtoListener_SeparateBucketsPerSource is the AC1 regression:
// the rate limiter keys on the PROXY source, so exhausting one source's
// burst leaves another source untouched even though both arrive from the
// same socket peer (127.0.0.1).
func TestProxyProtoListener_SeparateBucketsPerSource(t *testing.T) {
	t.Parallel()
	sentinel := &sentinelHandler{}
	h := NewRateLimitMiddleware(newMiddlewareTestLimiter(t, 1), discardLogger(), false)(sentinel)
	srv := startProxyProtoTLSServer(t, h, 2*time.Second)

	a := proxyClient(t, srv, withHeader("203.0.113.1"))
	b := proxyClient(t, srv, withHeader("198.51.100.7"))

	if got := getStatus(t, a, srv.URL); got != http.StatusOK {
		t.Fatalf("source A first request: status %d, want 200", got)
	}
	if got := getStatus(t, a, srv.URL); got != http.StatusTooManyRequests {
		t.Fatalf("source A second request: status %d, want 429", got)
	}
	if got := getStatus(t, b, srv.URL); got != http.StatusOK {
		t.Fatalf("source B first request: status %d, want 200 (separate bucket)", got)
	}
	if got := sentinel.calls.Load(); got != 2 {
		t.Fatalf("handler calls: got %d, want 2", got)
	}
}

// TestProxyProtoListener_RejectsBadHeaderUnserved covers AC2's
// missing/malformed cases and the no-fallback rule: none of these
// connections reaches the handler under the socket peer's address.
func TestProxyProtoListener_RejectsBadHeaderUnserved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prefix func(net.Conn) error
	}{
		{"missing", func(net.Conn) error { return nil }},
		{"malformed", func(c net.Conn) error {
			// Valid v2 signature, then an invalid version/command byte.
			sig := []byte("\r\n\r\n\x00\r\nQUIT\n")
			_, err := c.Write(append(sig, 0x55, 0x11, 0x00, 0x0c))
			return err
		}},
		{"local-command", func(c net.Conn) error {
			h := v2Header("203.0.113.1")
			h.Command = proxyproto.LOCAL
			_, err := h.WriteTo(c)
			return err
		}},
		{"v1", func(c net.Conn) error {
			_, err := io.WriteString(c, "PROXY TCP4 203.0.113.1 10.0.0.1 40000 8443\r\n")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sentinel := &sentinelHandler{}
			srv := startProxyProtoTLSServer(t, sentinel, 2*time.Second)
			c := proxyClient(t, srv, tc.prefix)
			resp, err := c.Get(srv.URL)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("GET succeeded with status %d; want connection refused before serving", resp.StatusCode)
			}
			if got := sentinel.calls.Load(); got != 0 {
				t.Fatalf("handler calls: got %d, want 0", got)
			}
		})
	}
}

// TestProxyProtoListener_StalledHeader covers AC2's bounded-time case: a
// connection that never sends its header does not stop another connection
// from being accepted and served, and is itself closed once the header
// timeout expires.
func TestProxyProtoListener_StalledHeader(t *testing.T) {
	t.Parallel()
	const headerTimeout = time.Second
	sentinel := &sentinelHandler{}
	srv := startProxyProtoTLSServer(t, sentinel, headerTimeout)

	stalled, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer stalled.Close()
	stalledAt := time.Now()

	good := proxyClient(t, srv, withHeader("203.0.113.1"))
	if got := getStatus(t, good, srv.URL); got != http.StatusOK {
		t.Fatalf("request behind a stalled conn: status %d, want 200", got)
	}
	if elapsed := time.Since(stalledAt); elapsed >= headerTimeout {
		t.Fatalf("request behind a stalled conn took %v; want < header timeout %v", elapsed, headerTimeout)
	}

	_ = stalled.SetReadDeadline(time.Now().Add(headerTimeout + 3*time.Second))
	n, err := stalled.Read(make([]byte, 1))
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("stalled conn still open after header timeout; want closed by server")
	}
	if n != 0 || err == nil {
		t.Fatalf("stalled conn read: n=%d err=%v; want 0 bytes and a close", n, err)
	}
	if got := sentinel.calls.Load(); got != 1 {
		t.Fatalf("handler calls: got %d, want 1 (stalled conn must not be served)", got)
	}
}
