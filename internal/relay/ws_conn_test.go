package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// startEcho stands up an httptest server whose handler upgrades to a
// WebSocket and forwards every received frame to a buffered channel.
// It returns a connected WSConn (client side), the receive channel,
// and a cleanup function the caller defers.
func startEcho(t *testing.T) (*WSConn, <-chan []byte, func()) {
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

	wc := NewWSConn(client, "test-conn-id")
	cleanup := func() {
		wc.Close()
		srv.Close()
	}
	return wc, received, cleanup
}

func TestWSConn_ConnID_ReturnsConstructorValue(t *testing.T) {
	wc := NewWSConn(nil, "abc")
	if got := wc.ConnID(); got != "abc" {
		t.Fatalf("ConnID() = %q, want %q", got, "abc")
	}
}

func TestWSConn_ConcurrentSend_ProducesIntactFrames(t *testing.T) {
	wc, received, cleanup := startEcho(t)
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
	wc, _, cleanup := startEcho(t)
	defer cleanup()

	wc.Close()
	wc.Close()
}

func TestWSConn_SendAfterClose_ReturnsError(t *testing.T) {
	wc, _, cleanup := startEcho(t)
	defer cleanup()

	wc.Close()
	if err := wc.Send([]byte("late")); err == nil {
		t.Fatal("Send after Close returned nil error, want non-nil")
	}
}
