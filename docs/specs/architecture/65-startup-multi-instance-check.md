# Spec: startup self-check refuses to run as multi-instance deploy

Ticket: [#65](https://github.com/pyrycode/pyrycode-relay/issues/65). Size XS. Split from #39. Sibling of merged #64 (the doc half of the belt-and-suspenders pair).

## Files to read first

- `internal/relay/production.go` (whole file, 78 lines) — the canonical "boot-time Check\*" helper shape this spec mirrors. Lines 5–9 show the `envXxx` constant convention (env var name lives in `internal/relay`, not duplicated at the call site); lines 19–32 show the exact-`"1"` getter contract; lines 41–46 / 72–77 show the `Check*(getenv …) error` signature with the injected-seam rationale.
- `internal/relay/env_config.go` (whole file, 93 lines) — the env-var registry. Every new env var the relay reads at boot registers a row here; the spec adds one row for `PYRYCODE_RELAY_SINGLE_INSTANCE` so a typo like `=true` fails the boot config check before the multi-instance check runs.
- `internal/relay/production_test.go:1-45` — the value-matrix table pattern and the `fakeGetenv` helper (line 8–10) that the new test file reuses verbatim. The new test file lives in the same package (`relay`), so the helper is in scope without re-declaration.
- `cmd/pyrycode-relay/main.go:77-101` — the existing Check\* wiring band, between `CheckEnvConfig` (line 82) and `CheckInsecureListenInProduction` (line 95). The new check slots in between these two. The error-log shape (logger.Error + `"refusing to start: …"` + structured fields + `return 2`) is the established pattern.
- `docs/architecture.md:48-63` — the bypass env var contract pinned by sibling #64. The literal env-var name `PYRYCODE_RELAY_SINGLE_INSTANCE` and the literal value `1` are non-negotiable inputs to this spec. Note also the "intended uses" framing (emergency rollback, migration windows) and the "NOT recommended for production" line — this spec creates tension with that framing; see § Open questions.
- `fly.toml` (whole file, 74 lines) — the deploy manifest. The check fires when `FLY_APP_NAME` (set by the Fly platform on every machine in the app) is non-empty. Note also `auto_start_machines = false` / `min_machines_running = 1` (lines 40–42, 53–55) — the platform-side controls this check backs up.
- `docs/specs/architecture/64-single-instance-architecture-doc.md` — the doc-half spec that landed `architecture.md § Single-instance constraint`. Read only for tone and cross-link patterns; this spec implements the code half and references that section, it does not extend it.

## Context

`fly scale count 3` is a one-line operator (or AI-agent) command. The connection registry (`internal/relay/registry.go`) is in-memory per process, so two replicas hold two disjoint server-id tables. A phone connected to replica A with a server-id claimed on replica B closes with `4404` even though the binary is online — and nothing in the relay logs identifies "the binary is on the other replica" as the cause, because no replica knows the others exist.

Sibling #64 ships the stochastic guard: prose in `docs/architecture.md` that tells operators and AI agents "v1 is single-instance, here's why, here's the bypass env var". This ticket ships the deterministic backstop: a boot-time refusal that fires when the relay detects it is running on a platform where multi-instance scale-out is a one-command operation (today: Fly.io) and the operator has not explicitly asserted single-instance intent via the bypass env var.

The check is heuristic, not adversarial. It catches "operator deployed to Fly and never read `architecture.md`"; it does **not** catch "operator ran `fly scale count 3` after configuring the bypass in `fly.toml [env]`" — once the bypass propagates via the manifest, all replicas inherit it. The constraint to use **only env vars exposed by the host** (per Technical Notes in the ticket body) precludes the only mechanism that could catch the scale-out case at runtime (a DNS query against `<app>.internal` or a Fly Machines API call). The honest framing: this check forces an *explicit deploy-time gate*, not a runtime instance count.

Related: #39 (parent), #64 (doc half, merged), #77 / #78 / #80 (sibling boot-time Check\* helpers that established the helper / wiring / env-registry pattern this spec extends).

## Design

Two new files in `internal/relay/`. One ~10-line addition to `cmd/pyrycode-relay/main.go`. One row added to `envContracts` in `internal/relay/env_config.go`. No existing code is refactored.

### New file: `internal/relay/single_instance.go`

Declares:

- `envSingleInstanceBypass = "PYRYCODE_RELAY_SINGLE_INSTANCE"` — package-internal constant, the literal bypass env var name pinned by `docs/architecture.md`.
- `envFlyAppName = "FLY_APP_NAME"` — package-internal constant, the multi-instance-platform signal. Fly's platform sets this on every machine in the app; presence is sufficient evidence that we are on a substrate where `fly scale count > 1` is a one-line command.
- `ErrMultiInstanceDeployDetected` — exported sentinel error. Its `Error()` string MUST contain the substring `multi-instance deploy detected` AND the literal env-var name `PYRYCODE_RELAY_SINGLE_INSTANCE` (AC #2). Recommended message: `"relay: multi-instance deploy detected; set PYRYCODE_RELAY_SINGLE_INSTANCE=1 to bypass"`.
- `CheckSingleInstance(getenv func(string) string) error` — exported helper. Signature mirrors `CheckInsecureListenInProduction` / `CheckRunningAsRoot` from `production.go`. Returns `ErrMultiInstanceDeployDetected` when the bypass is **not** set AND `FLY_APP_NAME` is non-empty; returns `nil` otherwise.

Decision order in `CheckSingleInstance`:

1. If `getenv(envSingleInstanceBypass) == "1"` → return `nil`. Bypass wins unconditionally. (The exact-`"1"` contract matches `IsProductionMode`; any other value of the bypass env — including `"true"`, `"0"`, leading/trailing whitespace — is treated as "not set" and the registry walk in `CheckEnvConfig` will have already rejected such values before this function runs.)
2. Else if `getenv(envFlyAppName) != ""` → return `ErrMultiInstanceDeployDetected`.
3. Else → return `nil`.

No new exported types beyond the sentinel error. No new packages.

### Edit: `internal/relay/env_config.go`

Add one row to `envContracts` for the bypass env var. The contract is "exact `"1"` or unset" — same shape as `envProductionMode`. This means `PYRYCODE_RELAY_SINGLE_INSTANCE=true` fails the boot config check (in `CheckEnvConfig`) before `CheckSingleInstance` reads the value, catching the obvious typo class.

The new row references the bypass-env constant declared in `single_instance.go` (`envSingleInstanceBypass`); the env-config file does **not** re-declare the literal string. This keeps the single-source-of-truth-for-env-var-names invariant the package already enforces (`envProductionMode` lives in `production.go`, used from `env_config.go`).

### Edit: `cmd/pyrycode-relay/main.go`

Insert `CheckSingleInstance` between the existing `CheckEnvConfig` (lines 82–93) and `CheckInsecureListenInProduction` (line 95). Wiring order rationale:

- **After `CheckEnvConfig`** so a typo'd bypass (`=true`, `=0`, `= 1`) fails with the structured config error (clear per-key remediation) rather than silently being treated as "not set" and tripping the multi-instance refusal with a misleading log line.
- **Before `CheckInsecureListenInProduction`** because a deploy-shape misconfiguration (multi-instance) is more fundamental than a production-mode misconfiguration (insecure-listen-in-prod). If both fire, the operator should see the multi-instance error first; fixing it may not be possible without changing the deploy substrate, while insecure-listen is a flag flip.

Error-log shape (mirrors the existing `CheckInsecureListenInProduction` block at lines 95–101):

- Level: `ERROR` (AC #2).
- Message: `"refusing to start: multi-instance deploy detected"` — the AC-mandated substring lives in the message, not just in the structured `err` field, so a `grep -i 'multi-instance deploy detected'` over the boot logs hits regardless of whether the operator's log-rendering tool surfaces structured fields.
- Structured fields: `"err"` (the sentinel error), `"bypass_env_var"` (the literal string `"PYRYCODE_RELAY_SINGLE_INSTANCE"`), `"fix"` (a one-line remediation pointing to `docs/architecture.md § Single-instance constraint`).
- Return value: `2` (matches the established refuse-to-start exit-code convention; see `main.go` lines 74, 92, 100, 116, 123).

### Concurrency model

None. `CheckSingleInstance` is pure — no goroutines, no I/O, no allocation beyond the sentinel error. Called once at boot before any goroutine is started.

### Error handling

`ErrMultiInstanceDeployDetected` is a single exported sentinel, branchable via `errors.Is`. No structured fields (cf. `*ErrInvalidConfig`) — the failure has no per-instance variance worth surfacing, and the bypass instructions are universal.

## Testing strategy

New file: `internal/relay/single_instance_test.go`. Same package (`relay`), reuses `fakeGetenv` from `production_test.go` without redeclaration.

Test cases (table-driven, AC #5 maps directly to the first three; the last two are guard-rails):

- **Multi-instance signal present, bypass unset → `ErrMultiInstanceDeployDetected`.** (`FLY_APP_NAME = "pyrycode-relay"`, no `PYRYCODE_RELAY_SINGLE_INSTANCE`.) Asserts `errors.Is(err, ErrMultiInstanceDeployDetected)` AND `strings.Contains(err.Error(), "multi-instance deploy detected")` AND `strings.Contains(err.Error(), "PYRYCODE_RELAY_SINGLE_INSTANCE")`. AC #5(a).
- **Bypass set to `"1"`, signal present → `nil`.** (`FLY_APP_NAME = "pyrycode-relay"`, `PYRYCODE_RELAY_SINGLE_INSTANCE = "1"`.) AC #5(b).
- **No signal, no bypass → `nil`.** (Empty env.) AC #5(c).
- **Bypass set, signal absent → `nil`.** (Bypass is benign when the check would have passed anyway. Documents the no-op case so a future refactor can't silently flip the precedence.)
- **Bypass set to a non-`"1"` value, signal present → `ErrMultiInstanceDeployDetected`.** Documents that only the exact string `"1"` bypasses. The matrix should include at least one row from the production-mode equivalence set (e.g. `"true"`, `"0"`, `" 1"`) — the actual typo'd-value failures are caught earlier by `CheckEnvConfig`, but `CheckSingleInstance` is the ultimate decision site and must not be lenient.

The bypass-env-config-validation contract (typo catches like `=true`) is already exercised by the existing `TestCheckEnvConfig_MalformedValueReturnsStructuredError` matrix. Add at minimum one row to that matrix exercising `PYRYCODE_RELAY_SINGLE_INSTANCE=true` to lock the new envContracts entry. No new test file for the env-config side.

No new e2e test. The boot-refusal path is exercised by the helper-level test; the wiring is a one-line call site whose correctness is reviewable by code inspection (cf. the established practice for `CheckRunningAsRoot` / `CheckCapabilities` — no e2e for the helper-level Check\* boot refusals).

## Security review

Categories walked:

- **Trust boundaries.** The check reads process env vars set by the deploy substrate (Fly's platform sets `FLY_APP_NAME`; the operator sets `PYRYCODE_RELAY_SINGLE_INSTANCE`). Both are *out-of-band* relative to the relay's internet-exposed surface — a remote attacker on `/v1/server` or `/v1/client` cannot influence either value. No new attack surface introduced by this ticket.
- **Bypass abuse.** The bypass env var is fail-open: setting it skips the check. The threat model assumes an adversary capable of setting process env vars already has shell access to the deploy host, at which point the multi-instance refusal is not the load-bearing control. The bypass is a deliberate operator-discipline tool, not a security boundary.
- **Log injection.** The error log emits two literal strings (`"refusing to start: …"` and `"PYRYCODE_RELAY_SINGLE_INSTANCE"`) plus the sentinel `err`. No env-var *values* are interpolated into the log line. (Compare `CheckEnvConfig` which logs the offending key and reason — `cfgErr.Key` / `cfgErr.Reason` are constructed from the registry, not from raw env values, so the same property holds. This spec adds only the literal bypass-env-var name.)
- **Resource exhaustion.** Two env-var reads, one string comparison, one allocation (`errors.New` runs once at package init for the sentinel). Boot only, no per-request cost.
- **Denial of service via misconfiguration.** A malicious operator (with deploy access) could set `FLY_APP_NAME=x` on a non-Fly host to crash boot. This is in the same trust class as the operator deleting the binary — not a relay-side concern.
- **Wire-protocol surface.** None. No new HTTP handler, no new WebSocket frame parser, no new TLS-config branch.

Verdict: **PASS.** The change is a boot-time refusal helper with no new network or wire-protocol surface; all inputs are operator-controlled out-of-band env vars; the bypass semantics are explicit and aligned with the documented operational contract.

## Open questions

- **Tension with `architecture.md`'s "NOT recommended for production" framing.** The architecture doc, written before this code spec, characterises `PYRYCODE_RELAY_SINGLE_INSTANCE=1` as an emergency / migration-window bypass and warns against setting it permanently. This spec, by making `FLY_APP_NAME` presence the multi-instance signal, requires the bypass to be set permanently in `fly.toml [env]` for the production Fly deploy to start. The framing should be reconciled — the bypass is operationally an *explicit single-instance assertion* on multi-instance-capable platforms, not just an emergency override. **Recommended follow-up:** a separate XS doc-ticket against `docs/architecture.md § Single-instance constraint` that rewrites the "Intended uses" / "NOT recommended for production" lines to reflect the deployed reality. This ticket should land first (the code is the contract; the doc follows). **Out of scope for this ticket.**
- **Updating `fly.toml` to set the bypass in `[env]`.** Once this ticket lands, the production Fly deploy will refuse to start until `fly.toml` adds an `[env] PYRYCODE_RELAY_SINGLE_INSTANCE = "1"` block. There is no production deploy today (the `__REGION__` / `__DOMAIN__` placeholders in `fly.toml` would fail the first deploy regardless), so the update can ride alongside the bootstrap procedure documented in `docs/deploy.md`. **Recommended follow-up:** include the `[env]` block in the same PR or a follow-up XS ticket against `fly.toml` + `docs/deploy.md`. **Out of scope for this ticket** — this spec is scoped to the binary-side check.
- **Other multi-instance-capable platforms.** Only `FLY_APP_NAME` is checked today. Kubernetes-style deploys (`KUBERNETES_SERVICE_HOST`), ECS (`ECS_CONTAINER_METADATA_URI`), and AWS Lambda (`AWS_LAMBDA_FUNCTION_NAME`) would be candidates if the relay is ever ported. The signal-set is intentionally narrow — adding signals on speculation would create false positives for operators running the binary in unusual local-dev shells. **Defer until a non-Fly substrate is actually considered.**
