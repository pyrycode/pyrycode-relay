# Production-mode contract & startup refusals

Single-env-var production-mode signal for the relay, plus the boot-time checks that consume it. Defined by #77; #78 added a second consumer (refuse-to-run-as-uid-0) that reuses the contract rather than re-reading the env var. Future production-only startup checks compose on `IsProductionMode` the same way.

## The contract

- **Env var:** `PYRYCODE_RELAY_PRODUCTION`
- **On:** the exact string `"1"`. Anything else — unset, `"0"`, `"true"`, `"yes"`, `" 1"`, `"1 "`, `"PRODUCTION"` — means non-production.
- **Read lazily** on every call. No init-time cache. Tests inject a getter; production wires `os.Getenv`.

Strict-equality (not `strconv.ParseBool` "truthy" parsing) is intentional and mirrors `PYRYCODE_RELAY_SINGLE_INSTANCE` (#64/#65). An exact-string contract is harder to misread in a deploy manifest than a "what does this parser accept?" question, and an operator who believes production mode is on but the relay disagrees is exactly the silent-misconfiguration class the contract exists to prevent.

## API (`internal/relay/production.go`)

- `IsProductionMode(getenv func(string) string) bool` — reports whether `getenv("PYRYCODE_RELAY_PRODUCTION") == "1"`. Exported so sibling checks compose on the same predicate (today: `CheckInsecureListenInProduction`, `CheckRunningAsRoot`).
- `CheckInsecureListenInProduction(insecureListen string, getenv func(string) string) error` — returns `ErrInsecureListenInProduction` when production mode is on AND `insecureListen != ""`, nil otherwise. Intended to run after `flag.Parse()`, before any listener is started.
- `ErrInsecureListenInProduction` — exported sentinel, branchable via `errors.Is`. Message names both inputs (`"relay: --insecure-listen is set with PYRYCODE_RELAY_PRODUCTION=1; refusing to start"`) so a log line is self-documenting.
- `CheckRunningAsRoot(geteuid func() int, getenv func(string) string) error` — returns `ErrRunningAsRoot` when production mode is on AND `geteuid() == 0`, nil otherwise. Intended to run after `flag.Parse()`, before any listener is started. `geteuid` exists as an injected `func() int` for the same reason `getenv` does: there is no stdlib equivalent of `t.Setenv` for the uid (a process cannot change its own euid mid-test without re-exec), so the uid-0 branch is only reachable in a unit test via the seam.
- `ErrRunningAsRoot` — exported sentinel, branchable via `errors.Is`. Message names both inputs (`"relay: effective uid is 0 with PYRYCODE_RELAY_PRODUCTION=1; refusing to start"`).
- `envProductionMode` (unexported) — the env-var-name constant. In-package siblings reuse it; out-of-package callers go through `IsProductionMode`.

## Why this shape (test seam)

The check takes a `func(string) string` rather than calling `os.Getenv` directly. The seam is the smallest design that satisfies the AC's "do not mutate process env" — no interface, no struct, no package-level mutable variable. `os.Getenv` satisfies the signature literally at the call site; tests build a closure over a `map[string]string`. Process env is never touched, so the tests are safe under `t.Parallel()` and `go test -race -count=N`.

## Wiring (`cmd/pyrycode-relay/main.go`)

Boot-time check ordering, top to bottom: `CheckEnvConfig` (#80) → `CheckSingleInstance` (#65) → `CheckInsecureListenInProduction` (#77) → `CheckRunningAsRoot` (#78) → `CheckCapabilities` (#79). Each `if err != nil` branch logs at error level and `os.Exit(2)`s; no listener has been opened yet at any point in the sequence.

```go
if err := relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv); err != nil {
    logger.Error("refusing to start: production-mode misconfiguration",
        "err", err,
        "env_var", "PYRYCODE_RELAY_PRODUCTION",
        "fix", "remove --insecure-listen and set --domain, or unset PYRYCODE_RELAY_PRODUCTION")
    os.Exit(2)
}

if err := relay.CheckRunningAsRoot(syscall.Geteuid, os.Getenv); err != nil {
    logger.Error("refusing to start: production-mode misconfiguration",
        "err", err,
        "env_var", "PYRYCODE_RELAY_PRODUCTION",
        "effective_uid", syscall.Geteuid(),
        "fix", "drop privileges before exec (e.g. Dockerfile USER directive or --user <non-zero>, kubernetes securityContext.runAsUser), or unset PYRYCODE_RELAY_PRODUCTION if the deploy is truly dev")
    os.Exit(2)
}
```

- **Exit code 2** matches the flag-validation guard and every other config-rejected-at-boot block. Exit 1 is reserved for runtime failures in this binary (listener died, autocert failed); exit 2 = configuration-rejected-at-boot. Splitting the codes lets ops dashboards distinguish "deploy never started" from "deploy started and crashed."
- **`fix` field** lists both valid resolutions (remove the flag / drop privileges OR unset the env var). The operator picks whichever input was wrong.
- **`env_var` field carries the name, never the value.** Even if a confused operator put a secret in `PYRYCODE_RELAY_PRODUCTION`, it would not be logged. Do not extend this log line to include the env var value.
- **`effective_uid` field is the kernel-supplied integer**, not a user-supplied string — log-injection is structurally impossible. The value is read a second time at the log site (after the check returns `ErrRunningAsRoot`); the process cannot change its own euid between two adjacent syscalls without an intervening `setuid` call, which the relay never makes.
- **Order between `CheckInsecureListenInProduction` and `CheckRunningAsRoot` is not load-bearing** (they are siblings on the production-mode axis). They both run before `CheckCapabilities` because production-mode misconfigurations are deploy-shape concerns that should surface before Linux-specific runtime concerns.

## Behaviour matrix

### `CheckInsecureListenInProduction` (#77)

| `PYRYCODE_RELAY_PRODUCTION` | `--insecure-listen` | Result |
|---|---|---|
| unset / not `"1"` | any | nil (no check fires) |
| `"1"` | empty (autocert path) | nil |
| `"1"` | non-empty | `ErrInsecureListenInProduction` → `os.Exit(2)` |

Autocert (`--domain`) is not inspected — the contract is purely about plaintext-in-prod. Setting `--domain` with `PYRYCODE_RELAY_PRODUCTION=1` is the happy path.

### `CheckRunningAsRoot` (#78)

| `PYRYCODE_RELAY_PRODUCTION` | `geteuid()` | Result |
|---|---|---|
| unset / not `"1"` | `0` | nil (dev-mode root is allowed; sandboxed dev runs as root are unremarkable) |
| `"1"` | `1000` (any non-zero) | nil |
| `"1"` | `0` | `ErrRunningAsRoot` → `os.Exit(2)` |

uid `0` is the *only* refused uid — the check has no notion of a "privileged uids" set. A non-zero uid that nominally owns sensitive resources (e.g. a service-account uid mapped onto host root via user namespaces) is the deploy layer's problem, not the relay's. The fourth test row (`uid=65534`) exists to lock in this scope.

Since #80 the matrix's "not `"1"`" row only describes the *post-validation* state: a malformed `PYRYCODE_RELAY_PRODUCTION` value (`"true"`, `"yes"`, `" 1"`, `"PRODUCTION"`, etc.) is now caught by [`CheckEnvConfig`](env-config-validator.md) *before* `CheckInsecureListenInProduction` is consulted, so the relay refuses to boot at the env-validation stage rather than silently treating the typo as "not production". `IsProductionMode`'s strict-`"1"` contract is unchanged; the validator simply ensures no other value ever reaches it.

## Threat model alignment

`pyrycode/pyrycode/docs/protocol-mobile.md` § Security model assumes TLS for all production traffic and that the host running the relay enforces least-privilege so a relay RCE does not escalate to host-root RCE. These checks are the in-binary enforcement of both assumptions *when production mode is explicitly tagged*. CI already verifies the *build* image is non-root (#32 Dockerfile USER directive, Trivy scan in #68); `CheckRunningAsRoot` closes the deploy-time gap (`docker run --user 0`, `securityContext.runAsUser: 0`, hand-edited Dockerfile dropping `USER`). The complements — CI / deploy-manifest checks that prod manifests actually set `PYRYCODE_RELAY_PRODUCTION=1` and never set `runAsUser: 0` — are out of scope and the responsibility of the deploy layer.

The checks are fail-closed: if any precondition trips, the relay refuses to boot. There is no degradation path, no fallback, no retry. Boot-time refusal is total.

## Out of scope (deferred)

- **No `Config` struct.** A bundled `relay.Config` with `Validate()` returning a multi-error is a natural extension once ~5 startup checks exist. With five today (`CheckEnvConfig`, `CheckSingleInstance`, `CheckInsecureListenInProduction`, `CheckRunningAsRoot`, `CheckCapabilities`), the wiring boilerplate has reached the cost threshold; a follow-up ticket will consolidate.
- **No fork-exec integration test on `main.go`.** The `main` wiring is observable via package-level unit tests; one `if err != nil { os.Exit(2) }` block does not warrant a binary-spawning test.
- **No `IsRunningAsRoot` bool helper.** A second consumer hasn't appeared (no log line branches on it, no metric labels it); exporting it today would be dead surface area and a temptation to call it without the production-mode guard tomorrow. Inline the predicate inside `CheckRunningAsRoot` until a second consumer materialises.

## Cross-links

- ADR: none filed; the shape is precedent-following (mirrors `PYRYCODE_RELAY_SINGLE_INSTANCE`), not a new architectural choice.
- [`internal/relay/tls.go`](../../../internal/relay/tls.go) — `ErrCacheDirInsecure` is the canonical boot-time-refusal sentinel this one models.
- [Single-instance constraint (v1)](../../architecture.md#single-instance-constraint-v1) — sibling env-var contract (`PYRYCODE_RELAY_SINGLE_INSTANCE`), shape precedent.
- [Codebase ticket note #77](../codebase/77.md) — per-ticket implementation detail for the `--insecure-listen` check.
- [Codebase ticket note #78](../codebase/78.md) — per-ticket implementation detail for the uid-0 check.
- [Docker image](docker-image.md) — #32; the build-time non-root contract this ticket complements at deploy time.
- [Linux capability allowlist](capability-allowlist.md) — #79; the next boot-time refusal in the wiring sequence, unconditional rather than production-mode-gated.
- [Env-var config validator](env-config-validator.md) — #80; the boot-time validator that polices the malformed-value cases listed in the contract, running before `CheckInsecureListenInProduction` so a typo can never reach `IsProductionMode`.
