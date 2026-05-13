package relay

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPServerReadHeaderTimeoutClosesSlowPeer verifies the standard
// library contract this ticket (#35) depends on: an http.Server with a
// finite ReadHeaderTimeout closes a peer that opens TCP and dribbles
// (or never completes) the request-header block. The production
// literal (5s) is verified by code review at each wiring site
// (cmd/pyrycode-relay/main.go and internal/relay/metrics_listen.go);
// this test pins the behavioural contract at a short value so a future
// Go regression or accidental zeroing of the field would fail CI.
func TestHTTPServerReadHeaderTimeoutClosesSlowPeer(t *testing.T) {
	t.Parallel()

	const readHeaderTimeout = 150 * time.Millisecond

	var handlerCalls atomic.Int64
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		// Other timeouts deliberately generous so the test cannot
		// accidentally pass by tripping the wrong timeout.
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  5 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	go func() {
		_ = srv.Serve(ln)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Write a partial request-header block: opening line + Host header,
	// but deliberately NO terminating "\r\n\r\n". The server is now
	// waiting for the rest of the headers; ReadHeaderTimeout should fire.
	if _, err := conn.Write([]byte("GET /v1/server HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}

	// Bound our wait at ReadHeaderTimeout + slack. A correctly-behaving
	// http.Server closes the connection at the deadline; our Read returns
	// EOF (or a reset-style error on some kernels).
	slack := 500 * time.Millisecond
	if err := conn.SetReadDeadline(time.Now().Add(readHeaderTimeout + slack)); err != nil {
		t.Fatalf("conn.SetReadDeadline: %v", err)
	}

	buf := make([]byte, 64)
	start := time.Now()
	n, err := conn.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("conn.Read returned (n=%d, err=nil) for slow peer; want connection closed by server", n)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("conn.Read hit client-side deadline after %v (timeout=%v + slack=%v); server did not close the connection",
			elapsed, readHeaderTimeout, slack)
	}
	if !errors.Is(err, io.EOF) {
		// Non-EOF, non-timeout errors (e.g. ECONNRESET) are also a valid
		// signal that the server closed the connection. Log for context
		// but do not fail.
		t.Logf("conn.Read closed with non-EOF error after %v: %v", elapsed, err)
	}

	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler was invoked %d time(s); ReadHeaderTimeout did not fire before the handler ran", got)
	}
}
