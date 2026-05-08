# Knowledge Index

One-line pointers into the evergreen knowledge base. Newest entries at the top of each section.

## Features

- [`/v1/server` WS upgrade](features/server-endpoint.md) — binary-side ingress: validates `X-Pyrycode-Server` / `X-Pyrycode-Version` / `User-Agent` pre-upgrade, claims the slot, emits `4409` on conflict, holds the conn via `CloseRead` until #6's frame loop replaces it.
- [`/healthz` JSON endpoint](features/healthz.md) — unauthenticated `GET /healthz` returning `{status, version, connected_binaries, connected_phones, uptime_seconds}`; `Cache-Control: no-store`, body bounded ≈135 bytes.
- [WSConn adapter](features/ws-conn-adapter.md) — wraps `nhooyr.io/websocket.Conn` to satisfy the registry's `Conn`; owns the per-conn write mutex and a `Close`-cancelled context with a 10s per-`Send` deadline.
- [Connection registry](features/connection-registry.md) — thread-safe `Registry` (server-id → binary 1:1, server-id → phones 1:N) with `Conn` interface, snapshot-returning `PhonesFor`, sentinel errors for `4409` / `4404`.
- [Autocert TLS](features/autocert-tls.md) — `--domain` mode wiring: `:443` WSS via Let's Encrypt + `:80` ACME http-01, host gates, cache-dir permission policy, TLS 1.2 floor.
- [Routing envelope](features/routing-envelope.md) — typed Go wrapper (`Envelope`, `Marshal`, `Unmarshal`) for the `{conn_id, frame}` wire shape exchanged between relay and binary.

## Decisions

- [ADR-0005: Application WS close codes go through the underlying `*websocket.Conn`](decisions/0005-application-close-codes-via-underlying-conn.md) — `WSConn.Close()` stays minimal (`StatusNormalClosure`); handlers emit `4409` / `4401` / `4404` directly on the `*websocket.Conn` in stillborn-WSConn paths.
- [ADR-0004: WS library choice and adapter context strategy](decisions/0004-ws-library-and-adapter-context-strategy.md) — `nhooyr.io/websocket` v1.8.x; adapter-owned context cancelled by `Close`, 10s per-`Send` deadline; registry's `Send` signature stays context-free.
- [ADR-0003: Connection registry as a passive store](decisions/0003-connection-registry-passive-store.md) — single RWMutex, snapshot returns, no callbacks, narrow `ReleaseServer` so grace logic (#8) can wrap it.
- [ADR-0002: Explicit-failure 404 on `:80` instead of HTTPS redirect](decisions/0002-autocert-explicit-failure-on-port-80.md) — pass `http.NotFoundHandler()` to `autocert.Manager.HTTPHandler`, not `nil`; loud failure over silent scheme-switching.
- [ADR-0001: Routing envelope shape and opacity](decisions/0001-routing-envelope-shape-and-opacity.md) — `json.RawMessage` for the inner frame; sentinel errors; validate at boundary, never parse payloads.

## Architecture

- [System overview](../architecture.md) — top-level: stateless WS router between phones and pyry binaries. (Lives at `docs/architecture.md`; not yet split into `architecture/`.)

## Cross-cutting

- [Threat model](../threat-model.md) — operational threats to the relay-as-deployed-process: deploy, supply chain, DoS, log hygiene, cert handling, TLS config, error leakage. Complements (does not replace) the protocol spec's wire-protocol Security model.
- [Project memory](../PROJECT-MEMORY.md) — what's built, patterns established, current state.
- [Lessons](../lessons.md) — gotchas worth carrying forward.
