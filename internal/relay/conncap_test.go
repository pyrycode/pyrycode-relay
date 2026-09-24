package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestNewConnCap_RejectsNonPositive(t *testing.T) {
	t.Parallel()
	for _, max := range []int{0, -1} {
		if _, err := NewConnCap(max, discardLogger()); !errors.Is(err, ErrInvalidConnCap) {
			t.Errorf("NewConnCap(%d): err = %v, want ErrInvalidConnCap", max, err)
		}
	}
	if _, err := NewConnCap(1, discardLogger()); err != nil {
		t.Fatalf("NewConnCap(1): %v", err)
	}
}

// waitInUse polls cc.inUse until it equals want. Slots release when the
// wrapped handler returns, which trails the client-side close by a close
// handshake, so tests poll rather than assert once.
func waitInUse(t *testing.T, cc *ConnCap, want int) {
	t.Helper()
	if !pollUntil(time.Now().Add(3*time.Second), func() bool { return cc.inUse() == want }) {
		t.Fatalf("inUse = %d, want %d", cc.inUse(), want)
	}
}

func TestConnCap_OverCap_503BeforeHandler(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger()
	cc, err := NewConnCap(1, logger)
	if err != nil {
		t.Fatalf("NewConnCap: %v", err)
	}

	var calls atomic.Int64
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h := cc.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	firstDone := make(chan int, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/server", nil))
		firstDone <- rr.Code
	}()
	<-entered

	req := httptest.NewRequest(http.MethodGet, "/v1/client", nil)
	req.RemoteAddr = "203.0.113.7:4242"
	req.Header.Set("X-Pyrycode-Server", "secret-server-id-value")
	req.Header.Set("X-Pyrycode-Token", "secret-token-value")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-cap status = %d, want 503", rr.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (over-cap request must not reach the handler)", got)
	}

	out := logs.String()
	if n := strings.Count(out, "conn_cap_reached"); n != 1 {
		t.Fatalf("conn_cap_reached lines = %d, want 1; logs:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("deny line not at Warn; logs:\n%s", out)
	}
	for _, secret := range []string{"secret-server-id-value", "secret-token-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("log carries header value %q; logs:\n%s", secret, out)
		}
	}

	close(release)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	waitInUse(t, cc, 0)

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/client", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("after release status = %d, want 200", rr.Code)
	}
}

// startCapped serves the real /v1/server and /v1/client handlers behind
// one shared ConnCap, as cmd/pyrycode-relay wires them.
func startCapped(t *testing.T, max int, grace time.Duration) (*Registry, *ConnCap, string) {
	t.Helper()
	reg := NewRegistry()
	cc, err := NewConnCap(max, discardLogger())
	if err != nil {
		t.Fatalf("NewConnCap: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/server", cc.Wrap(ServerHandler(reg, discardLogger(), grace, 256*1024, nil)))
	mux.Handle("/v1/client", cc.Wrap(ClientHandler(reg, discardLogger(), 256*1024, 0, nil)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return reg, cc, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// mustDial dials path and fails the test unless the upgrade succeeds.
func mustDial(t *testing.T, url string, hdr http.Header) *websocket.Conn {
	t.Helper()
	c, _, err := dialWith(t, url, hdr)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// expect503 dials path and asserts the relay refused with HTTP 503 before
// any upgrade.
func expect503(t *testing.T, url string, hdr http.Header) {
	t.Helper()
	c, resp, err := dialWith(t, url, hdr)
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatalf("dial %s at cap: upgrade succeeded, want 503", url)
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial %s at cap: err = %v, want HTTP 503", url, err)
	}
}

// readClose reads c until it closes and returns the close code.
func readClose(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func TestConnCap_SharedSlotReleasedOnEveryExit(t *testing.T) {
	t.Parallel()

	t.Run("normal close", func(t *testing.T) {
		t.Parallel()
		reg, cc, base := startCapped(t, 2, time.Minute)
		mustDial(t, base+"/v1/server", validHeaders("s1"))
		awaitBinary(t, reg, "s1")
		phone := mustDial(t, base+"/v1/client", validClientHeaders("s1"))
		waitForPhones(t, reg, "s1", 1, 2*time.Second)
		waitInUse(t, cc, 2)
		expect503(t, base+"/v1/client", validClientHeaders("s1"))
		expect503(t, base+"/v1/server", validHeaders("s2"))

		phone.Close(websocket.StatusNormalClosure, "")
		waitInUse(t, cc, 1)
		mustDial(t, base+"/v1/client", validClientHeaders("s1"))
		waitForPhones(t, reg, "s1", 1, 2*time.Second)
		waitInUse(t, cc, 2)
		expect503(t, base+"/v1/client", validClientHeaders("s1"))
	})

	t.Run("reject after accept 4404", func(t *testing.T) {
		t.Parallel()
		_, cc, base := startCapped(t, 1, time.Minute)
		for i := 0; i < 2; i++ {
			phone := mustDial(t, base+"/v1/client", validClientHeaders("absent"))
			if code := readClose(t, phone); code != websocket.StatusCode(4404) {
				t.Fatalf("attempt %d: close code = %d, want 4404", i, code)
			}
			waitInUse(t, cc, 0)
		}
	})

	t.Run("phone torn down on grace expiry", func(t *testing.T) {
		t.Parallel()
		reg, cc, base := startCapped(t, 2, 100*time.Millisecond)
		bin := mustDial(t, base+"/v1/server", validHeaders("s1"))
		awaitBinary(t, reg, "s1")
		phone := mustDial(t, base+"/v1/client", validClientHeaders("s1"))
		waitForPhones(t, reg, "s1", 1, 2*time.Second)
		waitInUse(t, cc, 2)
		expect503(t, base+"/v1/client", validClientHeaders("s1"))

		bin.Close(websocket.StatusNormalClosure, "")
		waitInUse(t, cc, 1)
		readClose(t, phone)
		waitInUse(t, cc, 0)

		mustDial(t, base+"/v1/server", validHeaders("s1"))
		awaitBinary(t, reg, "s1")
		mustDial(t, base+"/v1/client", validClientHeaders("s1"))
		waitForPhones(t, reg, "s1", 1, 2*time.Second)
		waitInUse(t, cc, 2)
		expect503(t, base+"/v1/client", validClientHeaders("s1"))
	})
}
