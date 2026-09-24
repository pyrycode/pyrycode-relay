# Routing envelope

The relay forwards WebSocket frames between phones and a pyrycode binary. On the binary-side connection it wraps each frame in a small JSON envelope so the binary knows which phone connection a frame belongs to:

```json
{ "conn_id": "c-7f3a...", "frame": { /* inner envelope, opaque to relay */ } }
```

A binary→relay envelope may instead carry no `conn_id` and a `push_wake` object — a wake request addressed to the relay itself, not to a phone (see [`push_wake` shape](#push_wake-shape-relay-addressed) below and [Push wake dispatch](push-wake-dispatch.md) for what happens to it).

This package provides the canonical Go vocabulary for that wrapper. All future routing-layer code (header validation, registry, frame forwarding) uses these helpers — no inline JSON.

Authoritative wire spec: [`pyrycode/pyrycode/docs/protocol-mobile.md` § Routing envelope](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#routing-envelope) and [§ `push_wake`](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#push_wake).

## API

Package `internal/relay` (`envelope.go`):

```go
type Envelope struct {
    ConnID    string          `json:"conn_id"`
    Frame     json.RawMessage `json:"frame"`
    CloseCode uint16          `json:"close_code,omitempty"`
    PushWake  json.RawMessage `json:"push_wake,omitempty"`
}

func Marshal(connID string, frame []byte) ([]byte, error)
func Unmarshal(data []byte) (Envelope, error)
```

`CloseCode` (#119) and `PushWake` (#133) are the two relay-interpreted control
fields — everything else in `Frame` stays opaque. Both stay untyped-at-rest
(`uint16` / `json.RawMessage`) rather than driving a variant/union shape, so a
`conn_id` envelope that happens to carry either field (even a malformed
`push_wake`) decodes exactly as it did before either field existed.

Sentinel errors (branch with `errors.Is`):

| Error | Returned by | When |
|---|---|---|
| `ErrEmptyConnID` | `Marshal` | `connID == ""` |
| `ErrInvalidFrameJSON` | `Marshal` | `frame` is nil/empty/not valid JSON |
| `ErrMalformedEnvelope` | `Unmarshal` | outer JSON parse fails or payload isn't a JSON object (wraps the decoder error) |
| `ErrMissingConnID` | `Unmarshal` | `conn_id` absent, empty string, or null, and no valid `push_wake` present |
| `ErrMissingFrame` | `Unmarshal` | `frame` absent or JSON null, `close_code` is zero, and `conn_id` is present |
| `ErrMalformedPushWake` | `parsePushWake` | `push_wake` isn't a JSON object or has wrong field types (decoder error not wrapped — no input fragment reaches error text) |
| `ErrUnsupportedPushPlatform` | `parsePushWake` | `push_wake.platform` isn't exactly `"fcm"` (includes `apns`, empty, missing) |
| `ErrEmptyPushToken` | `parsePushWake` | `push_wake.token` is empty |

## `push_wake` shape (relay-addressed)

```json
{ "push_wake": { "platform": "fcm", "token": "<fcm-device-token>" } }
```

No `conn_id`, no `frame`. `Unmarshal` treats an envelope as a wake request when `conn_id` is absent/empty/null **and** `push_wake` is present and non-null — the same absent-vs-null presence rule `hasFrame` already applied to `frame`. A wake envelope is returned with a nil error, `ConnID == ""`, and the raw `PushWake` bytes; callers branch on `env.ConnID == ""` to route conn_id envelopes and wake envelopes down different paths (see [`StartBinaryForwarder`](binary-forwarder.md#push_wake-handling)).

`PushWake` stays a raw `json.RawMessage` at this layer — only `parsePushWake` (called from `PushWaker.Request`, never from `Unmarshal` itself) decodes it into the unexported `pushWake{Platform, Token string}` struct and applies the platform/token checks above. Keeping the validation off `Unmarshal` means a `conn_id` envelope that happens to carry a malformed `push_wake` still decodes normally — the wake path and the phone-routing path can't interfere with each other.

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
