# Project memory — pyrycode-relay

Stateless WebSocket router between mobile clients and pyry binaries. Internet-exposed; adversarial input is the default assumption. Authoritative wire spec lives in `pyrycode/pyrycode/docs/protocol-mobile.md`.

## What's built

| Area | Status | Where |
|---|---|---|
| Go skeleton | Done | `cmd/pyrycode-relay/main.go`, `internal/relay/doc.go` |
| `http.Server` with explicit timeouts (gosec G114) | Done | `cmd/pyrycode-relay/main.go` |
| Routing envelope wrapper type (`Envelope`, `Marshal`, `Unmarshal`, sentinel errors) | Done (#1) | `internal/relay/envelope.go` |
| TLS via autocert in `--domain` mode (`NewAutocertManager`, `EnforceHost`, `TLSConfig`, `ErrCacheDirInsecure`) | Done (#9) | `internal/relay/tls.go`, `cmd/pyrycode-relay/main.go` |
| WS upgrade on `/v1/server` and `/v1/client` | Not started | — |
| Header validation (`x-pyrycode-server`, `x-pyrycode-token`) | Not started | — |
| Connection registry (server-id ↔ binary, server-id ↔ phones) | Not started | — |
| Frame forwarding using the routing envelope | Not started | — |
| `conn_id` generation scheme | Not started | — |
| Threat model doc | Not started | `docs/threat-model.md` planned |

## Patterns established

- **Sentinel errors, branched via `errors.Is`** for validation failures at protocol boundaries. New routing-layer code follows the `Err...` naming and wraps with `fmt.Errorf("…: %w", err, sentinel)` when adding context.
- **Opacity by type.** Inner-frame payloads are carried as `json.RawMessage`. The relay never deserialises payloads; the type makes that hard to violate accidentally.
- **Validate at the envelope boundary, not deeper.** Structural checks (presence, non-empty, JSON well-formedness) belong here; semantic checks (token validity, message kind) belong to the binary.
- **Stdlib + `golang.org/x/crypto`.** First non-stdlib dep arrived in #9 (`acme/autocert`). Keep the dependency surface deliberate; new deps need a justification.
- **Loud failure over silent correction.** `--domain` and `--insecure-listen` are mutually exclusive and explicit. `ErrCacheDirInsecure` refuses to start on a world/group-readable cert cache rather than re-chmoding it. `:80` returns `404` for non-challenge traffic instead of redirecting to HTTPS (ADR-0002).
- **Tests live in the same package** (`package relay`, not `relay_test`) so they can `errors.Is` against unexported sentinels if needed and reach private helpers without re-exporting.

## Conventions

- Single ticket = single commit on `feature/<n>` named `feat(relay): <summary> (#<n>)`.
- `make vet`, `make test` (`-race`), and `make build` must all be clean before merge.
- Knowledge base lives under `docs/knowledge/` (features, decisions). One-line index in `docs/knowledge/INDEX.md`.
