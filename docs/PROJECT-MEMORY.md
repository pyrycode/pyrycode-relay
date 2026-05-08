# Project memory — pyrycode-relay

Stateless WebSocket router between mobile clients and pyry binaries. Internet-exposed; adversarial input is the default assumption. Authoritative wire spec lives in `pyrycode/pyrycode/docs/protocol-mobile.md`.

## What's built

| Area | Status | Where |
|---|---|---|
| Go skeleton | Done | `cmd/pyrycode-relay/main.go`, `internal/relay/doc.go` |
| `http.Server` with explicit timeouts (gosec G114) | Done | `cmd/pyrycode-relay/main.go` |
| Routing envelope wrapper type (`Envelope`, `Marshal`, `Unmarshal`, sentinel errors) | Done (#1) | `internal/relay/envelope.go` |
| Connection registry (`Conn`, `Registry`; 1:1 binary, 1:N phones; sentinel errors for `4409`/`4404`; race-tested) | Done (#3) | `internal/relay/registry.go` |
| TLS via autocert in `--domain` mode (`NewAutocertManager`, `EnforceHost`, `TLSConfig`, `ErrCacheDirInsecure`) | Done (#9) | `internal/relay/tls.go`, `cmd/pyrycode-relay/main.go` |
| `WSConn` adapter (`nhooyr.io/websocket.Conn` → registry `Conn`; per-conn write mutex; `Close`-cancelled context; 10s `Send` deadline) | Done (#15) | `internal/relay/ws_conn.go` |
| `/healthz` JSON endpoint (`status`, `version`, `connected_binaries`, `connected_phones`, `uptime_seconds`; `Cache-Control: no-store`; unauthenticated) | Done (#10) | `internal/relay/healthz.go`, `cmd/pyrycode-relay/main.go` |
| WS upgrade on `/v1/server` (header gate pre-upgrade; `ClaimServer`; `4409` on conflict; `CloseRead`-held until #6) | Done (#16) | `internal/relay/server_endpoint.go`, `cmd/pyrycode-relay/main.go` |
| WS upgrade on `/v1/client` | Not started | — |
| Header validation (`x-pyrycode-server`, `x-pyrycode-version`, `user-agent` on `/v1/server`; `x-pyrycode-token` on `/v1/client`) | `/v1/server` done (#16); `/v1/client` not started | `internal/relay/server_endpoint.go` |
| Frame forwarding using the routing envelope | Not started | — |
| `conn_id` generation scheme | Not started | — |
| Threat model doc — operational surface (deploy, supply chain, DoS, log hygiene, cert handling, TLS, error leakage) | Done (#11) | `docs/threat-model.md` |

## Patterns established

- **Sentinel errors, branched via `errors.Is`** for validation failures at protocol boundaries. New routing-layer code follows the `Err...` naming and wraps with `fmt.Errorf("…: %w", err, sentinel)` when adding context.
- **Opacity by type.** Inner-frame payloads are carried as `json.RawMessage`. The relay never deserialises payloads; the type makes that hard to violate accidentally.
- **Validate at the envelope boundary, not deeper.** Structural checks (presence, non-empty, JSON well-formedness) belong here; semantic checks (token validity, message kind) belong to the binary.
- **Stdlib + `golang.org/x/crypto` + `nhooyr.io/websocket`.** Direct deps arrived in #9 (`acme/autocert`) and #15 (`nhooyr.io/websocket` v1.8.x). Keep the dependency surface deliberate; new deps need a justification (the ADR is the justification).
- **Loud failure over silent correction.** `--domain` and `--insecure-listen` are mutually exclusive and explicit. `ErrCacheDirInsecure` refuses to start on a world/group-readable cert cache rather than re-chmoding it. `:80` returns `404` for non-challenge traffic instead of redirecting to HTTPS (ADR-0002).
- **Tests live in the same package** (`package relay`, not `relay_test`) so they can `errors.Is` against unexported sentinels if needed and reach private helpers without re-exporting.
- **Passive in-memory stores guard mutation under one RWMutex; reads return copies, never references.** `PhonesFor` allocates a fresh slice so callers do slow work (broadcast, `Send` over the network) without holding the registry lock. Adopted in `internal/relay/registry.go` (#3); the same shape applies to any future "shared map of conns" type.
- **Interface methods called under the lock are documented as non-blocking getters.** The registry's `Conn.ConnID()` is invoked under the write lock during `UnregisterPhone`; `Send` and `Close` are never called while the lock is held. Pattern: state the contract on the interface, never call something that could block on I/O while a mutex is held.
- **Adapters bridge interface↔library API mismatches by owning policy locally.** When a library method needs a `context.Context` but the registry's `Conn` interface doesn't take one (and shouldn't — most callers don't have a context to thread), the adapter owns its own context: derived in the constructor, cancelled by `Close`, narrowed per-call with `WithTimeout` for deadline policy. Adopted in `WSConn` (#15); avoids forcing context-plumbing changes into upstream interfaces.
- **Handler factories return `http.Handler`, not `http.HandlerFunc`.** `NewHealthzHandler(reg, version, startedAt) http.Handler` keeps construction factory-shaped so a future ticket adding per-handler state (logger, injectable clock) can do so without touching the call site in `main`. Adopted in `/healthz` (#10).
- **Validate adversarial input *before* allocating WS state.** Header-gate checks (presence + non-empty) run before `websocket.Accept`; a malformed request never receives an upgrade response and the registry is untouched on every error path. Adopted in `/v1/server` (#16); the same shape applies to `/v1/client` token validation (#5).
- **Application WS close codes are emitted on the underlying `*websocket.Conn`, not via `WSConn`.** `WSConn.Close()` always emits `StatusNormalClosure` — no policy parameters. Handlers that need `4409` / `4401` / `4404` call `c.Close(websocket.StatusCode(code), reason)` directly in the stillborn-WSConn window (after construction, before any `Send`). See [ADR-0005](knowledge/decisions/0005-application-close-codes-via-underlying-conn.md). Adopted in `/v1/server` (#16); applies to `/v1/client` (#5).
- **`defer { Release; Close; log }` is registered AFTER the successful claim**, never before. A conflict path (or any pre-claim error) returns before the defer is in place, so we never `ReleaseServer` on a slot we don't hold. Adopted in `/v1/server` (#16); same structural ordering applies to `/v1/client`'s phone registration.
- **Hold long-lived WS handlers open with `c.CloseRead(r.Context())` plus `<-readCtx.Done()` until the real read loop lands.** `CloseRead` drains-and-discards frames (including control frames — pings must be processed for the connection to observe a peer-side close). The frame loop ticket replaces `<-readCtx.Done()` with the actual read body in the same call site. Adopted in `/v1/server` (#16) pending #6.
- **Capture process-state timestamps in `main` after `flag.Parse()`, not as package-level vars.** `startedAt := time.Now()` lives inside `main` and is passed into the handler factory. A package-level `var startedAt = time.Now()` would fire at import time — before flag parsing, before `--version` early-returns — and be wrong for short-lived test binaries and any future deferred-serve setup. Adopted in #10.

## Conventions

- Single ticket = single commit on `feature/<n>` named `feat(relay): <summary> (#<n>)`.
- `make vet`, `make test` (`-race`), and `make build` must all be clean before merge.
- Knowledge base lives under `docs/knowledge/` (features, decisions). One-line index in `docs/knowledge/INDEX.md`.
