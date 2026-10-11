# ADR-0014: Reuse `netutil.LimitListener` for public socket admission

**Status:** Accepted (#182)
**Date:** 2026-10-11

## Context

The [upgraded-session cap](../features/connection-cap.md) starts after TLS
and complete HTTP headers. Public listeners also need a bound on accepted
sockets before PROXY, TLS or HTTP parsing, lasting through keep-alive and
WebSocket hijacking. Standard `net` has no equivalent admission wrapper.
`golang.org/x/net v0.60.0` was already pinned indirectly.

[ADR-0008](0008-prometheus-client-adoption.md) requires justification before
a new direct dependency. The [committed plan](../../specs/architecture/182-public-listener-connection-cap.md#context)
records this choice before the module edit; this record preserves it.

## Decision

Import `golang.org/x/net/netutil` directly at the existing v0.60.0 pin and
wrap each public TCP listener with its own `LimitListener`. Place it beneath
the optional PROXY listener and `ServeTLS`'s TLS wrapper. Leave loopback
metrics outside the public limits.

## Rationale

The Go project's existing implementation acquires a semaphore before
underlying `Accept`, releases exactly once through connection `Close`
(`sync.Once`), and wakes capacity-blocked acceptance when the listener
closes. Reusing that contract avoids duplicating admission, concurrent
release and shutdown logic in a new relay abstraction.

The package imports only standard-library `net` and `sync`, adds no
transitive packages, and operates on opaque sockets without reading bytes
or parsing any protocol. Promoting the module to direct use changes neither
dependency versions nor `go.sum` checksums.

Handler return or `ConnState` would release too early for keep-alive or
hijacked WebSockets. Socket ownership preserves the raw slot until the
transport actually closes, independently of the upgraded-handler cap.

## Consequences

Each listener has independent capacity and backpressure outside acceptance,
without an HTTP refusal for queued peers. TCP dial success alone is not
admission. A source can still fill a listener; per-source allowances and Fly
edge ceilings are separate work.

Tests observe actual public wiring, slot reuse after phase failures and
WebSocket close, repeated/concurrent close after a new owner takes the slot,
and saturated shutdown. Dependency upgrades must preserve these contracts.
HTTPS also retains deferred listener cleanup for `ServeTLS` setup failures
before `Serve` takes ownership.

The [threat model](../../threat-model.md#supply-chain--go-dependencies)
accounts for direct x/net use and its re-review trigger. The combined
256 MB sizing estimate requires [isolated operator validation](../../deploy.md#verify-a-deploy)
before production deployment; the library provides a socket bound, not a
memory guarantee.
