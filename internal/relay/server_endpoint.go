package relay

// WebSocket close codes used by /v1/server. RFC 6455 reserves 4000–4999 for
// application use.
//
//   - 1000 (StatusNormalClosure)        clean close on shutdown / release.
//   - 4409 (server-id already claimed)  another binary holds the slot.
//
// Future tickets layer 4401 (token), 4404 (no server) on /v1/client and
// frame-forward errors on /v1/server.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"nhooyr.io/websocket"
)

// ServerHandler returns the http.Handler for /v1/server: the binary-side
// WebSocket upgrade endpoint. It validates required headers, upgrades the
// connection, claims the server-id slot in reg, and holds the connection
// open until the binary closes it (or the slot is released).
//
// On binary disconnect (clean close, network error, ping timeout) the
// server-id slot is scheduled for release after grace; a reconnect within
// that window inherits the slot atomically (registry-side reclaim path,
// see Registry.ScheduleReleaseServer). Production passes 30*time.Second
// per protocol spec § Authentication → Binary → relay.
//
// maxFrameBytes is the per-frame read cap threaded into NewWSConn; see
// docs/specs/architecture/29-wsconn-read-limit.md for the derivation.
//
// metrics is nil-safe: callers that don't observe upgrade outcomes pass
// nil and every counter call no-ops.
func ServerHandler(reg *Registry, logger *slog.Logger, grace time.Duration, maxFrameBytes int64, metrics *UpgradeMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverID := r.Header.Get("X-Pyrycode-Server")
		versionHeader := r.Header.Get("X-Pyrycode-Version")
		userAgent := r.Header.Get("User-Agent")
		if serverID == "" || versionHeader == "" || userAgent == "" {
			http.Error(w, "", http.StatusBadRequest)
			metrics.ServerHeaderReject()
			return
		}

		// OriginPatterns: ["*"] — this endpoint is not browser-facing.
		// The custom-header gate above structurally excludes browser-driven
		// CSWSH attempts: raw WebSocket from a browser cannot set custom
		// request headers, and a fetch with custom headers triggers CORS
		// pre-flight that this endpoint does not satisfy.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			// nhooyr.io/websocket has already written a 4xx response.
			return
		}

		connID := "server-" + serverID + "-" + randHex8()
		wsconn := NewWSConn(c, connID, maxFrameBytes)

		if err := reg.ClaimServer(serverID, wsconn); err != nil {
			if errors.Is(err, ErrServerIDConflict) {
				// Close directly on the underlying *websocket.Conn so we
				// can send the 4409 close code; WSConn.Close always emits
				// StatusNormalClosure. This is a stillborn WSConn — no
				// Send was attempted, no goroutine holds writeMu — so the
				// "reach the connection only through WSConn methods"
				// invariant is preserved in spirit.
				_ = c.Close(websocket.StatusCode(4409), "server-id already claimed")
				metrics.ServerIDConflict()
				logger.Info("server_id_conflict",
					"server_id", serverID,
					"remote", remoteHost(r))
				return
			}
			// No other error is reachable from the current registry; close
			// defensively without logging, since the diff that introduces
			// such an error should add the log.
			wsconn.Close()
			return
		}

		metrics.ServerAccept()
		logger.Info("server_claimed",
			"server_id", serverID,
			"binary_version", versionHeader,
			"remote", remoteHost(r))

		defer func() {
			reg.ScheduleReleaseServer(serverID, grace)
			wsconn.Close()
			logger.Info("server_released", "server_id", serverID)
		}()

		// Heartbeat: per-conn goroutine sends RFC 6455 pings at
		// heartbeatInterval and closes the conn with 1011 "heartbeat
		// timeout" if a pong does not arrive within heartbeatTimeout.
		// cancelHB runs first under LIFO defer order: it signals the
		// goroutine to exit cleanly before the release defer closes the
		// conn. closeOnce ensures whichever close fires first wins.
		hbCtx, cancelHB := context.WithCancel(r.Context())
		defer cancelHB()
		go runHeartbeat(hbCtx, wsconn, heartbeatInterval, heartbeatTimeout)

		// Binary-side read pump (#26): unwraps each inbound routing
		// envelope and writes its inner frame to the phone matching
		// env.ConnID under serverID. Blocks until the binary closes the
		// WS or ctx cancels. Per-frame errors (malformed envelope,
		// unknown conn_id, phone Send failure) log+drop and continue —
		// the binary connection is not torn down for a single bad frame.
		// Return value is observability-only; the forwarder logs the
		// cause.
		_ = StartBinaryForwarder(r.Context(), reg, serverID, wsconn, logger)
	})
}

// randHex8 returns 8 hex chars (32 bits) from crypto/rand. Panics on a
// crypto/rand read failure — the OS RNG failing is not a recoverable error
// in this process.
func randHex8() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// remoteHost strips the port from r.RemoteAddr. Falls back to RemoteAddr
// verbatim if SplitHostPort fails. Used only for logging.
func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
