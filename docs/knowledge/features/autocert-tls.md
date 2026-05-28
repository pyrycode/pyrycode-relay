# Autocert TLS (`--domain` mode)

The relay terminates TLS itself in production via Let's Encrypt and `golang.org/x/crypto/acme/autocert`. One command brings the public listener up:

```
sudo pyrycode-relay --domain relay.example.com
```

Two listeners come up: `:443` for WSS (cert via autocert) and `:80` for the ACME http-01 challenge. No reverse proxy in the production path.

Authoritative wire spec: [`pyrycode/pyrycode/docs/protocol-mobile.md` § TLS](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#tls).

## Modes

| Flag | When to use |
|---|---|
| `--domain <fqdn>` | Production. Relay holds the cert. Requires `:80` and `:443` reachable from the public internet. |
| `--insecure-listen :8080` | Behind a reverse proxy that terminates TLS upstream, or local dev. Disables autocert. |

The two are mutually exclusive; one of them must be set or the binary refuses to start.

## API

Package `internal/relay` (`tls.go`):

```go
func NewAutocertManager(domain, cacheDir string) (*autocert.Manager, error)
func EnforceHost(domain string, next http.Handler) http.Handler
func TLSConfig(m *autocert.Manager) *tls.Config

var ErrCacheDirInsecure = errors.New("relay: cert cache dir has insecure permissions (must be 0700)")
```

`cmd/pyrycode-relay/main.go` does the wiring (server construction, timeouts, listener startup); `internal/relay` provides only the testable pieces.

## Configuration

| Flag | Default | Notes |
|---|---|---|
| `--domain` | (required for autocert) | Single domain. Bound via `autocert.HostWhitelist(domain)`; ACME issuance for any other host is rejected. |
| `--cert-cache` | `~/.pyrycode-relay/certs` | Created with `0700` if missing. Refuses to start if an existing dir is world- or group-readable (`ErrCacheDirInsecure`). |
| `--insecure-listen` | (unset) | Disables autocert. |

The cache dir is the only on-disk state the relay keeps. TLS private keys live there (autocert writes them with `0o600`).

## Two host gates

Single-domain enforcement happens in two places, because the TLS handshake's SNI does not bind the HTTP `Host` header — they can legally disagree on the same connection.

1. **`autocert.HostWhitelist(domain)`** — gates ACME *issuance*. Without it, the relay would attempt to fetch certs for any name a client requests via SNI, which Let's Encrypt would either rate-limit us out of or, worse, succeed at for an attacker-controlled domain pointed at our IP.
2. **`EnforceHost(domain, mux)`** — gates *application traffic* on `:443`. Any request whose `Host` header doesn't match the configured domain (case-insensitive, port-tolerant) gets `421 Misdirected Request` with no body, before reaching the routing handlers.

`:80` does not use `EnforceHost`. The autocert HTTPHandler owns the host check there via the manager's `HostPolicy`.

## Explicit-failure semantics on `:80`

`:80` is wired as `mgr.HTTPHandler(http.NotFoundHandler())` — *not* `mgr.HTTPHandler(nil)`. Passing `nil` would redirect non-challenge traffic (GET/HEAD) to HTTPS with `302`. The relay deliberately returns `404` instead so that misconfigured clients fail loudly rather than silently switching schemes. See ADR-0002.

## Cert cache permissions

`NewAutocertManager` is stricter than the ticket's AC ("existing dir → no-op"). The actual policy:

- Missing → `os.MkdirAll(dir, 0o700)`.
- Exists, is dir, mode `& 0o077 == 0` → no-op (secure).
- Exists, is dir, mode `& 0o077 != 0`, **owned by relay's runtime euid** (Linux only) → `os.Chmod(dir, 0o700)` + INFO log (`"tightened cert cache dir perms"` with `path` / `from` / `to`), continue startup.
- Exists, is dir, mode `& 0o077 != 0`, owned by foreign uid (or ownership not discoverable, e.g. non-Linux) → `ErrCacheDirInsecure` (refuses to start).
- Exists, not a dir → error.

Reasoning: TLS private keys live there. Silently accepting an attacker-staged cache fits neither the relay's "internet-exposed; adversarial input is the default" stance nor the `--insecure-listen` precedent of loud failure.

The same-uid tightening is the only departure from the "loud failure over silent correction" project rule for this sentinel. It exists because Fly.io's `[[mounts]]` block has no `mode` field and the volume is remounted at 0755 on every machine boot — without the narrowing, every restart after a volume detach/reattach would trip `ErrCacheDirInsecure`. Foreign-uid 0755 still means "someone else staged a directory the relay is about to write TLS keys into" and continues to fail closed (#95). Ownership probe is Linux-only via `syscall.Stat_t.Uid` (`internal/relay/owner_linux.go`); on darwin / other GOOS the `owner_other.go` stub returns "ownership unknown" so the unchanged rejection path runs (strict superset of pre-#95 behaviour, no regression).

Per-file mode (`0o600`) is autocert's contract, not enforced here. The directory-perm tightening above relies on that contract — if autocert's `DirCache` ever changed file perms, a 0755 dir handed over with stale files would need a separate walk-and-re-tighten. Today it does not.

## Server timeouts

Both `httpsSrv` and `httpSrv` mirror the insecure path's timeouts:

| Setting | Value |
|---|---|
| `ReadHeaderTimeout` | 5s |
| `ReadTimeout` | 60s |
| `WriteTimeout` | 60s |
| `IdleTimeout` | 120s |

`ReadHeaderTimeout` bounds slow-loris on the pre-upgrade window on both ports and keeps `gosec` G114 quiet. Tightened from 10s to 5s in #35; post-upgrade slow peers are covered by per-frame deadlines (#15) and heartbeat (#7).

## TLS version

`TLSConfig(m)` returns `m.TLSConfig()` with `MinVersion = tls.VersionTLS12`. Centralised because:

- autocert's default config leaves `MinVersion` zero, which trips `gosec` G402 on `make lint`.
- A future "switch to 1.3-min" is a one-line change with one test to update.

Cipher selection is delegated to Go's secure-default suites for TLS 1.2; TLS 1.3 doesn't expose suite selection.

## Concurrency

Two goroutines, no shared mutable state:

1. Main — `httpsSrv.ListenAndServeTLS("", "")`. The empty cert/key paths defer to `TLSConfig.GetCertificate`, which `manager.TLSConfig()` populates. This is the documented autocert pattern.
2. Background — `httpSrv.ListenAndServe()` for ACME http-01.

Either listener failing → first error wins via #31's buffered `listenerErr` channel; `relay.Shutdown` drains both listeners (and the metrics listener) together; `run` returns exit 1. On SIGTERM/SIGINT both listeners drain via the same path and `run` returns exit 0.

The `*autocert.Manager` is constructed once and used read-only; its internal locking is autocert's contract.

## Operational notes

- `:80` and `:443` are privileged. Run with `sudo`, or grant `CAP_NET_BIND_SERVICE` via `setcap` / systemd `AmbientCapabilities`.
- First request to the domain after a fresh start may take ~10–20s while autocert issues and caches the cert. Subsequent restarts reuse the cache.
- Cert renewal is silent — autocert handles it. No metrics in v1.

## Out of scope (deferred)

- Multi-domain certs (`autocert.HostWhitelist` is variadic but the UX/ops story isn't worth solving until a second domain is needed).
- DNS-01 / wildcard certs.
- `--acme-email` for Let's Encrypt expiry mail.
- Renewal observability.
- Self-signed dev fallback (`--insecure-listen` exists for that).
- Per-IP / per-connection rate limits (separate ticket).
- Graceful shutdown / signal handling.

## Related

- [ADR-0002: Explicit-failure 404 on `:80` instead of HTTPS redirect](../decisions/0002-autocert-explicit-failure-on-port-80.md)
- [Architecture overview](../../architecture.md) — relay is the cert holder.
