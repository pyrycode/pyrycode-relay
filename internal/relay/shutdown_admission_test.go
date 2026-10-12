package relay

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// pauseUpgradeWriter lets net/http flush the 101 response and mark the socket
// hijacked, then stops Accept from returning to the endpoint's admission call.
// It adds no production hook and holds no registry lock.
type pauseUpgradeWriter struct {
	http.ResponseWriter
	upgraded chan struct{}
	resume   <-chan struct{}
}

func (w pauseUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		close(w.upgraded)
		<-w.resume
	}
	return c, rw, err
}

func awaitShutdownEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown test synchronization timed out")
	}
}

func drainForTest(t *testing.T, reg *Registry, srv *http.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Shutdown(ctx, silentLogger(), reg, srv); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func assertGoingAway(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("close status = %d, want 1001; err=%v", code, err)
	}
}

func TestShutdown_LateUpgradeGoingAway(t *testing.T) {
	for _, name := range []string{"fresh", "grace", "phone", "phone-no-server", "phone-at-cap"} {
		t.Run(name, func(t *testing.T) {
			reg := NewRegistry()
			incumbent := &shutdownFakeConn{id: "incumbent"}
			phone := &shutdownFakeConn{id: "old-phone"}
			if name != "fresh" && name != "phone-no-server" {
				if err := reg.ClaimServer("s1", incumbent); err != nil {
					t.Fatal(err)
				}
			}
			if name == "grace" || name == "phone-at-cap" {
				if err := reg.RegisterPhone("s1", phone); err != nil {
					t.Fatal(err)
				}
			}
			if name == "grace" {
				reg.ScheduleReleaseServer("s1", time.Hour)
				t.Cleanup(func() {
					reg.mu.Lock()
					defer reg.mu.Unlock()
					if entry := reg.timers["s1"]; entry != nil {
						entry.timer.Stop()
					}
				})
			}
			var endpoint http.Handler = ServerHandler(reg, silentLogger(), time.Hour, 1024, nil)
			hdr := validHeaders("s1")
			if strings.HasPrefix(name, "phone") {
				endpoint = ClientHandler(reg, silentLogger(), 1024, 1, nil)
				hdr = validClientHeaders("s1")
			}
			upgraded, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release sync.Once
			unpause := func() { release.Do(func() { close(resume) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				endpoint.ServeHTTP(pauseUpgradeWriter{w, upgraded, resume}, r)
			}))
			defer srv.Close()
			defer unpause()
			c, _, err := dialWith(t, "ws"+strings.TrimPrefix(srv.URL, "http"), hdr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			awaitShutdownEvent(t, upgraded)
			before := reg.Snapshot()
			// The handler remains paused even after bounded shutdown completes.
			drainForTest(t, reg, srv.Config)
			unpause()
			assertGoingAway(t, c)
			awaitShutdownEvent(t, done)
			after := reg.Snapshot()
			if len(after) != len(before) {
				t.Fatalf("registry size changed after late admission: %d -> %d", len(before), len(after))
			}
			for _, old := range before {
				found := false
				for _, cur := range after {
					found = found || cur == old
				}
				if !found {
					t.Fatalf("late admission replaced %s", old.ConnID())
				}
			}
		})
	}
}

// A failed probe must still pass through the registry's admission boundary.
type drainProbeConn struct {
	shutdownFakeConn
	started chan struct{}
	resume  <-chan struct{}
}

func (c *drainProbeConn) Ping(ctx context.Context) error {
	close(c.started)
	select {
	case <-c.resume:
		return context.DeadlineExceeded
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestShutdown_ConflictProbeDuringDrain(t *testing.T) {
	reg := NewRegistry()
	resume := make(chan struct{})
	var release sync.Once
	unpause := func() { release.Do(func() { close(resume) }) }
	defer unpause()
	incumbent := &drainProbeConn{shutdownFakeConn: shutdownFakeConn{id: "incumbent"}, started: make(chan struct{}), resume: resume}
	if err := reg.ClaimServer("s1", incumbent); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	endpoint := serverHandler(reg, silentLogger(), time.Hour, time.Minute, 1024, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		endpoint.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, _, err := dialWith(t, "ws"+strings.TrimPrefix(srv.URL, "http"), validHeaders("s1"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	awaitShutdownEvent(t, incumbent.started)
	drainForTest(t, reg, srv.Config)
	unpause()
	assertGoingAway(t, c)
	awaitShutdownEvent(t, done)
	if cur, _ := reg.BinaryFor("s1"); cur != incumbent {
		t.Fatal("in-flight probe installed a binary after drain")
	}
	if closed, code, _ := incumbent.snapshot(); !closed || code != websocket.StatusGoingAway {
		t.Fatalf("incumbent close = %v/%d, want true/1001", closed, code)
	}
}

type gatedShutdownConn struct {
	shutdownFakeConn
	closing chan struct{}
	resume  <-chan struct{}
}

func (c *gatedShutdownConn) CloseWithCode(code websocket.StatusCode, reason string) {
	close(c.closing)
	<-c.resume
	c.shutdownFakeConn.CloseWithCode(code, reason)
}

func TestShutdown_AdmittedPeersGoingAway(t *testing.T) {
	reg := NewRegistry()
	mux := http.NewServeMux()
	mux.Handle("/server", ServerHandler(reg, silentLogger(), time.Hour, 1024, nil))
	mux.Handle("/phone", ClientHandler(reg, silentLogger(), 1024, 1, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	binary, _, err := dialWith(t, url+"/server", validHeaders("s1"))
	if err != nil {
		t.Fatal(err)
	}
	defer binary.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	binaryRead := make(chan error, 1)
	go func() { _, _, err := binary.Read(ctx); binaryRead <- err }()
	if err := binary.Ping(ctx); err != nil {
		t.Fatalf("binary admission ping: %v", err)
	}
	phone, _, err := dialWith(t, url+"/phone", validClientHeaders("s1"))
	if err != nil {
		t.Fatal(err)
	}
	defer phone.CloseNow()
	phoneRead := make(chan error, 1)
	go func() { _, _, err := phone.Read(ctx); phoneRead <- err }()
	if err := phone.Ping(ctx); err != nil {
		t.Fatalf("phone admission ping: %v", err)
	}
	if b, p := reg.Counts(); b != 1 || p != 1 {
		t.Fatalf("admitted counts = %d/%d, want 1/1", b, p)
	}
	resume := make(chan struct{})
	var release sync.Once
	unpause := func() { release.Do(func() { close(resume) }) }
	defer unpause()
	stuck := &gatedShutdownConn{shutdownFakeConn: shutdownFakeConn{id: "stuck"}, closing: make(chan struct{}), resume: resume}
	if err := reg.ClaimServer("stuck", stuck); err != nil {
		t.Fatal(err)
	}
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- Shutdown(ctx, silentLogger(), reg, srv.Config) }()
	awaitShutdownEvent(t, stuck.closing)
	// Both real peers must receive 1001 while the other close is blocked.
	for _, read := range []<-chan error{binaryRead, phoneRead} {
		if err := <-read; websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Fatalf("admitted peer close: %v", err)
		}
	}
	unpause()
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown admitted peers: %v", err)
	}
}

func TestShutdown_UnresponsivePeerDeadline(t *testing.T) {
	reg := NewRegistry()
	admitted, done := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := NewWSConn(c, "unread", 1024)
		defer conn.Close()
		if err := reg.ClaimServer("s1", conn); err != nil {
			return
		}
		close(admitted)
		_, _ = conn.Read(r.Context())
	}))
	defer srv.Close()
	c, _, err := dialWith(t, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	awaitShutdownEvent(t, admitted)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Shutdown(ctx, silentLogger(), reg, srv.Config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown unread peer: %v, want deadline exceeded", err)
	}
	// Reading after expiry still receives the already-sent close frame.
	assertGoingAway(t, c)
	awaitShutdownEvent(t, done)
}

func TestRegistry_DrainRejectsAdmission(t *testing.T) {
	for _, name := range []string{"fresh", "conflict", "grace", "takeover", "takeover-empty", "takeover-changed", "phone", "phone-at-cap", "phone-no-server", "phone-uncapped"} {
		t.Run(name, func(t *testing.T) {
			reg := NewRegistry()
			incumbent := &shutdownFakeConn{id: "incumbent"}
			phone := &shutdownFakeConn{id: "phone"}
			late := &shutdownFakeConn{id: "late"}
			if name != "fresh" && name != "takeover-empty" && name != "phone-no-server" {
				if err := reg.ClaimServer("s1", incumbent); err != nil {
					t.Fatal(err)
				}
				if err := reg.RegisterPhone("s1", phone); err != nil {
					t.Fatal(err)
				}
			}
			if name == "grace" {
				reg.ScheduleReleaseServer("s1", time.Hour)
				t.Cleanup(func() {
					reg.mu.Lock()
					defer reg.mu.Unlock()
					if entry := reg.timers["s1"]; entry != nil {
						entry.timer.Stop()
					}
				})
			}
			reg.mu.RLock()
			timer := reg.timers["s1"]
			reg.mu.RUnlock()
			before := reg.Snapshot()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := Shutdown(ctx, silentLogger(), reg); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				var err error
				switch {
				case name == "takeover":
					err = reg.TakeoverServer("s1", incumbent, late)
				case name == "takeover-empty":
					err = reg.TakeoverServer("s1", nil, late)
				case name == "takeover-changed":
					err = reg.TakeoverServer("s1", late, late)
				case name == "phone-uncapped":
					err = reg.RegisterPhone("s1", late)
				case strings.HasPrefix(name, "phone"):
					err = reg.RegisterPhoneCapped("s1", late, 1)
				default:
					err = reg.ClaimServer("s1", late)
				}
				if !errors.Is(err, ErrRegistryDraining) {
					t.Fatalf("admission error = %v, want ErrRegistryDraining", err)
				}
			}
			if len(reg.Snapshot()) != len(before) {
				t.Fatal("rejected admission mutated registry size")
			}
			for _, conn := range reg.Snapshot() {
				if conn == late {
					t.Fatal("late connection installed")
				}
			}
			reg.mu.RLock()
			sameTimer := reg.timers["s1"] == timer
			reg.mu.RUnlock()
			if !sameTimer {
				t.Fatal("rejected grace reclaim changed timer identity")
			}
			if closed, _, _ := late.snapshot(); closed {
				t.Fatal("registry closed rejected connection; handler owns close")
			}
		})
	}
}
