# #110 — Parse Fly PROXY protocol v2 on the HTTPS listener

## Files read

- `cmd/pyrycode-relay/main.go` → `run` (flag set, the `--insecure-listen` mutual-exclusion guard, the autocert branch building `httpsSrv`, the `listen` closure passed to `runServers`), `runServers` — where the HTTPS listener is bound today via `ListenAndServeTLS`.
- `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_InsecureMutexWithAutocertListenFlags`, `freePort`, `waitForDial` — the mutex table the new flag joins and the harness for a wiring test.
- `internal/relay/client_ip.go` → `ClientIP` — reads `r.RemoteAddr`; unchanged, it inherits the PROXY source once the listener rewrites `RemoteAddr`.
- `internal/relay/ratelimit_middleware.go` → `NewRateLimitMiddleware`; `internal/relay/ratelimit_middleware_test.go` → `sentinelHandler`, `newMiddlewareTestLimiter` — reused by the new listener tests.
- `internal/relay/tls.go` → `NewAutocertManager` — creates a missing cache dir `0700`, so an e2e test can point `--cert-cache` at a fresh subdir of `t.TempDir()`.
- `internal/relay/log_keys_test.go` → `TestLogKeysAreAllowlisted` globs `internal/relay/*.go` only; the new file logs nothing.
- `github.com/pires/go-proxyproto@v0.15.0` → `Listener.Accept`, `Conn.RemoteAddr`, `Conn.readHeader`, `MaxV2HeaderSize` — the header is read lazily (first `RemoteAddr`/`Read`) on the caller's goroutine, not in `Accept`; `RemoteAddr` falls back to the socket peer on a header error or a `LOCAL` command, which drives the validator below.
- `fly.toml` → the `443` `[[services.ports]]` block and `[processes]`.
- `docs/threat-model.md` § *Supply chain*, § *DoS resistance*, § *Triggers for re-review*.

## Context

Behind Fly's raw-TCP services the socket peer is Fly's edge proxy, so every client shares one rate-limit bucket (see ticket). Fly forwards the client address only via the `proxy_proto` handler. This ticket enables PROXY v2 on the external 443 port and parses it on the autocert HTTPS listener before TLS, so `RemoteAddr` — and therefore `ClientIP` and the per-IP limiter — carry the real client address. No change to `ClientIP` or the middleware.

A new `go.mod` dependency (`github.com/pires/go-proxyproto v0.15.0`) trips `docs/threat-model.md` § *Triggers for re-review*. Justification: a maintained, fuzzed parser for an internet-exposed, pre-TLS path is preferable to a hand-rolled one; the root package imports stdlib only (no transitive modules compiled in); it sits before TLS and sees only the PROXY header and TLS ciphertext, never a routed frame in cleartext. The dependency choice is worth an ADR — the documentation phase writes it.

## Design

### `internal/relay/proxyproto.go` (new)

```go
// ErrProxyHeaderRejected: a syntactically valid PROXY header that is not
// a v2 PROXY-command header carrying a TCP source address.
var ErrProxyHeaderRejected = errors.New(...)

// NewProxyProtoListener wraps inner so each accepted conn must open with a
// PROXY protocol v2 header, read within headerTimeout; the header's source
// becomes RemoteAddr. headerTimeout <= 0 is an error (the library would
// otherwise fall back to 10s or disable the timeout).
func NewProxyProtoListener(inner net.Listener, headerTimeout time.Duration) (net.Listener, error)
```

Returns a `*proxyproto.Listener` configured with:

- `ConnPolicy` returning `proxyproto.REQUIRE` explicitly — not relying on the mutable package global `proxyproto.DefaultPolicy`.
- `ValidateHeader: validateProxyHeader` (unexported) — rejects with `ErrProxyHeaderRejected` unless `Version == 2`, `Command.IsProxy()`, and `SourceAddr` is a `*net.TCPAddr`. This closes the library's fallback paths: a `LOCAL` command or an `UNSPEC`/unix header would otherwise make `RemoteAddr` return the socket peer while reads succeed.
- `ReadHeaderTimeout: headerTimeout`.

Failure path for a missing / malformed / rejected / timed-out header: the library stores the error; the first `Read` (the TLS handshake inside `http.Server`'s per-conn goroutine) fails, so the handshake fails and `http.Server` closes the conn. No handler runs, so the fallback `RemoteAddr` never reaches `ClientIP`. Header reads happen in the per-conn goroutine (`http.Server` calls `RemoteAddr` at the top of its conn `serve`), so a stalled header never blocks `Accept`.

### `cmd/pyrycode-relay/main.go`

- New flag `--https-proxy-protocol` (bool, default `false`), usage string naming the Fly `proxy_proto` pairing and that every connection without a header is refused.
- Mutual-exclusion guard: add `setFlags["https-proxy-protocol"]` to the existing `--insecure-listen` guard; extend the error text. Exit 2.
- Policy constant `proxyHeaderTimeout = 5 * time.Second` (matches `ReadHeaderTimeout`; Fly writes the header immediately on connect, so a legitimate header never approaches it).
- The `listen` closure: for `httpsSrv` with the flag on — `net.Listen("tcp", s.Addr)`, `relay.NewProxyProtoListener`, `s.ServeTLS(ln, "", "")`; flag off keeps `s.ListenAndServeTLS("", "")` verbatim. Errors return through `runServers` exactly as today. The HTTP-01 listener is untouched. `CheckListenerPorts` sets unchanged.
- The `starting` log line gains `https_proxy_protocol` (bool; `main` is outside the allowlist glob, and a boolean config value is safe to log).

### `fly.toml`

On the `443` port: `handlers = ["proxy_proto"]` and `proxy_proto_options = { version = "v2" }`; rewrite the "handlers omitted on purpose" comment to say `proxy_proto` is the one allowed handler because it only prepends a header and leaves TLS passthrough intact, and that the handler and the flag must deploy/roll back together. Append `--https-proxy-protocol` to `[processes] app`. Port 80 unchanged.

### `go.mod` / `go.sum`

`require github.com/pires/go-proxyproto v0.15.0`.

## Concurrency model

No new goroutines. `http.Server.Serve` already runs one goroutine per accepted conn; the header read, bounded by `proxyHeaderTimeout`, happens there. Shutdown: `ServeTLS` tracks the `tls` listener wrapping the proxyproto listener; `Server.Shutdown`/`Close` closes it, which closes the inner TCP listener. Stalled conns are in `StateNew` and are force-closed by `Shutdown` like any other idle-new conn.

## Error handling

- Bind failure → `net.Listen` error returned from `listen` → `runServers` logs `listener failed` and drains, exit 1 (same as `ListenAndServeTLS` today).
- `NewProxyProtoListener` error (non-positive timeout; unreachable with the constant) → returned from `listen`, same path. Loud, not silent.
- Per-conn header errors are not logged by the relay; `http.Server`'s own `TLS handshake error` line (stdlib `ErrorLog`) is unchanged behaviour.

## Testing strategy

`internal/relay/proxyproto_test.go` (package `relay`), using `httptest.NewUnstartedServer` with `Listener` replaced by the wrapper and `StartTLS` (so header-before-TLS ordering is exercised), a client transport whose `DialContext` writes a v2 header built with `proxyproto.Header.WriteTo`, keep-alives off:

- `validateProxyHeader` table: v2 PROXY TCPv4 / TCPv6 accept; v1, LOCAL, UNSPEC, unix-stream reject with `ErrProxyHeaderRejected`.
- Separate buckets (AC1): rate limiter burst 1 wrapping `sentinelHandler`; source A → 200 then 429; source B → 200.
- Missing header (AC2): plain TLS dial → request fails, handler call count 0.
- Malformed header (AC2): v2 signature followed by an invalid version/command byte → request fails, handler 0. LOCAL header → request fails, handler 0 (no socket-peer fallback).
- Stall (AC2): header timeout ~1s; open a conn and write nothing; a valid-header request completes (200) while it is stalled; the stalled conn then reads EOF/reset within a bounded wait, handler count excludes it.
- Non-positive timeout → error.

`cmd/pyrycode-relay/main_e2e_test.go`:

- Mutex table: row `insecure+https-proxy-protocol` → exit 2 (AC3).
- `TestRun_HTTPSProxyProtocolWiring`: `run` in autocert mode (`--domain relay.invalid`, temp cert cache, free ports, metrics off) with and without the flag; write a plain `GET` to the HTTPS port. Flag off → Go's `400` "HTTP request to an HTTPS server" response (behaviour unchanged); flag on → conn closed with zero bytes. No ACME traffic: no TLS handshake ever reaches `GetCertificate`.

Existing tests unchanged (AC3 flag-off).

## Open questions

- Exact Fly key spelling `proxy_proto_options = { version = "v2" }` — from Fly's `fly.toml` reference; cannot be validated offline. Named in the PR for the operator deploy.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/fly-deploy.md` § *TCP passthrough, not Fly's HTTP proxy*: replace "real socket peer IP reaches the relay verbatim"; client IP now comes from PROXY v2 on 443.
- `docs/knowledge/features/rate-limit-middleware.md` § *Trust model*: PROXY protocol is the production rate-limit key source.
- `docs/threat-model.md` § *DoS resistance*: per-IP key comes from Fly's PROXY header; § *Supply chain*: re-review for `github.com/pires/go-proxyproto v0.15.0` (trigger tripped).
- `docs/deploy.md` (fly.toml gotchas, rollback): `proxy_proto` handler and `--https-proxy-protocol` must move together.
- ADR for adopting `go-proxyproto` (see Context).

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] The PROXY header is a new trust boundary: its source address becomes the rate-limit key. It is parsed once, in `NewProxyProtoListener`'s wrapped conns, and narrowed by `validateProxyHeader` (v2, PROXY command, TCP source) — no path hands a handler the socket peer when the flag is on. Content-blindness holds: the header is L4 addressing read before TLS; no frame is touched.
- [Trust boundaries] OUT OF SCOPE — under `REQUIRE` the header is honoured from any upstream. Anything that can reach internal port 8443 without Fly's edge (another machine on the org's 6PN private network) can forge a source IP and pick its rate-limit bucket. That is an org-internal attacker; restricting upstreams to Fly proxy ranges needs ranges Fly does not publish as stable. Candidate for `docs/security-followups.md` via the documentation stage.
- [Tokens / File ops / Subprocess / Crypto] No findings — no credentials, files, subprocesses or crypto added; TLS config (`TLSConfig`) unchanged and still wraps the stream after the header.
- [Network & I/O] Header read is size-capped by the library's `MaxV2HeaderSize` (4096; a package var nobody in the relay sets) and time-capped by `proxyHeaderTimeout` (5s), enforced by a read deadline per conn. The `http.Server` timeouts on `httpsSrv` are unchanged. A stalled header holds one goroutine + fd for ≤5s — the same exposure `ReadHeaderTimeout` already accepts; no amplification.
- [Network & I/O] SHOULD FIX (in Phase B) — set `ConnPolicy` explicitly to `REQUIRE` rather than trusting `proxyproto.DefaultPolicy`, a mutable global; and reject non-positive timeouts, since the library treats 0 as 10s and negative as "no timeout".
- [Errors / logs] No findings — no new relay log keys; no header value is written to a client; failed conns are closed with no bytes.
- [Concurrency] No new goroutines; shutdown path is `http.Server`'s tracked-listener close.
- [Threat model] Re-review trigger (new `go.mod` dependency) tripped → Documentation handoff. Deploy coupling: flag and `proxy_proto` must ship and roll back together or 443 fails closed — named in `fly.toml` comment and the handoff for `docs/deploy.md`. Fails closed, not open, in both mismatch directions.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24

## Revisions

- 2026-09-24 (implementation): Open question on `proxy_proto_options = { version = "v2" }` — kept as written; `fly.toml` parses as valid TOML with the expected shape, but Fly's acceptance of the key is only provable at `flyctl deploy`, so it is named as an operator follow-up in the PR. No design change.
