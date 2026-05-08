# Spec — Routing-envelope wrapper type (#1)

## Files to read first

- `internal/relay/doc.go` — current package doc; the new file lives next to this and stays consistent with the package's "routing core" framing.
- `cmd/pyrycode-relay/main.go:1-7` — links the protocol spec; the envelope's wire shape is authoritative there.
- `docs/architecture.md:11-12,21,27` — reaffirms two non-negotiables: (a) relay prepends/strips the routing envelope; (b) frames are opaque bytes — the relay never reads payloads.
- `Makefile` — `test` runs `go test -race ./...`; `vet` runs `go vet ./...`. The new code must be clean under both.
- `go.mod` — module path `github.com/pyrycode/pyrycode-relay`, Go 1.26.2; stdlib only.

Wire shape (per ticket body, mirroring `pyrycode/pyrycode/docs/protocol-mobile.md § Routing envelope`):

```json
{ "conn_id": "c-7f3a...", "frame": { /* inner envelope, opaque to relay */ } }
```

## Context

The relay forwards WS frames between phones and a binary, wrapping them in a small `{conn_id, frame}` envelope so the binary knows which phone connection a frame belongs to. Today there is no Go vocabulary for this envelope; future routing-layer tickets (header validation, registry, frame forwarding) all need it. This ticket establishes the canonical type plus marshal/unmarshal helpers, with no networking and no consumers yet.

The two invariants that drive the design:

1. **Opacity** — the relay must never deserialise the inner frame. `frame` is bytes in, bytes out, byte-for-byte (modulo whitespace from `encoding/json`'s canonicalisation, which is acceptable per the ticket's "modulo whitespace" wording).
2. **Validation at the boundary** — empty/missing `conn_id` and missing `frame` are rejected. Inner-frame *syntax* is checked (must be valid JSON) but inner-frame *semantics* are not.

## Design

### Package & files

- New file: `internal/relay/envelope.go`
- New test file: `internal/relay/envelope_test.go`
- Both `package relay` (test file uses the same package, not `relay_test`, so it can reference unexported sentinel errors directly via `errors.Is`).

### Type

```go
// Envelope is the routing wrapper exchanged between the relay and a pyrycode
// binary on the /v1/server connection. It carries an opaque inner frame
// addressed by the relay-assigned phone connection id.
//
// The relay treats Frame as opaque bytes and MUST NOT deserialise them.
// See pyrycode/pyrycode/docs/protocol-mobile.md § Routing envelope.
type Envelope struct {
    ConnID string          `json:"conn_id"`
    Frame  json.RawMessage `json:"frame"`
}
```

`json.RawMessage` is the right choice: its `MarshalJSON`/`UnmarshalJSON` preserve the underlying bytes verbatim, which is what the opacity invariant requires. It's also stdlib — no extra dependency.

### Helpers

```go
// Marshal builds the JSON-encoded routing envelope for an inner frame
// addressed to the given phone connection.
//
// connID must be non-empty. frame must be syntactically valid JSON; its
// contents are otherwise opaque to the relay. Returns the JSON-encoded
// envelope, or one of ErrEmptyConnID / ErrInvalidFrameJSON wrapped with
// context.
func Marshal(connID string, frame []byte) ([]byte, error)

// Unmarshal parses a JSON-encoded routing envelope. It returns
// ErrMalformedEnvelope for syntactically invalid JSON, ErrMissingConnID
// when conn_id is absent or empty, and ErrMissingFrame when frame is
// absent or JSON null.
//
// The Frame field of the returned Envelope is the verbatim bytes of the
// inner frame (possibly with insignificant whitespace normalised by the
// JSON decoder). The relay MUST NOT inspect them.
func Unmarshal(data []byte) (Envelope, error)
```

### Sentinel errors

Exported in the same file:

```go
var (
    ErrEmptyConnID       = errors.New("relay: empty conn_id")
    ErrInvalidFrameJSON  = errors.New("relay: frame is not valid JSON")
    ErrMalformedEnvelope = errors.New("relay: malformed routing envelope")
    ErrMissingConnID     = errors.New("relay: routing envelope missing conn_id")
    ErrMissingFrame      = errors.New("relay: routing envelope missing frame")
)
```

Sentinel errors over ad-hoc `fmt.Errorf` strings: tests can assert with `errors.Is`, and downstream routing-layer tickets get a stable contract for distinguishing "client sent garbage" from "relay bug" without string matching. Wrap with `fmt.Errorf("...: %w", err, ErrXxx)` when the helper wants to add context (e.g. underlying `json.Unmarshal` error).

### Marshal — algorithm

1. If `connID == ""` → return `nil, ErrEmptyConnID`.
2. If `!json.Valid(frame)` → return `nil, ErrInvalidFrameJSON`.
   - Note: `json.Valid(nil)` and `json.Valid([]byte{})` are both `false`, so this check also catches empty/missing inner frames at marshal time. Good — no extra branch needed.
3. Construct `Envelope{ConnID: connID, Frame: json.RawMessage(frame)}` and `return json.Marshal(env)`. The `RawMessage` marshaller emits the bytes verbatim (modulo whitespace canonicalisation by the outer encoder).

### Unmarshal — algorithm

1. `var env Envelope; err := json.Unmarshal(data, &env)`.
2. If `err != nil` → return `Envelope{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)`.
3. If `env.ConnID == ""` → return `Envelope{}, ErrMissingConnID`.
   - This collapses three cases into one: field absent, field present as `""`, field present as `null` (`json.RawMessage` of a string field treats `null` as zero value). All three are equivalent garbage at this layer; no need to distinguish.
4. If `len(env.Frame) == 0 || bytes.Equal(env.Frame, []byte("null"))` → return `Envelope{}, ErrMissingFrame`.
   - `len == 0` covers the absent case (`json.RawMessage` left as nil).
   - The explicit `null`-literal check covers `"frame": null`. Without it, the `RawMessage` would be the 4-byte string `"null"` and pass length validation while semantically representing absence. Treat both as missing.
5. Return `env, nil`.

### Why these checks and not more

The ticket is explicit: validate envelope-level structure, treat the inner frame as opaque. So:

- **Don't** validate `conn_id` format (length, character set, prefix). The relay assigns it; format is decided in a later ticket (out-of-scope per ticket body).
- **Don't** parse the inner frame on Unmarshal. `json.RawMessage` deliberately defers parsing; that defers it forever, which is the point.
- **Do** validate the inner frame on Marshal. We're emitting the envelope; if the caller hands us garbage bytes, `json.Marshal` would still succeed (it'd just emit invalid JSON inside valid JSON, which silently corrupts the wire). `json.Valid` is cheap and the only place we touch frame contents.

### Concurrency model

None. Both functions are pure and stateless: no goroutines, no shared mutable state, no I/O. Safe to call from any goroutine concurrently.

### Error handling

All error returns are typed sentinels (possibly wrapped). Callers use `errors.Is` to branch on `ErrMissingConnID` etc. when they need to distinguish "client sent bad envelope" (close with a protocol error) from "relay bug" (panic or alert). No panics; no `os.Exit`; no logging — this is library code.

## Testing strategy

Single test file, `internal/relay/envelope_test.go`, `package relay`. Use the standard `testing` package; no external libs. Subtest names map 1:1 to the AC bullets so the trace is obvious.

### Round-trip with bytewise opacity

```go
inner := []byte(`{"type":"hello","nested":{"a":[1,2,3],"b":null,"c":"xé"}}`)
out, err := relay.Marshal("c-abc123", inner)
// require: err == nil
got, err := relay.Unmarshal(out)
// require: err == nil
// require: got.ConnID == "c-abc123"
// require: bytes.Equal(canonicaliseJSON(got.Frame), canonicaliseJSON(inner))
```

The "modulo whitespace" caveat from the AC is real: the outer `json.Marshal` re-encodes the `RawMessage` and may strip insignificant whitespace. To assert opacity robustly, canonicalise both sides with `json.Compact` before comparing — that proves the *semantic* bytes are identical without flaking on formatting. Include the unicode escape (`é`) and a `null` value to demonstrate the inner frame survives unchanged.

### Marshal rejections

Table-driven, asserting `errors.Is(err, ErrXxx)`:

| Case | connID | frame | Expected error |
|---|---|---|---|
| empty conn id | `""` | `{"x":1}` | `ErrEmptyConnID` |
| nil frame | `"c-1"` | `nil` | `ErrInvalidFrameJSON` |
| empty frame | `"c-1"` | `[]byte("")` | `ErrInvalidFrameJSON` |
| garbage frame | `"c-1"` | `[]byte("not json")` | `ErrInvalidFrameJSON` |
| truncated frame | `"c-1"` | `[]byte("{\"a\":")` | `ErrInvalidFrameJSON` |

### Unmarshal rejections

| Case | input | Expected error |
|---|---|---|
| malformed JSON | `{"conn_id":"c-1"` (truncated) | `ErrMalformedEnvelope` |
| not an object | `[]` | `ErrMalformedEnvelope` (json/Unmarshal returns a type error) |
| missing conn_id | `{"frame":{}}` | `ErrMissingConnID` |
| empty conn_id | `{"conn_id":"","frame":{}}` | `ErrMissingConnID` |
| null conn_id | `{"conn_id":null,"frame":{}}` | `ErrMissingConnID` |
| missing frame | `{"conn_id":"c-1"}` | `ErrMissingFrame` |
| null frame | `{"conn_id":"c-1","frame":null}` | `ErrMissingFrame` |

All rejection assertions go through `errors.Is`, not string compare.

### What we deliberately do not test

- Inner-frame *content* — the whole point is we never look. A test that introspects `Frame` past `bytes.Equal` would violate the invariant the type is enforcing.
- `conn_id` format / character set — out of scope; later ticket.
- Goroutine safety — pure functions; not interesting to exercise.

## Open questions

1. **Outer JSON whitespace.** `json.Marshal` produces compact output, so the round-trip's "modulo whitespace" caveat shouldn't bite in practice. Specified anyway because tests assert via `json.Compact` to be robust if Go's encoder ever changes.
2. **`conn_id` character validation.** Deferred to the connection-id generation ticket. This file's contract: any non-empty string is acceptable.
3. **Max envelope / frame size.** No size limit imposed here. The networking layer (later ticket) will enforce a read limit on the WS read loop; envelope-level helpers are size-agnostic.

## Out of scope (re-stated, for the developer)

- No edits to `cmd/pyrycode-relay/main.go`. `make build` must keep producing a working binary, but only because nothing changes in `main`.
- No new package; this lives in `internal/relay` next to `doc.go`.
- No connection registry, no WS upgrade, no header validation, no `conn_id` generation.

## Done means

- `internal/relay/envelope.go` exists with `Envelope`, `Marshal`, `Unmarshal`, and the five sentinel errors, each exported symbol carrying a doc comment that names the opacity invariant.
- `internal/relay/envelope_test.go` covers every row of both rejection tables plus the opacity round-trip.
- `make vet`, `make test`, `make build` all clean from the repo root.
- One commit on `feature/1`: `feat(relay): routing-envelope wrapper type (#1)`.
