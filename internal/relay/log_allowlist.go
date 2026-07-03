package relay

// allowedLogKeys is the closed set of structured-log keys the relay is
// permitted to emit on a slog.Logger call. Adding a key here is the
// commit-time gate that pairs with the threat-model rule in
// docs/threat-model.md § Log hygiene. TestLogKeysAreAllowlisted reads
// from this map; never inline new keys at a call site without adding
// the key here in the same commit.
var allowedLogKeys = map[string]struct{}{
	"server_id":      {},
	"conn_id":        {},
	"binary_conn_id": {},
	"device_name":    {},
	"remote":         {},
	"binary_version": {},
	"err":            {},
	"path":           {},
	"from":           {},
	"to":             {},
	// WS close code (uint16) from a daemon close directive. Non-sensitive:
	// a protocol status number, never user or key material.
	"close_code": {},
}
