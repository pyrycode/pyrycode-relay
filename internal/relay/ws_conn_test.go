package relay

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startEcho stands up an httptest server whose handler upgrades to a
// WebSocket, forwards every received frame to a buffered channel, and
// echoes the frame back to the client. The echo lets tests exercise the
// client-side WSConn.Read (and its SetReadLimit cap) by round-tripping
// frames; the server side has no custom read cap, so oversize frames
// reach the client where the cap surfaces.
//
// Returns a connected client-side WSConn capped at maxFrameBytes, the
// server-side receive channel, and a cleanup function the caller defers.
func startEcho(t *testing.T, maxFrameBytes int64) (*WSConn, <-chan []byte, func()) {
	t.Helper()
	received := make(chan []byte, 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "test ended")
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			received <- data
			if err := c.Write(r.Context(), websocket.MessageBinary, data); err != nil {
				return
			}
		}
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}

	wc := NewWSConn(client, "test-conn-id", maxFrameBytes)
	cleanup := func() {
		wc.Close()
		srv.Close()
	}
	return wc, received, cleanup
}

// TestWSConn_Send_UsesTextOpcode pins the outbound WebSocket frame opcode
// to text. The wire spec (pyrycode/pyrycode/docs/protocol-mobile.md §
// Encoding) requires line-delimited JSON over WS text frames; the mobile
// client closes the connection on any binary frame. The relay-side fakes
// all use text, so they cannot catch a regression in the relay's own
// outbound opcode — this test stands up its own peer that captures the
// observed message type, since startEcho deliberately discards it.
func TestWSConn_Send_UsesTextOpcode(t *testing.T) {
	gotType := make(chan websocket.MessageType, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "test ended")
		typ, _, err := c.Read(r.Context())
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		gotType <- typ
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wc := NewWSConn(client, "test-conn-id", 256*1024)
	defer wc.Close()

	if err := wc.Send([]byte("envelope")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case typ := <-gotType:
		if typ != websocket.MessageText {
			t.Errorf("peer observed message type %v, want %v", typ, websocket.MessageText)
		}
		if typ == websocket.MessageBinary {
			t.Errorf("Send wrote a binary frame; the mobile client rejects binary (regression of #108)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for peer to observe the sent frame")
	}
}

func TestWSConn_ConnID_ReturnsConstructorValue(t *testing.T) {
	wc, _, cleanup := startEcho(t, 256*1024)
	defer cleanup()
	if got := wc.ConnID(); got != "test-conn-id" {
		t.Fatalf("ConnID() = %q, want %q", got, "test-conn-id")
	}
}

func TestWSConn_ConcurrentSend_ProducesIntactFrames(t *testing.T) {
	wc, received, cleanup := startEcho(t, 256*1024)
	defer cleanup()

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(id int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("g%02d", id))
			if err := wc.Send(payload); err != nil {
				t.Errorf("Send(%d): %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	got := make(map[string]int, n)
	timeout := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case frame := <-received:
			got[string(frame)]++
		case <-timeout:
			t.Fatalf("timed out after receiving %d/%d frames; got=%v", i, n, got)
		}
	}
	if len(got) != n {
		t.Fatalf("got %d distinct frames, want %d (got=%v)", len(got), n, got)
	}
	for k, v := range got {
		if v != 1 {
			t.Errorf("frame %q received %d times, want 1", k, v)
		}
	}
}

func TestWSConn_DoubleClose_DoesNotPanic(t *testing.T) {
	wc, _, cleanup := startEcho(t, 256*1024)
	defer cleanup()

	wc.Close()
	wc.Close()
}

func TestWSConn_SendAfterClose_ReturnsError(t *testing.T) {
	wc, _, cleanup := startEcho(t, 256*1024)
	defer cleanup()

	wc.Close()
	if err := wc.Send([]byte("late")); err == nil {
		t.Fatal("Send after Close returned nil error, want non-nil")
	}
}

// TestWSConn_Read_FrameExceedingCap_ReturnsError verifies that a frame
// whose payload exceeds the per-frame read cap surfaces as a non-nil
// error on the receiving WSConn, and that subsequent reads also fail
// (the library closes the underlying conn per SetReadLimit contract).
//
// The test deliberately does NOT assert on the specific error type: the
// library is free to wrap the close error differently across versions.
// Only the non-nil contract from the AC is asserted.
func TestWSConn_Read_FrameExceedingCap_ReturnsError(t *testing.T) {
	const maxBytes = int64(64)
	wc, _, cleanup := startEcho(t, maxBytes)
	defer cleanup()

	oversize := bytes.Repeat([]byte("x"), int(maxBytes)*4)
	if err := wc.Send(oversize); err != nil {
		t.Fatalf("Send oversize: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := wc.Read(ctx); err == nil {
		t.Fatal("Read on over-cap frame returned nil error, want non-nil")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if _, err := wc.Read(ctx2); err == nil {
		t.Fatal("subsequent Read after over-cap returned nil error, want non-nil")
	}
}

// TestWSConn_Read_FrameAtCap_DeliveredIntact verifies that a frame whose
// payload is exactly at the cap is delivered intact through WSConn.Read.
// Together with the over-cap test this pins the boundary behaviour.
func TestWSConn_Read_FrameAtCap_DeliveredIntact(t *testing.T) {
	const maxBytes = int64(256)
	wc, received, cleanup := startEcho(t, maxBytes)
	defer cleanup()

	payload := bytes.Repeat([]byte("y"), int(maxBytes))
	if err := wc.Send(payload); err != nil {
		t.Fatalf("Send at-cap: %v", err)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Fatalf("server received %d bytes, want %d (and equal payload)", len(got), len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to receive at-cap frame")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := wc.Read(ctx)
	if err != nil {
		t.Fatalf("Read at-cap echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("client Read returned %d bytes, want %d (and equal payload)", len(got), len(payload))
	}
}

// testReadTimeout is the shortened per-message read deadline the
// read-deadline tests install on their WSConn in place of
// readMessageTimeout.
const testReadTimeout = 500 * time.Millisecond

// dialWSConn dials srv and wraps the client side in a WSConn whose
// per-message read deadline is shortened to testReadTimeout.
func dialWSConn(t *testing.T, srv *httptest.Server) *WSConn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wc := NewWSConn(client, "test-conn-id", 256*1024)
	wc.readTimeout = testReadTimeout
	return wc
}

// startScriptedPeer stands up an httptest server whose handler upgrades
// and runs peer against the server-side *websocket.Conn, and returns the
// connected client-side WSConn. peer's ctx is cancelled at cleanup, so a
// peer that stalls on it exits and the server can shut down.
func startScriptedPeer(t *testing.T, peer func(ctx context.Context, c *websocket.Conn)) *WSConn {
	t.Helper()
	peerCtx, stopPeer := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "test ended")
		peer(peerCtx, c)
	}))
	wc := dialWSConn(t, srv)
	t.Cleanup(func() {
		wc.Close()
		stopPeer()
		srv.Close()
	})
	return wc
}

// readWithin runs wc.Read in a goroutine and returns its result and the
// time it took, failing the test if Read has not returned within limit.
func readWithin(t *testing.T, wc *WSConn, limit time.Duration) ([]byte, time.Duration, error) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		data, err := wc.Read(context.Background())
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return r.data, time.Since(start), r.err
	case <-time.After(limit):
		wc.Close()
		t.Fatalf("Read did not return within %v", limit)
		return nil, 0, nil
	}
}

// TestWSConn_Read_StalledFragmentedMessage_ClosesWithinDeadline pins the
// slowloris bound: a peer that starts a message (one non-final data
// frame, header plus part of the payload) and never finishes it is cut
// off within the message deadline, even though it keeps answering pings
// and so would satisfy the heartbeat indefinitely.
func TestWSConn_Read_StalledFragmentedMessage_ClosesWithinDeadline(t *testing.T) {
	wc := startScriptedPeer(t, func(ctx context.Context, c *websocket.Conn) {
		// Read loop so the peer answers the relay's pings.
		go func() {
			for {
				if _, _, err := c.Read(ctx); err != nil {
					return
				}
			}
		}()
		w, err := c.Writer(ctx, websocket.MessageText)
		if err != nil {
			t.Errorf("peer Writer: %v", err)
			return
		}
		if _, err := w.Write([]byte(`{"partial":`)); err != nil {
			t.Errorf("peer Write: %v", err)
			return
		}
		<-ctx.Done() // never finish the message
	})

	// The non-final frame sits in the peer's write buffer until the
	// peer's next flush; the pong answering the first ping is that flush.
	// The second ping lands mid-stall, after the message has started.
	pings := make(chan error, 2)
	go func() {
		for i := 0; i < 2; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), testReadTimeout/2)
			pings <- wc.Ping(ctx)
			cancel()
			time.Sleep(testReadTimeout / 4)
		}
	}()

	_, elapsed, err := readWithin(t, wc, testReadTimeout+2*time.Second)
	if err == nil {
		t.Fatal("Read of a stalled message returned nil error, want non-nil")
	}
	if elapsed > testReadTimeout+time.Second {
		t.Errorf("Read returned after %v, want within deadline %v plus slack", elapsed, testReadTimeout)
	}
	for i := 0; i < 2; i++ {
		if perr := <-pings; perr != nil {
			t.Errorf("ping %d during stall: %v; the peer must look alive to the heartbeat", i, perr)
		}
	}
	if err := wc.Send([]byte("late")); err == nil {
		t.Error("Send after the read deadline fired returned nil error; want the connection closed")
	}
}

// TestWSConn_Read_DribbledFrame_ClosesWithinDeadline covers the
// single-frame form: the peer sends a frame header declaring a payload
// and then dribbles it one byte at a time, faster than the deadline but
// never finishing. Progress within the frame does not extend the
// deadline. The peer is hand-rolled over a hijacked conn because
// coder/websocket cannot emit a partial frame; it cannot answer pings
// mid-frame (a control frame may not interrupt a frame's payload).
func TestWSConn_Read_DribbledFrame_ClosesWithinDeadline(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]))
		// Final text frame, unmasked (server to client), 16-bit length 100.
		brw.Write([]byte{0x81, 126, 0x00, 100})
		if err := brw.Flush(); err != nil {
			return
		}
		tick := time.NewTicker(testReadTimeout / 10)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := conn.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}))
	wc := dialWSConn(t, srv)
	t.Cleanup(func() {
		wc.Close()
		close(stop)
		srv.Close()
	})

	_, elapsed, err := readWithin(t, wc, testReadTimeout+2*time.Second)
	if err == nil {
		t.Fatal("Read of a dribbled frame returned nil error, want non-nil")
	}
	if elapsed > testReadTimeout+time.Second {
		t.Errorf("Read returned after %v, want within deadline %v plus slack", elapsed, testReadTimeout)
	}
}

// TestWSConn_Read_IdleLongerThanDeadline_ThenMessage_DeliveredIntact pins
// the idle-safety half of the contract: the deadline bounds receipt of a
// message once it starts, never the gap before it. A peer that is silent
// for several deadlines and then sends a complete message is not dropped.
func TestWSConn_Read_IdleLongerThanDeadline_ThenMessage_DeliveredIntact(t *testing.T) {
	payload := []byte(`{"after":"idle"}`)
	wc := startScriptedPeer(t, func(ctx context.Context, c *websocket.Conn) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * testReadTimeout):
		}
		if err := c.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Errorf("peer Write: %v", err)
			return
		}
		// Read until the client closes, so its close handshake is answered.
		_, _, _ = c.Read(ctx)
	})

	got, _, err := readWithin(t, wc, 3*testReadTimeout+2*time.Second)
	if err != nil {
		t.Fatalf("Read after idle gap: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Read returned %q, want %q", got, payload)
	}
}
