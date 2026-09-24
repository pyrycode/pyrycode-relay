package relay

// WebSocket close codes used by /v1/client. RFC 6455 reserves 4000–4999 for
// application use.
//
//   - 1000 (StatusNormalClosure)        clean close on shutdown / unregister.
//   - 4404 (no server with that id)     no binary currently holds the slot.
//   - 4429 (too many phones for server-id)
//                                       phones-per-server-id cap exceeded.
//
// The relay treats x-pyrycode-token as opaque — its presence is required,
// its value is never parsed, compared, or logged. Token verification is the
// binary's responsibility (per protocol-mobile.md § Phone → relay → binary).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
)

// ClientHandler returns the http.Handler for /v1/client: the phone-side
// WebSocket upgrade endpoint. It validates required headers, upgrades the
// connection, registers the phone in reg under the requested server-id, and
// holds the connection open until the phone closes it (or the registry tears
// it down on binary-grace expiry).
//
// maxFrameBytes is the per-frame read cap threaded into NewWSConn; see
// docs/specs/architecture/29-wsconn-read-limit.md for the derivation.
//
// maxPhones caps the number of phones that may register against a single
// server-id at one time; an over-cap registration is rejected with WS close
// code 4429. maxPhones <= 0 disables the cap (used by tests that do not
// exercise the boundary).
//
// metrics is nil-safe: callers that don't observe upgrade outcomes pass
// nil and every counter call no-ops.
func ClientHandler(reg *Registry, logger *slog.Logger, maxFrameBytes int64, maxPhones int, metrics *UpgradeMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverID := r.Header.Get("X-Pyrycode-Server")
		token := r.Header.Get("X-Pyrycode-Token")
		userAgent := r.Header.Get("User-Agent")
		if serverID == "" || token == "" || userAgent == "" {
			http.Error(w, "", http.StatusBadRequest)
			metrics.ClientHeaderReject()
			return
		}
		// token goes out of scope here unread: never parsed, compared, or
		// logged. The binary owns token verification (protocol-mobile.md).
		deviceName := r.Header.Get("X-Pyrycode-Device-Name")

		// OriginPatterns: ["*"] — same rationale as /v1/server: the
		// custom-header gate above structurally excludes browser-driven
		// CSWSH attempts.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}

		connID := "client-" + serverID + "-" + randHex8()
		wsconn := NewWSConn(c, connID, maxFrameBytes)
		// The registry holds the phone behind its own bounded delivery
		// queue (#113), so the binary forwarder never waits on this socket.
		phone := newPhoneOutbox(wsconn, serverID, phoneOutboxDepth, reg.onBinaryForwarded, logger)

		if err := reg.RegisterPhoneCapped(serverID, phone, maxPhones); err != nil {
			if errors.Is(err, ErrNoServer) {
				// Stillborn WSConn — close directly on the underlying
				// *websocket.Conn so the 4404 application code reaches the
				// wire (WSConn.Close always emits StatusNormalClosure).
				_ = c.Close(websocket.StatusCode(4404), "no server with that id")
				metrics.ClientNoServer()
				logger.Info("phone_register_no_server",
					"server_id", serverID,
					"remote", remoteHost(r))
				return
			}
			if errors.Is(err, ErrPhonesAtCap) {
				_ = c.Close(websocket.StatusCode(4429), "too many phones for server-id")
				metrics.ClientPhonesAtCap()
				logger.Info("phone_register_at_cap",
					"server_id", serverID,
					"remote", remoteHost(r))
				return
			}
			wsconn.Close()
			return
		}

		metrics.ClientAccept()
		logger.Info("phone_registered",
			"server_id", serverID,
			"conn_id", connID,
			"device_name", deviceName,
			"remote", remoteHost(r))

		// The queue's writer lives exactly as long as the connection: the
		// defer below closes the conn (aborting any in-flight write), stops
		// the queue and waits for the writer before the handler returns.
		outboxDone := make(chan struct{})
		go func() {
			defer close(outboxDone)
			phone.run()
		}()

		defer func() {
			reg.UnregisterPhone(serverID, connID)
			wsconn.Close()
			phone.stop()
			<-outboxDone
			logger.Info("phone_unregistered",
				"server_id", serverID,
				"conn_id", connID)
		}()

		// Heartbeat: per-conn goroutine sends RFC 6455 pings at
		// heartbeatInterval and closes the conn with 1011 "heartbeat
		// timeout" if a pong does not arrive within heartbeatTimeout.
		// cancelHB runs first under LIFO defer order: it signals the
		// goroutine to exit cleanly before the unregister defer closes
		// the conn. closeOnce ensures whichever close fires first wins.
		hbCtx, cancelHB := context.WithCancel(r.Context())
		defer cancelHB()
		go runHeartbeat(hbCtx, wsconn, heartbeatInterval, heartbeatTimeout)

		// Phone-side read pump (#25): wraps each inbound frame in the
		// routing envelope and writes it to the binary holding serverID.
		// Blocks until the phone closes the WS, ctx cancels, the binary
		// disappears, or a Send to the binary fails. Replaces the old
		// CloseRead+Done placeholder. Return value is observability-only;
		// the forwarder logs the cause.
		_ = StartPhoneForwarder(r.Context(), reg, serverID, wsconn, logger)
	})
}
