package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// ErrInvalidConnCap is returned by NewConnCap for a non-positive cap. The
// relay refuses to start on it rather than run uncapped.
var ErrInvalidConnCap = errors.New("relay: connection cap must be positive")

// ConnCap is a relay-wide cap on live WebSocket connections. One instance
// wraps both upgrade handlers, so binaries and phones draw from one pool.
//
// A slot is taken before the wrapped handler runs and released when it
// returns. ServerHandler and ClientHandler stay inside ServeHTTP for the
// whole life of the connection and return only after their deferred
// cleanup, so a slot covers every close path: peer close, read or write
// error, heartbeat timeout, grace-expiry teardown, shutdown, and the
// reject-after-accept closes (4404, 4409, 4429).
//
// Connections still in the TLS or header phase are not counted;
// ReadHeaderTimeout and the per-IP rate limiter bound those.
type ConnCap struct {
	slots  chan struct{}
	logger *slog.Logger
}

// NewConnCap returns a cap of max live connections. max <= 0 returns an
// error wrapping ErrInvalidConnCap.
func NewConnCap(max int, logger *slog.Logger) (*ConnCap, error) {
	if max <= 0 {
		return nil, fmt.Errorf("max-connections %d: %w", max, ErrInvalidConnCap)
	}
	return &ConnCap{slots: make(chan struct{}, max), logger: logger}, nil
}

// Wrap returns next behind the cap. When every slot is taken the request
// gets 503 Service Unavailable with an empty body, one Warn line is
// logged, and next never runs — so websocket.Accept never runs either.
// The try-acquire never blocks: an over-cap request does not queue.
//
// No header is read on this path; the log line carries only the route and
// the peer host.
func (c *ConnCap) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case c.slots <- struct{}{}:
		default:
			http.Error(w, "", http.StatusServiceUnavailable)
			c.logger.Warn("conn_cap_reached",
				"path", r.URL.Path,
				"remote", remoteHost(r))
			return
		}
		defer func() { <-c.slots }()
		next.ServeHTTP(w, r)
	})
}

// inUse reports the number of slots currently held.
func (c *ConnCap) inUse() int {
	return len(c.slots)
}
