package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// TestRun_InsecureMutexWithAutocertListenFlags asserts the AC3 mutex:
// --insecure-listen alongside either --http-listen or --https-listen is a
// boot-time refusal (exit 2), because the new flags configure the autocert
// listeners and silently no-op in insecure mode. The mutex check fires
// before any listener is constructed, so the failure rows need no freePort
// dance. The control row exists to lock in that the mutex does NOT fire
// when --insecure-listen runs alone.
func TestRun_InsecureMutexWithAutocertListenFlags(t *testing.T) {
	controlAddr, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	cases := []struct {
		name     string
		args     []string
		wantCode int
		// run with a cancellable context only when we expect a clean signal-
		// triggered exit; the mutex-refusal rows return before any listener
		// is bound and need no cancel.
		controlListener bool
	}{
		{
			name: "insecure+http-listen",
			args: []string{
				"--insecure-listen", ":8080",
				"--http-listen", ":80",
				"--metrics-listen", "",
			},
			wantCode: 2,
		},
		{
			name: "insecure+https-listen",
			args: []string{
				"--insecure-listen", ":8080",
				"--https-listen", ":443",
				"--metrics-listen", "",
			},
			wantCode: 2,
		},
		{
			name: "insecure+both",
			args: []string{
				"--insecure-listen", ":8080",
				"--http-listen", ":80",
				"--https-listen", ":443",
				"--metrics-listen", "",
			},
			wantCode: 2,
		},
		{
			name: "insecure+https-proxy-protocol",
			args: []string{
				"--insecure-listen", ":8080",
				"--https-proxy-protocol",
				"--metrics-listen", "",
			},
			wantCode: 2,
		},
		{
			name: "insecure-alone-control",
			args: []string{
				"--insecure-listen", controlAddr,
				"--metrics-listen", "",
			},
			wantCode:        0,
			controlListener: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.controlListener {
				sigCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				exit := make(chan int, 1)
				go func() { exit <- run(tc.args, sigCtx) }()
				if err := waitForDial(controlAddr, 3*time.Second); err != nil {
					t.Fatalf("relay did not accept connections: %v", err)
				}
				cancel()
				select {
				case got := <-exit:
					if got != tc.wantCode {
						t.Errorf("run exit code: got %d, want %d", got, tc.wantCode)
					}
				case <-time.After(drainDeadline + 2*time.Second):
					t.Fatal("run did not return after shutdown")
				}
				return
			}
			got := run(tc.args, context.Background())
			if got != tc.wantCode {
				t.Errorf("run exit code: got %d, want %d", got, tc.wantCode)
			}
		})
	}
}

// TestRun_HTTPSProxyProtocolWiring boots run in autocert mode and writes a
// plain HTTP request to the HTTPS port. Without --https-proxy-protocol,
// Go's TLS server answers it with its "HTTP request to an HTTPS server"
// 400 (the listener is unchanged); with the flag, the bytes are not a
// PROXY header, so the connection is closed with nothing written. Neither
// path reaches a TLS handshake, so autocert never contacts an ACME server.
func TestRun_HTTPSProxyProtocolWiring(t *testing.T) {
	cases := []struct {
		name       string
		flag       bool
		wantPrefix string
	}{
		{name: "flag-off", flag: false, wantPrefix: "HTTP/1.0 400"},
		{name: "flag-on", flag: true, wantPrefix: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpsAddr, err := freePort()
			if err != nil {
				t.Fatalf("freePort: %v", err)
			}
			httpAddr, err := freePort()
			if err != nil {
				t.Fatalf("freePort: %v", err)
			}
			args := []string{
				"--domain", "relay.invalid",
				"--cert-cache", t.TempDir() + "/certs",
				"--https-listen", httpsAddr,
				"--http-listen", httpAddr,
				"--metrics-listen", "",
			}
			if tc.flag {
				args = append(args, "--https-proxy-protocol")
			}

			sigCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exit := make(chan int, 1)
			go func() { exit <- run(args, sigCtx) }()
			if err := waitForDial(httpsAddr, 3*time.Second); err != nil {
				t.Fatalf("relay did not accept connections: %v", err)
			}

			conn, err := net.Dial("tcp", httpsAddr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: relay.invalid\r\n\r\n"); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := io.ReadAll(conn)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("conn neither answered nor closed within the deadline (read %q)", got)
			}
			if tc.wantPrefix == "" {
				if len(got) != 0 {
					t.Fatalf("flag on: got %q, want connection closed with no bytes", got)
				}
			} else if !strings.HasPrefix(string(got), tc.wantPrefix) {
				t.Fatalf("flag off: got %q, want prefix %q", got, tc.wantPrefix)
			}

			cancel()
			select {
			case code := <-exit:
				if code != 0 {
					t.Errorf("run exit code: got %d, want 0", code)
				}
			case <-time.After(drainDeadline + 2*time.Second):
				t.Fatal("run did not return after shutdown")
			}
		})
	}
}

// TestRun_ReclaimDuringGraceClosesPhoneWith4404 is the end-to-end
// regression for #127: a binary conn, a phone registered against it, a
// binary close, and a second binary claim within the 30s grace window
// that main.go wires. The phone must observe close status 4404 (the code
// every client already retries), then re-dial and register against the
// new binary conn, proven by a frame the new binary reads.
func TestRun_ReclaimDuringGraceClosesPhoneWith4404(t *testing.T) {
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

	const serverID = "s-e2e-127"

	b1 := dialServer(t, addr, serverID)
	defer b1.Close(websocket.StatusInternalError, "")
	b1Reads := readPump(b1)

	phone := dialPhone(t, addr, serverID)
	defer phone.Close(websocket.StatusInternalError, "")
	// A frame round-tripped through b1 proves the phone is registered
	// (the handler registers before it starts the forwarder).
	sendAndExpectAtBinary(t, phone, b1Reads, `{"v":2,"t":"probe-1"}`)

	// Binary "restarts": close b1, then reclaim the slot inside grace.
	if err := b1.Close(websocket.StatusNormalClosure, "restart"); err != nil {
		t.Fatalf("b1.Close: %v", err)
	}
	b2, b2Reads := dialServerUntilClaimed(t, addr, serverID)
	defer b2.Close(websocket.StatusInternalError, "")

	// The inherited phone must be evicted with 4404.
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	if _, _, err := phone.Read(readCtx); err == nil {
		t.Fatal("phone Read: got nil error after reclaim, want close error")
	} else if code := websocket.CloseStatus(err); code != websocket.StatusCode(4404) {
		t.Fatalf("phone close status: got %d, want 4404; err=%v", code, err)
	}

	// The phone re-dials and lands on the new binary.
	phone2 := dialPhone(t, addr, serverID)
	defer phone2.Close(websocket.StatusNormalClosure, "")
	// A reader on phone2 lets it answer the drain's close handshake
	// promptly instead of making Shutdown wait out the 5s handshake
	// timeout.
	_ = readPump(phone2)
	sendAndExpectAtBinary(t, phone2, b2Reads, `{"v":2,"t":"probe-2"}`)

	cancel()
	select {
	case <-exit:
	case <-time.After(drainDeadline + 2*time.Second):
		t.Fatal("run did not return after shutdown")
	}
}

// dialServer opens a /v1/server WebSocket for serverID.
func dialServer(t *testing.T, addr, serverID string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/server", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Pyrycode-Server":  []string{serverID},
			"X-Pyrycode-Version": []string{"0.0.0"},
			"User-Agent":         []string{"pyrycode-relay-e2e/1.0"},
		},
	})
	if err != nil {
		t.Fatalf("websocket.Dial /v1/server: %v", err)
	}
	return c
}

// wsRead is one result from readPump.
type wsRead struct {
	data []byte
	err  error
}

// readPump reads c on a dedicated goroutine and delivers every result on
// the returned channel, ending with the first error. A single long-lived
// reader is the only way to wait "is this conn still open?" without a
// Read deadline, because coder/websocket closes the conn when a Read's
// context expires. The goroutine exits once the conn is closed by the
// test's defers or the relay's drain.
func readPump(c *websocket.Conn) <-chan wsRead {
	ch := make(chan wsRead, 8)
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			ch <- wsRead{data: data, err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// dialServerUntilClaimed dials /v1/server until the claim sticks. After
// the prior binary's close handshake completes there is a short window
// before its handler defer runs ScheduleReleaseServer, during which a
// claim is refused with 4409. A conn whose pump has delivered nothing
// after a short wait is the one the registry accepted; it is returned
// together with its pump so the caller can keep reading it.
func dialServerUntilClaimed(t *testing.T, addr, serverID string) (*websocket.Conn, <-chan wsRead) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := dialServer(t, addr, serverID)
		reads := readPump(c)
		select {
		case r := <-reads:
			if r.err == nil {
				t.Fatalf("unexpected frame on a fresh /v1/server conn: %q", r.data)
			}
			if websocket.CloseStatus(r.err) != websocket.StatusCode(4409) {
				t.Fatalf("fresh /v1/server conn closed: %v", r.err)
			}
			time.Sleep(20 * time.Millisecond)
		case <-time.After(150 * time.Millisecond):
			return c, reads
		}
	}
	t.Fatal("binary could not reclaim its slot within 3s")
	return nil, nil
}

// dialPhone opens a /v1/client WebSocket for serverID.
func dialPhone(t *testing.T, addr, serverID string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/client", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Pyrycode-Server": []string{serverID},
			"X-Pyrycode-Token":  []string{"phone-token-opaque"},
			"User-Agent":        []string{"pyrycode-relay-e2e/1.0"},
		},
	})
	if err != nil {
		t.Fatalf("websocket.Dial /v1/client: %v", err)
	}
	return c
}

// sendAndExpectAtBinary writes frame on phone and asserts the binary's
// pump delivers a routing envelope carrying it.
func sendAndExpectAtBinary(t *testing.T, phone *websocket.Conn, binaryReads <-chan wsRead, frame string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := phone.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("phone.Write: %v", err)
	}
	var data []byte
	select {
	case r := <-binaryReads:
		if r.err != nil {
			t.Fatalf("binary read: %v", r.err)
		}
		data = r.data
	case <-time.After(3 * time.Second):
		t.Fatal("binary did not receive the phone's frame within 3s")
	}
	var env struct {
		ConnID string          `json:"conn_id"`
		Frame  json.RawMessage `json:"frame"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v (raw %q)", err, data)
	}
	if env.ConnID == "" || string(env.Frame) != frame {
		t.Fatalf("envelope: got conn_id=%q frame=%s, want non-empty conn_id and frame %s", env.ConnID, env.Frame, frame)
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
