package relay

// WebSocket close codes used by /v1/client. RFC 6455 reserves 4000–4999 for
// application use.
//
//   - 1000 (StatusNormalClosure)        clean close on shutdown / unregister.
//   - 4404 (no server with that id)     no binary currently holds the slot.
//
// The relay treats x-pyrycode-token as opaque — its presence is required,
// its value is never parsed, compared, or logged. Token verification is the
// binary's responsibility (per protocol-mobile.md § Phone → relay → binary).

import (
	"errors"
	"log/slog"
	"net/http"

	"nhooyr.io/websocket"
)

// ClientHandler returns the http.Handler for /v1/client: the phone-side
// WebSocket upgrade endpoint. It validates required headers, upgrades the
// connection, registers the phone in reg under the requested server-id, and
// holds the connection open until the phone closes it (or the registry tears
// it down on binary-grace expiry).
func ClientHandler(reg *Registry, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverID := r.Header.Get("X-Pyrycode-Server")
		token := r.Header.Get("X-Pyrycode-Token")
		userAgent := r.Header.Get("User-Agent")
		if serverID == "" || token == "" || userAgent == "" {
			http.Error(w, "", http.StatusBadRequest)
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
		wsconn := NewWSConn(c, connID)

		if err := reg.RegisterPhone(serverID, wsconn); err != nil {
			if errors.Is(err, ErrNoServer) {
				// Stillborn WSConn — close directly on the underlying
				// *websocket.Conn so the 4404 application code reaches the
				// wire (WSConn.Close always emits StatusNormalClosure).
				_ = c.Close(websocket.StatusCode(4404), "no server with that id")
				logger.Info("phone_register_no_server",
					"server_id", serverID,
					"remote", remoteHost(r))
				return
			}
			wsconn.Close()
			return
		}

		logger.Info("phone_registered",
			"server_id", serverID,
			"conn_id", connID,
			"device_name", deviceName,
			"remote", remoteHost(r))

		defer func() {
			reg.UnregisterPhone(serverID, connID)
			wsconn.Close()
			logger.Info("phone_unregistered",
				"server_id", serverID,
				"conn_id", connID)
		}()

		// Hold the connection open until the peer closes it (or the
		// registry tears it down on binary-grace expiry). CloseRead drains
		// control frames so the conn observes peer-close. The frame loop
		// (#6) replaces this block with a real read loop later.
		readCtx := c.CloseRead(r.Context())
		<-readCtx.Done()
	})
}
