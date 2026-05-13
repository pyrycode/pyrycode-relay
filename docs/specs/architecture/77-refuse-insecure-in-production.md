# Spec: refuse to boot with `--insecure-listen` in production mode

Ticket: [#77](https://github.com/pyrycode/pyrycode-relay/issues/77). Size S. Split from #42.

## Files to read first

- `cmd/pyrycode-relay/main.go` (whole file, 128 lines) — the only call site. Lines 23–43 (flag declarations, `flag.Parse`, the existing "either `--domain` or `--insecure-listen`" guard) are where the new check slots in. Lines 61–119 show the two listener-start branches the check must run *before*.
- `internal/relay/tls.go:15-19` — the canonical sentinel-error pattern in this package: `var ErrCacheDirInsecure = errors.New("relay: …")` plus a Go doc comment that names the contract. Mirror that shape exactly.
- `internal/relay/tls.go:29-49` — the canonical "boot-time refusal" return shape (`return nil, fmt.Errorf("%w: %s …", ErrCacheDirInsecure, …)`). The new check returns `error`, not `(T, error)`, but the wrapping convention is the same.
- `internal/relay/tls_test.go:1-80` — the test style for this package: `t.Parallel()`, table-driven where it helps, `errors.Is` for sentinel assertions, no shared globals. Mirror it.
- `docs/specs/architecture/9-autocert-tls.md:55-100` — the prior architect's reasoning for the `ErrCacheDirInsecure` sentinel and its boot-time use. The "fail loud, before any listener starts" framing is identical to this ticket's intent.
- `docs/specs/architecture/64-single-instance-architecture-doc.md:57-76` — the env-var-contract precedent (`PYRYCODE_RELAY_SINGLE_INSTANCE`). The shape this ticket commits to (`=1` means on, anything else means off, read lazily) is intentionally identical.

## Context

Today the relay binary accepts two mutually-exclusive transport flags — `--domain` (autocert TLS on :443/:80) and `--insecure-listen` (plain HTTP on a custom address). The plaintext mode exists for dev loops and behind-a-proxy deployments where TLS is terminated upstream. Nothing distinguishes a dev environment from a production environment; an operator (or an AI agent generating a deploy manifest) who copy-pastes a dev config into prod will boot a plaintext listener facing the internet.

The bug this ticket prevents is the silent-misconfiguration class: the relay starts, `/healthz` returns 200, traffic flows in plaintext, and the operator only notices when a passive observer (or `tcpdump`) shows tokens or message frames on the wire. The defence must run *at boot, before any listener accepts a connection*, so a misconfigured deploy fails the health-check rather than serving traffic.

The contract is a single env var, `PYRYCODE_RELAY_PRODUCTION=1`. The shape — name `PYRYCODE_RELAY_*`, value `1` means on, anything else means off — mirrors `PYRYCODE_RELAY_SINGLE_INSTANCE` (#39 / #64) so operators only have to remember one shape and can grep `PYRYCODE_RELAY_*` in their manifests to audit production gating.

This ticket is the canonical place where the `PYRYCODE_RELAY_PRODUCTION` contract is defined. Sibling startup checks (#78 = refuse to run as uid 0 in production; potentially more) will import the exported `IsProductionMode` helper introduced here rather than re-reading the env var.

Related: #9 (`ErrCacheDirInsecure` set the boot-time-refusal precedent); #39 / #64 (env-var-shape precedent); #42 (parent — was split into #77 / #78).

## Design

One new file: `internal/relay/production.go`. One new test file: `internal/relay/production_test.go`. One ~6-line addition to `cmd/pyrycode-relay/main.go`. No existing code is refactored.

### Package-level constant (unexported)

```go
const envProductionMode = "PYRYCODE_RELAY_PRODUCTION"
```

Unexported because callers should not read the env var directly — they should call `IsProductionMode`. Sibling startup checks in this package (e.g. #78) reuse the constant inside the package; callers in `cmd/` go through `IsProductionMode`.

### Sentinel error (exported)

```go
// ErrInsecureListenInProduction is returned by CheckInsecureListenInProduction
// when the relay is configured for production mode (PYRYCODE_RELAY_PRODUCTION=1)
// AND the --insecure-listen flag is set. Serving plaintext traffic from a
// production-tagged process is a fail-fast misconfiguration, not a runtime
// degradation: the relay refuses to start so a dev manifest accidentally
// promoted to prod fails the deploy's health check rather than serving traffic.
var ErrInsecureListenInProduction = errors.New("relay: --insecure-listen is set with PYRYCODE_RELAY_PRODUCTION=1; refusing to start")
```

The error message names both the flag and the env var so a developer reading the log line knows the two inputs to flip without consulting docs. Downstream code that wants to branch on this failure mode uses `errors.Is(err, ErrInsecureListenInProduction)`.

### Helpers (exported)

```go
// IsProductionMode reports whether the relay is in production mode.
//
// The contract is: PYRYCODE_RELAY_PRODUCTION="1" means production; any other
// value (including unset, "0", "true", "yes", "PRODUCTION", or whitespace)
// means non-production. Only the exact string "1" enables production mode.
// The strictness is intentional: anything fuzzier ("truthy" parsing) creates
// a class of subtle misconfigurations where an operator believes production
// mode is on but the relay disagrees.
//
// The env var is read on every call via getenv. Pass os.Getenv at call sites;
// tests inject a func to exercise the matrix without mutating process env.
func IsProductionMode(getenv func(string) string) bool {
    return getenv(envProductionMode) == "1"
}

// CheckInsecureListenInProduction returns ErrInsecureListenInProduction when
// production mode is on (per IsProductionMode) AND insecureListen is non-empty.
// Returns nil otherwise. Intended to be called from main after flag parse,
// before any listener is started.
//
// getenv is the env-var lookup function; pass os.Getenv at the call site,
// an injected func in tests.
func CheckInsecureListenInProduction(insecureListen string, getenv func(string) string) error {
    if IsProductionMode(getenv) && insecureListen != "" {
        return ErrInsecureListenInProduction
    }
    return nil
}
```

Three design decisions worth naming:

1. **Injected-getter test seam, not `t.Setenv`.** The AC requires "the env var is read through a test seam (injected getter or equivalent), not by mutating process env." A `func(string) string` parameter is the smallest seam: no interface, no struct, no package-level mutable variable. `os.Getenv` satisfies the signature directly at the call site (`relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv)`). Tests pass a closure built from a `map[string]string`. Process env is never mutated, so the test is safe under `t.Parallel()` and under `go test -count=N -race`.
2. **Lazy read, not cached at init.** The AC says "read the env var lazily on each call." This matters for two reasons: (a) tests inject different values per case without re-importing the package; (b) sibling checks (#78 and future) call `IsProductionMode` at their own boot moment without taking a hidden dependency on initialisation order. Cost is one `os.Getenv` per boot-check call — negligible.
3. **Exact `"1"` match, not truthy parsing.** Stricter than `strconv.ParseBool` (`"t"`, `"T"`, `"TRUE"`, `"1"`, `"True"`, `"true"` all true). The strictness is the same one the precedent doc for `PYRYCODE_RELAY_SINGLE_INSTANCE` (#64) bakes in — an exact-string contract is harder to misread in a deploy manifest than a "what does this parser accept?" question. The Go doc comment on `IsProductionMode` documents this explicitly.

### Wiring in `cmd/pyrycode-relay/main.go`

Insert after the existing "either `--domain` or `--insecure-listen`" guard (line 43), before `startedAt := time.Now()` (line 45):

```go
if err := relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv); err != nil {
    logger.Error("refusing to start: production-mode misconfiguration",
        "err", err,
        "env_var", "PYRYCODE_RELAY_PRODUCTION",
        "fix", "remove --insecure-listen and set --domain, or unset PYRYCODE_RELAY_PRODUCTION")
    os.Exit(2)
}
```

Three details:

- **Exit code 2**, matching the existing flag-validation guard immediately above (line 42). Exit 1 is reserved in this file for runtime failures (listener died, autocert failed); exit 2 is configuration-rejected-at-boot. Keeping the two codes distinct lets ops dashboards split "deploy never started" from "deploy started and crashed."
- **Structured log fields named in the AC** — `env_var` and `fix`. The `fix` field is the one-line remediation: it tells the operator the two flips that resolve the boot failure without making them read the source. The phrasing intentionally lists *both* exits (remove the flag OR unset the env var) because either is a valid resolution depending on which input was wrong.
- **Placement before `relay.NewRegistry()`** ensures the check runs before any per-boot side effects (goroutine spawn, port bind, file creation). The current code does not have side effects between flag-parse and `NewRegistry`, so this is purely defensive against future drift.

### Why not a `Config` struct

A natural extension is to bundle production-mode + insecure-listen + future flags into a `relay.Config` struct and have `Config.Validate()` return a multi-error. **Out of scope for this ticket.** Each sibling startup check (#78 = uid-0, future = others) will introduce one new exported `CheckXxxInProduction` function. When the count reaches ~3 and the wiring boilerplate in `main.go` becomes a real cost, a follow-up ticket consolidates them. Doing it now is premature abstraction.

## Concurrency model

None. All three helpers (`IsProductionMode`, `CheckInsecureListenInProduction`, the `main` wiring) run on the main goroutine before any listener is started. No locks, no channels, no goroutines.

## Error handling

The check returns a single sentinel — no wrapping, no formatting with caller-supplied fields. The error message is the same on every failure (`"relay: --insecure-listen is set with PYRYCODE_RELAY_PRODUCTION=1; refusing to start"`); the structured log fields in `main` provide the operator-facing remediation context.

No error case requires retry, fallback, or partial-state recovery. Boot-time refusal is total: the process exits 2 immediately.

## Testing strategy

All tests live in `internal/relay/production_test.go`, are `t.Parallel()`-safe, and never call `os.Setenv` / `t.Setenv`. Helper for building a fake getter:

```go
func fakeGetenv(env map[string]string) func(string) string {
    return func(k string) string { return env[k] }
}
```

### Test 1 — `IsProductionMode` value matrix

Table-driven over the env-var value space. Cases:

| `PYRYCODE_RELAY_PRODUCTION` value | want `IsProductionMode` |
|---|---|
| `"1"` | `true` |
| `""` (unset) | `false` |
| `"0"` | `false` |
| `"true"` | `false` |
| `"yes"` | `false` |
| `" 1"` (leading space) | `false` |
| `"1 "` (trailing space) | `false` |
| `"PRODUCTION"` | `false` |

The non-`"1"` rows are not over-coverage: they document the "exact match, not truthy" contract under attack. If a future refactor introduces `strconv.ParseBool`, the `"true"` / `"yes"` rows fail and the contract violation is caught.

### Test 2 — `CheckInsecureListenInProduction` 2×2 matrix (the AC verbatim)

| production-mode env | `insecureListen` | want |
|---|---|---|
| unset | `":8080"` | `nil` |
| `"1"` | `":8080"` | `errors.Is(err, ErrInsecureListenInProduction)` |
| `"1"` | `""` (autocert path; `--domain` is set elsewhere) | `nil` |
| unset | `""` | `nil` |

The third row models the "production + `--domain` (autocert)" case — `insecureListen == ""` is the signal that autocert is in play; the check has no business inspecting `--domain` because the contract is purely about plaintext-in-prod.

### Test 3 — sentinel is branchable

A one-line test that `errors.Is(ErrInsecureListenInProduction, ErrInsecureListenInProduction)` is true, plus a check that the returned error from `CheckInsecureListenInProduction("...", getenv)` (production+insecure case) satisfies `errors.Is`. The point is to lock in the `errors.Is` contract so a future "let me return `fmt.Errorf("foo: %s", ...)` instead" refactor breaks the test, not downstream callers.

### What is NOT tested

- The `main.go` wiring. `main` is a coordination function; its behaviour is observable via the unit tests on the package it calls. Adding a fork-exec integration test for one `if err != nil { os.Exit(2) }` block is over-engineering. Code review of the diff is the gate.
- The structured log line's exact field names. The AC says "structured log line naming the env var and the fix"; the spec's wiring satisfies that literally. Asserting on slog output requires capturing the handler, which is friction for a contract that is operator-facing prose, not a downstream-machine-parsed format.

## Open questions

None. The env var name, the helper-name freedom, and the sentinel name are all settled by the ticket body. The injected-getter seam is the smallest design that satisfies "test seam without env mutation"; no other axis of choice is load-bearing.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the only input crossing into the check is the `PYRYCODE_RELAY_PRODUCTION` env var, which is operator-controlled (not network-attacker-controlled). The check itself is a fail-closed gate on a known shape; there is no parser, no allocator, no path that data flows into beyond a string equality.
- [Tokens, secrets, credentials] N/A — the ticket creates no tokens, reads no secrets, and writes no credential material. The env var carries a configuration signal, not a credential.
- [File operations] N/A — no file is read, written, statted, or unlinked. No path concatenation.
- [Subprocess / external command execution] N/A — no `exec.Command`, no `os.StartProcess`.
- [Cryptographic primitives] N/A — no RNG, no hash, no comparison of attacker-controlled values.
- [Network & I/O] No findings — the check runs before any listener is opened. The whole purpose of the ticket is to *prevent* a listener from starting in a misconfigured state. The autocert path (#9) and the existing `http.Server` timeout configuration in `main.go:84-104` are unchanged.
- [Error messages, logs, telemetry] No findings — the sentinel error message contains the flag name and the env var name (both operator-facing constants, no user data); the `main` log line names the env var and a fix string (no PII, no token, no path). The `env_var` field is the *name* of the env var, not its value — even if the operator put a secret in `PYRYCODE_RELAY_PRODUCTION` (they shouldn't; the contract is `"1"`), it would not be logged. Worth flagging to the developer: do not add a `value` field that logs `os.Getenv("PYRYCODE_RELAY_PRODUCTION")`; the contract value is `"1"` but a confused operator might write anything there and we don't want it ending up in centralised logs.
- [Concurrency] N/A — the check runs on the main goroutine before any goroutine is spawned; no shared mutable state.
- [Threat model alignment] No findings — `pyrycode/pyrycode/docs/protocol-mobile.md` § Security model assumes TLS is in place for all production traffic. This ticket is the in-binary enforcement of that assumption when production-mode is explicitly tagged. The complement (a CI / deploy-manifest check that prod manifests set `PYRYCODE_RELAY_PRODUCTION=1`) is out of scope and tracked separately.

One **SHOULD FIX** noted inline above (Error messages, logs, telemetry category): the developer must not extend the log line to include the env var *value*. The spec already says so explicitly; code-review must double-check.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
