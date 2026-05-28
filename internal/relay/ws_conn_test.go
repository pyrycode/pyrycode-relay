package relay

import (
	"bytes"
	"context"
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
