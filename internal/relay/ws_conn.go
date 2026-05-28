package relay

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// writeTimeout bounds a single Send. A slow peer cannot stall a caller
// past this deadline; Send returns the library's wrapped cancellation
// error and the caller decides whether to drop the connection.
const writeTimeout = 10 * time.Second

// WSConn adapts a *websocket.Conn from github.com/coder/websocket to the
// registry's Conn interface. It owns the per-connection write mutex
// (the underlying library forbids concurrent Write) and a per-connection
// cancellation context that Close trips to abort in-flight writes.
//
// Send, ConnID, and Close are safe for concurrent use: Send serialises
// concurrent callers; ConnID is a pure getter; Close is idempotent and
// may run concurrently with Send. Read is single-caller — the per-WSConn
// forwarder goroutine is the sole reader (see internal/relay/forward.go).
type WSConn struct {
	conn   *websocket.Conn
	connID string

	writeMu sync.Mutex

	closeOnce sync.Once
	closeCtx  context.Context
	cancel    context.CancelFunc
}

// NewWSConn wraps c with the relay-assigned connection id and applies a
// per-frame read-size cap. The caller retains responsibility for the
// WebSocket handshake and for choosing connID; this constructor neither
// validates connID nor inspects c.
//
// maxFrameBytes bounds every inbound WebSocket frame read through this
// WSConn. Applied before the constructor returns — i.e. before any
// goroutine other than the constructor holds a reference — so the first
// Read against the underlying *websocket.Conn already sees the cap. If a
// peer sends a frame whose payload exceeds the cap, the library closes
// the conn with StatusMessageTooBig (1009) and the next Read surfaces a
// non-nil error.
//
// After construction, the WSConn owns c: callers must reach the
// connection only through WSConn methods. Calling c.Write or c.Close
// directly defeats the adapter's serialisation and cancellation
// guarantees.
func NewWSConn(c *websocket.Conn, connID string, maxFrameBytes int64) *WSConn {
	c.SetReadLimit(maxFrameBytes)
	ctx, cancel := context.WithCancel(context.Background())
	return &WSConn{
		conn:     c,
		connID:   connID,
		closeCtx: ctx,
		cancel:   cancel,
	}
}

// ConnID returns the constructor-supplied id. Pure getter; safe to call
// from any goroutine and never blocks. The registry calls this under
// its write lock — see registry.go UnregisterPhone.
func (w *WSConn) ConnID() string {
	return w.connID
}

// Send writes msg as a single binary WebSocket frame. Concurrent Send
// callers are serialised on a per-WSConn mutex; the wire receives whole,
// non-interleaved frames in some order. Each call has a fixed write
// deadline (writeTimeout); Send after Close returns a non-nil error.
// Callers treat any non-nil return as "drop the connection."
func (w *WSConn) Send(msg []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(w.closeCtx, writeTimeout)
	defer cancel()
	return w.conn.Write(ctx, websocket.MessageBinary, msg)
}

// Read returns the next inbound message as opaque bytes. ctx bounds the
// wait; cancellation aborts the read with the library's wrapped error.
// The message type (binary vs text) is discarded — the relay treats
// inner frames as opaque bytes. Concurrent Read callers are NOT
// supported; the per-WSConn forwarder goroutine is the sole reader.
// After Close, an in-flight Read returns with the library's close
// error from the underlying *websocket.Conn.
func (w *WSConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.conn.Read(ctx)
	return data, err
}

// Close cancels in-flight writes and closes the WebSocket with
// StatusNormalClosure. Idempotent: only the first call reaches the
// underlying *websocket.Conn; subsequent calls are no-ops. Safe to call
// concurrently with Send.
func (w *WSConn) Close() {
	w.CloseWithCode(websocket.StatusNormalClosure, "")
}

// CloseWithCode cancels in-flight writes and closes the WebSocket with
// the given code and reason. Idempotent under the same closeOnce guard
// as Close: only the first Close-family call reaches the underlying
// *websocket.Conn; later calls (whether Close or CloseWithCode) are
// no-ops. Safe to call concurrently with Send.
//
// Used by paths that need to signal a non-normal close on an *active*
// WSConn — e.g. the heartbeat goroutine emitting 1011 "heartbeat
// timeout" when a peer stops responding to pings. See ADR-0007 for why
// this lives on WSConn rather than at the call site (ADR-0005 covers
// the stillborn-WSConn handler-side pattern that this method extends).
func (w *WSConn) CloseWithCode(code websocket.StatusCode, reason string) {
	w.closeOnce.Do(func() {
		w.cancel()
		_ = w.conn.Close(code, reason)
	})
}

// Ping sends an RFC 6455 ping control frame and blocks until the
// matching pong returns or ctx expires. Pure forwarder over the
// library's Conn.Ping; does not take writeMu — github.com/coder/websocket
// serialises control frames against data writes internally.
//
// Used by runHeartbeat. A non-nil return is the caller's signal that
// the peer is unresponsive (ctx-deadline elapsed without a pong) or
// that the conn has died; runHeartbeat translates this into a
// CloseWithCode(1011, "heartbeat timeout").
func (w *WSConn) Ping(ctx context.Context) error {
	return w.conn.Ping(ctx)
}
