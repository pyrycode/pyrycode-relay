# ADR-0001: Routing envelope shape and opacity

**Status:** Accepted (#1)
**Date:** 2026-05-08

## Context

The relay forwards WS frames between phones and a pyry binary, wrapping them in a `{conn_id, frame}` JSON envelope on the binary-side connection. Every future routing-layer ticket (header validation, registry, frame forwarding) needs a single Go vocabulary for that envelope. Two non-negotiable constraints from `docs/architecture.md`:

1. The relay must never deserialise inner-frame payloads — frames are opaque bytes.
2. Validation happens at the envelope boundary; inner-frame semantics belong to the binary.

## Decision

A new package-internal type `relay.Envelope` with `Marshal`/`Unmarshal` helpers in `internal/relay/envelope.go`. The inner frame field is typed `json.RawMessage`. Errors are exported sentinel values (`ErrEmptyConnID`, `ErrInvalidFrameJSON`, `ErrMalformedEnvelope`, `ErrMissingConnID`, `ErrMissingFrame`); callers branch with `errors.Is`.

`Marshal` validates non-empty `connID` and `json.Valid(frame)`. `Unmarshal` validates that the outer payload is a JSON object with non-empty `conn_id` and a `frame` that is neither absent nor JSON null.

Stdlib only — no external dependency.

## Rationale

### `json.RawMessage` for `Frame`

Alternatives considered:

- `[]byte` — `encoding/json` would base64-encode it on Marshal; wrong wire shape.
- `interface{}` / `map[string]interface{}` — forces the relay to deserialise the inner frame on every Unmarshal, violating the opacity invariant and burning CPU re-marshalling on forward.
- `string` — readable but still pays a copy and hides that the bytes are JSON.

`json.RawMessage` is purpose-built: it preserves the underlying bytes verbatim through both directions and is the natural way to express "valid JSON, but I don't care what's inside" in stdlib.

### Sentinel errors over ad-hoc strings

Tests assert with `errors.Is` rather than string-matching, and downstream code can distinguish "client sent bad envelope" (close with a protocol error) from "relay bug" (alert) without parsing strings. Wrapping with `fmt.Errorf("…: %w", …, sentinel)` retains the underlying decoder detail when useful.

### Validate inner-frame syntax only on Marshal

`json.Marshal` of an `Envelope` containing garbage `RawMessage` bytes succeeds and silently emits invalid JSON inside valid JSON, corrupting the wire. `json.Valid` is cheap and the only place we touch frame contents. On Unmarshal, the decoder has already syntax-checked the whole payload, so re-validating the inner frame would be redundant.

### Reject empty / null `conn_id` and `frame` uniformly

Three cases for `conn_id` (absent, empty string, null) are equivalent garbage at this layer; collapsing them into a single `ErrMissingConnID` keeps the API small. `frame` needs a special-case check for the 4-byte string `"null"` because `json.RawMessage` of an explicit JSON null doesn't appear absent by length alone.

## Consequences

- All future routing-layer code uses `relay.Envelope` and the helpers; inline JSON for routing is a code-review smell.
- The opacity invariant is type-enforced: a contributor who wants to read a phone's payload can't, without changing this type and the architecture doc together.
- Sentinel errors are a stable contract — adding new ones is fine, removing or repurposing one is a breaking change for downstream packages.
- Round-trip is byte-stable modulo whitespace; tests assert via `json.Compact` rather than raw `bytes.Equal`. If a future change ever needs strict byte-for-byte preservation of inner-frame whitespace, that requires moving away from `json.RawMessage` re-encoding (e.g. hand-assembling the outer JSON).
- No `conn_id` format validation here. The conn-id generation ticket owns that.
- No size limits here. The WS read loop will impose them.
