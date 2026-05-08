# Knowledge Index

One-line pointers into the evergreen knowledge base. Newest entries at the top of each section.

## Features

- [Autocert TLS](features/autocert-tls.md) — `--domain` mode wiring: `:443` WSS via Let's Encrypt + `:80` ACME http-01, host gates, cache-dir permission policy, TLS 1.2 floor.
- [Routing envelope](features/routing-envelope.md) — typed Go wrapper (`Envelope`, `Marshal`, `Unmarshal`) for the `{conn_id, frame}` wire shape exchanged between relay and binary.

## Decisions

- [ADR-0002: Explicit-failure 404 on `:80` instead of HTTPS redirect](decisions/0002-autocert-explicit-failure-on-port-80.md) — pass `http.NotFoundHandler()` to `autocert.Manager.HTTPHandler`, not `nil`; loud failure over silent scheme-switching.
- [ADR-0001: Routing envelope shape and opacity](decisions/0001-routing-envelope-shape-and-opacity.md) — `json.RawMessage` for the inner frame; sentinel errors; validate at boundary, never parse payloads.

## Architecture

- [System overview](../architecture.md) — top-level: stateless WS router between phones and pyry binaries. (Lives at `docs/architecture.md`; not yet split into `architecture/`.)

## Cross-cutting

- [Threat model](../threat-model.md) — operational threats to the relay-as-deployed-process: deploy, supply chain, DoS, log hygiene, cert handling, TLS config, error leakage. Complements (does not replace) the protocol spec's wire-protocol Security model.
- [Project memory](../PROJECT-MEMORY.md) — what's built, patterns established, current state.
- [Lessons](../lessons.md) — gotchas worth carrying forward.
