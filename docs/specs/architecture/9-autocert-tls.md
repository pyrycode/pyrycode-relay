# Spec — Autocert TLS for `--domain` mode (#9)

## Files to read first

- `cmd/pyrycode-relay/main.go:21-78` — flag parsing, the existing insecure-listen path with its `http.Server` timeouts (mirror them in the TLS path), the current "not yet implemented" stub the spec replaces, and `defaultCertCache()`.
- `internal/relay/doc.go` — package doc; the new TLS file lives next to `envelope.go` and stays consistent with the package's "routing core" framing.
- `internal/relay/envelope.go:1-19` — established conventions (sentinel errors via `errors.New`, package-level docstrings naming the invariant).
- `docs/architecture.md:14` — names the relay as the cert holder ("TLS terminus … autocert via Let's Encrypt"); confirms the design intent.
- [`pyrycode/pyrycode/docs/protocol-mobile.md` § TLS](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#tls) — wire-spec authority for TLS terminus and `421 Misdirected Request` semantics.
- `Makefile:14-23` — `make test` is `go test -race ./...`; `make lint` runs `gosec` (so `tls.Config.MinVersion` must be set explicitly to satisfy G402) and `govulncheck` (autocert is a fresh dep — must be a tagged release).
- `go.mod` — module is `github.com/pyrycode/pyrycode-relay`, Go 1.26.2; this ticket adds the first non-stdlib dep.
- `README.md:33-44` — current "Run" section + flag table; the doc edits adjust this region.

## Context

`cmd/pyrycode-relay/main.go:68-70` currently refuses to start when `--domain` is set ("autocert TLS path not yet implemented"). This ticket fills it in so production deployment is a single command:

```
pyrycode-relay --domain relay.example.com
```

Two listeners come up: `:443` for WSS (cert via `golang.org/x/crypto/acme/autocert`), `:80` for the ACME http-01 challenge. The relay terminates TLS itself; no reverse proxy in the production path.

Two invariants drive the design:

1. **Single-domain only** — `autocert.HostWhitelist(*domain)` rejects ACME issuance for any other host, and a request-level wrapper returns `421 Misdirected Request` for any HTTPS request whose `Host` header doesn't match (a TLS handshake using the cert's SNI doesn't bind the Host header — they can disagree, and the spec says they must match).
2. **Explicit failure for misconfigured clients** — port-80 traffic that isn't an ACME challenge gets `404`, not a redirect to HTTPS. Per AC: *"we do NOT redirect to HTTPS — we want explicit-failure semantics."*

> **Spec note (departure from AC literal wording).** The ticket says
> "`manager.HTTPHandler(nil)` … the `nil` argument means non-challenge
> HTTP requests get a 404." That is incorrect about autocert's
> behaviour. Per the autocert docs, `HTTPHandler(nil)` redirects GET/HEAD
> to HTTPS with `302` and returns `400` for other methods — i.e. exactly
> the redirect semantics the AC says we don't want. To honour the AC's
> stated *intent* (explicit-failure 404), the developer must pass
> `http.NotFoundHandler()` explicitly, not `nil`. The wiring section
> below uses `mgr.HTTPHandler(http.NotFoundHandler())`. PO/code-review:
> please confirm intent matches; if a 302 redirect is actually wanted,
> reopen the ticket and the spec changes one line.

## Design

### Package & files

- New file: `internal/relay/tls.go` (`package relay`) — autocert manager construction, cache-dir creation, host-enforcement middleware.
- New test file: `internal/relay/tls_test.go` (`package relay` — same package, so it can reach unexported helpers and follow the convention established in `envelope_test.go`).
- Modified: `cmd/pyrycode-relay/main.go` — replace the stub at lines 66-70 with the autocert wiring.
- Modified: `go.mod` (+ `go.sum` regenerated) — add `golang.org/x/crypto/acme/autocert`.
- Modified: `README.md:33-44` — move `--domain` to the primary Run section, add ACME caveat.

No new package; this is one of three things the existing `internal/relay` package owns ("routing core: per-server connection registry, frame forwarding, header validation" + now TLS terminus). Splitting `tls` into its own subpackage isn't worth the extra import surface for ~80 LOC.

### Public surface (`internal/relay/tls.go`)

Three exports plus one sentinel error. Kept small on purpose — `cmd/pyrycode-relay/main.go` does the wiring (server construction, timeouts, listener startup); `internal/relay` provides only the testable pieces.

```go
// ErrCacheDirInsecure is returned by NewAutocertManager when CacheDir
// already exists with permissions broader than 0700. TLS private keys
// live there; the relay refuses to start with a world- or group-readable
// cache rather than silently weakening the deployment.
var ErrCacheDirInsecure = errors.New("relay: cert cache dir has insecure permissions (must be 0700)")

// NewAutocertManager returns an autocert.Manager bound to the single
// domain via HostWhitelist. The cache dir is created with mode 0700 if
// missing. If it already exists with permissions broader than 0700, the
// function returns ErrCacheDirInsecure (wrapped with the dir path).
//
// The returned manager terminates ACME http-01 challenges via its
// HTTPHandler (mount on :80) and serves certificates via TLSConfig
// (mount on :443).
func NewAutocertManager(domain, cacheDir string) (*autocert.Manager, error)

// EnforceHost wraps next so that any request whose Host header does not
// match domain (case-insensitive, port-tolerant) receives 421
// Misdirected Request with no body. Used on the :443 handler chain so a
// client that resolves the cert via SNI but sends an unrelated Host
// header is rejected per the protocol spec's TLS section.
//
// Port-80 traffic does NOT use this wrapper: the autocert HTTPHandler
// owns its own host policy via the manager's HostPolicy.
func EnforceHost(domain string, next http.Handler) http.Handler

// TLSConfig returns m.TLSConfig() with MinVersion forced to TLS 1.2.
// autocert's default config does not set MinVersion explicitly, which
// trips gosec G402; this helper centralises the override so callers
// never see an un-pinned config.
func TLSConfig(m *autocert.Manager) *tls.Config
```

### `NewAutocertManager` — algorithm

1. If `domain == ""` or `cacheDir == ""` → return `nil, errors.New("relay: domain and cacheDir required")`. (Defensive — `main` validates flags upstream, but the helper doesn't trust its caller blindly.)
2. Stat `cacheDir`:
   - **Not exist** → `os.MkdirAll(cacheDir, 0o700)`. Return error wrapped with the path on failure.
   - **Exists, is dir, mode `& 0o077 == 0`** → no-op.
   - **Exists, is dir, mode `& 0o077 != 0`** → return `fmt.Errorf("%w: %s (mode %o)", ErrCacheDirInsecure, cacheDir, mode)`.
   - **Exists, not a dir** → return `fmt.Errorf("relay: cert cache path is not a directory: %s", cacheDir)`.
3. Construct and return:

   ```go
   &autocert.Manager{
       Cache:      autocert.DirCache(cacheDir),
       Prompt:     autocert.AcceptTOS,
       HostPolicy: autocert.HostWhitelist(domain),
   }
   ```

   No `Email` field — Let's Encrypt expiry mail is nice-to-have, not load-bearing, and a `--acme-email` flag is out of scope per the ticket's "kept small" framing.

The mode check on the existing-dir branch is *stricter* than the AC's "existing dir → no-op" wording. The reason is the security-review pass below: TLS private keys live there; silently accepting an attacker-readable cache dir is the kind of footgun the relay's "internet-exposed; adversarial input is the default" stance forbids. The existing dir is a no-op only when it's already secure; otherwise the relay refuses to start (loud failure, fits the project's `--insecure-listen` precedent).

### `EnforceHost` — algorithm

```go
func EnforceHost(domain string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        host := r.Host
        if h, _, err := net.SplitHostPort(host); err == nil {
            host = h
        }
        if !strings.EqualFold(host, domain) {
            w.WriteHeader(http.StatusMisdirectedRequest) // 421
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

- `r.Host` may carry a `:port` suffix (RFC 7230 §5.4 allows it); `net.SplitHostPort` strips it. If splitting fails, `r.Host` had no port — use as-is.
- Comparison is case-insensitive (`strings.EqualFold`) because hostnames are. No IDN-equivalence handling — `--domain` is operator input; if they configure a punycode domain, the cert and the Host header will both be punycode.
- No body on the 421 response. Adversaries don't need a description; legitimate clients shouldn't be hitting this.
- Don't write any header beyond status — calling `w.WriteHeader(421)` then `return` is the minimal correct response.

### `TLSConfig(m *autocert.Manager) *tls.Config`

```go
func TLSConfig(m *autocert.Manager) *tls.Config {
    cfg := m.TLSConfig()
    cfg.MinVersion = tls.VersionTLS12
    return cfg
}
```

Tiny, but it's the only place in the repo that touches `tls.Config`, and centralising it means a future "switch to 1.3-min" is a one-line change with one test to update. Without `MinVersion`, `gosec` G402 fires on `make lint`.

### `cmd/pyrycode-relay/main.go` — wiring

Replace the stub at lines 66-70 with:

```go
mgr, err := relay.NewAutocertManager(*domain, *certCache)
if err != nil {
    logger.Error("autocert setup failed", "err", err)
    os.Exit(1)
}

httpsSrv := &http.Server{
    Addr:              ":443",
    Handler:           relay.EnforceHost(*domain, mux),
    TLSConfig:         relay.TLSConfig(mgr),
    ReadHeaderTimeout: 10 * time.Second,
    ReadTimeout:       60 * time.Second,
    WriteTimeout:      60 * time.Second,
    IdleTimeout:       120 * time.Second,
}

httpSrv := &http.Server{
    Addr:              ":80",
    // NotFoundHandler — NOT nil. nil would redirect GET/HEAD to HTTPS
    // with 302; the AC requires explicit 404 for non-challenge traffic.
    // See "Spec note" in the Context section above.
    Handler:           mgr.HTTPHandler(http.NotFoundHandler()),
    ReadHeaderTimeout: 10 * time.Second,
    ReadTimeout:       60 * time.Second,
    WriteTimeout:      60 * time.Second,
    IdleTimeout:       120 * time.Second,
}

logger.Info("starting", "version", Version, "mode", "autocert",
    "domain", *domain, "cert_cache", *certCache)

go func() {
    if err := httpSrv.ListenAndServe(); err != nil {
        logger.Error("http-01 listener failed", "err", err)
        os.Exit(1)
    }
}()

if err := httpsSrv.ListenAndServeTLS("", ""); err != nil {
    logger.Error("https listener failed", "err", err)
    os.Exit(1)
}
```

`ListenAndServeTLS("", "")` with `TLSConfig.GetCertificate` populated (which `manager.TLSConfig()` does) is the documented autocert pattern — the empty cert/key paths tell `net/http` to defer to `GetCertificate`.

### Concurrency model

Two goroutines:

1. **Main goroutine** — runs `httpsSrv.ListenAndServeTLS`. Blocks until the listener fails. On error, log and `os.Exit(1)`.
2. **Background goroutine** — runs `httpSrv.ListenAndServe` for ACME http-01. Blocks similarly; on error, log and `os.Exit(1)`.

No shared mutable state between them. `*autocert.Manager` is read-only after construction (its `Cache` and `HostPolicy` are set once); the manager is internally goroutine-safe per the autocert docs.

No graceful shutdown. The existing insecure path doesn't have one either, and a graceful-shutdown ticket can be opened separately if it ever matters operationally. `os.Exit(1)` on listener failure mirrors the insecure path exactly.

### Error handling

- `NewAutocertManager` failures (cache dir insecure / not a directory / mkdir failed) → log + `os.Exit(1)` in `main`. Loud failure; the operator must see this.
- HTTPS listener failure → log + `os.Exit(1)`.
- HTTP listener failure → log + `os.Exit(1)`. Even though :80 is "just" the ACME challenge port, losing it means cert renewals stop ~30 days from now and the operator should know immediately.
- Per-request errors:
  - Wrong Host on :443 → `421` (handled by `EnforceHost`).
  - Non-challenge traffic on :80 → `404` (handled by `manager.HTTPHandler(nil)`).
  - ACME issuance failure for the wrong domain → autocert returns the error to the TLS handshake; the client sees a TLS alert. No log handling on our side beyond what autocert emits.

No sentinel errors beyond `ErrCacheDirInsecure`. Other failures are deployment-time and one-shot; sentinels would be over-engineering.

## Testing strategy

`internal/relay/tls_test.go`, `package relay`. No external libs; `testing` + `httptest` only.

### `TestNewAutocertManager_CreatesCacheDirWith0700`

```go
parent := t.TempDir()
cache := filepath.Join(parent, "certs")           // does not exist yet
m, err := NewAutocertManager("relay.example.com", cache)
// require: err == nil, m != nil
info, _ := os.Stat(cache)
// require: info.IsDir()
// require: info.Mode().Perm() == 0o700
```

### `TestNewAutocertManager_ExistingSecureDirIsNoOp`

Pre-create the dir with mode `0o700`, drop a sentinel file inside, call again, verify no error and the sentinel still there with the dir mode unchanged.

### `TestNewAutocertManager_ExistingInsecureDirRejected`

Pre-create with `0o755` (or `0o750`). Expect `errors.Is(err, ErrCacheDirInsecure)`. Skip on Windows where Unix mode bits are advisory.

```go
if runtime.GOOS == "windows" {
    t.Skip("permission bits don't apply on Windows")
}
```

### `TestNewAutocertManager_HostPolicyAcceptsConfiguredDomain`

```go
m, _ := NewAutocertManager("relay.example.com", t.TempDir()+"/c")
// require: m.HostPolicy(context.Background(), "relay.example.com") == nil
```

### `TestNewAutocertManager_HostPolicyRejectsOtherDomains`

Table-driven:

| Input | Expectation |
|---|---|
| `evil.example.com` | non-nil error |
| `RELAY.EXAMPLE.COM` | non-nil error (HostWhitelist is case-sensitive; document this) |
| `""` | non-nil error |
| `relay.example.com.evil.com` | non-nil error |

The relay's contract is: HostWhitelist is the gate. We're testing autocert behaves as advertised, not re-implementing it — but the test pins our assumption so a future autocert change doesn't silently weaken the deployment.

### `TestEnforceHost`

`httptest.NewRecorder` + a stub next-handler that flips a bool. Table-driven cases:

| `r.Host` | `domain` | Expected status | Stub called? |
|---|---|---|---|
| `relay.example.com` | `relay.example.com` | 200 | yes |
| `relay.example.com:8443` | `relay.example.com` | 200 | yes |
| `RELAY.EXAMPLE.COM` | `relay.example.com` | 200 | yes (case-insensitive) |
| `evil.com` | `relay.example.com` | 421 | no |
| `` (empty) | `relay.example.com` | 421 | no |
| `relay.example.com.evil.com` | `relay.example.com` | 421 | no |

### `TestTLSConfig_PinsMinVersionToTLS12`

```go
m := &autocert.Manager{} // bare; we only inspect the returned config
cfg := TLSConfig(m)
// require: cfg.MinVersion == tls.VersionTLS12
// require: cfg.GetCertificate != nil  // autocert wired through
```

### What we deliberately do not test

- **Real ACME issuance** — needs a public DNS record and a Let's Encrypt account. Verified manually on first deploy. The ticket says so.
- **`:443` / `:80` listener startup** — that's `main.go`, integration territory; binding privileged ports in a unit test isn't worth it.
- **Cert renewal timing** — autocert's job; not in our codebase to test.

## README updates (`README.md:33-44`)

Replace the current Run + flag table region with:

```markdown
## Run

Production (autocert):

\`\`\`bash
sudo ./bin/pyrycode-relay --domain relay.example.com
\`\`\`

The relay binds `:443` (WSS) and `:80` (ACME http-01 challenge). Both
ports must be reachable from the public internet — Let's Encrypt issues
the cert by hitting `:80` on first request to the domain. The first WSS
request after a fresh start may take ~10–20s while the cert is issued
and cached to `--cert-cache`. Subsequent restarts reuse the cached cert.

Behind a reverse proxy (TLS terminated upstream):

\`\`\`bash
./bin/pyrycode-relay --insecure-listen :8080
\`\`\`

| Flag | Default | Notes |
|---|---|---|
| `--domain` | (required for autocert) | Public domain for Let's Encrypt cert issuance. Required when `--insecure-listen` is unset. |
| `--cert-cache` | `~/.pyrycode-relay/certs` | Directory for autocert's TLS certificate cache. Created with `0700` if missing; refuses to start if an existing dir is world- or group-readable. |
| `--insecure-listen` | (unset) | Listen address for plain HTTP (e.g. `:8080`). Disables autocert. Use only when fronted by a reverse proxy. |
| `--version` | | Print version and exit. |
```

The `sudo` is honest — `:80` and `:443` are privileged ports. Operators who use `setcap` or systemd's `AmbientCapabilities` know to substitute. We don't pretend root is optional.

## Open questions

1. **`--acme-email` flag.** Out of scope. Let's Encrypt expiry-warning mail goes to the registered account; the registered account is autocert's anonymous default. A future ticket can add the flag if/when an operator wants the warnings. Cost of adding it now: another flag, another doc entry, no proven demand.
2. **Connection / IP rate limits.** Out of scope. Adversarial-input concerns at the connection-count level are a separate ticket (no number assigned yet) — the relay's threat model treats DoS as an operator-deploys-behind-a-CDN concern in v1.
3. **Existing-dir mode policy.** The spec is stricter than the AC ("existing dir → no-op"). Justified in the security review section below; flagged here so PO/code-review notice the deliberate departure.
4. **Cert cache file mode.** autocert's `DirCache` writes files with `0o600` (verified against `golang.org/x/crypto/acme/autocert/cache.go` at the version we'll pin). We don't enforce; we assume autocert's published behaviour. If autocert ever loosens this, a future audit catches it.
5. **`HostWhitelist` case sensitivity.** Verified: autocert lowercases the configured domain at construction and compares lowercased. Operator passing `Relay.Example.com` works for issuance. Documented in the test that case-mismatched issuance requests fail — that's autocert's behaviour, not ours.

## Out of scope (re-stated, for the developer)

- No graceful shutdown / signal handling. Mirror the existing insecure path's startup style (`os.Exit(1)` on listener error).
- No multi-domain support. `autocert.HostWhitelist` accepts a variadic — do not exploit that. Single domain only.
- No DNS-01 / wildcard certs.
- No cert-renewal metrics or observability.
- No self-signed dev fallback. `--insecure-listen` exists for that.
- No edits to `internal/relay/envelope.go`, `internal/relay/doc.go`, or any other existing file beyond `cmd/pyrycode-relay/main.go` and `README.md`.

## Done means

- `internal/relay/tls.go` exports `NewAutocertManager`, `EnforceHost`, `TLSConfig`, and `ErrCacheDirInsecure`, each with a doc comment naming the contract.
- `internal/relay/tls_test.go` covers every row of the test tables above.
- `cmd/pyrycode-relay/main.go` autocert path is wired; the "not yet implemented" stub is gone.
- `go.mod` adds `golang.org/x/crypto/acme/autocert` (latest tagged release at developer's run time); `go.sum` regenerated via `go mod tidy`.
- `README.md` updated per the section above.
- `make vet`, `make test`, `make build`, `make lint` all clean from the repo root.
- One commit on `feature/9`: `feat(relay): autocert TLS for --domain mode (#9)`.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings — the only data crossing untrusted-to-trusted is (a) the `Host` header on inbound HTTPS requests, gated by `EnforceHost` returning 421 before downstream code sees it, and (b) the ACME challenge URL on `:80`, gated by autocert's own `HostWhitelist`. No user input reaches filesystem paths; `cacheDir` is operator-controlled at flag-parse time.
- **[Tokens, secrets, credentials]** SHOULD FIX (deferred to ops doc, not gating) — TLS private keys live in `cacheDir`. The spec enforces dir mode `0o700` on creation AND on existing dirs (`ErrCacheDirInsecure`), going beyond the AC's "no-op on existing dir." Per-file mode is autocert's responsibility (`DirCache` writes `0o600`, verified). No tokens to rotate or revoke at this layer; that's binary-side.
- **[File operations]** No findings — no path traversal (no user input concatenated into paths), no TOCTOU window between perm check and use that an attacker could exploit (the cache dir is operator-owned; an attacker who already has write access to `~/.pyrycode-relay/` has already won), no symlink-following concerns documented in autocert (it uses `os.WriteFile`/`os.ReadFile` on the configured dir; symlink attacks would require pre-existing write access). Atomic writes are autocert's contract, not ours.
- **[Subprocess]** N/A — no subprocess execution in this ticket.
- **[Cryptographic primitives]** No findings — TLS via `crypto/tls`; ACME via the standard `golang.org/x/crypto/acme/autocert` package. `MinVersion` pinned to TLS 1.2 (Go's secure-default cipher suites kick in for 1.2; 1.3 doesn't expose suite selection). No hand-rolled crypto. No constant-time compare needed at this layer (no secret-vs-attacker-input comparisons).
- **[Network & I/O]** No findings — both `:443` and `:80` `http.Server` instances carry the established `ReadHeaderTimeout: 10s, ReadTimeout: 60s, WriteTimeout: 60s, IdleTimeout: 120s` set, mirroring the insecure path. Slow-loris is bounded by `ReadHeaderTimeout`. `gosec` G114 stays clean. **OUT OF SCOPE:** per-IP / per-connection caps deferred to a future connection-limits ticket; the protocol spec's threat model treats v1 deployments as expecting a CDN or operator-run firewall in front for volumetric DoS.
- **[Error messages, logs, telemetry]** No findings — the 421 response carries no body (no internal-state leak). `slog` log lines name `domain`, `cert_cache`, `version` — none are secrets. No request headers, no payloads, no tokens are logged. Listener-failure errors include the underlying `net.OpError` string (port + reason); that's expected operator-facing output, not user-controlled.
- **[Concurrency]** No findings — two goroutines, no shared mutable state (the `autocert.Manager` is constructed once and used read-only; its internal locks are autocert's contract). Both goroutines exit only on listener failure → process exit; no leakage path. No locks taken in our code.
- **[Threat model alignment]** Addresses the protocol spec's TLS section: relay is the cert holder ✓, `421 Misdirected Request` for Host mismatches ✓, single-domain enforcement ✓. Deferred: rate-limiting (out of scope), graceful shutdown (out of scope), threat-model doc itself (`docs/threat-model.md` is still planned, not blocking this ticket).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-08
