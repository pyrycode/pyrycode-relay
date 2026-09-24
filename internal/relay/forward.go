package relay

import (
	"context"
	"log/slog"

	"github.com/coder/websocket"
)

// phoneCloser is the optional close-with-code capability StartBinaryForwarder
// needs to honor a routing envelope's close_code. Defined at the consumer so
// the Conn interface (registry.go) stays minimal and the many Conn test mocks
// need no change. *WSConn satisfies it via CloseWithCode; a Conn that does not
// is closed with a normal closure as a fallback.
type phoneCloser interface {
	CloseWithCode(code websocket.StatusCode, reason string)
}

// closePhone closes phone with the given WS close code, using CloseWithCode
// when the connection supports it and falling back to a normal Close.
func closePhone(phone Conn, code uint16) {
	if cc, ok := phone.(phoneCloser); ok {
		cc.CloseWithCode(websocket.StatusCode(code), "")
		return
	}
	phone.Close()
}

// phoneQueue is the non-blocking delivery capability StartBinaryForwarder
// prefers: Enqueue hands a frame, and a close directive when closeCode is
// non-zero, to the phone's own writer and returns without touching the
// socket. Defined at the consumer like phoneCloser. *phoneOutbox, which
// ClientHandler registers for every phone, satisfies it; a Conn that does
// not is written synchronously on the forwarder.
type phoneQueue interface {
	Enqueue(frame []byte, closeCode uint16) error
}

// phoneSource is the read-side contract the forwarder needs from a phone
// connection. Defined at the consumer (this file), not on WSConn, so tests
// can substitute a fake. Production passes *WSConn, which satisfies it via
// its Read method.
//
// Concurrent callers are NOT supported; the forwarder goroutine is the sole
// reader (matches WSConn.Read's contract).
type phoneSource interface {
	ConnID() string
	Read(ctx context.Context) ([]byte, error)
}

// StartPhoneForwarder runs the per-phone read pump synchronously: it reads
// frames from phone, wraps each in the routing envelope keyed by
// phone.ConnID(), looks up the binary holding serverID, and writes the
// wrapped envelope to that binary. Returns when phone.Read errors, ctx is
// cancelled, the registered binary disappears, or a Send to the binary
// fails — the underlying error is returned for observability.
//
// The caller's defer (in /v1/client) handles UnregisterPhone and Close;
// the forwarder must NOT touch either. The relay treats inner frames as
// opaque bytes: only Marshal's structural json.Valid check inspects them.
//
// Despite the Start verb, the call is synchronous; the verb matches the AC.
func StartPhoneForwarder(
	ctx context.Context,
	reg *Registry,
	serverID string,
	phone phoneSource,
	logger *slog.Logger,
) error {
	for {
		frame, err := phone.Read(ctx)
		if err != nil {
			logger.Info("phone_forwarder_read_end",
				"server_id", serverID,
				"conn_id", phone.ConnID(),
				"err", err)
			return err
		}

		wrapped, err := Marshal(phone.ConnID(), frame)
		if err != nil {
			logger.Warn("phone_forwarder_marshal_err",
				"server_id", serverID,
				"conn_id", phone.ConnID(),
				"err", err)
			return err
		}

		binary, ok := reg.BinaryFor(serverID)
		if !ok {
			logger.Info("phone_forwarder_no_binary",
				"server_id", serverID,
				"conn_id", phone.ConnID())
			return nil
		}

		if err := binary.Send(wrapped); err != nil {
			logger.Info("phone_forwarder_send_failed",
				"server_id", serverID,
				"conn_id", phone.ConnID(),
				"err", err)
			return err
		}

		if h := reg.onPhoneForwarded; h != nil {
			h()
		}
	}
}

// binarySource is the read-side contract the forwarder needs from a binary
// connection. Defined at the consumer (this file), not on WSConn, so tests
// can substitute a fake. Production passes *WSConn, which satisfies it via
// its Read method.
//
// Structurally identical to phoneSource; the distinct named type documents
// the call site's intent and keeps the two Start*Forwarder signatures
// self-describing. Concurrent Read callers are NOT supported (matches
// WSConn.Read's contract).
type binarySource interface {
	ConnID() string
	Read(ctx context.Context) ([]byte, error)
}

// StartBinaryForwarder runs the per-binary read pump synchronously: it
// reads envelopes from binary, unwraps each, finds the phone registered
// under serverID whose ConnID equals env.ConnID, and writes env.Frame
// (the verbatim inner bytes) to that phone.
//
// When an envelope carries a non-zero CloseCode it is a close directive:
// the forwarder delivers env.Frame (if present) and then closes the phone's
// socket with that WS close code, honoring the daemon's disconnect requests.
// A missing addressed phone (already gone) is a normal race, logged + skipped.
//
// A phone that implements phoneQueue (every production phone, via
// phoneOutbox) gets the frame and any close directive enqueued instead: its
// own writer delivers them in order, so a stalled phone never holds this
// loop or delays its siblings (#113). A refused enqueue (backlog full, phone
// gone) is logged and skipped like any other per-frame drop.
//
// A push_wake envelope (no conn_id) goes to the registry's PushWaker, which
// sends asynchronously; with no waker set, push is off and the wake is
// logged and dropped.
//
// Returns when binary.Read errors or ctx is cancelled. Does NOT return on
// per-frame errors: a malformed envelope, an unknown conn_id, or a phone
// Send failure all log + drop + continue. A single bad frame from the
// binary MUST NOT tear the binary connection down — phones come and go,
// and an envelope addressing a just-disconnected phone is a normal race,
// not a binary fault. This diverges from StartPhoneForwarder, where the
// only sink is the binary so a Send failure ends the loop.
//
// The caller's defer (in /v1/server) handles ScheduleReleaseServer,
// wsconn.Close, and the heartbeat cancel; the forwarder must NOT touch
// any of those. The relay treats inner frames as opaque bytes: only
// Unmarshal's structural checks inspect the envelope, never env.Frame.
//
// Despite the Start verb, the call is synchronous; the verb matches the
// AC and mirrors StartPhoneForwarder.
func StartBinaryForwarder(
	ctx context.Context,
	reg *Registry,
	serverID string,
	binary binarySource,
	logger *slog.Logger,
) error {
	for {
		wrapped, err := binary.Read(ctx)
		if err != nil {
			logger.Info("binary_forwarder_read_end",
				"server_id", serverID,
				"binary_conn_id", binary.ConnID(),
				"err", err)
			return err
		}

		env, err := Unmarshal(wrapped)
		if err != nil {
			logger.Warn("binary_forwarder_unmarshal_err",
				"server_id", serverID,
				"binary_conn_id", binary.ConnID(),
				"err", err)
			continue
		}

		// Wake request: Unmarshal returns an empty ConnID only for a
		// push_wake envelope, which addresses the relay, not a phone. The
		// waker sends off this loop; a refused wake is logged and dropped.
		if env.ConnID == "" {
			if err := reg.pushWaker.Request(serverID, env.PushWake); err != nil {
				logger.Warn("binary_forwarder_push_wake_dropped",
					"server_id", serverID,
					"binary_conn_id", binary.ConnID(),
					"err", err)
			}
			continue
		}

		phone, ok := reg.PhoneFor(serverID, env.ConnID)
		if !ok {
			logger.Warn("binary_forwarder_unknown_conn_id",
				"server_id", serverID,
				"conn_id", env.ConnID)
			continue
		}

		// Queued delivery (#113): the phone's own writer sends the frame
		// and applies any close directive in order, so this loop never
		// waits on a phone socket.
		if q, ok := phone.(phoneQueue); ok {
			if err := q.Enqueue(env.Frame, env.CloseCode); err != nil {
				logger.Info("binary_forwarder_phone_enqueue_failed",
					"server_id", serverID,
					"conn_id", env.ConnID,
					"err", err)
			}
			continue
		}

		// Close directive: the daemon asks the relay to deliver the final
		// frame (if any) and then close the phone's socket with the given WS
		// close code. This is the disconnect half of the routing contract —
		// an auth reject, a protocol mismatch, or a handshake failure. Without
		// it the phone never learns the session died and hangs on a dead
		// connection. The frame-then-close ordering matches the daemon's
		// atomic Frame+CloseCode emit (docs/protocol-mobile.md § Error handling).
		if env.CloseCode != 0 {
			if hasFrame(env.Frame) {
				if err := phone.Send(env.Frame); err != nil {
					logger.Info("binary_forwarder_close_frame_send_failed",
						"server_id", serverID,
						"conn_id", env.ConnID,
						"close_code", env.CloseCode,
						"err", err)
				} else if h := reg.onBinaryForwarded; h != nil {
					h()
				}
			}
			closePhone(phone, env.CloseCode)
			logger.Info("binary_forwarder_phone_closed",
				"server_id", serverID,
				"conn_id", env.ConnID,
				"close_code", env.CloseCode)
			continue
		}

		if err := phone.Send(env.Frame); err != nil {
			logger.Info("binary_forwarder_phone_send_failed",
				"server_id", serverID,
				"conn_id", env.ConnID,
				"err", err)
			continue
		}

		if h := reg.onBinaryForwarded; h != nil {
			h()
		}
	}
}
