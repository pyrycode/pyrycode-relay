# ADR-0012: Adopt `github.com/pires/go-proxyproto` to parse Fly's PROXY header

**Status:** Accepted (#110)
**Date:** 2026-09-24

## Context

[The HTTPS listener needed to read a PROXY protocol v2 header](../features/proxy-protocol-listener.md)
before the TLS handshake, on an internet-exposed, pre-TLS path, so the per-IP
rate limiter could key on the real client address instead of Fly's edge
proxy. [ADR-0008](0008-prometheus-client-adoption.md) established "an ADR
file, before the `go.mod` edit" as the pattern for any next direct
dependency; [ADR-0011](0011-oauth2-jwt-not-google-for-fcm.md) is the most
recent application. This is the third.

The alternative was a hand-rolled parser: the PROXY v2 wire format is a
12-byte fixed signature, a version/command byte, a family/protocol byte, a
16-bit length, and an address block whose shape depends on the family byte.
Small, but a parser for attacker-reachable bytes on a pre-TLS path is exactly
the kind of code where a subtle off-by-one or an unhandled address family
becomes a crash or a spoofing primitive, and no fuzzing or prior production
exposure would back a same-day implementation.

## Decision

Depend on `github.com/pires/go-proxyproto v0.15.0` and wrap its
`proxyproto.Listener` in `internal/relay/proxyproto.go`
(`NewProxyProtoListener`), rather than parsing the header by hand.

## Rationale

- **Maintained, exercised parser for adversarial bytes.** The header is read
  from any TCP peer that reaches the listener, before TLS — the least
  trusted position in the relay's request path. A library with existing
  production usage and issue history is a better bet here than new code
  written for one ticket.
- **Small build-graph cost.** The root `github.com/pires/go-proxyproto`
  package imports the standard library only; `go.sum` added exactly one
  module, no transitive dependencies compiled into the binary.
- **Scoped exposure.** The library sits before TLS and sees only the PROXY
  header bytes and, once past that, opaque TLS ciphertext — it is never
  handed a routed frame in cleartext. The relay's content-blindness
  (`docs/architecture.md`) is unaffected: PROXY protocol is L4 addressing
  metadata, not payload.
- **The library's defaults needed narrowing, not replacing.** `Listener.Accept`
  reads the header lazily (on the connection's own goroutine, not inside
  `Accept`), which is exactly the non-blocking-stall behaviour the ticket
  needed — no fork of the library's connection-accept logic was necessary.

## Consequences

- Two of the library's default behaviours are surprising enough that the
  wrapper closes them explicitly rather than relying on defaults — see
  [Lessons learned in the ticket notes](../codebase/110.md):
  `Conn.RemoteAddr()` falls back to the *socket peer* address, not an error,
  for a syntactically valid but semantically unwanted header (a `LOCAL`
  command, or a v1 `UNKNOWN`) — reads still succeed. `NewProxyProtoListener`
  supplies `ValidateHeader` to reject anything but a v2 PROXY-command header
  with a TCP source, so that fallback is unreachable in this relay's
  configuration. Separately, `ReadHeaderTimeout: 0` is read by the library
  as its own 10s default, and a negative value as "no timeout" — the
  wrapper rejects any `headerTimeout <= 0` at construction instead of
  silently inheriting a Timeout the caller didn't ask for.
- `ConnPolicy` is set explicitly to a closure returning `proxyproto.REQUIRE`
  rather than relying on the package-level `proxyproto.DefaultPolicy` var —
  a future import of the library elsewhere in the binary (or a future
  library version changing its default) cannot silently change this
  listener's policy.
- Supply-chain accounting for `github.com/pires/go-proxyproto` lives in
  [`docs/threat-model.md` § Supply chain](../../threat-model.md); its
  addition is the trigger event for that section's *Triggers for re-review*
  entry, satisfied by this ADR plus the threat-model update landing in the
  same ticket.
- The library is trusted for header *shape* validation only, not for *who*
  is allowed to send one — restricting the listener to Fly's own upstream
  addresses (closing the org-internal-spoofing gap under `REQUIRE`) is a
  separate, deferred concern tracked in
  [`docs/security-followups.md`](../../security-followups.md), unrelated to
  this library choice.
