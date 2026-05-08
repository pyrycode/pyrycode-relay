# ADR-0002: Connection registry as a passive store

**Status:** Accepted (#3)
**Date:** 2026-05-08

## Context

The relay needs one canonical structure that owns "who is connected as what for which server-id." Every routing-layer ticket (#4 server upgrade, #5 client upgrade, #6 forwarding, #7 heartbeat, #8 grace, #10 health) reads or writes through it. The shape comes from `docs/architecture.md`: 1:1 server-id → binary with first-claim-wins, 1:N server-id → phones gated on a binary holding the slot.

The design space had several axes — what the registry *owns*, what locks it takes, how it returns data, and where the grace period lives.

## Decision

`internal/relay/registry.go` defines a `Conn` interface, a `Registry` type with one `sync.RWMutex` over both maps, seven methods (`ClaimServer`, `ReleaseServer`, `RegisterPhone`, `UnregisterPhone`, `BinaryFor`, `PhonesFor`, `Counts`), and two sentinel errors (`ErrServerIDConflict`, `ErrNoServer`). The registry is a **passive store**: it never invokes `Send` / `Close` on conns it tracks, never times anything, never calls back to anyone. Stdlib only.

Rules adopted:

1. **One RWMutex over both maps.** No sharding, no per-server-id locks.
2. **`Conn` interface lives in `relay` package.** Defined at the consumer.
3. **`PhonesFor` returns a freshly allocated copy.** Callers iterate the snapshot lock-free.
4. **`UnregisterPhone` calls `ConnID()` under the lock.** The `Conn` contract requires it to be a non-blocking getter.
5. **`ReleaseServer` does not touch phones.** Orphan phones survive a release; their owner must unregister them.
6. **Sentinel errors over wrapped errors.** Same idiom as ADR-0001 (#1).

## Rationale

### One RWMutex, not per-map or sharded

Two separate locks (one for `binaries`, one for `phones`) invite ordering bugs (always-A-then-B) for no measurable gain — `RegisterPhone` already needs both, and the critical sections are sub-microsecond map operations. Sharding by server-id is a possible later optimisation but irrelevant at v1: the relay handles tens to hundreds of concurrent connections, not millions, and lock contention is unobserved.

### `Conn` defined in `relay`, not at the consumer

Conventional Go style says "define interfaces at the consumer." Here there is one consumer package — `internal/relay` — and the Conn impls themselves will live in a `relay/ws` subpackage in #4/#5. Defining `Conn` in `relay` avoids an import cycle and matches the practical scope (one consumer).

### `PhonesFor` returns a copy, not a reference

Returning the underlying slice would force callers to broadcast under the registry lock or risk a data race against `RegisterPhone` / `UnregisterPhone`. A copy is cheap (pointer-sized entries, typically 1–3 phones per server-id) and lets callers do the slow thing — `Send` over the network — without holding the lock. The Conn references inside the copy are still live; that's intentional, and it's what makes the broadcast pattern work.

### `ConnID` is a getter, called under the lock

Alternative: copy the slice, drop the lock, scan for the matching `ConnID`, re-acquire the lock to remove. This creates a TOCTOU window — a phone added between snapshot and removal could be erroneously skipped or removed. The cleaner contract is "`ConnID` is a non-blocking getter" stated on the interface; if a future Conn impl violates that, fix the impl. Documented; tested only structurally.

### `ReleaseServer` is narrow

It removes the binary entry and nothing else. The grace ticket (#8) needs phones to remain reachable for 30 seconds across a binary disconnect — so cleanup of orphan phones cannot live inside `ReleaseServer`. Keeping release immediate-and-narrow lets #8 layer policy on top without changing the registry's API. If a future ticket wants auto-close-phones-on-release, it adds a wrapper, not a flag on this method.

### Sentinel errors

Same idiom as ADR-0001. `errors.Is` for branching, no string matching, downstream WS handlers map sentinels → close codes (`4404`, `4409`).

## Consequences

- The registry is the single canonical place for "who is connected." Future routing-layer code reads/writes through it; ad-hoc maps in #4–#10 are a code-review smell.
- Concurrency is provable from the lock graph (one node) plus the documented invariant that `Send` / `Close` never run under the lock. Race-test coverage (32 goroutines × 200 ops) plus `make test`'s default `-race` mode is the enforcement.
- The registry is testable without WebSockets — a 5-line `fakeConn` is enough. WS-library choice in #4/#5 doesn't reach back into this code.
- `Conn` is a contract: any future Conn impl must honour non-blocking `ConnID`, idempotent `Close`, and per-impl write serialisation in `Send`. Those properties belong in #4/#5's spec.
- Per-server connection caps are explicitly NOT here. #5 owns that.
- Orphan phones after `ReleaseServer` are a documented state. #8 will own the grace timer that decides when to close them.
- Linear scan in `UnregisterPhone` is correct for v1 (1–3 phones per server-id). If profiling later flags it, switch to `map[connID]Conn` per server-id; the public API doesn't change.
