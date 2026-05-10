package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// startHeartbeatPair stands up an httptest server whose handler upgrades
// to WebSocket, wraps the conn in a WSConn, calls c.CloseRead so the
// library auto-pongs and processes incoming control frames (mirrors the
// production handler shape), and runs runHeartbeat in a goroutine with
// the supplied interval/timeout.
//
// Returns:
//   - the *WSConn (server side, for assertions; the heartbeat operates on it)
//   - the *websocket.Conn (client side, for the test to drive ping/pong/read)
//   - done — closes when runHeartbeat returns
//   - cancel — fires the heartbeat ctx (lets test (c) trigger clean exit)
//   - teardown — closes everything; safe to defer.
//
// The handler keeps the conn alive until teardown so individual tests
// can probe the conn's state after heartbeat exits.
func startHeartbeatPair(t *testing.T, interval, timeout time.Duration) (
	*WSConn, *websocket.Conn, <-chan struct{}, context.CancelFunc, func(),
) {
	t.Helper()

	done := make(chan struct{})
	hbCtx, cancelHB := context.WithCancel(context.Background())
	handlerStop := make(chan struct{})
	handlerReady := make(chan struct{})
	var serverWS *WSConn

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			close(handlerReady)
			return
		}
		serverWS = NewWSConn(c, "test-hb")
		// CloseRead drains control frames (so the server side auto-
		// processes incoming pongs and pings). Mirrors the production
		// /v1/server and /v1/client handler shape.
		c.CloseRead(r.Context())
		close(handlerReady)

		go func() {
			runHeartbeat(hbCtx, serverWS, interval, timeout)
			close(done)
		}()

		<-handlerStop
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	client, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		cancelHB()
		close(handlerStop)
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	<-handlerReady
	if serverWS == nil {
		cancelHB()
		close(handlerStop)
		_ = client.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		t.Fatalf("accept failed; no serverWS")
	}

	var teardownOnce bool
	teardown := func() {
		if teardownOnce {
			return
		}
		teardownOnce = true
		cancelHB()
		close(handlerStop)
		_ = client.Close(websocket.StatusNormalClosure, "")
		srv.Close()
	}
	return serverWS, client, done, cancelHB, teardown
}

// (a) Healthy peer responds to pings; heartbeat keeps the conn open
// across multiple cycles.
func TestHeartbeat_HealthyPeer_KeepsConnOpen(t *testing.T) {
	interval := 50 * time.Millisecond
	timeout := 50 * time.Millisecond
	_, client, done, _, teardown := startHeartbeatPair(t, interval, timeout)
	defer teardown()

	// Drive client reads so the library processes incoming pings and
	// auto-pongs them. Without this, heartbeat would time out on the
	// first cycle (which is exactly what test (b) below relies on).
	client.CloseRead(context.Background())

	select {
	case <-done:
		t.Fatalf("runHeartbeat exited unexpectedly during healthy cycles")
	case <-time.After(3*interval + 100*time.Millisecond):
		// Expected: heartbeat still running across ~3 ping cycles.
	}
}

// (b) Unresponsive peer (pong never arrives) → heartbeat closes the
// conn with code 1011 and reason "heartbeat timeout" within one
// timeout window.
//
// nhooyr's Conn.Close performs a close handshake with a 5s grace
// waiting for the peer's reciprocal close frame. The close frame
// itself reaches the wire immediately; runHeartbeat's exit (and so
// `done`) is gated on the handshake's grace expiring (the unresponsive
// peer never replies). Test asserts both: the close frame arrives at
// the client side promptly (proves the 1011 path), then waits longer
// for `done` to close (proves the goroutine exits without leaking).
func TestHeartbeat_UnresponsivePeer_TriggersClose(t *testing.T) {
	interval := 50 * time.Millisecond
	timeout := 100 * time.Millisecond
	_, client, done, _, teardown := startHeartbeatPair(t, interval, timeout)
	defer teardown()

	// Client deliberately does NOT read until the heartbeat has had
	// time to fire and emit its close. Reading earlier would cause the
	// client library to auto-pong incoming pings and keep the heartbeat
	// satisfied — exactly the inverse of what this test wants. Sleep
	// past one full ping cycle (interval + timeout) plus margin, then
	// read; the close frame the heartbeat queued is waiting in the
	// client's TCP buffer and the Read also auto-sends the reciprocal
	// close that lets the server's Close handshake complete.
	time.Sleep(interval + timeout + 200*time.Millisecond)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRead()
	_, _, err := client.Read(readCtx)
	if err == nil {
		t.Fatalf("client.Read returned nil error, want CloseError")
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("client.Read err = %v (%T), want *websocket.CloseError", err, err)
	}
	if ce.Code != websocket.StatusInternalError {
		t.Fatalf("close code = %d, want %d (StatusInternalError / 1011)", ce.Code, websocket.StatusInternalError)
	}
	if ce.Reason != "heartbeat timeout" {
		t.Fatalf("close reason = %q, want %q", ce.Reason, "heartbeat timeout")
	}

	// The library's Close handshake holds runHeartbeat in CloseWithCode
	// for up to ~5s waiting for the peer's reciprocal close. Wait long
	// enough for that to drain.
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatalf("runHeartbeat did not exit within 7s after emitting close; goroutine leak suspected")
	}
}

// (c) Cancelling the parent ctx → heartbeat exits cleanly without
// closing the conn itself. The handler defer (mirrored here by the
// fixture's teardown) owns the clean-close path.
func TestHeartbeat_ContextCancelled_ExitsCleanly(t *testing.T) {
	interval := 50 * time.Millisecond
	timeout := 50 * time.Millisecond
	_, client, done, cancel, teardown := startHeartbeatPair(t, interval, timeout)
	defer teardown()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("runHeartbeat did not exit after ctx cancellation")
	}

	// Conn must still be alive: a Read with a short deadline must
	// return a context-deadline-exceeded, NOT a *websocket.CloseError.
	// If heartbeat had wrongly emitted CloseWithCode on the cancel
	// path, the close frame would have landed and Read would surface
	// it as a CloseError instead of timing out.
	readCtx, cancelRead := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelRead()
	_, _, err := client.Read(readCtx)
	if err == nil {
		t.Fatalf("client.Read returned nil error, want context-deadline-exceeded")
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		t.Fatalf("client.Read got CloseError %v — heartbeat closed the conn on the cancel path (must not)", err)
	}
}
