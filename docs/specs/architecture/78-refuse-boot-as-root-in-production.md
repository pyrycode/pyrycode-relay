# Spec: refuse to boot as effective uid 0 in production mode

Ticket: [#78](https://github.com/pyrycode/pyrycode-relay/issues/78). Size S. Split from #42.

## Files to read first

- `internal/relay/production.go` (whole file, 47 lines) — the sibling helper this ticket extends in place. Mirror the shapes exactly:
  - `envProductionMode` const (line 9) — reuse, do not redeclare.
  - `IsProductionMode` (line 30) — call from the new check; do not re-read the env var.
  - `ErrInsecureListenInProduction` (line 17) and `CheckInsecureListenInProduction` (line 41) — the new sentinel + check function are literal siblings of these, same package, same file, same style.
- `internal/relay/production_test.go` (whole file, 119 lines) — the test style for this package. Reuse `fakeGetenv` (line 8); mirror the table shape of `TestCheckInsecureListenInProduction_Matrix` (line 47); add an `Is*Branchable` test for the new sentinel paralleling line 107.
- `cmd/pyrycode-relay/main.go:46-70` — the wiring slot. The new check installs immediately after the existing `CheckInsecureListenInProduction` block (line 64–70) and immediately before `relay.CheckCapabilities()` (line 72). Read those lines to see the structured-log shape (`logger.Error(..., "err", err, "env_var", "...", "fix", "...")`) and the `os.Exit(2)` convention for production-mode misconfigurations.
- `docs/specs/architecture/77-refuse-insecure-in-production.md` — the sibling architect spec. Sections "Sentinel error", "Wiring in `cmd/pyrycode-relay/main.go`", and "Testing strategy" set the precedent this ticket follows verbatim.
- `docs/specs/architecture/9-autocert-tls.md` (for the `ErrCacheDirInsecure` precedent narrative; skim only if context on "fail-loud, before any listener starts" is needed) — the boot-time refusal pattern.
- `syscall` stdlib — `syscall.Geteuid() int`. Returns the effective uid on Linux and darwin. No build tag required. On Windows it returns -1, but the relay binary is built for linux/amd64 (Dockerfile, #32) and darwin for local dev; either way the function is in scope without conditional compilation.

## Context

Ticket #77 closed the "production-mode + plaintext listener" silent-misconfiguration class. This ticket closes the parallel silent-misconfiguration class: the relay process running as root.

CI already verifies the *build* image runs as a non-root user (Dockerfile USER directive, verified by #32 and the Trivy scan in #68). But the build is only one half of the contract. At *deploy* time, an operator (or an AI agent generating a manifest) can:

- `docker run --user 0 …` — overrides the image's USER and runs as root.
- `kubectl apply` a pod spec with `securityContext: { runAsUser: 0 }` — same outcome, different cause.
- Modify the Dockerfile to drop the USER directive — slips past code review if the diff is small enough.

None of these are caught by CI: the build is green, the image scans clean, the deploy succeeds, and the internet-exposed process runs as root. The blast radius is "any RCE in the relay is now a root RCE on the host"; in containers without strict capability drops, that escalates further. The defence must run *at boot, before any listener accepts a connection*, so a misconfigured deploy fails the deploy's health check rather than serving traffic.

This ticket adds the deterministic in-process backstop: when `PYRYCODE_RELAY_PRODUCTION=1` AND `syscall.Geteuid() == 0`, the process refuses to start.

The `PYRYCODE_RELAY_PRODUCTION` contract is defined by #77 (`internal/relay/production.go`). This ticket consumes `IsProductionMode` and adds a sibling check; it does not redefine the env-var contract or introduce a second read path.

Related: #9 (the `ErrCacheDirInsecure` boot-time-refusal precedent); #32 (Dockerfile non-root build); #68 (Trivy image scan); #77 (sibling — introduced `ErrInsecureListenInProduction` and `IsProductionMode`); #42 (parent — was split into #77 / #78).

## Design

No new files. Append to `internal/relay/production.go` (new sentinel + new check), append to `internal/relay/production_test.go` (new test cases + new branchability test), insert ~7 lines of wiring in `cmd/pyrycode-relay/main.go`.

### Sentinel error (exported)

A new package-level `var ErrRunningAsRoot = errors.New("relay: …")` declared alongside `ErrInsecureListenInProduction` in `internal/relay/production.go`. Message names both the observed condition (effective uid 0) and the env-var contract (`PYRYCODE_RELAY_PRODUCTION=1`), mirroring how `ErrInsecureListenInProduction` names the flag and the env var. Doc comment follows the same Godoc shape as `ErrInsecureListenInProduction` (lines 11–17): names the function that returns it, explains the production-mode condition, justifies fail-fast over runtime degradation, and says the contract that triggers it is internet-exposure of a root-uid process.

### Check function (exported)

```go
func CheckRunningAsRoot(geteuid func() int, getenv func(string) string) error
```

Behaviour, in one sentence: returns `ErrRunningAsRoot` when `IsProductionMode(getenv)` is true AND `geteuid()` returns 0; returns nil otherwise. No wrapping, no formatting with caller-supplied fields — the structured log fields in `main` carry the operator-facing context. The test that asserts this contract is the 3-row matrix below; if the implementation deviates from "production AND uid 0", the matrix breaks.

Two design decisions worth naming:

1. **`geteuid` is an injected `func() int` parameter, not a package var or interface.** Mirrors the `getenv func(string) string` seam pattern that `CheckInsecureListenInProduction` already establishes (line 41). The call site in `main.go` passes `syscall.Geteuid` directly (function values, no closure needed); tests pass a closure returning a fixed int. No `t.Setenv`-equivalent for uid exists in stdlib (you cannot change a process's uid mid-test without re-exec), so an injected seam is the *only* way to exercise the uid-0 branch in a unit test without re-execing as root. The AC names this requirement explicitly.
2. **Single check covers both prongs.** An alternative is two helpers — `IsRunningAsRoot() bool` and `CheckRunningAsRoot(...)` — paralleling `IsProductionMode` / `CheckInsecureListenInProduction`. **Rejected.** `IsRunningAsRoot` has no second consumer (no logging needs to branch on it, no metric labels it); exporting it is dead surface area today and a temptation to call it without the production guard tomorrow. Keep it inline inside the check; export `IsRunningAsRoot` later if and when a second consumer appears.

### Wiring in `cmd/pyrycode-relay/main.go`

Insert immediately after the existing `CheckInsecureListenInProduction` block (currently lines 64–70) and immediately before `CheckCapabilities` (line 72):

- Call signature: `relay.CheckRunningAsRoot(syscall.Geteuid, os.Getenv)`.
- Error branch: structured log at `Error` level with fields `err`, `env_var="PYRYCODE_RELAY_PRODUCTION"`, `effective_uid=syscall.Geteuid()`, and a `fix` field naming the two valid resolutions (drop privileges before exec; unset `PYRYCODE_RELAY_PRODUCTION` if the deploy is truly dev). Then `os.Exit(2)`.
- The AC explicitly requires the log line to include both the observed effective uid and `env_var=PYRYCODE_RELAY_PRODUCTION`. The check returns *only* the sentinel — `main.go` is where the operator-facing context (the uid value, the remediation hint) is composed. Capturing the uid for the log line means calling `syscall.Geteuid()` a second time at the log site; this is fine — it is the same uid (the process cannot change its own euid between two adjacent syscalls without an intervening `setuid` call, which the relay never makes) and the cost is negligible.
- Exit code 2 matches the sibling block immediately above (line 69) and the env-config validator block above that (line 61). Exit 1 stays reserved for runtime failures (listener died, autocert failed); exit 2 is configuration-rejected-at-boot. The split lets ops dashboards distinguish "deploy never started" from "deploy started and crashed."

The wiring order — `CheckEnvConfig` → `CheckInsecureListenInProduction` → `CheckRunningAsRoot` → `CheckCapabilities` — is intentional and worth a one-line comment at the new block:

- `CheckEnvConfig` (#80) validates the env-var shapes including `PYRYCODE_RELAY_PRODUCTION`'s "exact `1` or unset" contract. Running it first means downstream production-mode checks cannot be fooled by `PYRYCODE_RELAY_PRODUCTION=true` slipping through as "non-production." This is the wiring-order invariant #80's spec calls out.
- `CheckInsecureListenInProduction` (#77) and `CheckRunningAsRoot` (#78) are siblings on the production-mode axis; the order between them is not load-bearing. Place #78 second to preserve `git blame` legibility (the new block lands adjacent to the new function it calls).
- `CheckCapabilities` (#79) runs after both — capability-allowlist failures are a Linux-specific runtime concern; production-mode misconfiguration is a deploy-shape concern that should be reported first if both are wrong.

### Why a `Config` struct is still out of scope

Same reasoning as #77 § "Why not a Config struct": `main.go` now has four boot-time checks. When the count reaches ~5 and the wiring boilerplate becomes a real cost, a follow-up ticket consolidates them into `Config.Validate() error`. Doing it now is premature abstraction and would balloon this S ticket past its red lines.

## Concurrency model

None. The new check runs on the main goroutine before any listener is started, before any goroutine is spawned. No locks, no channels, no shared mutable state. `syscall.Geteuid` is a stateless syscall — concurrent callers would see the same value, but there are no concurrent callers.

## Error handling

Single sentinel, no wrapping, no formatting with caller-supplied fields. Same shape as `CheckInsecureListenInProduction`. The error message is the same on every failure; the structured log fields in `main` (`effective_uid`, `env_var`, `fix`) provide the operator-facing context. Downstream code that wants to branch on this failure mode uses `errors.Is(err, ErrRunningAsRoot)`.

No error case requires retry, fallback, or partial-state recovery. Boot-time refusal is total: the process exits 2 immediately.

## Testing strategy

All new tests live in `internal/relay/production_test.go`, are `t.Parallel()`-safe, and never mutate process env or call `syscall.Setuid` (which the test binary cannot do as non-root anyway). Reuse the existing `fakeGetenv` helper. Define a tiny helper `fakeGeteuid(n int) func() int { return func() int { return n } }` if the indirection helps readability, or inline `func() int { return n }` at each row.

### Test 1 — `CheckRunningAsRoot` matrix (the AC verbatim)

Table-driven, four rows minimum:

| `PYRYCODE_RELAY_PRODUCTION` | `geteuid()` returns | want |
|---|---|---|
| unset | `0` | `nil` (non-production overrides uid-0; we don't refuse to boot a dev relay running as root in a sandbox) |
| `"1"` | `1000` | `nil` (production but not root — the happy path) |
| `"1"` | `0` | `errors.Is(err, ErrRunningAsRoot)` (production + root — refuse) |
| `"1"` | `65534` | `nil` (nobody-uid, sanity check that non-zero non-1000 also returns nil) |

The fourth row is a small over-add to lock in "uid 0 is the *only* refused uid"; a future refactor that ranges over a "privileged uids" list would break it. Drop the row if it feels gratuitous — the AC names only the three primary cases.

### Test 2 — sentinel is branchable

One-line test paralleling `TestErrInsecureListenInProduction_IsBranchable` (line 107): assert `errors.Is(ErrRunningAsRoot, ErrRunningAsRoot)` and assert the error returned from `CheckRunningAsRoot(func() int { return 0 }, fakeGetenv(map[string]string{envProductionMode: "1"}))` satisfies `errors.Is(err, ErrRunningAsRoot)`. The point is to lock in the `errors.Is` contract so a future "let me return `fmt.Errorf("uid %d: %w", ...)` instead" refactor breaks the test, not downstream callers.

### What is NOT tested

- The `main.go` wiring (same justification as #77): adding a fork-exec integration test for one `if err != nil { os.Exit(2) }` block is over-engineering. Code review of the diff is the gate.
- The structured log line's exact field names (same justification as #77): operator-facing prose, not a machine-parsed format.
- Actually re-execing as root to verify `syscall.Geteuid()` returns 0 in that case. The standard-library function is trusted; the test seam is precisely so we do not need to.
- Per-OS behaviour (linux vs darwin). `syscall.Geteuid` is implemented on both with identical semantics; no build tag, no platform fork. If the Windows port ever materialises (it will not — the relay targets linux for prod and darwin for dev), `syscall.Geteuid` returns -1 there, which trivially does not equal 0, so the check is benignly inert. No test required.

## Open questions

None. The sentinel name (`ErrRunningAsRoot`), check function name (`CheckRunningAsRoot`), uid source (injected `func() int`), env-var reuse (`IsProductionMode` from #77), wiring slot (after `CheckInsecureListenInProduction`, before `CheckCapabilities`), and exit code (2) are all settled by the ticket body, the sibling spec (#77), and the existing wiring in `main.go`.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings. Two inputs cross into the check: the `PYRYCODE_RELAY_PRODUCTION` env var (operator-controlled, not network-attacker-controlled; reused via `IsProductionMode`) and the effective uid (kernel-supplied, not user-supplied). Neither flows from a network-facing path. The check is a fail-closed gate on two known shapes; there is no parser, no allocator, no value-dependent code path beyond the boolean conjunction.
- **[Tokens, secrets, credentials]** N/A. No tokens, no secrets, no credential material is read, written, or compared.
- **[File operations]** N/A. No file is read, written, statted, or unlinked. No path concatenation.
- **[Subprocess / external command execution]** N/A. No `exec.Command`, no `os.StartProcess`, no `syscall.ForkExec`. The check observes the existing process; it does not spawn another.
- **[Cryptographic primitives]** N/A. No RNG, no hash, no comparison of attacker-controlled values.
- **[Network & I/O]** No findings. The check runs *before* any listener is opened (`mux := http.NewServeMux()` is on line 88 of the current `main.go`, several lines after where this check inserts). The whole purpose of the ticket is to *prevent* a listener from starting in a misconfigured state. The autocert path (#9) and the existing `http.Server` timeout configuration are unchanged.
- **[Error messages, logs, telemetry]** One **SHOULD FIX** for the developer (called out inline in the Wiring section above). The log line includes `effective_uid` as a structured field. The value comes from `syscall.Geteuid()` — a kernel-supplied integer, not a user-supplied string, so log-injection is structurally impossible. The `env_var` field is the *name* of the env var, not its value (same convention as #77's wiring); do not extend the log to include `os.Getenv("PYRYCODE_RELAY_PRODUCTION")` for the same reason #77 calls out: a confused operator might write anything there and we do not want it ending up in centralised logs. The `fix` field is a static string constant. No PII, no token, no path.
- **[Concurrency]** N/A. The check runs on the main goroutine before any goroutine is spawned; no shared mutable state. `syscall.Geteuid` is reentrant and stateless.
- **[Threat model alignment]** No findings. `pyrycode/pyrycode/docs/protocol-mobile.md` § Security model assumes the relay is internet-exposed and untrusted by the binary; the host the relay runs on is assumed to enforce least-privilege so that a relay RCE does not escalate to host-root RCE. This ticket is the in-process enforcement of the "non-root execution" half of that assumption when production-mode is explicitly tagged. The complement (a CI / deploy-manifest check that prod manifests do not set `runAsUser: 0` or `--user 0`) is out of scope and could be a follow-up ticket against the deploy manifest (#38).
- **[Adversarial framing]** What if an attacker controls `PYRYCODE_RELAY_PRODUCTION`? They cannot — the env var is set by the operator/orchestrator before exec; a network attacker has no path to mutate it. What if an attacker controls the effective uid? Same answer — uid is set by the kernel based on the exec context; a network attacker has no path to flip it. What if an attacker exploits an RCE *during* boot, before the check runs? The check is the third in a sequence (`CheckEnvConfig` → `CheckInsecureListenInProduction` → `CheckRunningAsRoot` → `CheckCapabilities`); none of those open a listener or accept untrusted input, so there is no pre-check RCE surface. What if the check itself has a logic bug that lets uid 0 through in production? The matrix test (3 of the 4 rows) is the lock; a regression would break the third row.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
