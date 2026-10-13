package relay

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// shutdownWriteGate holds the first post-upgrade transport write. Entry proves
// the WebSocket library has armed its write context before shutdown begins.
type shutdownWriteGate struct {
	net.Conn
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *shutdownWriteGate) Write(p []byte) (int, error) {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		select {
		case <-c.release:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *shutdownWriteGate) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestShutdown_InFlightWriteGoingAway(t *testing.T) {
	for _, phone := range []bool{false, true} {
		name := "binary"
		if phone {
			name = "phone_outbox"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			payload := []byte(`{"opaque":"in-flight ciphertext"}`)
			peerFrame := make(chan []byte, 1)
			peerClose := make(chan error, 1)
			peerDone := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(peerDone)
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					peerClose <- err
					return
				}
				defer c.CloseNow()
				_, data, err := c.Read(ctx)
				if err == nil {
					peerFrame <- data
					_, _, err = c.Read(ctx)
				}
				peerClose <- err
			}))
			defer srv.Close()
			var gate *shutdownWriteGate
			transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				gate = &shutdownWriteGate{Conn: c, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
				return gate, nil
			}}
			defer transport.CloseIdleConnections()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
				HTTPClient: &http.Client{Transport: transport},
			})
			if err != nil {
				t.Fatal(err)
			}
			ws := NewWSConn(c, "in-flight", 256*1024)
			var workers sync.WaitGroup
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			// Always unblock the transport and join the outbox, Send and Shutdown,
			// including on the deliberately failing pre-fix assertion.
			defer func() {
				release()
				c.CloseNow()
				cancel()
				workers.Wait()
				<-peerDone
			}()
			reg := NewRegistry()
			logger := discardLogger()
			written := make(chan error, 1)
			if phone {
				if err := reg.ClaimServer("shutdown", &fakeConn{id: "binary"}); err != nil {
					t.Fatal(err)
				}
				outbox := newPhoneOutbox(ws, "shutdown", phoneOutboxBudget, func() { written <- nil }, logger)
				defer outbox.stop()
				if err := reg.RegisterPhone("shutdown", outbox); err != nil {
					t.Fatal(err)
				}
				workers.Add(1)
				go func() { defer workers.Done(); outbox.run() }()
				gate.armed.Store(true)
				if err := outbox.Enqueue(payload, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := reg.ClaimServer("shutdown", ws); err != nil {
					t.Fatal(err)
				}
				gate.armed.Store(true)
				workers.Add(1)
				go func() { defer workers.Done(); written <- ws.Send(payload) }()
			}
			select {
			case <-gate.entered:
			case <-ctx.Done():
				t.Fatal("write did not reach transport")
			}
			drained := make(chan error, 1)
			workers.Add(1)
			go func() { defer workers.Done(); drained <- Shutdown(ctx, logger, reg) }()
			select {
			case <-ws.closeCtx.Done():
			case <-ctx.Done():
				t.Fatal("close did not begin")
			}
			lateSend := make(chan error, 1)
			workers.Add(1)
			go func() { defer workers.Done(); lateSend <- ws.Send(payload) }()
			// A held write must remain alive after close starts. This bounded
			// negative assertion detects cancellation's transport-close callback;
			// the gate, not this timer, establishes the overlap and readiness.
			select {
			case <-gate.closed:
				t.Error("close cancelled the active write and destroyed the transport before its 1001 handshake")
			case <-time.After(100 * time.Millisecond):
			}
			release()
			select {
			case data := <-peerFrame:
				if !bytes.Equal(data, payload) {
					t.Errorf("in-flight frame changed: %q", data)
				}
			case <-ctx.Done():
				t.Error("in-flight frame was not delivered")
			}
			select {
			case err := <-peerClose:
				if websocket.CloseStatus(err) != websocket.StatusGoingAway {
					t.Errorf("wire close = %v, want 1001", err)
				}
			case <-ctx.Done():
				t.Error("peer did not observe close")
			}
			select {
			case err := <-drained:
				if err != nil {
					t.Errorf("Shutdown: %v", err)
				}
			case <-ctx.Done():
				t.Error("shutdown did not join its close")
			}
			select {
			case err := <-written:
				if err != nil {
					t.Errorf("active Send: %v", err)
				}
			case <-ctx.Done():
				t.Error("active write did not complete")
			}
			if err := ws.Send(payload); err == nil {
				t.Error("Send after close began succeeded")
			}
			select {
			case err := <-lateSend:
				if err == nil {
					t.Error("Send queued after close began succeeded")
				}
			case <-ctx.Done():
				t.Error("Send queued after close began did not return")
			}
			ws.Close() // Cleanup cannot replace the already-chosen close code.
		})
	}
}
