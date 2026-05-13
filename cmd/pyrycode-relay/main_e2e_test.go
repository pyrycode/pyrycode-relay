package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// TestRun_SigtermClosesWSConnsWithGoingAway boots the relay via run()
// on a loopback port, opens a /v1/server WebSocket, triggers a graceful
// shutdown via the signal context, and asserts that the WS Read returns
// a close error carrying websocket.StatusGoingAway. Exits the run()
// goroutine and asserts the exit code is 0 (signal-triggered drain).
func TestRun_SigtermClosesWSConnsWithGoingAway(t *testing.T) {
	addr, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exit := make(chan int, 1)
	go func() {
		exit <- run([]string{
			"--insecure-listen", addr,
			"--metrics-listen", "",
		}, sigCtx)
	}()

	// Wait for the listener to actually be accepting connections.
	if err := waitForDial(addr, 3*time.Second); err != nil {
		t.Fatalf("relay did not accept connections: %v", err)
	}

	// Open a /v1/server WS connection.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	c, _, err := websocket.Dial(dialCtx, "ws://"+addr+"/v1/server", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Pyrycode-Server":  []string{"s-e2e-1"},
			"X-Pyrycode-Version": []string{"0.0.0"},
			"User-Agent":         []string{"pyrycode-relay-e2e/1.0"},
		},
	})
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer c.Close(websocket.StatusInternalError, "")

	// Trigger shutdown.
	cancel()

	// Next Read must surface a close error with StatusGoingAway.
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	if _, _, err := c.Read(readCtx); err == nil {
		t.Fatal("Read: got nil error after shutdown, want close error")
	} else if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Errorf("close status: got %d, want %d (StatusGoingAway); err=%v",
			code, websocket.StatusGoingAway, err)
	}

	// run() must exit within the drain deadline.
	select {
	case got := <-exit:
		if got != 0 {
			t.Errorf("run exit code: got %d, want 0", got)
		}
	case <-time.After(drainDeadline + 2*time.Second):
		t.Fatal("run did not return after shutdown")
	}
}

// TestRun_SigtermWithNoConns exits cleanly even with no live WS conns.
func TestRun_SigtermWithNoConns(t *testing.T) {
	addr, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exit := make(chan int, 1)
	go func() {
		exit <- run([]string{
			"--insecure-listen", addr,
			"--metrics-listen", "",
		}, sigCtx)
	}()

	if err := waitForDial(addr, 3*time.Second); err != nil {
		t.Fatalf("relay did not accept connections: %v", err)
	}
	cancel()

	select {
	case got := <-exit:
		if got != 0 {
			t.Errorf("run exit code: got %d, want 0", got)
		}
	case <-time.After(drainDeadline + 2*time.Second):
		t.Fatal("run did not return after shutdown")
	}

	// After shutdown the listener is no longer reachable.
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("listener still accepting connections after run returned")
	}
}

// freePort opens a loopback listener on port 0, captures the assigned
// port, closes the listener, and returns the addr string. There is an
// inherent race between close and the test re-binding it, but the
// window is short enough that this is reliable in practice.
func freePort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", err
	}
	return addr, nil
}

// waitForDial polls until a TCP dial to addr succeeds or the deadline
// elapses.
func waitForDial(addr string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("dial %s: %w", addr, errors.New("deadline elapsed"))
}
