# Knowledge Index

One-line pointers into the evergreen knowledge base. Newest entries at the top of each section.

## Features

- [Connection registry](features/connection-registry.md) — thread-safe `Registry` (server-id → binary 1:1, server-id → phones 1:N) with `Conn` interface, snapshot-returning `PhonesFor`, sentinel errors for `4409` / `4404`.
- [Routing envelope](features/routing-envelope.md) — typed Go wrapper (`Envelope`, `Marshal`, `Unmarshal`) for the `{conn_id, frame}` wire shape exchanged between relay and binary.

## Decisions

- [ADR-0002: Connection registry as a passive store](decisions/0002-connection-registry-passive-store.md) — single RWMutex, snapshot returns, no callbacks, narrow `ReleaseServer` so grace logic (#8) can wrap it.
- [ADR-0001: Routing envelope shape and opacity](decisions/0001-routing-envelope-shape-and-opacity.md) — `json.RawMessage` for the inner frame; sentinel errors; validate at boundary, never parse payloads.

## Architecture

- [System overview](../architecture.md) — top-level: stateless WS router between phones and pyry binaries. (Lives at `docs/architecture.md`; not yet split into `architecture/`.)

## Cross-cutting

- [Project memory](../PROJECT-MEMORY.md) — what's built, patterns established, current state.
- [Lessons](../lessons.md) — gotchas worth carrying forward.
