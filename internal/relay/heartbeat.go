package relay

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// heartbeatInterval and heartbeatTimeout implement the per-connection
// liveness regime from protocol-mobile.md § Heartbeat: send an RFC 6455
// ping every interval; if the matching pong has not arrived within
// timeout of the most recent ping, treat the connection as dead. Worst-
// case dead-connection detection is interval+timeout = 60s.
//
// File-level constants (not exposed as flags in v1) because two wiring
// entry points — /v1/server and /v1/client — share the same value, and
// the AC names them as such. Tests in heartbeat_test.go pass shorter
// values directly to runHeartbeat.
const (
	heartbeatInterval = 30 * time.Second
	heartbeatTimeout  = 30 * time.Second
)

// runHeartbeat sends a Ping every interval and closes w with
// 1011 "heartbeat timeout" if a pong does not arrive within timeout.
//
// Returns when ctx is cancelled (clean-shutdown path) without calling
// CloseWithCode itself: the handler's existing release defer owns the
// clean-close path. runHeartbeat owns ONLY the timeout-driven close.
//
// Two cancellation cases must be distinguished. In both, ctx has fired
// while a Ping was either pending or in flight; in neither does
// runHeartbeat close the conn — the handler defer will. The second
// case (Ping returns a ctx-cancellation error) is detected by checking
// ctx.Err() after the call.
func runHeartbeat(ctx context.Context, w *WSConn, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			err := w.Ping(pingCtx)
			cancel()
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			w.CloseWithCode(websocket.StatusInternalError, "heartbeat timeout")
			return
		}
	}
}
