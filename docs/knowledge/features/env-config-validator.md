# Env-var config validator (boot-time refusal)

Structured, table-driven validation of every env var the relay reads at boot. Introduced by #80. The validator polices an explicit in-package contract table — one row per env var, with a `required` flag and a per-key shape validator — and logs a structured error and makes `run` return exit code 2 on the first failure. Sits in front of the `IsProductionMode` consumer (#77) so a typo like `PYRYCODE_RELAY_PRODUCTION=true` cannot slip through the production-mode helper's silent-non-production fallback.

## What it catches

The contract table is the single source of truth for "what env vars the relay reads". It registers five variables; future env-var reads register here.

| Variable | Startup contract |
|---|---|
| `PYRYCODE_RELAY_PRODUCTION` | Unset or exactly `1`. |
| `PYRYCODE_RELAY_SINGLE_INSTANCE` | Unset or exactly `1`. |
| `PYRYCODE_RELAY_FCM_CREDENTIALS` | Unset disables push; supplied service-account JSON must pass the secret-safe [FCM parser](fcm-push-sender.md). |
| `PYRYCODE_RELAY_DAEMON_ADMISSION_MODE` | Unset defaults to `off`; supplied value must be exactly `off`, `observe` or `enforce`. |
| `PYRYCODE_RELAY_DAEMON_ADMISSION_CREDENTIALS` | JSON object of exact identities to token arrays; required when mode is enforce. |

For each entry the validator distinguishes three states:

- **Unset.** If `required: true`, returns `*ErrInvalidConfig{Reason: "missing"}`. If `required: false`, skipped.
- **Set-and-non-empty.** Runs the per-key `validate(value)`; on `error`, returns `*ErrInvalidConfig{Reason: "malformed-value: <err>"}`.
- **Set-but-empty.** Treated as "present" — runs the validator. Per-key validators decide whether empty is meaningful; today's `PYRYCODE_RELAY_PRODUCTION` validator rejects empty as malformed.

Unregistered env vars (anything not in `envContracts`) are ignored. The validator polices its declared contract, not arbitrary process env — a future refactor that walks `os.Environ()` for unknown keys would break this invariant silently and is explicitly forbidden by a unit test.

## API (`internal/relay/env_config.go`)

- `CheckEnvConfig(lookup func(string) (string, bool)) error` — public entry point. Pass `os.LookupEnv` at the call site; tests inject a closure over a `map[string]string`. Returns nil on full pass, `*ErrInvalidConfig` on the first failure.
- `ErrInvalidConfig{Key, Reason string}` — structured error. `Key` is the env-var name; `Reason` is operator-facing prose (`"missing"` or `"malformed-value: <validator's err>"`).
- `ErrInvalidConfigSentinel` — package-level sentinel. `*ErrInvalidConfig.Is(target)` matches against it, so `errors.Is(err, ErrInvalidConfigSentinel)` returns true for any instance regardless of `Key` / `Reason`. Use `errors.As(err, &cfgErr)` to extract the fields.
- `envContract{name, required, validate}` (unexported) — one row of the registry.
- `envContracts` (unexported, package-level var) — the registry. Read-only at runtime; new env-var reads append here at code-review time.
- `checkEnvConfigWith(lookup, contracts)` (unexported) — registry walk parameterised by a table. `CheckEnvConfig` additionally checks the enforce/credentials coupling. Exists so tests can exercise the `required: true` branch without mutating the production `envContracts` slice.

### Getter shape: `func(string) (string, bool)`, not `func(string) string`

The validator's getter has the `os.LookupEnv` signature (returning a presence bit), not the `os.Getenv` signature that `IsProductionMode` uses. The two seams coexist:

- `IsProductionMode` (#77) wants `func(string) string` because exact-`"1"` match has no use for the presence bit (anything-not-`"1"` collapses to "not production").
- `CheckEnvConfig` (#80) wants `func(string) (string, bool)` because distinguishing "missing-but-required" from "present-but-empty" is semantically necessary for the required-key case, and `os.Getenv` cannot make that distinction.

Mixing the two seam shapes is intentional; do not refactor `IsProductionMode` to the `LookupEnv` shape for consistency's sake without a motivating failure.

## Wiring (`cmd/pyrycode-relay/main.go`)

After flag-parse, **before** `CheckSingleInstance` (#65), `CheckInsecureListenInProduction` (#77), `CheckRunningAsRoot` (#78) and `CheckCapabilities` (#79):

```go
if err := relay.CheckEnvConfig(os.LookupEnv); err != nil {
    var cfgErr *relay.ErrInvalidConfig
    if errors.As(err, &cfgErr) {
        logger.Error("refusing to start: invalid env-var config",
            "err", err,
            "env_var", cfgErr.Key,
            "reason", cfgErr.Reason)
    } else {
        logger.Error("refusing to start: invalid env-var config", "err", err)
    }
    return 2
}
```

- **Ordering is load-bearing.** Downstream `Check*` helpers (`CheckInsecureListenInProduction`, `CheckSingleInstance`) treat every non-`"1"` value as "not set" — silent fall-through, not refusal. If the operator wrote `PYRYCODE_RELAY_PRODUCTION=true`, the insecure-listen guard would return nil and a plaintext production listener would boot; similarly `PYRYCODE_RELAY_SINGLE_INSTANCE=true` would let a multi-instance deploy through. Running `CheckEnvConfig` first catches malformed values and the dangerous paths are never reached. Code review enforces.
- **Exit code 2** matches the sibling boot-time refusals; exit 1 is reserved for runtime failures.
- **`errors.As` extraction at the log site**, not in the validator. The structured fields are `err`, `env_var` (the key), and `reason` (the per-key prose). The fallback branch (no `errors.As` hit) is defensive against a future refactor that wraps the error in something opaque; under the current design it is unreachable.
- **No `value` log field.** The reason string already echoes the malformed value (`got "true"`) where safe; the `env_var` field carries the name only. Future entries whose values could carry secrets must keep raw bytes out of the reason string — describe the shape violation in operator-facing terms instead (`"expected hex-encoded 32-byte value"`, not `got %q`).

## Daemon admission configuration

The [shared admission contract](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#daemon-admission-declared)
is authoritative. Both `CheckEnvConfig` and `NewDaemonAdmissionFromEnv` validate
supplied configuration in every mode, including off. Unset credentials mean an
empty mapping in off/observe. Enforce requires an explicitly supplied mapping;
`{}` is valid and denies all. Set-but-empty credentials are malformed, rather
than an unset value. Mode values have no trimming or case folding.

The JSON root must be an object mapping exact server-id strings to arrays of
strings. Each token is exactly 64 lowercase hex characters. Empty arrays admit
nobody; repeated tokens within one identity are allowed. Reject null/non-object
roots, null/non-array entries, non-string or malformed tokens, trailing data,
duplicate decoded identity keys (including escaped spellings) and any token
assigned to more than one identity. Keys are not trimmed or normalized. Admission
adds no UUID syntax rule to the existing nonempty routing key.

Decode object keys incrementally instead of decoding straight into a map:
ordinary map decoding silently overwrites duplicate keys, including escaped
spellings that decode to the same identity. The parser checks each decoded key
before installing its array and checks token ownership across identities.

All admission errors are static and value-free. The parser discards JSON decoder
causes and returns `ErrDaemonAdmissionCredentials`; it never wraps a parser
excerpt. The constructor and validator expose `*ErrInvalidConfig` with the
variable name and a static reason, safe for the startup error chain and log.
No credential, map, identity or raw header set enters those diagnostics.

`run` constructs `DaemonAdmission` immediately after `CheckEnvConfig`, before
workers/listeners. It owns newly decoded, private maps and slices with no exposed
mutation or reload hook. Editing provisioning changes no decision until Relay
restarts. Multiple tokens permit old/new overlap for one exact identity; removal
of a token or identity plus restart revokes it. Restart closes existing connections
and discards their claims/grace state. See the [pre-upgrade gate and mode outcomes](server-endpoint.md#daemon-admission-before-upgrade)
and [operator activation/rotation checklist](../../deploy.md#daemon-admission-activation).

`TestDaemonAdmission_Config` exercises both validation entry points in all three
modes, including duplicate/escaped keys and input markers that must stay out of
errors. `TestDaemonAdmission_SnapshotRestart` covers immutable decisions, overlap
and revocation; `TestRun_DaemonAdmissionInvalidConfigBeforeListeners` covers exit 2
before startup can listen. These tests describe offline behavior, not live rollout.

## Adding a new env var

When a future ticket adds an `os.Getenv` / `os.LookupEnv` read under `cmd/pyrycode-relay/` or `internal/relay/`:

1. Add an entry to `envContracts` with `name`, `required`, and an inline `validate func(string) error`.
2. The validator handles presence/absence; `validate` only sees the raw value when the var is set.
3. Decide `required` per env var. Today `PYRYCODE_RELAY_PRODUCTION` is `required: false` (unset means non-production — legitimate for dev environments); the validator only fires on a non-`"1"` set value.
4. For secret-bearing env vars, the validator's error must not echo the raw value into the reason string (it surfaces in the log line).

Code review enforces "every env-var read in the binary is registered here" by re-running the grep at landing.

## Boolean-variable behavior matrix

The two boolean variables share the same `exact "1" or unset` shape, so the matrix is identical for either key:

| Value (applies to `PYRYCODE_RELAY_PRODUCTION` and `PYRYCODE_RELAY_SINGLE_INSTANCE`) | `CheckEnvConfig` result |
|---|---|
| unset | nil |
| `"1"` | nil |
| `""` (set-but-empty) | `*ErrInvalidConfig{Key, Reason: "malformed-value: expected \"1\" or unset, got \"\""}` |
| anything else (`"true"`, `"0"`, `"yes"`, `" 1"`, …) | `*ErrInvalidConfig{Key, Reason: "malformed-value: expected \"1\" or unset, got \"…\""}` |

## Configuration ownership

The contract table validates startup inputs; consumers own their typed state.
Daemon admission uses its own immutable snapshot without changing the getter
seam or introducing hot reload. The coupling check belongs in `CheckEnvConfig`
because enforce requires credentials even though the table row is otherwise optional.

## Cross-links

- [Daemon endpoint](server-endpoint.md#daemon-admission-before-upgrade) — admission modes, ordering and aggregate metrics.
- [Production-mode contract & `--insecure-listen` startup refusal](production-mode.md) — first env-var contract (#77); the malformed-value cases listed there are now caught by `CheckEnvConfig` before `IsProductionMode` is consulted.
- [Single-instance startup self-check](single-instance-check.md) — second env-var contract (#65); same exact-`"1"`-or-unset shape, second consumer of the registry.
- [Linux capability allowlist](capability-allowlist.md) — sibling boot-time refusal (#79); same wiring shape, runs after env-config validation.
- [Codebase note #80](../codebase/80.md) — implementation summary and lessons.
- [Codebase note #77](../codebase/77.md) — env-var contract precedent (`PYRYCODE_RELAY_PRODUCTION`).
- [`internal/relay/env_config.go`](../../../internal/relay/env_config.go) — implementation.
