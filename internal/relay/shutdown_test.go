package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// shutdownFakeConn extends the registry-test fakeConn pattern with a
// captured close code and an optional Close-blocking knob, so shutdown
// tests can assert both the per-conn code emission and the
// deadline-expiry path.
type shutdownFakeConn struct {
	id        string
	mu        sync.Mutex
	closed    bool
	closeCode websocket.StatusCode
	closeMsg  string
	blockFor  time.Duration
}

func (c *shutdownFakeConn) ConnID() string        { return c.id }
func (c *shutdownFakeConn) Send(msg []byte) error { return nil }

func (c *shutdownFakeConn) Close() {
	c.CloseWithCode(websocket.StatusNormalClosure, "")
}

func (c *shutdownFakeConn) CloseWithCode(code websocket.StatusCode, reason string) {
	if c.blockFor > 0 {
		time.Sleep(c.blockFor)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeCode = code
	c.closeMsg = reason
}

func (c *shutdownFakeConn) snapshot() (closed bool, code websocket.StatusCode, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed, c.closeCode, c.closeMsg
}

// silentLogger discards output so tests don't spam stderr.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestServer builds an *http.Server bound to a fresh loopback port and
// starts ListenAndServe in a goroutine. Returns the server and a
// listenerErr channel that receives the ListenAndServe return value.
// The returned function blocks until the listener has actually accepted
// bind (port is reachable) so tests don't race the goroutine.
func newTestServer(t *testing.T) (*http.Server, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.NewServeMux(), Addr: ln.Addr().String()}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	return srv, errCh
}

func TestShutdown_EmptyRegistry(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	srv, errCh := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := Shutdown(ctx, silentLogger(), reg); err != nil {
		t.Errorf("Shutdown empty: got %v, want nil", err)
	}

	// And with a server present but no conns.
	if err := Shutdown(ctx, silentLogger(), reg, srv); err != nil {
		t.Errorf("Shutdown with server, no conns: got %v, want nil", err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve return: got %v, want http.ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

func TestShutdown_ClosesAllConnsWithGoingAway(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	srv, errCh := newTestServer(t)

	b := &shutdownFakeConn{id: "b-1"}
	p1 := &shutdownFakeConn{id: "p-1"}
	p2 := &shutdownFakeConn{id: "p-2"}

	if err := reg.ClaimServer("s1", b); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if err := reg.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}
	if err := reg.RegisterPhone("s1", p2); err != nil {
		t.Fatalf("RegisterPhone p2: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := Shutdown(ctx, silentLogger(), reg, srv); err != nil {
		t.Errorf("Shutdown: got %v, want nil", err)
	}

	for _, c := range []*shutdownFakeConn{b, p1, p2} {
		closed, code, _ := c.snapshot()
		if !closed {
			t.Errorf("%s: not closed", c.id)
		}
		if code != websocket.StatusGoingAway {
			t.Errorf("%s: close code = %d, want %d (StatusGoingAway)", c.id, code, websocket.StatusGoingAway)
		}
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve return: got %v, want http.ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

func TestShutdown_DeadlineExpiryReturnsCtxErr(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	// Conn whose Close blocks past the deadline.
	stuck := &shutdownFakeConn{id: "stuck", blockFor: 500 * time.Millisecond}
	if err := reg.ClaimServer("s1", stuck); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}

	// A server whose listener stays open — Shutdown should still return
	// once the ctx fires, and srv.Close should force-close the listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.NewServeMux(), Addr: ln.Addr().String()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = Shutdown(ctx, silentLogger(), reg, srv)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown: got %v, want context.DeadlineExceeded", err)
	}
	// Should return promptly after deadline, well before the stuck-close completes.
	if elapsed > 300*time.Millisecond {
		t.Errorf("Shutdown took %v after 50ms deadline; expected prompt return after srv.Close()", elapsed)
	}

	// srv.Close was invoked → Serve returns http.ErrServerClosed.
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve return: got %v, want http.ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after Shutdown deadline expiry")
	}
}

func TestShutdown_CloseIdempotentOnRealWSConn(t *testing.T) {
	// Verifies that Shutdown's CloseWithCode against a *WSConn is
	// idempotent under closeOnce. The pyrycode WSConn already covers
	// this directly; this test pins the assumption that the shutdown
	// path inherits it.
	t.Parallel()

	srv := httpServerForWS(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		wsconn := NewWSConn(c, "x", 1024)

		// Race: handler's own close vs shutdown's CloseWithCode.
		var ran atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			wsconn.CloseWithCode(websocket.StatusGoingAway, "shutting down")
			ran.Add(1)
		}()
		go func() {
			defer wg.Done()
			wsconn.Close()
			ran.Add(1)
		}()
		wg.Wait()
		if ran.Load() != 2 {
			t.Errorf("expected both close calls to return; ran=%d", ran.Load())
		}
	})
	defer srv.Close()

	dial(t, srv)
}

// httpServerForWS spins up an httptest-style HTTP server with the given
// handler so the test can exercise a real *websocket.Conn close path.
func httpServerForWS(t *testing.T, h http.HandlerFunc) *http.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, Addr: ln.Addr().String()}
	go func() { _ = srv.Serve(ln) }()
	return srv
}

func dial(t *testing.T, srv *http.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+srv.Addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Read until close so the handler observes the peer disconnect.
	_, _, _ = c.Read(ctx)
	c.Close(websocket.StatusNormalClosure, "")
}
