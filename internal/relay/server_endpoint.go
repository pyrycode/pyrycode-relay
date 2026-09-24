package relay

// WebSocket close codes used by /v1/server. RFC 6455 reserves 4000–4999 for
// application use.
//
//   - 1000 (StatusNormalClosure)        clean close on shutdown / release.
//   - 4409 (server-id already claimed)  another binary holds the slot and
//     answered the conflict liveness probe.
//   - 1011 (heartbeat timeout)          sent to an incumbent that did not
//     answer the probe and was taken over (#112).
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

	"github.com/coder/websocket"
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
// A claim for a held server-id pings the incumbent for up to
// conflictProbeTimeout. An incumbent that answers keeps the slot and the
// claim is closed with 4409; one that does not is taken over with the
// reclaim semantics (see Registry.TakeoverServer), so a crash-restarted
// daemon need not wait for the heartbeat to notice its dead connection.
//
// maxFrameBytes is the per-frame read cap threaded into NewWSConn; see
// docs/specs/architecture/29-wsconn-read-limit.md for the derivation.
//
// metrics is nil-safe: callers that don't observe upgrade outcomes pass
// nil and every counter call no-ops.
func ServerHandler(reg *Registry, logger *slog.Logger, grace time.Duration, maxFrameBytes int64, metrics *UpgradeMetrics) http.Handler {
	return serverHandler(reg, logger, grace, conflictProbeTimeout, maxFrameBytes, metrics)
}

// conflictProbeTimeout bounds the liveness probe a conflicting claim sends
// to the incumbent binary (#112). It is the ticket's 10s ceiling because a
// live incumbent's pong is only read between its forwarder's frames, and
// one synchronous phone Send can hold the forwarder for writeTimeout.
const conflictProbeTimeout = 10 * time.Second

// serverHandler is ServerHandler with the conflict probe timeout exposed,
// so tests can shorten it.
func serverHandler(reg *Registry, logger *slog.Logger, grace, probeTimeout time.Duration, maxFrameBytes int64, metrics *UpgradeMetrics) http.Handler {
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
			// github.com/coder/websocket has already written a 4xx response.
			return
		}

		connID := "server-" + serverID + "-" + randHex8()
		wsconn := NewWSConn(c, connID, maxFrameBytes)

		err = reg.ClaimServer(serverID, wsconn)
		if errors.Is(err, ErrServerIDConflict) {
			// The incumbent may be a binary whose network dropped silently
			// and that the heartbeat has not caught yet: probe it, and take
			// its slot if it does not answer (#112).
			err = takeoverUnresponsive(r.Context(), reg, serverID, wsconn, probeTimeout)
			if err == nil {
				logger.Info("server_id_takeover",
					"server_id", serverID,
					"remote", remoteHost(r))
			}
		}
		if err != nil {
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
			// The only other error is the request context ending during
			// the probe (server shutdown); close without logging, as for
			// any other shutdown-time teardown.
			wsconn.Close()
			return
		}

		metrics.ServerAccept()
		logger.Info("server_claimed",
			"server_id", serverID,
			"binary_version", versionHeader,
			"remote", remoteHost(r))

		defer func() {
			// Scoped to wsconn: if a conflicting claim took the slot over
			// from this conn (#112), releasing it would arm a grace timer
			// that deletes the new binary.
			reg.ScheduleReleaseServerIfHeld(serverID, wsconn, grace)
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

// pinger is the liveness probe a conflicting claim needs from the
// incumbent binary. *WSConn satisfies it.
type pinger interface {
	Ping(ctx context.Context) error
}

// takeoverUnresponsive runs after ClaimServer refused conn with
// ErrServerIDConflict. It pings the incumbent for at most timeout, outside
// any registry lock. An answer, or an incumbent that cannot be pinged,
// yields ErrServerIDConflict: a live binary is never displaced. No answer
// hands the slot to conn through the identity-scoped TakeoverServer, then
// closes the incumbent with the heartbeat's 1011 "heartbeat timeout",
// which is what the heartbeat would have sent once it noticed. A probe cut
// short by ctx (shutdown) returns ctx's error and never counts as silence.
func takeoverUnresponsive(ctx context.Context, reg *Registry, serverID string, conn Conn, timeout time.Duration) error {
	incumbent, held := reg.BinaryFor(serverID)
	if held {
		p, ok := incumbent.(pinger)
		if !ok {
			return ErrServerIDConflict
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		err := p.Ping(probeCtx)
		cancel()
		if err == nil {
			return ErrServerIDConflict
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := reg.TakeoverServer(serverID, incumbent, conn); err != nil {
		return err
	}
	if incumbent != nil {
		// A close handshake with a dead peer blocks until the library's
		// own timeout, so it must not hold up the new binary's handler.
		go closeWithCode(incumbent, websocket.StatusInternalError, "heartbeat timeout")
	}
	return nil
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
