package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors returned by Marshal and Unmarshal. Callers branch on these
// with errors.Is to distinguish "client sent garbage" from "relay bug" without
// matching error strings.
var (
	ErrEmptyConnID       = errors.New("relay: empty conn_id")
	ErrInvalidFrameJSON  = errors.New("relay: frame is not valid JSON")
	ErrMalformedEnvelope = errors.New("relay: malformed routing envelope")
	ErrMissingConnID     = errors.New("relay: routing envelope missing conn_id")
	ErrMissingFrame      = errors.New("relay: routing envelope missing frame")

	// Wake-request rejections. None of them carries any part of the
	// push_wake object: the token is never logged or put in error text.
	ErrMalformedPushWake       = errors.New("relay: malformed push_wake")
	ErrUnsupportedPushPlatform = errors.New("relay: push_wake platform not supported")
	ErrEmptyPushToken          = errors.New("relay: push_wake token is empty")
)

// Envelope is the routing wrapper exchanged between the relay and a pyrycode
// binary on the /v1/server connection. It carries an opaque inner frame
// addressed by the relay-assigned phone connection id.
//
// The relay treats Frame as opaque bytes and MUST NOT deserialise them.
// See pyrycode/pyrycode/docs/protocol-mobile.md § Routing envelope.
//
// CloseCode is the one relay-interpreted control field. On a binary→relay
// envelope a non-zero CloseCode asks the relay to deliver Frame (if present)
// to the addressed phone and then close that phone's WebSocket with this
// close code. It is the disconnect half of the routing contract: without it
// the daemon cannot tell the relay to drop a phone (auth reject, protocol
// mismatch, handshake failure), so the phone hangs on a dead session. The
// field mirrors the daemon's protocol.RoutingEnvelope.CloseCode; omitempty
// keeps every non-close envelope byte-identical to the pre-close-code shape.
//
// PushWake is the second relay-interpreted field: an envelope with no
// conn_id and a push_wake object asks the relay to wake a phone
// (docs/protocol-mobile.md § push_wake). It stays raw here so a conn_id
// envelope decodes exactly as before whatever push_wake it carries;
// parsePushWake validates it only on the wake path.
type Envelope struct {
	ConnID    string          `json:"conn_id"`
	Frame     json.RawMessage `json:"frame"`
	CloseCode uint16          `json:"close_code,omitempty"`
	PushWake  json.RawMessage `json:"push_wake,omitempty"`
}

// pushWake is the relay-addressed wake request carried in Envelope.PushWake.
type pushWake struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
}

// parsePushWake validates a push_wake object at the envelope boundary. fcm
// is the only platform the relay sends to; apns and anything else are
// rejected. The decoder error is not wrapped so no fragment of the input
// reaches error text.
func parsePushWake(raw json.RawMessage) (pushWake, error) {
	var w pushWake
	if err := json.Unmarshal(raw, &w); err != nil {
		return pushWake{}, ErrMalformedPushWake
	}
	if w.Platform != "fcm" {
		return pushWake{}, ErrUnsupportedPushPlatform
	}
	if w.Token == "" {
		return pushWake{}, ErrEmptyPushToken
	}
	return w, nil
}

// hasFrame reports whether f carries a real inner frame. An absent field
// (nil/empty) and the JSON literal null both count as "no frame" — the
// daemon emits "frame":null on a close directive that carries no final
// application frame (protocol.RoutingEnvelope.Frame has no omitempty).
func hasFrame(f json.RawMessage) bool {
	return len(f) > 0 && !bytes.Equal(f, []byte("null"))
}

// Marshal builds the JSON-encoded routing envelope for an inner frame
// addressed to the given phone connection.
//
// connID must be non-empty. frame must be syntactically valid JSON; its
// contents are otherwise opaque to the relay and are emitted byte-for-byte
// (modulo whitespace canonicalisation by encoding/json). Returns
// ErrEmptyConnID for an empty connection id and ErrInvalidFrameJSON for
// frame bytes that are not valid JSON.
func Marshal(connID string, frame []byte) ([]byte, error) {
	if connID == "" {
		return nil, ErrEmptyConnID
	}
	if !json.Valid(frame) {
		return nil, ErrInvalidFrameJSON
	}
	env := Envelope{ConnID: connID, Frame: json.RawMessage(frame)}
	out, err := json.Marshal(env)
	if err != nil {
		// Unreachable in practice: the only non-static field is a
		// json.RawMessage we have already validated. Wrap defensively.
		return nil, fmt.Errorf("relay: marshalling envelope: %w", err)
	}
	return out, nil
}

// marshalCloseNotice builds the relay→binary close notice: {conn_id,
// close_code} with no frame key, telling the binary a phone's own
// connection ended (docs/protocol-mobile.md § Routing envelope). Envelope
// is not reused because its frame field has no omitempty and would emit
// "frame":null.
func marshalCloseNotice(connID string, code uint16) ([]byte, error) {
	if connID == "" {
		return nil, ErrEmptyConnID
	}
	out, err := json.Marshal(struct {
		ConnID    string `json:"conn_id"`
		CloseCode uint16 `json:"close_code"`
	}{connID, code})
	if err != nil {
		return nil, fmt.Errorf("relay: marshalling close notice: %w", err)
	}
	return out, nil
}

// Unmarshal parses a JSON-encoded routing envelope.
//
// It returns ErrMalformedEnvelope (wrapping the underlying decoder error)
// for syntactically invalid JSON or a non-object payload, ErrMissingConnID
// when conn_id is absent, empty, or null, and ErrMissingFrame when frame
// is absent or null AND no close code is set.
//
// A wake request (conn_id absent, empty or null, and push_wake present and
// not null) is valid: it is returned with an empty ConnID and the raw
// PushWake, whatever frame it carries. Callers branch on ConnID == "".
//
// A close directive (CloseCode != 0) may legitimately carry no frame: the
// daemon closes a phone without a final application frame, e.g. an auth
// reject with no error body. Such an envelope is valid and ErrMissingFrame
// is not returned.
//
// The Frame field of the returned Envelope is the verbatim bytes of the
// inner frame (modulo insignificant whitespace normalised by the JSON
// decoder). The relay MUST NOT inspect them.
func Unmarshal(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}
	if env.ConnID == "" {
		// Same presence rule as a frame: absent and null mean "none".
		if hasFrame(env.PushWake) {
			return env, nil
		}
		return Envelope{}, ErrMissingConnID
	}
	if env.CloseCode == 0 && !hasFrame(env.Frame) {
		return Envelope{}, ErrMissingFrame
	}
	return env, nil
}
