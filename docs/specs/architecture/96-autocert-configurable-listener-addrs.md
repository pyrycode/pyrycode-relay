# Spec: configurable listener addresses for the autocert HTTP-01 and TLS listeners

Ticket: [#96](https://github.com/pyrycode/pyrycode-relay/issues/96). Size S. Substrate-portability change: today's hardcoded `:80` / `:443` is unbindable from a non-root uid without `CAP_NET_BIND_SERVICE`. Two new operator-facing flags let the substrate forward external 80/443 to high internal ports while the relay keeps owning autocert end-to-end.

## Files to read first

- `cmd/pyrycode-relay/main.go:49-75` — `run()`'s flag declarations and the existing `--insecure-listen` / `--domain` mutual-exclusion block. The two new flags slot in beside `insecureListen` (line 54); the new "insecure ⊕ autocert flags" guard slots in beside the existing line-72 guard.
- `cmd/pyrycode-relay/main.go:201-255` — insecure-mode branch. No new behaviour here, but read it to confirm the new flags must short-circuit BEFORE this branch runs (they are autocert-mode-only).
- `cmd/pyrycode-relay/main.go:257-331` — autocert-mode branch. Lines 263–283 hold the two `*http.Server` literals whose `Addr:` fields change; lines 285–315 hold the `ListenerPort` + `CheckListenerPorts` block whose expected-port set is the AC4 target.
- `internal/relay/listeners.go:24-51` — `ListenerPort` contract (rejects empty, port 0, non-numeric, out-of-range). Reused verbatim for the new flags; no edits.
- `internal/relay/listeners.go:53-86` — `CheckListenerPorts` asymmetric-set contract. The expected/actual set semantics that AC4 generalises live here.
- `internal/relay/metrics_listen.go:80-95` — `NewMetricsServer` adds the optional metrics port to both expected and actual sets in `main.go`. Mirror this pattern; do not introduce a different shape for the new flags.
- `internal/relay/production.go:34-46` — canonical "boot-time refusal helper" shape (sentinel + `Check…` + injected seam). The new mutex guard is NOT extracted into this shape — see § Why the mutex guard is inline. Read this so the contrast is intentional and audit-able.
- `cmd/pyrycode-relay/main_e2e_test.go:120-149` — `freePort` / `waitForDial` / `run([]string{...}, ctx)` test harness. The AC3 mutex test reuses `run()` directly; no listener bind is needed because the mutex check fires before any listener is constructed.
- `docs/specs/architecture/81-listener-port-allowlist-boot-check.md` § "Wiring in cmd/pyrycode-relay/main.go" — the precedent that the expected-port set is built from the operator-configured listeners (insecure branch already does this; this spec extends the same shape to the autocert branch). § "What is NOT tested" sets the policy that `main` wiring is not unit-tested; only the AC3 mutex gets a thin e2e check because the helper for the mutex is not extracted.
- `docs/specs/architecture/77-refuse-insecure-in-production.md` § "Why not a `Config` struct" — the rationale this spec inherits for keeping trivial flag-shape checks inline. The new mutex check is the second inline mutex in `run()` (the first is `--insecure-listen ⊕ --domain` at line 72) and follows the same shape.
- `docs/PROJECT-MEMORY.md` § "Project-level conventions" — "Loud failure over silent correction" (`--domain` and `--insecure-listen` are mutually exclusive and explicit) names the precise rule this spec extends to the new flags.
- Ticket body AC1–AC5. AC5 (Fly post-merge verification) is operator-side; the developer ships the in-binary code change. `fly.toml` is NOT edited in this ticket — sibling #97 owns the deploy-manifest change that flips the substrate's forwarding to high ports plus the corresponding `docs/deploy.md` update.

## Context

First-deploy bootstrap to Fly (2026-05-24) failed at autocert listener startup, immediately after sibling #95's cert-cache fix lets that earlier gate pass:

```
level=ERROR msg="listener failed" addr=:443 err="listen tcp :443: bind: permission denied"
level=ERROR msg="listener failed" addr=:80  err="listen tcp :80:  bind: permission denied"
```

The Dockerfile runs the binary as uid 65532 (`gcr.io/distroless/static-debian12:nonroot`). On a default Linux kernel, uid != 0 cannot bind to ports < 1024 without `CAP_NET_BIND_SERVICE`. The autocert path in `main.go` hardcodes `Addr: ":443"` and `Addr: ":80"`; the only configurable listen address today is `--insecure-listen`, which switches off autocert entirely. Substrate-portable deploys (any distroless/nonroot container, K8s with restricted SCC, bare-metal systemd run as an unprivileged service account) need to bind elsewhere and have the substrate forward.

ACME HTTP-01 keeps working under a port-forwarded substrate: Let's Encrypt opens TCP 80 on the public IP, the substrate forwards verbatim to the relay's internal port (e.g. Fly's TCP passthrough → 8080), and the autocert handler answers. No protocol change; only a bind-address change. Out of scope: the Dockerfile-`setcap CAP_NET_BIND_SERVICE` alternative (file capabilities + multi-stage `COPY --chmod`). The ticket commits to flags because flags are substrate-portable; the capability alternative bakes a Linux-kernel-only assumption into the image.

The change is minimal: introduce two operator-facing flags whose defaults preserve today's `:443` / `:80` binding (AC2), and parameterise both the listener `Addr:` fields and the existing expected-port set so the asymmetric surplus-listener check (#81) continues to do its job. One new mutual-exclusion guard catches the misconfiguration where `--insecure-listen` is set alongside either new flag (AC3) — `--insecure-listen` bypasses the autocert branch entirely, so the new flags would silently no-op; failing loudly is the project rule (`PROJECT-MEMORY.md` § "Loud failure over silent correction").

`fly.toml` is unchanged in this PR. AC5's verification dance involves the operator editing `fly.toml` locally (set `[processes] app = "… --http-listen=:8080 --https-listen=:8443"`, set both `[[services]].internal_port` to the new high ports, leave external `port = 80` / `port = 443` untouched), deploying, and confirming `/healthz` 200-over-TLS. Once verified, sibling #97 commits the canonical `fly.toml` + `docs/deploy.md` recipe. Splitting the substrate-config commit from the binary change lets #96 land independently and lets #97 reference a merged binary.

## Design

One production file changes: `cmd/pyrycode-relay/main.go`. No new files, no new internal-package helpers, no signature changes to anything in `internal/relay/`.

| Path | Change |
|---|---|
| `cmd/pyrycode-relay/main.go` | Two new `fs.String` flag declarations, one new inline mutex guard, two `Addr:` substitutions, two `ListenerPort` parser calls (already present — repurposed to drive the expected-port set rather than the hardcoded literals), one expected-port-set substitution. |
| `cmd/pyrycode-relay/main_e2e_test.go` | One new `TestRun_*` test for the AC3 mutex (table-driven over the two new flags). No new test helpers. |

No new exported symbols. No edits to `internal/relay/`. No edits to `fly.toml`. No edits to docs.

### Flags

Add the two flags adjacent to `insecureListen` (line 54), in the `var (...)` block that already groups them. Defaults are the literal `:443` / `:80` they are about to replace — AC2 requires that absence-of-flag preserves today's binding exactly, so the `fs.String` default is the canonical place to express that.

Sketch (contract, not implementation):

```go
httpsListen = fs.String("https-listen", ":443",
    "Listen address for the autocert TLS terminator (host:port). "+
        "Used only when --domain is set. Pass a high port like :8443 "+
        "when a substrate forwards external 443 to a high internal port "+
        "(e.g. distroless/nonroot containers without CAP_NET_BIND_SERVICE).")
httpListen = fs.String("http-listen", ":80",
    "Listen address for the ACME HTTP-01 challenge listener (host:port). "+
        "Used only when --domain is set. Pass :8080 when a substrate "+
        "forwards external 80 to a high internal port.")
```

The help-text wording must explicitly call out (a) "used only when `--domain` is set" so an operator reading `--help` understands the autocert-only constraint, and (b) "host:port" so the operator does not pass a bare port number (`ListenerPort` rejects that — see § Error handling).

**Flag-naming decision.** Ticket's `--http-listen` / `--https-listen` suggestion is adopted. The naming-symmetry argument is strong (every listener flag in this binary ends in `-listen`: `--insecure-listen`, `--metrics-listen`), and once the operator has read either flag's help text the autocert-only constraint is unambiguous. `--autocert-http-listen` / `--autocert-tls-listen` were considered for explicitness but rejected on length and on the asymmetry they would introduce against the existing flag vocabulary.

### Mutual-exclusion guard (AC3)

Slots into `run()` immediately after the existing line-72 `--insecure-listen ⊕ --domain` block. Detects the misconfiguration where the operator passes `--insecure-listen` alongside either new flag — in current code the autocert branch is short-circuited at line 201, so the new flags would silently no-op. The guard is "explicit setting" rather than "non-default value": an operator who explicitly passes `--http-listen=:80` (the default) alongside `--insecure-listen=:8080` is still confused and deserves the fast-fail.

Sketch (contract, not implementation):

```go
setFlags := make(map[string]bool)
fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
if *insecureListen != "" && (setFlags["http-listen"] || setFlags["https-listen"]) {
    logger.Error("refusing to start: --insecure-listen is mutually exclusive with --http-listen / --https-listen",
        "fix", "use --insecure-listen alone (proxy-fronted plaintext mode), "+
            "OR use --domain with optional --http-listen / --https-listen (autocert mode); "+
            "the new flags configure the autocert listeners and have no effect in insecure mode")
    return 2
}
```

**Why `fs.Visit` rather than value comparison.** `fs.Visit` walks only flags that were set explicitly on argv. Comparing `*httpListen != ":80"` would let an operator who passes `--http-listen=:80` alongside `--insecure-listen=:8080` slip through silently — still a misconfiguration the guard must catch. `fs.Visit` is the idiomatic Go signal for "was this flag actually set."

**Exit code 2** (matching the established `run()` convention for boot-time configuration refusal — #77 / #78 / #79 / #81 / the existing line-72 guard). Exit 1 is reserved for runtime listener errors.

**Why this guard is inline rather than extracted into a `Check…` helper in `internal/relay/`.** See § "Why the mutex guard is inline" below.

### Listener `Addr:` substitution

In the autocert branch, replace the two literal `Addr:` values with the dereferenced flag pointers. The substitution is mechanical: line 264 (`Addr: ":443"`) → `Addr: *httpsListen`; line 274 (`Addr: ":80"`) → `Addr: *httpListen`. No other field of either `*http.Server` literal changes.

### Expected-port-set generalisation (AC4)

The existing autocert-branch block at lines 285–315 already parses `httpsSrv.Addr` and `httpSrv.Addr` via `ListenerPort` for the actual-set construction. The expected-set construction at line 297 still hardcodes `{443: {}, 80: {}}`. The change: replace those literals with the same parsed ports:

| Before (line 297) | After |
|---|---|
| `expected := map[uint16]struct{}{443: {}, 80: {}}` | `expected := map[uint16]struct{}{httpsPort: {}, httpPort: {}}` |

This keeps `CheckListenerPorts`'s asymmetric semantics intact: a surplus listener (any port the process binds beyond the operator-configured ones plus optional metrics) still fires `ErrUnexpectedListener` and refuses boot. The expected set is now a function of operator input, exactly as the insecure-branch block at line 227 already is.

**Edge: operator sets `--http-listen` and `--https-listen` to the same port.** Map dedupe makes `expected` and `actual` each carry one entry instead of two. `CheckListenerPorts` passes (no surplus). The runtime bind path then surfaces `EADDRINUSE` from the second `ListenAndServeTLS` / `ListenAndServe` call, which exits via the listener-failure path with code 1. Existing behaviour; no new handling required.

**Edge: operator sets `--http-listen` or `--https-listen` to a value that collides with `--metrics-listen`.** Same as above — `EADDRINUSE` at runtime. Not a boot-check responsibility; surfacing it earlier would require coordinating three set sources and is over-engineering for a misconfiguration the runtime path catches loudly within milliseconds.

**Edge: operator sets `--https-listen=:0` (ephemeral).** `ListenerPort` rejects `0` explicitly (`listeners.go:47`); the existing parse-error path logs `refusing to start: invalid listener address` and returns 2. No new handling needed.

### Why the mutex guard is inline (not a `Check…` helper)

The codebase has a clear precedent split:

- **Inline mutex checks** for trivial flag-shape predicates that need no external seam — the existing `*insecureListen == "" && *domain == ""` at line 72 is the canonical instance.
- **Extracted `Check…` helpers** for predicates that need an injectable seam (env var, geteuid, syscall) so unit tests can drive the matrix without mutating process state — `CheckInsecureListenInProduction`, `CheckRunningAsRoot`, `CheckCapabilities`, `CheckSingleInstance`, `CheckEnvConfig`, `CheckListenerPorts`.

The new mutex predicate (`*insecureListen != "" && (setFlags["http-listen"] || setFlags["https-listen"])`) needs no external seam. It is pure boolean logic over `*flag.FlagSet` state, and the `fs.Visit`-derived `setFlags` map is itself populated from a `*flag.FlagSet` whose lifetime is `run()`'s stack frame. Extracting this into `internal/relay/` would require passing the `*flag.FlagSet` (or its `Visit` projection) as a parameter — strictly more wiring than the inline form. Following the line-72 precedent keeps the structural shape of `run()` consistent: trivial guards live in `run()`; seam-dependent guards live in `internal/relay/`.

The AC3 mutex coverage instead lands as a thin e2e test in `main_e2e_test.go` (see § Testing strategy). This is the same shape the e2e file already uses for `run()` exit-code assertions; the test exercises the inline guard without any new test seam.

### Why not refactor the four ListenerPort parse-error branches into a helper

The autocert branch already has four near-identical `ListenerPort` parse-error blocks (httpsSrv.Addr, httpSrv.Addr, metricsSrv.Addr, plus the insecure-branch instance). All four have the shape: `port, err := relay.ListenerPort(addr); if err != nil { logger.Error("refusing to start: invalid listener address", "err", err, "addr", addr); return 2 }`. They scream "extract a helper." Out of scope for #96 — the AC body's only call-to-action on these blocks is "parameterise the expected set," and consolidating the boot-check helpers under a `Config.Validate()`-shape API is the consolidation follow-up #81 already flagged. Worth noting in the spec so the developer does not eagerly clean this up while editing the surrounding code.

## Concurrency model

None. All edits are in `run()`'s synchronous boot path, before any goroutine is spawned (the listener goroutines in `runServers` start only after `CheckListenerPorts` returns nil and `runServers` is entered). `fs.Visit` walks the flag set on the calling goroutine; no shared state. The mutex check is a single read of `*insecureListen` and two map lookups.

## Error handling

| Scenario | Source | Behaviour |
|---|---|---|
| `--insecure-listen` set with `--http-listen` or `--https-listen` explicitly set | new inline guard | logger.Error + return 2 (AC3) |
| `--http-listen` value fails `ListenerPort` (empty, no colon, port 0, out of range, non-numeric) | existing `ListenerPort` call site at line 291 (now operating on `httpSrv.Addr` = `*httpListen`) | logger.Error("refusing to start: invalid listener address", …) + return 2 |
| `--https-listen` value fails `ListenerPort` | existing `ListenerPort` call site at line 285 (now operating on `httpsSrv.Addr` = `*httpsListen`) | logger.Error + return 2 |
| `--http-listen` and `--https-listen` resolve to the same port | runtime bind path (post-`CheckListenerPorts`) | second `ListenAndServe*` returns `EADDRINUSE`; `runServers` enters the listener-error drain path; exit code 1 |
| `--http-listen` or `--https-listen` collides with `--metrics-listen` | runtime bind path | same as above |
| No new flag set; defaults `:443` / `:80` apply | `fs.String` defaults | identical to pre-#96 behaviour (AC2) |
| `--http-listen` or `--https-listen` resolves to a privileged port and the process lacks `CAP_NET_BIND_SERVICE` | runtime bind path | `permission denied` listener failure → exit 1. This is the failure shape the ticket exists to let operators avoid; the relay does not preemptively check capability fit for the configured port. |

All failure paths are total — no retry, no fallback. Boot-time refusal is intentional.

## Testing strategy

Three new things to cover, plus one explicit non-coverage decision.

### `cmd/pyrycode-relay/main_e2e_test.go` — AC3 mutex

Add one table-driven test that drives `run()` with conflicting flag combinations and asserts exit code 2. The mutex check fires before any listener is constructed, so the test does not need `freePort()` / `waitForDial()` — `run()` returns immediately on the guard.

Scenarios (each row in the table):

- `insecure + http-listen`: `--insecure-listen=:8080 --http-listen=:80` → expect exit code 2.
- `insecure + https-listen`: `--insecure-listen=:8080 --https-listen=:443` → expect exit code 2.
- `insecure + both new flags`: `--insecure-listen=:8080 --http-listen=:80 --https-listen=:443` → expect exit code 2.
- `insecure alone (control)`: `--insecure-listen=:8080 --metrics-listen=` → expect exit code 0 after signal-cancel (existing pattern; the row exists to lock in that the mutex does not fire when the new flags are absent).

The control row reuses the existing `freePort()` + `signal-cancel` shape because it actually starts the listener. The three failure rows do not (the guard returns 2 before any listener is constructed), so the test must branch on expected exit code. Simplest shape: a single test function with an inline table; the assertion is `code == wantCode`, with a `context.Background()` sigCtx for the failure rows and a `context.WithCancel` for the control row.

The test must use `--metrics-listen=` (empty string opt-out) so `NewMetricsServer` returns `(nil, nil)` and the test does not need a second `freePort()` for the metrics listener.

### `cmd/pyrycode-relay/main.go` parameterised expected-port set (AC4)

Not unit-tested. Same rationale as #81 § "What is NOT tested" — `main` wiring is observable through the helpers it calls, and `CheckListenerPorts` is already exhaustively tested in `internal/relay/listeners_test.go`. The substitution `{443, 80}` → `{httpsPort, httpPort}` is mechanical and reviewable at diff-time.

### `--http-listen` / `--https-listen` defaults (AC2)

Not unit-tested. The defaults are expressed as `fs.String` literals (`":443"`, `":80"`); their preserve-current-behaviour property is observable by inspection of the diff and by the developer's local smoke test (run the relay with no new flags; observe `:443` / `:80` binding attempts). The operator's post-merge Fly verification (AC5) is the integration test for the full default-preserving path. A `flag` package round-trip test (`fs.Parse([]string{}); *httpListen` == `":80"`) would be tautological.

### AC1 / AC4 — `ListenerPort` validation chain for the new flags

Not unit-tested as a chain. `ListenerPort` is exhaustively covered in `internal/relay/listeners_test.go:9-51` (12 scenarios including empty, port 0, out-of-range, non-numeric). The new flags reuse those code paths verbatim by being threaded through the same `relay.ListenerPort(srv.Addr)` calls at lines 285 / 291. No new validation logic.

### AC5 — Fly post-merge verification

Operator-side. Not a CI test. The developer's deliverable is the binary change; the operator runs the verification dance against a real Fly deploy and reports back in the issue.

### What is NOT in this spec

- No change to `fly.toml`. AC5 explicitly says fly.toml lands under #97.
- No change to `docs/deploy.md`. Same — #97 owns it.
- No new `internal/relay/` symbol, no new sentinel error, no new `Check…` helper. The mutex guard is inline (§ Why the mutex guard is inline).
- No refactor of the existing four `ListenerPort` parse-error blocks into a helper. Out of scope; future consolidation per #81's open follow-up.
- No change to the help text of `--insecure-listen` or `--metrics-listen`. The two new flags' help text references autocert-mode-only; the existing flags are unchanged.

## Open questions

None blocking the developer.

One note for the next ticket: when #97 lands, the `[processes] app =` line in `fly.toml` grows to include both new flags. Today the autocert manifest is `--domain pyrycode-relay.ilmoniemi.fi --cert-cache /var/lib/relay/autocert`; after #97 it becomes `--domain pyrycode-relay.ilmoniemi.fi --cert-cache /var/lib/relay/autocert --http-listen :8080 --https-listen :8443`. The high-port choice (`:8080` / `:8443`) is a Fly-specific convention; the in-binary code is port-agnostic.

## Acceptance criteria — coverage map

| AC | Where covered |
|---|---|
| AC1 — two new flags with `ListenerPort`-validated `host:port` syntax | Production: two `fs.String` declarations adjacent to `insecureListen` (§ Flags); `ListenerPort` calls at lines 285 / 291 already do the validation on `srv.Addr` (which now derives from the new flags). Tests: indirect, via `internal/relay/listeners_test.go:9-51`. |
| AC2 — defaults preserve current `:80` / `:443` behaviour | Production: `fs.String("https-listen", ":443", …)` and `fs.String("http-listen", ":80", …)`. Tests: not unit-tested (§ Testing strategy). Operator-verifiable by inspection of the diff. |
| AC3 — `--insecure-listen` with either new flag fails fast | Production: inline `fs.Visit` mutex guard at the start of `run()` (§ Mutual-exclusion guard). Test: `cmd/pyrycode-relay/main_e2e_test.go` table over four scenarios (§ Testing strategy). |
| AC4 — expected-port set derived from the configured flag values | Production: substitute `{443: {}, 80: {}}` → `{httpsPort: {}, httpPort: {}}` at line 297. Tests: indirect, via `internal/relay/listeners_test.go:61-126`. |
| AC5 — operator Fly verification post-merge | Operator-side. Code change above is the enabling delta. No fly.toml / deploy.md edit in this PR (handled in #97). |

## Security review

**Verdict:** PASS

Walked adversarially against each category per `architect/security-review.md`.

**Findings:**

- **[Trust boundaries]** Two new operator-supplied inputs (`--http-listen`, `--https-listen`) cross from argv into the process. Both are parsed by `relay.ListenerPort`, which is the canonical, pre-existing parser this codebase uses for every listener flag (`--insecure-listen`, `--metrics-listen`, both autocert-branch `Addr` fields today). The parser's contract is bounded: `net.SplitHostPort` + `strconv.ParseUint(_, 10, 16)` + explicit `0` reject + empty-string reject. No allocation driven by attacker-controlled length (the input is a process flag, not network-borne). The boundary is the same as today's `--insecure-listen` boundary, and is no wider. No findings.
- **[Tokens, secrets, credentials]** N/A. The flags carry port numbers; no credential material is read, written, or logged. The mutex-guard error message and the parse-error message reference only the flag names and (on parse-error) the offending flag value verbatim — both are operator-supplied non-secrets, identical to how `--insecure-listen`'s parse path already logs its own value. No findings.
- **[File operations]** N/A. No filesystem path is constructed from either new flag. The autocert cert-cache path (`--cert-cache`) is unchanged. No findings.
- **[Subprocess / external command execution]** N/A. No subprocess invocation. No findings.
- **[Cryptographic primitives]** N/A. TLS configuration (`relay.TLSConfig(mgr)`, `MinVersion: tls.VersionTLS12`) is unchanged. Autocert's HostPolicy (HostWhitelist via `--domain`) is unchanged. The flags configure the TCP bind address; the cipher/key/handshake surface is unaffected. No findings.
- **[Network & I/O]** This is the ticket's primary surface. The flags configure where the relay binds, not what it accepts. All four `http.Server` policy fields (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`) are preserved verbatim — the spec only edits the `Addr:` literal in two `*http.Server` literals; no other field of either struct changes. `EnforceHost(*domain, mux)` on the TLS terminator is unchanged; `mgr.HTTPHandler(http.NotFoundHandler())` on the HTTP-01 listener is unchanged. The frame caps (`maxFrameBytes`), the rate limiter, the per-IP / per-server-id caps, the WS upgrade handshake timeout — all unchanged. No new attack surface; the change is strictly a bind-address generalisation.

  One worth-noting consideration: the new flags let an operator point either listener at a public interface (`0.0.0.0:8080`) explicitly. Today's `:443` / `:80` already implicitly binds all interfaces; the new flags do not widen the binding domain. An operator who fat-fingers `--http-listen=:8080` exposes the ACME HTTP-01 listener on a high port; the autocert handler still answers only on the ACME challenge path and 404s everything else (the explicit `http.NotFoundHandler()` at line 278 enforces this). No traffic-class regression.

  No findings.
- **[Error messages, logs, telemetry]** Two new log lines (the mutex-guard error and the parse-error path for the new flags). Both follow the established field shape (`err`, `addr`, `fix`) and contain only operator-supplied values (the offending flag value on parse-error; the flag names in plain prose on the mutex). No secrets, no headers, no per-connection state. The success path emits no new log line — the existing `logger.Info("starting", "version", Version, "mode", "autocert", "domain", *domain, "cert_cache", *certCache)` is unchanged, and deliberately does NOT log the bind addresses (silence-on-success per the convention in #77 / #81). No findings.
- **[Concurrency]** N/A. All edits are in `run()`'s synchronous boot path, on a single goroutine, before `runServers` spawns any listener goroutine. The `fs.Visit` walk and the `setFlags` map live entirely on `run()`'s stack frame. No shared mutable state. No findings.
- **[Threat model alignment]** `docs/threat-model.md` § Deploy treats "operator misconfiguration leaks an unauthenticated surface" as the dominant failure class for an internet-exposed relay. This ticket does not introduce a new misconfiguration shape; it changes one knob (bind address) that was previously a hardcoded literal. The two new misconfiguration shapes it could introduce — `--insecure-listen ⊕ --http-listen/--https-listen` and the new flags resolving to invalid `ListenerPort` input — are both caught by boot-time refusal (the inline mutex guard for the first, the existing `ListenerPort` parse-error block for the second).

  The `CheckListenerPorts` surplus-listener gate (#81) still fires on any unexpected bind beyond the configured set; AC4's expected-set parameterisation preserves that gate's purpose under the new flags. No threat-model regression. No findings.
- **[Defaults]** Worth explicit walk: AC2 mandates that the new flags default to `:443` / `:80`. This means the misconfiguration "operator omits the new flags on a nonroot deploy" still produces today's `bind: permission denied` runtime failure — exactly as observed during the 2026-05-24 bootstrap. That is the intended behaviour: the ticket is a *capability extension*, not a *substrate-detect*. No silent change to defaults; operators of currently-working deploys (root or `CAP_NET_BIND_SERVICE`-granted) see zero behavioural change. No findings.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-28
