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
| `ErrMalformedPushWake` | `parsePushWake` | malformed JSON, non-object wake or wrong platform/token types (decoder error not wrapped — no input fragment reaches error text); invalid `request_id` only disables feedback |
| `ErrUnsupportedPushPlatform` | `parsePushWake` | `push_wake.platform` isn't exactly `"fcm"` (includes `apns`, empty, missing) |
| `ErrEmptyPushToken` | `parsePushWake` | `push_wake.token` is empty |

## `push_wake` shape (relay-addressed)

```json
{"push_wake":{"platform":"fcm","token":"<fcm-device-token>","request_id":"wake-A-1"}}
```

No `conn_id`, no `frame`. `Unmarshal` treats an envelope as a wake request when `conn_id` is absent/empty/null **and** `push_wake` is present and non-null — the same absent-vs-null presence rule `hasFrame` already applied to `frame`. A wake envelope is returned with a nil error, `ConnID == ""`, and the raw `PushWake` bytes; callers branch on `env.ConnID == ""` to route conn_id envelopes and wake envelopes down different paths (see [`StartBinaryForwarder`](binary-forwarder.md#push_wake-handling)).

`PushWake` stays a raw `json.RawMessage` at this layer — only `parsePushWake`
(called by the waker, never by `Unmarshal`) decodes platform/token and the
optional correlation into `pushWake{Platform, Token, RequestID string}`.
Keeping validation off `Unmarshal` means a `conn_id` envelope carrying a
malformed `push_wake` still decodes and routes normally. Unknown fields remain
tolerated.

The optional `request_id` follows the published
[correlated wake opt-in](https://github.com/pyrycode/pyrycode/blob/d400bc3a4ad7e8bba3fffd6be53fc025e7c1f002/docs/protocol-mobile.md#correlated-wake-opt-in):
1–64 ASCII bytes from `[A-Za-z0-9_-]`, generated independently of tokens,
hashes, credentials and content, fresh for each request on a connection.
Omitted, empty, overlong, non-ASCII, disallowed-character, null or wrong-typed
IDs disable only feedback, preserving ordinary admission/coalescing and the
connection. Malformed JSON is still rejected. Old wake-capable relays ignore
the optional field; pre-wake relays may drop the request. Neither returns
feedback, and no capability handshake is required.

Only terminal, uncancelled `ErrFCMUnregistered` permits the canonical
[result](https://github.com/pyrycode/pyrycode/blob/d400bc3a4ad7e8bba3fffd6be53fc025e7c1f002/docs/protocol-mobile.md#push_wake_result):

```json
{"push_wake_result":{"request_id":"wake-A-1","code":"invalid_token"}}
```

This separate relay→daemon shape contains only the accepted ID and fixed
13-byte `invalid_token` code: no `conn_id`, `frame`, token or Noise sealing,
and never delivery to a phone. Success, other 4xx, authentication failure,
transient failure/exhaustion, cancellation and admission rejection produce
no invalid result; silence does not classify a token. The writer exposes
only a static failure sentinel, with no new feedback logs or metric labels.

Same-complete-token wakes coalesce across daemons. At most one result attempt
is made per accepted `(original connection, request_id)` in a retained job,
including after failed writes. Repeated IDs allocate nothing and renew no deadline;
equal IDs on different connections are independent. New eligible IDs can use
a cached invalid outcome before the original ten-second window ends; seen IDs
receive none. The cache retains only sanitised outcome/deduplication metadata,
never raw tokens. Eviction requires provider completion and elapsed original
window; late completion can serve existing subscribers but opens no new window.

Pending, queued and seen records share **128/connection, 64/job and 2048/global**
hard caps, with continuous accounting through transitions. Overflow declines
only the new feedback binding. Subscriptions expire at most **60s after
acceptance**, without renewal. Delivery has **5s total**, including scheduling
and writer wait, starting at provider completion or cached-binding acceptance;
expiry, connection cancellation or shutdown can end it sooner. Queued original
deliveries stay charged to their original job/connection after cache eviction
until completion/cancellation; they cannot subscribe more callers or restore
the cache. Existing **32 retained jobs, 45s immutable job deadline, three
attempts, retry charges/backoff and original 10s window** remain unchanged.

Results use the captured originating `/v1/server` socket, never a current-holder
lookup. Disconnect/replacement cancels pending/queued feedback; reconnecting
under the same server-id receives none of it. Reads continue independently of
writes. Origin teardown and waker close cancel/join bounded workers and release
state; repeated/concurrent closes join the same work and start no new result
writes. No cache replay survives eviction, disconnect or restart, and there
is no durable or exactly-once delivery guarantee. See
[Push wake dispatch](push-wake-dispatch.md#request-validate-admit-dispatch)
for lifecycle details and the companion daemon's snapshot-safety boundary.

## Close notice (relay-to-binary, #152)

```json
{ "conn_id": "c-7f3a...", "close_code": 1001 }
```

The relay's own outbound counterpart to the `conn_id` envelope above: sent by
the relay, never received by it, telling a binary that one of its phones'
WebSocket connections ended on its own. No `frame` key. Built by the
unexported `marshalCloseNotice(connID string, code uint16) ([]byte, error)`
in `envelope.go` as a standalone two-field struct rather than
`Envelope{ConnID: connID}` — `Envelope.Frame` has no `omitempty`, so
marshalling a frameless `Envelope` would emit `"frame":null`, not this shape.
Returns `ErrEmptyConnID` on an empty `connID` (unreachable in production: the
caller always has a live `conn_id`). `code` is the phone's own WS close
status, or `1001` (`StatusGoingAway`) when it closed without sending one.

Sent from [`ClientHandler`](client-endpoint.md#close-notice-to-the-binary-152)
once the phone's forwarder loop ends — skipped when the binary itself
requested the close (tracked by
[`phoneOutbox.closedByBinary`](phone-outbox.md#binary-requested-close-tracking-152))
or when the binary that served this phone is gone or has since been
replaced under the same server-id.

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
- [`/v1/client`](client-endpoint.md) — sends the close notice; owns the decision of when to send it.
- [Per-phone delivery queue](phone-outbox.md) — tracks whether a phone's close was binary-requested, so the notice isn't sent for one.
