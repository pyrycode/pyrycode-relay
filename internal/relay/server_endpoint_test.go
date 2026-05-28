package relay

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startServer spins up an httptest.NewServer running ServerHandler against a
// fresh registry. The grace duration is the third arg to ServerHandler;
// tests that don't exercise the disconnect path can pass a small duration.
func startServer(t *testing.T, grace time.Duration) (*Registry, string, func()) {
	t.Helper()
	reg := NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(ServerHandler(reg, logger, grace, 256*1024, nil))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return reg, wsURL, srv.Close
}

func dialWith(t *testing.T, wsURL string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
}

func validHeaders(serverID string) http.Header {
	h := http.Header{}
	h.Set("X-Pyrycode-Server", serverID)
	h.Set("X-Pyrycode-Version", "0.1.0-test")
	h.Set("User-Agent", "pyry-test/0.1.0")
	return h
}

func TestServerEndpoint_ValidUpgrade_RegistersBinary(t *testing.T) {
	reg, wsURL, cleanup := startServer(t, 100*time.Millisecond)
	defer cleanup()

	c, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// The handler runs in its own goroutine; ClaimServer happens after
	// Accept returns to the dialer. Poll briefly for the registration.
	deadline := time.Now().Add(time.Second)
	var conn Conn
	var ok bool
	for time.Now().Before(deadline) {
		conn, ok = reg.BinaryFor("s1")
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("BinaryFor(s1) not registered")
	}

	id := conn.ConnID()
	const prefix = "server-s1-"
	if !strings.HasPrefix(id, prefix) {
		t.Fatalf("ConnID() = %q, want prefix %q", id, prefix)
	}
	suffix := strings.TrimPrefix(id, prefix)
	if len(suffix) != 8 {
		t.Fatalf("ConnID suffix = %q (len %d), want 8 hex chars", suffix, len(suffix))
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		t.Fatalf("ConnID suffix %q is not hex: %v", suffix, err)
	}
}

func TestServerEndpoint_HeaderGate_400(t *testing.T) {
	type row struct {
		name   string
		header string
		value  string
		delete bool
	}
	rows := []row{
		{name: "missing_X-Pyrycode-Server", header: "X-Pyrycode-Server", delete: true},
		{name: "empty_X-Pyrycode-Server", header: "X-Pyrycode-Server", value: ""},
		{name: "missing_X-Pyrycode-Version", header: "X-Pyrycode-Version", delete: true},
		{name: "empty_X-Pyrycode-Version", header: "X-Pyrycode-Version", value: ""},
		{name: "missing_User-Agent", header: "User-Agent", delete: true},
		{name: "empty_User-Agent", header: "User-Agent", value: ""},
	}

	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := httptest.NewServer(ServerHandler(reg, logger, 100*time.Millisecond, 256*1024, nil))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			req.Header.Set("X-Pyrycode-Server", "s1")
			req.Header.Set("X-Pyrycode-Version", "0.1.0-test")
			req.Header.Set("User-Agent", "pyry-test/0.1.0")
			if tc.delete {
				// For User-Agent, http.Client adds a default value when
				// the header is Del'd. Setting the key to a nil slice
				// keeps Header.has true so the default is not injected,
				// while sending no header on the wire.
				req.Header[tc.header] = nil
			} else {
				req.Header.Set(tc.header, tc.value)
			}

			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}

			if b, p := reg.Counts(); b != 0 || p != 0 {
				t.Fatalf("registry counts = (%d, %d), want (0, 0)", b, p)
			}
		})
	}
}

func TestServerEndpoint_DuplicateClaim_4409(t *testing.T) {
	reg, wsURL, cleanup := startServer(t, 100*time.Millisecond)
	defer cleanup()

	c1, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial #1: %v", err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "")

	// Wait for the first claim to land.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.BinaryFor("s1"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := reg.BinaryFor("s1"); !ok {
		t.Fatalf("first claim did not register")
	}

	c2, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial #2: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, readErr := c2.Read(readCtx)
	if readErr == nil {
		t.Fatalf("Read on duplicate conn returned nil error, want close")
	}
	var ce websocket.CloseError
	if !errors.As(readErr, &ce) {
		t.Fatalf("Read err = %v (%T), want *websocket.CloseError", readErr, readErr)
	}
	if ce.Code != websocket.StatusCode(4409) {
		t.Fatalf("close code = %d, want 4409", ce.Code)
	}
	if ce.Reason != "server-id already claimed" {
		t.Fatalf("close reason = %q, want %q", ce.Reason, "server-id already claimed")
	}

	if _, ok := reg.BinaryFor("s1"); !ok {
		t.Fatalf("first claim was evicted by duplicate; registry no longer holds s1")
	}
}

func TestServerEndpoint_PeerClose_ReleasesSlot(t *testing.T) {
	reg, wsURL, cleanup := startServer(t, 100*time.Millisecond)
	defer cleanup()

	c, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.BinaryFor("s1"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := reg.BinaryFor("s1"); !ok {
		t.Fatalf("claim did not register")
	}

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client Close: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	released := false
	for time.Now().Before(deadline) {
		if _, ok := reg.BinaryFor("s1"); !ok {
			released = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !released {
		t.Fatalf("BinaryFor(s1) still registered after peer close")
	}

	c2, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("re-dial: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.BinaryFor("s1"); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("re-claim did not register")
}

func TestServerEndpoint_PeerClose_SchedulesGraceRelease(t *testing.T) {
	grace := 200 * time.Millisecond
	reg, wsURL, cleanup := startServer(t, grace)
	defer cleanup()

	c, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.BinaryFor("s1"); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := reg.BinaryFor("s1"); !ok {
		t.Fatalf("BinaryFor(s1) not registered before close")
	}

	closeAt := time.Now()
	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client Close: %v", err)
	}

	// Phase 1: scheduled, NOT immediate. For ~grace/4 after closeAt the
	// binary entry must remain in the registry. The pre-#21 path (immediate
	// ReleaseServer) would flip BinaryFor to false within milliseconds of
	// the handler observing the close.
	earlyDeadline := closeAt.Add(grace / 4)
	for time.Now().Before(earlyDeadline) {
		if _, ok := reg.BinaryFor("s1"); !ok {
			t.Fatalf("BinaryFor(s1) released at T+%v < grace=%v (handler used immediate ReleaseServer, not scheduled)", time.Since(closeAt), grace)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Phase 2: timer fires within grace + slack.
	lateDeadline := closeAt.Add(2*grace + 500*time.Millisecond)
	for time.Now().Before(lateDeadline) {
		if _, ok := reg.BinaryFor("s1"); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("BinaryFor(s1) still registered at T+%v; expected scheduled release to have fired (grace=%v)", time.Since(closeAt), grace)
}

func TestServerEndpoint_WrongMethod_NoPanic(t *testing.T) {
	cases := []struct {
		name string
		do   func(srvURL string) (*http.Response, error)
	}{
		{
			name: "GET_no_upgrade_headers",
			do: func(srvURL string) (*http.Response, error) {
				req, err := http.NewRequest(http.MethodGet, srvURL+"/", nil)
				if err != nil {
					return nil, err
				}
				req.Header.Set("X-Pyrycode-Server", "s1")
				req.Header.Set("X-Pyrycode-Version", "0.1.0-test")
				req.Header.Set("User-Agent", "pyry-test/0.1.0")
				return (&http.Client{Timeout: 5 * time.Second}).Do(req)
			},
		},
		{
			name: "POST",
			do: func(srvURL string) (*http.Response, error) {
				req, err := http.NewRequest(http.MethodPost, srvURL+"/", nil)
				if err != nil {
					return nil, err
				}
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("X-Pyrycode-Server", "s1")
				req.Header.Set("X-Pyrycode-Version", "0.1.0-test")
				req.Header.Set("User-Agent", "pyry-test/0.1.0")
				return (&http.Client{Timeout: 5 * time.Second}).Do(req)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := httptest.NewServer(ServerHandler(reg, logger, 100*time.Millisecond, 256*1024, nil))
			defer srv.Close()

			resp, err := tc.do(srv.URL)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Fatalf("status = %d, want 4xx", resp.StatusCode)
			}
			if b, p := reg.Counts(); b != 0 || p != 0 {
				t.Fatalf("registry counts = (%d, %d), want (0, 0)", b, p)
			}
		})
	}
}
