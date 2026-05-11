# Knowledge Index

One-line pointers into the evergreen knowledge base. Newest entries at the top of each section.

## Features

- [Binary-side frame forwarder](features/binary-forwarder.md) — per-binary read pump: unwraps each inbound routing envelope, linear-scans `PhonesFor(serverID)` for `env.ConnID`, writes `env.Frame` verbatim to that phone; opaque inner bytes; synchronous (handler discards the return); diverges from #25 in error policy — unknown `conn_id`, malformed envelope, phone `Send` error all log+continue (a single bad frame never tears down the binary); replaced `/v1/server`'s `CloseRead` placeholder (#26).
- [WebSocket heartbeat](features/heartbeat.md) — per-conn goroutine on both endpoints sends RFC 6455 ping every 30s; closes with `1011 "heartbeat timeout"` if no pong within 30s. Detects half-open TCP within 60s; ctx-cancel exit path leaves close to the handler defer (#7).
- [Phone-side frame forwarder](features/phone-forwarder.md) — per-phone read pump: wraps each inbound phone frame in the routing envelope keyed by the phone's `conn_id` and `Send`s it to the binary holding `serverID`; opaque inner bytes; synchronous (handler discards the return); replaced `/v1/client`'s `CloseRead` placeholder; added `WSConn.Read` (single-caller) (#25).
- [`/v1/client` WS upgrade](features/client-endpoint.md) — phone-side ingress: validates `X-Pyrycode-Server` / `X-Pyrycode-Token` / `User-Agent` pre-upgrade (token presence-only, never parsed/logged); registers phone on the binary's slot, emits `4404` if no binary holds the id, hands the conn to `StartPhoneForwarder` for the data path (#5, #25).
- [`/v1/server` WS upgrade](features/server-endpoint.md) — binary-side ingress: validates `X-Pyrycode-Server` / `X-Pyrycode-Version` / `User-Agent` pre-upgrade, claims the slot, emits `4409` on conflict, hands the conn to `StartBinaryForwarder` for the data path (#26); on disconnect schedules a 30s grace release so a quick reconnect inherits the slot (#16, #21, #26).
- [`/healthz` JSON endpoint](features/healthz.md) — unauthenticated `GET /healthz` returning `{status, version, connected_binaries, connected_phones, uptime_seconds}`; `Cache-Control: no-store`, body bounded ≈135 bytes.
- [WSConn adapter](features/ws-conn-adapter.md) — wraps `nhooyr.io/websocket.Conn` to satisfy the registry's `Conn`; owns the per-conn write mutex and a `Close`-cancelled context with a 10s per-`Send` deadline.
- [Connection registry](features/connection-registry.md) — thread-safe `Registry` (server-id → binary 1:1, server-id → phones 1:N) with `Conn` interface, snapshot-returning `PhonesFor`, sentinel errors for `4409` / `4404`, deferred-release `ScheduleReleaseServer` with grace-window reclaim semantics.
- [Autocert TLS](features/autocert-tls.md) — `--domain` mode wiring: `:443` WSS via Let's Encrypt + `:80` ACME http-01, host gates, cache-dir permission policy, TLS 1.2 floor.
- [Routing envelope](features/routing-envelope.md) — typed Go wrapper (`Envelope`, `Marshal`, `Unmarshal`) for the `{conn_id, frame}` wire shape exchanged between relay and binary.

## Decisions

- [ADR-0007: `WSConn.CloseWithCode` for active-conn application close codes](decisions/0007-wsconn-closewithcode-for-active-conn.md) — extends ADR-0005 for the post-claim window: heartbeat (#7) needs `1011 "heartbeat timeout"` on a live WSConn; `Close()` delegates to `CloseWithCode(StatusNormalClosure, "")`, both share `closeOnce`.
- [ADR-0006: Grace window IS the reclaim path](decisions/0006-grace-period-as-reclaim-path.md) — `ClaimServer` during a pending grace timer succeeds (not `4409`); pointer-identity wrapper defends against stale `time.AfterFunc` fires after `Stop()`.
- [ADR-0005: Application WS close codes go through the underlying `*websocket.Conn`](decisions/0005-application-close-codes-via-underlying-conn.md) — `WSConn.Close()` stays minimal (`StatusNormalClosure`); handlers emit `4409` / `4401` / `4404` directly on the `*websocket.Conn` in stillborn-WSConn paths.
- [ADR-0004: WS library choice and adapter context strategy](decisions/0004-ws-library-and-adapter-context-strategy.md) — `nhooyr.io/websocket` v1.8.x; adapter-owned context cancelled by `Close`, 10s per-`Send` deadline; registry's `Send` signature stays context-free.
- [ADR-0003: Connection registry as a passive store](decisions/0003-connection-registry-passive-store.md) — single RWMutex, snapshot returns, no callbacks, narrow `ReleaseServer` so grace logic (#20/#21) can wrap it.
- [ADR-0002: Explicit-failure 404 on `:80` instead of HTTPS redirect](decisions/0002-autocert-explicit-failure-on-port-80.md) — pass `http.NotFoundHandler()` to `autocert.Manager.HTTPHandler`, not `nil`; loud failure over silent scheme-switching.
- [ADR-0001: Routing envelope shape and opacity](decisions/0001-routing-envelope-shape-and-opacity.md) — `json.RawMessage` for the inner frame; sentinel errors; validate at boundary, never parse payloads.

## Architecture

- [System overview](../architecture.md) — top-level: stateless WS router between phones and pyry binaries. (Lives at `docs/architecture.md`; not yet split into `architecture/`.)

## Cross-cutting

- [Threat model](../threat-model.md) — operational threats to the relay-as-deployed-process: deploy, supply chain, DoS, log hygiene, cert handling, TLS config, error leakage. Complements (does not replace) the protocol spec's wire-protocol Security model.
- [Project memory](../PROJECT-MEMORY.md) — what's built, patterns established, current state.
- [Lessons](../lessons.md) — gotchas worth carrying forward.
