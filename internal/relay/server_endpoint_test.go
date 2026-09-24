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
	"sync/atomic"
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
	// A live binary reads, and reading is what answers the relay's
	// conflict probe (#112); a client that never reads is unresponsive.
	c1.CloseRead(context.Background())

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
	phone := &fakeConn{id: "p-1"}
	if err := reg.RegisterPhone("s1", phone); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
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
	if phone.isClosed() {
		t.Fatalf("incumbent's phone was closed by a refused duplicate claim")
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

// startServerProbe is startServer with a short conflict-probe timeout and a
// captured log, for the #112 takeover tests.
func startServerProbe(t *testing.T, grace, probeTimeout time.Duration) (*Registry, string, *lockedBuffer, func()) {
	t.Helper()
	reg := NewRegistry()
	logger, logs := captureLogger()
	srv := httptest.NewServer(serverHandler(reg, logger, grace, probeTimeout, 256*1024, nil))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return reg, wsURL, logs, srv.Close
}

// awaitBinary polls until serverID is held and returns the holder.
func awaitBinary(t *testing.T, reg *Registry, serverID string) Conn {
	t.Helper()
	var conn Conn
	if !pollUntil(time.Now().Add(time.Second), func() bool {
		c, ok := reg.BinaryFor(serverID)
		conn = c
		return ok
	}) {
		t.Fatalf("no binary claimed %q", serverID)
	}
	return conn
}

// TestServerEndpoint_UnresponsiveIncumbent_TakenOver covers #112's first
// and third acceptance criteria. The incumbent client never reads, so it
// never answers the conflict probe: the second claim takes the slot within
// the probe timeout, the incumbent's phones get 4404, the incumbent gets
// the heartbeat's 1011, and the displaced handler's release defer does not
// remove the new binary once grace has long passed.
func TestServerEndpoint_UnresponsiveIncumbent_TakenOver(t *testing.T) {
	grace := 100 * time.Millisecond
	probeTimeout := 200 * time.Millisecond
	reg, wsURL, logs, cleanup := startServerProbe(t, grace, probeTimeout)
	defer cleanup()

	c1, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial #1: %v", err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "")
	incumbent := awaitBinary(t, reg, "s1")

	phone := &codedFakeConn{fakeConn: fakeConn{id: "p-1", closeCh: make(chan struct{})}}
	if err := reg.RegisterPhone("s1", phone); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	start := time.Now()
	c2, _, err := dialWith(t, wsURL, validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial #2: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	var holder Conn
	if !pollUntil(start.Add(probeTimeout+2*time.Second), func() bool {
		c, ok := reg.BinaryFor("s1")
		holder = c
		return ok && c != incumbent
	}) {
		t.Fatalf("second claim did not take over the unresponsive incumbent's slot")
	}

	select {
	case <-phone.closeCh:
	case <-time.After(time.Second):
		t.Fatal("incumbent's phone not closed on takeover")
	}
	if got := phone.closeCode(); got != websocket.StatusCode(4404) {
		t.Errorf("phone close code: got %d, want 4404", got)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, readErr := c1.Read(readCtx)
	if got := websocket.CloseStatus(readErr); got != websocket.StatusInternalError {
		t.Fatalf("incumbent close: got %v (code %d), want 1011", readErr, got)
	}

	// AC3: the displaced handler's disconnect path has run...
	if !pollUntil(time.Now().Add(2*time.Second), func() bool {
		return strings.Contains(logs.String(), "msg=server_released")
	}) {
		t.Fatal("displaced handler never ran its release defer")
	}
	// ...and well past grace the new binary still holds the slot.
	time.Sleep(3 * grace)
	if got, ok := reg.BinaryFor("s1"); !ok || got != holder {
		t.Fatalf("after displaced release + 3×grace: BinaryFor = (%v, %v), want the new binary", got, ok)
	}
	if !strings.Contains(logs.String(), "msg=server_id_takeover") {
		t.Error("takeover not logged as server_id_takeover")
	}
}

// TestServerEndpoint_StalledPhone_LiveIncumbentKeepsSlot pins #140: a
// phone write blocked on a stalled phone must not delay the incumbent's
// pong past the conflict probe. Before #113 the forwarder wrote to phones
// synchronously, so a pong waited behind the blocked write (up to
// writeTimeout) and a live incumbent was taken over.
func TestServerEndpoint_StalledPhone_LiveIncumbentKeepsSlot(t *testing.T) {
	probeTimeout := 200 * time.Millisecond
	reg := NewRegistry()
	var written atomic.Int64
	reg.SetForwarderHooks(nil, func() { written.Add(1) })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.Handle("/v1/server", serverHandler(reg, logger, 100*time.Millisecond, probeTimeout, 1<<20, nil))
	mux.Handle("/v1/client", ClientHandler(reg, logger, 256*1024, 0, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	c1, _, err := dialWith(t, wsURL+"/v1/server", validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial incumbent: %v", err)
	}
	defer c1.CloseNow()
	// The incumbent answers pings only while it reads.
	c1.CloseRead(context.Background())
	incumbent := awaitBinary(t, reg, "s1")

	// The phone never reads, so its socket buffers fill and stay full.
	pc, _, err := dialWithClient(t, wsURL+"/v1/client", validClientHeaders("s1"))
	if err != nil {
		t.Fatalf("dial phone: %v", err)
	}
	defer pc.CloseNow()
	phones := waitForPhones(t, reg, "s1", 1, time.Second)
	phoneID := phones[0].ConnID()

	// Send one frame at a time until a write to the phone stops
	// completing. At most one frame is ever queued behind the blocked
	// write, far under phoneOutboxDepth, so the outbox never overflows
	// into a 1011 close.
	frame := []byte(`"` + strings.Repeat("a", 256*1024) + `"`)
	env := mustMarshal(t, phoneID, frame)
	var sent int64
	blocked := false
	for sent < 400 && !blocked {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c1.Write(ctx, websocket.MessageText, env)
		cancel()
		if err != nil {
			t.Fatalf("binary write %d: %v", sent+1, err)
		}
		sent++
		want := sent
		blocked = !pollUntil(time.Now().Add(300*time.Millisecond), func() bool {
			return written.Load() == want
		})
	}
	if !blocked {
		t.Fatalf("phone writes never blocked after %d frames of %d bytes", sent, len(frame))
	}
	stuck := written.Load()

	c2, _, err := dialWith(t, wsURL+"/v1/server", validHeaders("s1"))
	if err != nil {
		t.Fatalf("dial duplicate: %v", err)
	}
	defer c2.CloseNow()
	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, readErr := c2.Read(readCtx)
	if got := websocket.CloseStatus(readErr); got != websocket.StatusCode(4409) {
		t.Fatalf("duplicate claim close: got %v (code %d), want 4409", readErr, got)
	}

	if got, ok := reg.BinaryFor("s1"); !ok || got != incumbent {
		t.Fatalf("BinaryFor(s1) = (%v, %v), want the incumbent", got, ok)
	}
	// The write stayed blocked across the whole probe, so the pong was
	// answered while a phone write was in flight.
	if got := written.Load(); got != stuck || got >= sent {
		t.Fatalf("phone writes = %d of %d sent (was %d before the probe), want still blocked", got, sent, stuck)
	}
	if got := reg.PhonesFor("s1"); len(got) != 1 || got[0].ConnID() != phoneID {
		t.Fatalf("PhonesFor(s1) = %v, want the stalled phone still registered", got)
	}
}
