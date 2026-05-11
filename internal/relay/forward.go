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

		var phone Conn
		for _, p := range reg.PhonesFor(serverID) {
			if p.ConnID() == env.ConnID {
				phone = p
				break
			}
		}
		if phone == nil {
			logger.Warn("binary_forwarder_unknown_conn_id",
				"server_id", serverID,
				"conn_id", env.ConnID)
			continue
		}

		if err := phone.Send(env.Frame); err != nil {
			logger.Info("binary_forwarder_phone_send_failed",
				"server_id", serverID,
				"conn_id", env.ConnID,
				"err", err)
			continue
		}
	}
}
