package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/pyrycode/pyrycode-relay/internal/relay"
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

	// A pong proves the handler has reached its read loop after ClaimServer.
	// Dial success alone only proves the HTTP upgrade completed. Keep a reader
	// active to process the pong, then reuse its existing three-second deadline
	// to observe the shutdown close without another application handshake.
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	readErr := make(chan error, 1)
	go func() {
		_, _, err := c.Read(readCtx)
		readErr <- err
	}()
	if err := c.Ping(readCtx); err != nil {
		t.Fatalf("admission ping: %v", err)
	}

	// Trigger shutdown after observable admission.
	cancel()

	if err := <-readErr; err == nil {
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

// TestRun_MaxConnectionsNonPositiveRefusesStart: a zero or negative
// --max-connections is a boot-time refusal (exit 2), not an uncapped relay.
func TestRun_MaxConnectionsNonPositiveRefusesStart(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		t.Run(v, func(t *testing.T) {
			got := run([]string{
				"--insecure-listen", "127.0.0.1:0",
				"--metrics-listen", "",
				"--max-connections", v,
			}, context.Background())
			if got != 2 {
				t.Errorf("run exit code: got %d, want 2", got)
			}
		})
	}
}

// TestRun_ConnCapGatesUpgradesNotHealthz fills two global slots from
// distinct sources: both upgrade routes answer 503 for a third source, and
// /healthz still answers 200.
func TestRun_ConnCapGatesUpgradesNotHealthz(t *testing.T) {
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
			"--max-connections", "2",
			"--max-connections-per-source", "1",
			"--trust-x-forwarded-for",
		}, sigCtx)
	}()
	if err := waitForDial(addr, 3*time.Second); err != nil {
		t.Fatalf("relay did not accept connections: %v", err)
	}

	for i := 0; i < 2; i++ {
		hdr := http.Header{"X-Pyrycode-Server": []string{fmt.Sprintf("s-cap-%d", i)}, "X-Pyrycode-Version": []string{"0.0.0"}, "User-Agent": []string{"relay-cap-test"}, "X-Forwarded-For": []string{fmt.Sprintf("192.0.2.%d", i+1)}}
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		bin, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/server", &websocket.DialOptions{HTTPHeader: hdr})
		stop()
		if err != nil {
			t.Fatal(err)
		}
		defer bin.CloseNow()
		_ = readPump(bin)
		if i == 0 {
			// With one global slot still free, the explicit source allowance
			// refuses both routes while another source reaches the header gate.
			for _, path := range []string{"/v1/server", "/v1/client"} {
				for source, want := range map[string]int{"192.0.2.1": 503, "192.0.2.3": 400} {
					req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("X-Forwarded-For", source)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != want {
						t.Fatalf("source %s %s: status=%d want=%d", source, path, resp.StatusCode, want)
					}
				}
			}
		}
	}

	for path, want := range map[string]int{
		"/v1/client": http.StatusServiceUnavailable,
		"/v1/server": http.StatusServiceUnavailable,
		"/healthz":   http.StatusOK,
	} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", path, resp.StatusCode, want)
		}
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
}

// This budget covers the configured drain, conservative subsequent push
// cleanup, and local limiter/scheduling/exit headroom. It is not a new deadline
// in production; Fly's best-effort stop allowance must exceed it by five seconds.
const completeProcessBudget = drainDeadline + 10*time.Second + 5*time.Second

type shutdownProcessEvent struct {
	Stage    string             `json:"stage"`
	Waiters  int                `json:"waiters,omitempty"`
	OAuth    int64              `json:"oauth,omitempty"`
	Other    int64              `json:"other,omitempty"`
	Outcomes map[string]float64 `json:"outcomes,omitempty"`
}

// shutdownOAuthTransport allows only the local synthetic OAuth URL. Holding
// its return AFTER network cancellation tests the in-child authentication join.
type shutdownOAuthTransport struct {
	url      string
	base     *http.Transport
	release  chan struct{}
	finished chan struct{}
	hits     atomic.Int64
	other    atomic.Int64
	report   func(shutdownProcessEvent)
}

func (s *shutdownOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != s.url {
		s.other.Add(1)
		return nil, errors.New("unexpected outbound request refused")
	}
	s.hits.Add(1)
	resp, err := s.base.RoundTrip(r)
	if !errors.Is(r.Context().Err(), context.Canceled) {
		s.report(shutdownProcessEvent{Stage: "authentication-not-cancelled"})
		return resp, err
	}
	s.report(shutdownProcessEvent{Stage: "authentication-cancelled"})
	<-s.release
	close(s.finished)
	return resp, err
}

// TestRun_ProcessSIGTERMHelper runs only in the re-executed test process. All
// observation happens around production run; nothing closes its waker for it.
func TestRun_ProcessSIGTERMHelper(t *testing.T) {
	if os.Getenv("RELAY_SHUTDOWN_CHILD") != "1" {
		return
	}
	var reportMu sync.Mutex
	encoder := json.NewEncoder(os.Stdout)
	report := func(e shutdownProcessEvent) {
		reportMu.Lock()
		defer reportMu.Unlock()
		if encoder.Encode(e) != nil {
			os.Exit(3)
		}
	}
	transport := &shutdownOAuthTransport{
		url:     os.Getenv("RELAY_SHUTDOWN_OAUTH"),
		base:    &http.Transport{Proxy: nil},
		release: make(chan struct{}), finished: make(chan struct{}), report: report,
	}
	http.DefaultTransport = transport
	type bootState struct {
		w       *relay.PushWaker
		metrics *prometheus.Registry
	}
	boot := make(chan bootState, 1)
	var waker *relay.PushWaker
	newPushWakerFromEnv = func(lookup func(string) (string, bool), logger *slog.Logger) (*relay.PushWaker, error) {
		w, err := relay.NewPushWakerFromEnv(lookup, logger)
		waker = w
		return w, err
	}
	newMetricsRegistry = func() *prometheus.Registry {
		r := relay.NewMetricsRegistry()
		boot <- bootState{waker, r}
		return r
	}
	ready := make(chan (<-chan struct{}), 1)
	observed := make(chan bootState, 1)
	go func() {
		b := <-boot
		observed <- b
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			n, done := b.w.RefreshState()
			if n == 6 && done != nil {
				report(shutdownProcessEvent{Stage: "joined", Waiters: n})
				break
			}
			select {
			case <-ticker.C:
			case <-timer.C:
				report(shutdownProcessEvent{Stage: "waiters-timeout"})
				os.Exit(3)
			}
		}
		control := json.NewDecoder(os.Stdin)
		for {
			var command string
			if control.Decode(&command) != nil {
				close(transport.release)
				return
			}
			switch command {
			case "check":
				n, done := b.w.RefreshState()
				if n != 6 || done == nil {
					report(shutdownProcessEvent{Stage: "not-held"})
					os.Exit(3)
				}
				select {
				case <-done:
					report(shutdownProcessEvent{Stage: "not-held"})
					os.Exit(3)
				default:
				}
				ready <- done
				report(shutdownProcessEvent{Stage: "ready", Waiters: n})
			case "release":
				close(transport.release)
				return
			}
		}
	}()
	code := run([]string{"--insecure-listen", os.Getenv("RELAY_SHUTDOWN_ADDR"), "--metrics-listen", ""},
		signalContextFor(syscall.SIGTERM, syscall.SIGINT))
	// Sending this first makes an early return observable even if assertions fail.
	report(shutdownProcessEvent{Stage: "run-returned"})
	var refreshDone <-chan struct{}
	select {
	case refreshDone = <-ready:
	default:
		os.Exit(3)
	}
	select {
	case <-transport.finished:
	default:
		os.Exit(3)
	}
	select {
	case <-refreshDone:
	default:
		os.Exit(3)
	}
	b := <-observed
	families, err := b.metrics.Gather()
	if err != nil {
		os.Exit(3)
	}
	outcomes := make(map[string]float64)
	for _, family := range families {
		if family.GetName() == "pyrycode_relay_push_wakes_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "outcome" {
						outcomes[label.GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	report(shutdownProcessEvent{Stage: "cleanup-complete", OAuth: transport.hits.Load(), Other: transport.other.Load(), Outcomes: outcomes})
	os.Exit(code)
}

func TestRun_ProcessSIGTERMCompleteShutdown(t *testing.T) {
	if completeProcessBudget != 25*time.Second {
		t.Fatal("review the complete-process budget and Fly margin")
	}
	addr, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	var oauthHits atomic.Int64
	authStarted := make(chan struct{}, 8)
	authCancelled := make(chan struct{}, 8)
	unblockOAuth := make(chan struct{})
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		oauthHits.Add(1)
		authStarted <- struct{}{}
		select {
		case <-r.Context().Done():
			authCancelled <- struct{}{}
		case <-unblockOAuth:
		}
	}))
	defer oauth.Close()
	defer close(unblockOAuth)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "synthetic@example.invalid",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"token_uri":   oauth.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRun_ProcessSIGTERMHelper$")
	cmd.Env = []string{"RELAY_SHUTDOWN_CHILD=1", "RELAY_SHUTDOWN_ADDR=" + addr,
		"RELAY_SHUTDOWN_OAUTH=" + oauth.URL, "PYRYCODE_RELAY_FCM_CREDENTIALS=" + string(credentials)}
	stderr, err := os.CreateTemp(t.TempDir(), "child-stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	cmd.Stderr = stderr
	// Own the pipe so cmd.Wait cannot close it before final events are read.
	output, childOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer childOutput.Close()
	cmd.Stdout = childOutput
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childOutput.Close()
	decoderDone := make(chan struct{})
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		input.Close()
		cancel()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("child was not reaped")
		}
		<-decoderDone
		if t.Failed() {
			data, _ := os.ReadFile(stderr.Name())
			t.Logf("child diagnostics:\n%s", data)
		}
	}()
	events := make(chan shutdownProcessEvent, 16)
	go func() {
		defer close(decoderDone)
		defer close(events)
		d := json.NewDecoder(output)
		for {
			var e shutdownProcessEvent
			if d.Decode(&e) != nil {
				return
			}
			events <- e
		}
	}()
	awaitEvent := func(stage string) shutdownProcessEvent {
		t.Helper()
		select {
		case e, ok := <-events:
			if !ok || e.Stage != stage {
				t.Fatalf("child event = %+v (open %v), want %s", e, ok, stage)
			}
			return e
		case <-time.After(3 * time.Second):
			t.Fatalf("child event timeout: %s", stage)
		}
		return shutdownProcessEvent{}
	}
	control := json.NewEncoder(input)
	if err := waitForDial(addr, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	binary := dialServer(t, addr, "s-process-200")
	defer binary.CloseNow()
	phone := dialPhone(t, addr, "s-process-200")
	defer phone.CloseNow()
	// Own and join the readers; responsive peers keep processing close frames
	// while traffic writers race the signal and drain.
	peerCtx, peerCancel := context.WithCancel(ctx)
	var readers sync.WaitGroup
	defer func() { peerCancel(); binary.CloseNow(); phone.CloseNow(); readers.Wait() }()
	binaryFrames := make(chan string, 1)
	phoneFrames := make(chan struct{}, 1)
	binaryClose, phoneClose := make(chan error, 1), make(chan error, 1)
	var binaryCount, phoneCount atomic.Int64
	const frame = `{"v":2,"opaque":"synthetic ciphertext remains opaque"}`
	startReader := func(c *websocket.Conn, isBinary bool, ended chan<- error) {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				_, data, err := c.Read(peerCtx)
				if err != nil {
					ended <- err
					return
				}
				if isBinary {
					var envelope struct {
						ConnID    string          `json:"conn_id"`
						Frame     json.RawMessage `json:"frame"`
						CloseCode int             `json:"close_code"`
					}
					if json.Unmarshal(data, &envelope) != nil {
						ended <- errors.New("invalid binary routing envelope")
						return
					}
					// A phone's drain cleanup can notify its still-closing binary
					// before the binary's concurrent 1001 handshake finishes.
					if len(envelope.Frame) == 0 && envelope.ConnID != "" && envelope.CloseCode == 1001 {
						continue
					}
					if string(envelope.Frame) != frame || envelope.ConnID == "" {
						ended <- errors.New("binary opaque frame mismatch")
						return
					}
					binaryCount.Add(1)
					select {
					case binaryFrames <- envelope.ConnID:
					default:
					}
				} else {
					if string(data) != frame {
						ended <- errors.New("phone opaque frame mismatch")
						return
					}
					phoneCount.Add(1)
					select {
					case phoneFrames <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	startReader(binary, true, binaryClose)
	startReader(phone, false, phoneClose)
	write := func(c *websocket.Conn, data []byte) {
		t.Helper()
		if err := c.Write(peerCtx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	write(phone, []byte(frame))
	var connID string
	select {
	case connID = <-binaryFrames:
	case <-time.After(2 * time.Second):
		t.Fatal("phone not admitted")
	}
	envelope, _ := json.Marshal(struct {
		ConnID string          `json:"conn_id"`
		Frame  json.RawMessage `json:"frame"`
	}{connID, json.RawMessage(frame)})
	write(binary, envelope)
	select {
	case <-phoneFrames:
	case <-time.After(2 * time.Second):
		t.Fatal("binary-to-phone forwarding failed")
	}
	admitCtx, admitCancel := context.WithTimeout(peerCtx, 2*time.Second)
	defer admitCancel()
	for _, c := range []*websocket.Conn{binary, phone} {
		if err := c.Ping(admitCtx); err != nil {
			t.Fatal(err)
		}
	}
	// The first wake starts the natural send/refresh clocks. A short readiness
	// budget prevents their ten-second expiry from substituting for SIGTERM.
	wakeStart := time.Now()
	for i := 0; i < 6; i++ {
		write(binary, []byte(`{"push_wake":{"platform":"fcm","token":"synthetic-device-token"}}`))
	}
	awaitEvent("joined")
	select {
	case <-authStarted:
	case <-time.After(time.Second):
		t.Fatal("no local OAuth request")
	}
	// Continue BOTH directions while OAuth is held, and through signalling.
	var writers sync.WaitGroup
	for _, pair := range []struct {
		c    *websocket.Conn
		data []byte
	}{{phone, []byte(frame)}, {binary, envelope}} {
		writers.Add(1)
		go func(c *websocket.Conn, data []byte) {
			defer writers.Done()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-peerCtx.Done():
					return
				case <-ticker.C:
					if c.Write(peerCtx, websocket.MessageText, data) != nil {
						return
					}
				}
			}
		}(pair.c, pair.data)
	}
	defer func() { peerCancel(); writers.Wait() }()
	trafficDeadline := time.Now().Add(time.Second)
	for binaryCount.Load() < 3 || phoneCount.Load() < 3 {
		if time.Now().After(trafficDeadline) {
			t.Fatal("traffic did not continue during held authentication")
		}
		time.Sleep(time.Millisecond)
	}
	if err := control.Encode("check"); err != nil {
		t.Fatal(err)
	}
	awaitEvent("ready")
	if time.Since(wakeStart) >= 5*time.Second {
		t.Fatal("signal too late to rule out natural push deadlines")
	}
	if oauthHits.Load() != 1 {
		t.Fatalf("OAuth requests before signal = %d", oauthHits.Load())
	}
	select {
	case <-authCancelled:
		t.Fatal("authentication not held before signal")
	default:
	}
	signalled := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitEvent("authentication-cancelled")
	authElapsed := time.Since(signalled)
	select {
	case <-authCancelled:
	case <-time.After(time.Second):
		t.Fatal("local OAuth did not observe cancellation")
	}
	// No child return is allowed while its cancelled transport is still held.
	select {
	case e := <-events:
		t.Fatalf("child returned before authentication join: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if err := control.Encode("release"); err != nil {
		t.Fatal(err)
	}
	awaitEvent("run-returned")
	cleanup := awaitEvent("cleanup-complete")
	if cleanup.OAuth != 1 || cleanup.Other != 0 || oauthHits.Load() != 1 {
		t.Fatalf("request counts: %+v, local OAuth %d", cleanup, oauthHits.Load())
	}
	if len(cleanup.Outcomes) != 5 || cleanup.Outcomes["token_fetch_failed"] != 6 {
		t.Fatalf("outcomes: %v", cleanup.Outcomes)
	}
	for outcome, count := range cleanup.Outcomes {
		if outcome != "token_fetch_failed" && count != 0 {
			t.Errorf("outcome %s = %v", outcome, count)
		}
	}
	for name, closed := range map[string]<-chan error{"binary": binaryClose, "phone": phoneClose} {
		select {
		case err := <-closed:
			if websocket.CloseStatus(err) != websocket.StatusGoingAway {
				t.Errorf("%s close = %v", name, err)
			}
		case <-time.After(time.Second):
			t.Errorf("%s did not observe close", name)
		}
	}
	select {
	case err := <-exited:
		exited <- err // Leave the completed result for unconditional cleanup.
		if err != nil {
			t.Fatalf("child exit: %v", err)
		}
	case <-time.After(completeProcessBudget - time.Since(signalled)):
		t.Fatal("child exceeded complete process budget")
	}
	exitElapsed := time.Since(signalled)
	if exitElapsed > completeProcessBudget {
		t.Fatalf("signal-to-exit %s exceeds %s", exitElapsed, completeProcessBudget)
	}
	t.Logf("budget=%s signal-to-exit=%s signal-to-authentication-cancellation=%s OAuth=1 FCM=0 token_fetch_failed=6; both peers closed 1001; authentication cleanup joined", completeProcessBudget, exitElapsed, authElapsed)
}

// Full TOML parsing and baseline comparison is also performed offline for the
// PR. This test pins the two top-level scalar settings to the process budget.
func TestFlyCompleteShutdownAllowance(t *testing.T) {
	manifest, err := os.ReadFile("../../fly.toml")
	if err != nil {
		t.Fatal(err)
	}
	settings := make(map[string]string)
	for _, line := range strings.Split(string(manifest), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(line, "[") {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			settings[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if settings["kill_signal"] != `"SIGTERM"` {
		t.Errorf("top-level kill_signal = %q, want SIGTERM", settings["kill_signal"])
	}
	seconds, err := strconv.Atoi(settings["kill_timeout"])
	if err != nil || seconds != 30 || time.Duration(seconds)*time.Second < completeProcessBudget+5*time.Second || seconds > 300 {
		t.Fatalf("top-level kill_timeout = %q; want 30 seconds, budget %s + 5s margin, within 300s", settings["kill_timeout"], completeProcessBudget)
	}
}
