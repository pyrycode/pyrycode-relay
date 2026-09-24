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

// startClient spins up an httptest.NewServer running ClientHandler against
// a fresh registry. Tests that need a binary registered call seedBinary on
// the returned registry before dialling.
func startClient(t *testing.T) (*Registry, string, func()) {
	t.Helper()
	reg := NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, 0, nil))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return reg, wsURL, srv.Close
}

// startClientWithCap is like startClient but threads maxPhones through to
// ClientHandler. Used by the 4429 cap-rejection integration test.
func startClientWithCap(t *testing.T, maxPhones int) (*Registry, string, func()) {
	t.Helper()
	reg := NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, maxPhones, nil))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return reg, wsURL, srv.Close
}

func dialWithClient(t *testing.T, wsURL string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
}

func validClientHeaders(serverID string) http.Header {
	h := http.Header{}
	h.Set("X-Pyrycode-Server", serverID)
	h.Set("X-Pyrycode-Token", "phone-token-opaque")
	h.Set("User-Agent", "pyry-phone-test/0.1.0")
	return h
}

func seedBinary(t *testing.T, reg *Registry, serverID string) {
	t.Helper()
	if err := reg.ClaimServer(serverID, &fakeConn{id: "bin-" + serverID}); err != nil {
		t.Fatalf("seed ClaimServer(%q): %v", serverID, err)
	}
}

func waitForPhones(t *testing.T, reg *Registry, serverID string, want int, timeout time.Duration) []Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := reg.PhonesFor(serverID)
		if len(got) == want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := reg.PhonesFor(serverID)
	t.Fatalf("PhonesFor(%q) len = %d, want %d after %v", serverID, len(got), want, timeout)
	return nil
}

func TestClientEndpoint_ValidUpgrade_RegistersPhone(t *testing.T) {
	reg, wsURL, cleanup := startClient(t)
	defer cleanup()
	seedBinary(t, reg, "s1")

	c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	phones := waitForPhones(t, reg, "s1", 1, time.Second)
	id := phones[0].ConnID()
	const prefix = "client-s1-"
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

func TestClientEndpoint_HeaderGate_400(t *testing.T) {
	type row struct {
		name   string
		header string
		value  string
		delete bool
	}
	rows := []row{
		{name: "missing_X-Pyrycode-Server", header: "X-Pyrycode-Server", delete: true},
		{name: "empty_X-Pyrycode-Server", header: "X-Pyrycode-Server", value: ""},
		{name: "missing_X-Pyrycode-Token", header: "X-Pyrycode-Token", delete: true},
		{name: "empty_X-Pyrycode-Token", header: "X-Pyrycode-Token", value: ""},
		{name: "missing_User-Agent", header: "User-Agent", delete: true},
		{name: "empty_User-Agent", header: "User-Agent", value: ""},
	}

	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, 0, nil))
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
			req.Header.Set("X-Pyrycode-Token", "phone-token-opaque")
			req.Header.Set("User-Agent", "pyry-phone-test/0.1.0")
			if tc.delete {
				// For User-Agent, http.Client adds a default value when the
				// header is Del'd. Setting the key to a nil slice keeps
				// Header.has true so the default is not injected, while
				// sending no header on the wire. (Same trick as
				// server_endpoint_test.go.)
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

func TestClientEndpoint_NoBinary_4404(t *testing.T) {
	reg, wsURL, cleanup := startClient(t)
	defer cleanup()

	c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, readErr := c.Read(readCtx)
	if readErr == nil {
		t.Fatalf("Read returned nil error, want close")
	}
	var ce websocket.CloseError
	if !errors.As(readErr, &ce) {
		t.Fatalf("Read err = %v (%T), want *websocket.CloseError", readErr, readErr)
	}
	if ce.Code != websocket.StatusCode(4404) {
		t.Fatalf("close code = %d, want 4404", ce.Code)
	}
	if ce.Reason != "no server with that id" {
		t.Fatalf("close reason = %q, want %q", ce.Reason, "no server with that id")
	}
	if b, p := reg.Counts(); b != 0 || p != 0 {
		t.Fatalf("registry counts = (%d, %d), want (0, 0)", b, p)
	}
}

func TestClientEndpoint_PeerClose_UnregistersPhone(t *testing.T) {
	reg, wsURL, cleanup := startClient(t)
	defer cleanup()
	seedBinary(t, reg, "s1")

	c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitForPhones(t, reg, "s1", 1, time.Second)

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client Close: %v", err)
	}
	waitForPhones(t, reg, "s1", 0, 2*time.Second)

	// Binary stays claimed — the test did not touch /v1/server.
	if _, ok := reg.BinaryFor("s1"); !ok {
		t.Fatalf("BinaryFor(s1) was released by phone close; binary entry should be untouched")
	}
}

func TestClientEndpoint_MultiplePhones_IndependentLifecycle(t *testing.T) {
	reg, wsURL, cleanup := startClient(t)
	defer cleanup()
	seedBinary(t, reg, "s1")

	dialPhone := func() *websocket.Conn {
		c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}

	c1 := dialPhone()
	waitForPhones(t, reg, "s1", 1, time.Second)
	c2 := dialPhone()
	waitForPhones(t, reg, "s1", 2, time.Second)
	c3 := dialPhone()
	phones3 := waitForPhones(t, reg, "s1", 3, time.Second)

	id1 := phones3[0].ConnID()
	id2 := phones3[1].ConnID()
	id3 := phones3[2].ConnID()
	if id1 == id2 || id2 == id3 || id1 == id3 {
		t.Fatalf("ConnIDs not unique: %q %q %q", id1, id2, id3)
	}

	// Close the middle phone; the other two remain.
	if err := c2.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("c2 Close: %v", err)
	}
	phones2 := waitForPhones(t, reg, "s1", 2, 2*time.Second)
	got := map[string]bool{
		phones2[0].ConnID(): true,
		phones2[1].ConnID(): true,
	}
	if !got[id1] || !got[id3] {
		t.Fatalf("after closing c2, remaining phones = %v, want %q and %q present", got, id1, id3)
	}
	if got[id2] {
		t.Fatalf("after closing c2, c2's id %q still registered", id2)
	}

	if err := c1.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("c1 Close: %v", err)
	}
	phones1 := waitForPhones(t, reg, "s1", 1, 2*time.Second)
	if phones1[0].ConnID() != id3 {
		t.Fatalf("after closing c1, remaining phone id = %q, want %q", phones1[0].ConnID(), id3)
	}

	if err := c3.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("c3 Close: %v", err)
	}
	waitForPhones(t, reg, "s1", 0, 2*time.Second)
}

func TestClientEndpoint_DeviceNameOptional_HandlerAccepts(t *testing.T) {
	reg, wsURL, cleanup := startClient(t)
	defer cleanup()
	seedBinary(t, reg, "s1")

	// No device-name header.
	hdr := validClientHeaders("s1")
	c1, _, err := dialWithClient(t, wsURL, hdr)
	if err != nil {
		t.Fatalf("dial without device-name: %v", err)
	}
	waitForPhones(t, reg, "s1", 1, time.Second)

	// With device-name header.
	hdr2 := validClientHeaders("s1")
	hdr2.Set("X-Pyrycode-Device-Name", "Juhana's iPhone")
	c2, _, err := dialWithClient(t, wsURL, hdr2)
	if err != nil {
		t.Fatalf("dial with device-name: %v", err)
	}
	waitForPhones(t, reg, "s1", 2, time.Second)

	// Clean up.
	_ = c1.Close(websocket.StatusNormalClosure, "")
	_ = c2.Close(websocket.StatusNormalClosure, "")
	waitForPhones(t, reg, "s1", 0, 2*time.Second)
}

func TestClientEndpoint_AtCap_4429(t *testing.T) {
	const cap = 2
	reg, wsURL, cleanup := startClientWithCap(t, cap)
	defer cleanup()
	seedBinary(t, reg, "s1")

	inCap := make([]*websocket.Conn, 0, cap)
	for i := 0; i < cap; i++ {
		c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
		if err != nil {
			t.Fatalf("dial #%d: %v", i, err)
		}
		inCap = append(inCap, c)
	}
	waitForPhones(t, reg, "s1", cap, time.Second)

	over, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatalf("dial over-cap: %v", err)
	}
	defer over.Close(websocket.StatusNormalClosure, "")

	readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRead()
	_, _, readErr := over.Read(readCtx)
	var ce websocket.CloseError
	if !errors.As(readErr, &ce) {
		t.Fatalf("over-cap Read err = %v (%T), want *websocket.CloseError", readErr, readErr)
	}
	if ce.Code != websocket.StatusCode(4429) {
		t.Fatalf("over-cap close code = %d, want 4429", ce.Code)
	}
	if ce.Reason != "too many phones for server-id" {
		t.Fatalf("over-cap close reason = %q, want %q", ce.Reason, "too many phones for server-id")
	}

	if got := reg.PhonesFor("s1"); len(got) != cap {
		t.Errorf("PhonesFor after over-cap rejection: got len=%d, want %d", len(got), cap)
	}

	for _, c := range inCap {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	waitForPhones(t, reg, "s1", 0, 2*time.Second)
}

// #113: the handler registers each phone behind its own delivery queue, runs
// the queue's writer for the connection's lifetime, and stops it on
// disconnect so nothing is left accepting or blocked on the phone.
func TestClientEndpoint_PhoneQueue_DeliversAndStopsOnDisconnect(t *testing.T) {
	reg, wsURL, cleanup := startClient(t)
	defer cleanup()
	seedBinary(t, reg, "s1")

	c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	phones := waitForPhones(t, reg, "s1", 1, time.Second)
	q, ok := phones[0].(phoneQueue)
	if !ok {
		t.Fatalf("registered phone %T does not implement phoneQueue", phones[0])
	}

	if err := q.Enqueue([]byte(`{"hello":1}`), 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("phone Read: %v", err)
	}
	if string(got) != `{"hello":1}` {
		t.Fatalf("phone got %s, want {\"hello\":1}", got)
	}

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client Close: %v", err)
	}
	waitForPhones(t, reg, "s1", 0, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := q.Enqueue([]byte(`{}`), 0)
		if errors.Is(err, ErrPhoneOutboxClosed) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Enqueue after disconnect = %v, want ErrPhoneOutboxClosed", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
