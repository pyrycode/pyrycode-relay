package relay

import (
	"context"
	"log/slog"
)

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
	}
}
