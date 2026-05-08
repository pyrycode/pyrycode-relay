# Routing envelope

The relay forwards WebSocket frames between phones and a pyrycode binary. On the binary-side connection it wraps each frame in a small JSON envelope so the binary knows which phone connection a frame belongs to:

```json
{ "conn_id": "c-7f3a...", "frame": { /* inner envelope, opaque to relay */ } }
```

This package provides the canonical Go vocabulary for that wrapper. All future routing-layer code (header validation, registry, frame forwarding) uses these helpers — no inline JSON.

Authoritative wire spec: [`pyrycode/pyrycode/docs/protocol-mobile.md` § Routing envelope](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#routing-envelope).

## API

Package `internal/relay` (`envelope.go`):

```go
type Envelope struct {
    ConnID string          `json:"conn_id"`
    Frame  json.RawMessage `json:"frame"`
}

func Marshal(connID string, frame []byte) ([]byte, error)
func Unmarshal(data []byte) (Envelope, error)
```

Sentinel errors (branch with `errors.Is`):

| Error | Returned by | When |
|---|---|---|
| `ErrEmptyConnID` | `Marshal` | `connID == ""` |
| `ErrInvalidFrameJSON` | `Marshal` | `frame` is nil/empty/not valid JSON |
| `ErrMalformedEnvelope` | `Unmarshal` | outer JSON parse fails or payload isn't a JSON object (wraps the decoder error) |
| `ErrMissingConnID` | `Unmarshal` | `conn_id` absent, empty string, or null |
| `ErrMissingFrame` | `Unmarshal` | `frame` absent or JSON null |

## Opacity invariant

`Frame` is the only field the relay ever forwards from the wire. The relay never deserialises it. The type enforces this via `json.RawMessage`, which preserves the underlying bytes verbatim through marshal/unmarshal (modulo insignificant whitespace canonicalised by `encoding/json`).

Round-trip guarantee: `Marshal(connID, frame)` → `Unmarshal(...)` → `got.Frame` is byte-equal to `frame` after `json.Compact` on both sides.

## Validation rules

`Marshal` validates *envelope structure* and *inner-frame syntax*:

- non-empty `connID`
- `json.Valid(frame)` — also rejects nil/empty since `json.Valid(nil)` and `json.Valid([]byte{})` are both false
- inner-frame *semantics* are not inspected

`Unmarshal` validates *envelope structure* only:

- outer payload parses as a JSON object
- `conn_id` present and non-empty (absent / `""` / `null` collapse to `ErrMissingConnID`)
- `frame` present and not null (`len(env.Frame) == 0` covers absent; explicit `bytes.Equal(env.Frame, []byte("null"))` covers `"frame": null`, which would otherwise pass length validation as the 4-byte string `"null"`)
- inner-frame syntax is not re-validated; `json.RawMessage` defers parsing forever

What is deliberately *not* validated:

- `conn_id` format (length, character set, prefix) — owned by the conn-id generation ticket; this layer accepts any non-empty string.
- inner-frame content — violating the opacity invariant.
- envelope or frame size — the WS read loop will enforce read limits in a later ticket.

## Concurrency

Both functions are pure and stateless. No goroutines, no shared state, no I/O. Safe to call concurrently.

## Related

- [ADR-0001: Routing envelope shape and opacity](../decisions/0001-routing-envelope-shape-and-opacity.md) — why `json.RawMessage` and sentinel errors.
- [Architecture overview](../../architecture.md) — where this fits in the relay's data flow.
