# Env-var config validator (boot-time refusal)

Structured, table-driven validation of every env var the relay reads at boot. Introduced by #80. The validator polices an explicit in-package contract table — one row per env var, with a `required` flag and a per-key shape validator — and aborts the process with `os.Exit(2)` and a structured log line on the first failure. Sits in front of the `IsProductionMode` consumer (#77) so a typo like `PYRYCODE_RELAY_PRODUCTION=true` cannot slip through the production-mode helper's silent-non-production fallback.

## What it catches

The contract table is the single source of truth for "what env vars the relay reads". Today it has two rows (`PYRYCODE_RELAY_PRODUCTION` from #80; `PYRYCODE_RELAY_SINGLE_INSTANCE` from #65, same exact-`"1"`-or-unset shape); future env-var reads register here. For each entry the validator distinguishes three states:

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
- `checkEnvConfigWith(lookup, contracts)` (unexported) — same body as `CheckEnvConfig` parameterised by a table. Exists so tests can exercise the `required: true` branch without mutating the production `envContracts` slice.

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
    os.Exit(2)
}
```

- **Ordering is load-bearing.** Downstream `Check*` helpers (`CheckInsecureListenInProduction`, `CheckSingleInstance`) treat every non-`"1"` value as "not set" — silent fall-through, not refusal. If the operator wrote `PYRYCODE_RELAY_PRODUCTION=true`, the insecure-listen guard would return nil and a plaintext production listener would boot; similarly `PYRYCODE_RELAY_SINGLE_INSTANCE=true` would let a multi-instance deploy through. Running `CheckEnvConfig` first catches malformed values and the dangerous paths are never reached. Code review enforces.
- **Exit code 2** matches the sibling boot-time refusals; exit 1 is reserved for runtime failures.
- **`errors.As` extraction at the log site**, not in the validator. The structured fields are `err`, `env_var` (the key), and `reason` (the per-key prose). The fallback branch (no `errors.As` hit) is defensive against a future refactor that wraps the error in something opaque; under the current design it is unreachable.
- **No `value` log field.** The reason string already echoes the malformed value (`got "true"`) where safe; the `env_var` field carries the name only. Future entries whose values could carry secrets must keep raw bytes out of the reason string — describe the shape violation in operator-facing terms instead (`"expected hex-encoded 32-byte value"`, not `got %q`).

## Adding a new env var

When a future ticket adds an `os.Getenv` / `os.LookupEnv` read under `cmd/pyrycode-relay/` or `internal/relay/`:

1. Add an entry to `envContracts` with `name`, `required`, and an inline `validate func(string) error`.
2. The validator handles presence/absence; `validate` only sees the raw value when the var is set.
3. Decide `required` per env var. Today `PYRYCODE_RELAY_PRODUCTION` is `required: false` (unset means non-production — legitimate for dev environments); the validator only fires on a non-`"1"` set value.
4. For secret-bearing env vars, the validator's error must not echo the raw value into the reason string (it surfaces in the log line).

Code review enforces "every env-var read in the binary is registered here" by re-running the grep at landing.

## Behaviour matrix (today)

Both registered env vars share the same `exact "1" or unset` shape, so the matrix is identical for either key:

| Value (applies to `PYRYCODE_RELAY_PRODUCTION` and `PYRYCODE_RELAY_SINGLE_INSTANCE`) | `CheckEnvConfig` result |
|---|---|
| unset | nil |
| `"1"` | nil |
| `""` (set-but-empty) | `*ErrInvalidConfig{Key, Reason: "malformed-value: expected \"1\" or unset, got \"\""}` |
| anything else (`"true"`, `"0"`, `"yes"`, `" 1"`, …) | `*ErrInvalidConfig{Key, Reason: "malformed-value: expected \"1\" or unset, got \"…\""}` |

## Why not a `Config` struct (yet)

A natural extension is a typed `relay.Config` populated from the env at boot, validated as a single step, and threaded through the binary. **Deferred.** The relay has two env vars today; the ticket body promises "future env-var additions register here", which means the contract table is the registry, not a typed struct. When env-var count crosses ~3 *and* a downstream caller wants a typed snapshot, a follow-up ticket consolidates. Doing it now is premature.

## Cross-links

- [Production-mode contract & `--insecure-listen` startup refusal](production-mode.md) — first env-var contract (#77); the malformed-value cases listed there are now caught by `CheckEnvConfig` before `IsProductionMode` is consulted.
- [Single-instance startup self-check](single-instance-check.md) — second env-var contract (#65); same exact-`"1"`-or-unset shape, second consumer of the registry.
- [Linux capability allowlist](capability-allowlist.md) — sibling boot-time refusal (#79); same wiring shape, runs after env-config validation.
- [Codebase note #80](../codebase/80.md) — implementation summary and lessons.
- [Codebase note #77](../codebase/77.md) — env-var contract precedent (`PYRYCODE_RELAY_PRODUCTION`).
- [`internal/relay/env_config.go`](../../../internal/relay/env_config.go) — implementation.
