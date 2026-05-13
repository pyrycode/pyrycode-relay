# Spec: validate required env vars at boot, fail-fast on missing/malformed

Ticket: [#80](https://github.com/pyrycode/pyrycode-relay/issues/80). Size S. Split from #42. Blocked-by #77 (now merged).

## Files to read first

- `internal/relay/production.go` (whole file, 47 lines) — the existing env-var helper. Lines 5–9 define the `envProductionMode` constant; lines 30–32 show the exact-`"1"` contract; lines 41–46 show the boot-time-refusal helper shape. The new validator polices the same env var, so the constant is reused (not duplicated).
- `internal/relay/production_test.go` (whole file, 119 lines) — the test style. Lines 8–10 define the `fakeGetenv` helper this spec mirrors (adapted to the `LookupEnv` shape). Lines 12–45 are the value-matrix table pattern to copy; lines 47–105 are the 2×2 matrix shape to copy for the validator's missing-vs-malformed-vs-valid cases.
- `cmd/pyrycode-relay/main.go:30-58` — the wiring location. The new validator slots between line 43 (the `--domain` xor `--insecure-listen` flag-validation guard) and line 45 (`CheckInsecureListenInProduction`). This ordering is load-bearing — see § Wiring order below.
- `internal/relay/caps.go:10-22` — the canonical "sentinel + struct error" precedent in this package. Mirror the doc-comment shape (intent, deploy-time framing, "branchable via errors.Is").
- `internal/relay/caps_test.go:108-155` — the `errors.Is`-branchable test idiom for sentinel-style errors.
- `docs/specs/architecture/77-refuse-insecure-in-production.md:23-89` — the sibling design that introduced `PYRYCODE_RELAY_PRODUCTION`. The "exact `"1"` match, not truthy parsing" rule and the injected-getter seam are this ticket's foundation. The validator extends, not replaces, that contract.

## Context

The relay reads exactly one env var today (`PYRYCODE_RELAY_PRODUCTION`, landed in #77) and reads it through `IsProductionMode(getenv)` which uses an exact-`"1"` match. Anything else — `"true"`, `"yes"`, `" 1"`, `"PRODUCTION"` — is silently treated as **non-production**. That is the failure mode this ticket exists to catch: an operator who writes `PYRYCODE_RELAY_PRODUCTION=true` in a deploy manifest gets a relay that runs in non-production mode, with no signal that the operator's intent was different from the relay's behaviour. The `--insecure-listen` guard from #77 does not fire, plaintext serving is allowed, and the operator only notices when traffic is already on the wire in cleartext.

This ticket adds a boot-time structured validator that runs over an explicit contract table of env-var shapes and aborts with a per-key reason on the first failure. After this ticket, `PYRYCODE_RELAY_PRODUCTION=true` is a `*ErrInvalidConfig{Key: "PYRYCODE_RELAY_PRODUCTION", Reason: "malformed-value: expected \"1\" or unset, got \"true\""}` at boot, the process exits 2, and the deploy fails its health check rather than running in the wrong mode.

The contract table is the *single source of truth* for "what env vars the relay reads". Every future env-var addition registers an entry; the architect/code-review pair enforces this at landing time. Today the table is one row (`PYRYCODE_RELAY_PRODUCTION`) — that's the entire population of `os.Getenv` / `os.LookupEnv` call sites in `cmd/pyrycode-relay/` and `internal/relay/` at this ticket's merge time (verified by grep, see § Contract table population below).

Related: #9 (boot-time refusal precedent, `ErrCacheDirInsecure`); #77 (introduced `PYRYCODE_RELAY_PRODUCTION`); #79 (sibling boot-time check, `ErrUnexpectedCapability`); #42 (parent — split into #77 / #78 / #80).

## Design

Two new files: `internal/relay/env_config.go` and `internal/relay/env_config_test.go`. One ~7-line addition to `cmd/pyrycode-relay/main.go`. No existing code is refactored.

### Structured error type and sentinel

```go
// ErrInvalidConfigSentinel is the package-level sentinel that every
// *ErrInvalidConfig matches against via errors.Is. Detect any
// env-var validation failure with errors.Is(err, ErrInvalidConfigSentinel);
// extract the offending key and reason with errors.As(err, &cfgErr) where
// cfgErr is *ErrInvalidConfig.
var ErrInvalidConfigSentinel = errors.New("relay: invalid env-var config")

// ErrInvalidConfig is the structured error returned by CheckEnvConfig when
// a registered env var is missing-but-required or present-but-malformed.
// It exposes the offending env-var name and a human-readable reason so the
// caller can log a per-key remediation message at boot.
//
// Branchable via errors.Is(err, ErrInvalidConfigSentinel). Fields are
// extracted via errors.As(err, &cfgErr).
type ErrInvalidConfig struct {
    Key    string // env-var name (e.g. "PYRYCODE_RELAY_PRODUCTION")
    Reason string // human-readable shape violation
}

func (e *ErrInvalidConfig) Error() string {
    return fmt.Sprintf("%s: %s: %s", ErrInvalidConfigSentinel, e.Key, e.Reason)
}

// Is matches against the package-level sentinel so errors.Is(err, ErrInvalidConfigSentinel)
// returns true for any *ErrInvalidConfig instance regardless of Key / Reason.
func (e *ErrInvalidConfig) Is(target error) bool {
    return target == ErrInvalidConfigSentinel
}
```

Three decisions worth naming:

1. **Type name follows the AC literally.** The AC says "an exported structured error type `ErrInvalidConfig`". Go convention would normally use `InvalidConfigError` for the struct type and reserve `Err*` for sentinel variables (cf. `os.PathError` vs `os.ErrNotExist`). The AC explicitly conflates the two names; the cost of departing is grep-confusion for future agents reading the ticket vs the code. Keeping the type name `ErrInvalidConfig` and naming the sentinel `ErrInvalidConfigSentinel` resolves the AC literally while keeping the two values distinguishable in caller code.
2. **`Is(target) bool` on the type, not `Unwrap()`.** `Unwrap` would mean `*ErrInvalidConfig`'s message is layered on top of the sentinel's message, which double-prints the prefix. A custom `Is` method gives `errors.Is(err, ErrInvalidConfigSentinel) == true` without the message duplication. (`errors.As` works on the type regardless of `Is` / `Unwrap`.)
3. **Reason is a plain string, not a typed enum.** Reasons today are `"missing"` and `"malformed-value: …"`. A typed enum would lock the categories at the type level, but it would also force every future validator to extend the enum. The AC's examples (`missing`, `malformed-value: expected "1", got "true"`) treat the reason as operator-facing prose; a string carries that directly. If two reasons later need machine branching, the upgrade path is `errors.As` + a method on the type, not an enum break.

### Contract table

```go
// envContract is one row in the env-var registry. The validator iterates
// the registry in order at boot; each entry specifies the env-var name,
// whether the relay refuses to start when the var is unset, and a per-key
// shape validator. Validators receive the raw value (never an empty
// "present-but-empty" case — that branch is handled by the registry walk).
type envContract struct {
    name     string
    required bool
    validate func(value string) error
}

// envContracts is the single source of truth for "what env vars the relay
// reads at boot". Every new env-var read added under cmd/pyrycode-relay
// or internal/relay must register an entry here. Code review enforces.
var envContracts = []envContract{
    {
        name:     envProductionMode, // "PYRYCODE_RELAY_PRODUCTION"
        required: false,
        validate: func(v string) error {
            if v == "1" {
                return nil
            }
            return fmt.Errorf("expected %q or unset, got %q", "1", v)
        },
    },
}
```

Three decisions:

1. **Inline validators, no separate `envvalidate` package.** Per the ticket's Technical Notes. Validators today are one-liners; pulling them out would invert the locality without buying anything until there are several with shared logic.
2. **Optional-but-format-validated for `PYRYCODE_RELAY_PRODUCTION`.** Per the ticket body's explicit guidance ("`PYRYCODE_RELAY_PRODUCTION` is expected to be optional-but-format-validated"). Operationally: unset means non-production, which is a legitimate state for dev environments. The validator only fires when the var is *set* to a non-`"1"` value — that is the silent-typo class the ticket targets.
3. **`required: false` + validator never sees an empty value.** The check function distinguishes "unset" (skip), "set-but-empty-and-required" (fail with `missing`), and "set-and-non-empty" (run validator). A validator does not need to special-case the empty-string branch; this keeps each per-key validator focused on shape, not presence.

### Check function and getter shape

```go
// CheckEnvConfig walks envContracts and returns *ErrInvalidConfig on the
// first failure. Returns nil on full pass. Intended to be called from main
// after flag parse, before any listener is started.
//
// lookup is the env-var lookup function with the os.LookupEnv signature:
// it returns (value, present). Pass os.LookupEnv at the call site; tests
// inject a func built from a map[string]string. Env values not registered
// in envContracts are ignored (the validator polices its declared contract,
// not arbitrary process env).
func CheckEnvConfig(lookup func(string) (string, bool)) error {
    for _, c := range envContracts {
        val, present := lookup(c.name)
        if !present {
            if c.required {
                return &ErrInvalidConfig{Key: c.name, Reason: "missing"}
            }
            continue
        }
        if err := c.validate(val); err != nil {
            return &ErrInvalidConfig{Key: c.name, Reason: "malformed-value: " + err.Error()}
        }
    }
    return nil
}
```

Two decisions:

1. **`func(string) (string, bool)` not `func(string) string`.** The AC is explicit: "Env values are injected via a test seam (a `func(string) (string, bool)`-shaped getter)". The Technical Notes section of the ticket says "same shape #77 uses" — that's the `func(string) string` (os.Getenv) shape, which conflicts with the AC. **The AC wins**: distinguishing "missing-but-required" from "present-but-empty" is semantically necessary for the required-key case, and `os.Getenv` cannot make that distinction (it returns `""` for both). `os.LookupEnv`'s shape carries the bit. The existing `IsProductionMode` keeps its `func(string) string` getter because exact-`"1"` match doesn't need the presence bit; the two seams coexist without conflict.
2. **First-failure, not collected.** Returning on the first failure is sufficient for this ticket's threat model (a single misconfigured key blocks boot; the operator fixes it and re-deploys; the next failure is then surfaced). A multi-error variant is a future-ticket move if and when the table grows large enough that "fix one, redeploy, fix next" becomes a real cost.

### Wiring order in `cmd/pyrycode-relay/main.go`

Insert immediately before the existing `CheckInsecureListenInProduction` call (currently line 45), after the `--domain` xor `--insecure-listen` flag-validation guard (line 43):

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

Four details:

- **Order matters: env-config check runs BEFORE `CheckInsecureListenInProduction`.** This is the core architectural decision of the ticket. `CheckInsecureListenInProduction` consults `IsProductionMode`, which silently accepts any non-`"1"` value as "non-production". If the operator wrote `PYRYCODE_RELAY_PRODUCTION=true`, `CheckInsecureListenInProduction` returns nil and a plaintext production listener boots. Running `CheckEnvConfig` first catches the malformed value, exits 2, and the dangerous code path is never reached. Code review must verify this ordering at landing.
- **Exit code 2**, matching the adjacent boot-time refusal checks (`CheckInsecureListenInProduction` at line 50, `CheckCapabilities` at line 57). Exit 1 is reserved for runtime failures (listener died, autocert failed); exit 2 is configuration-rejected-at-boot. Ops dashboards can split "deploy never started" from "deploy started and crashed" on this distinction.
- **`errors.As` extraction at the log site**, not in the validator. The validator returns `error`; the caller decides how to structure the log line. The extracted `cfgErr.Key` populates the `env_var` field directly so the operator does not have to grep the error message. The `reason` field carries the per-key shape violation. The fallback branch (no `errors.As` hit) is defensive against a future refactor that wraps the error in something `errors.As` can't see through — under the current design it is unreachable.
- **No `fix` field.** The #77 wiring uses a `fix` field with a static remediation string because the failure mode is one specific misconfiguration. For `CheckEnvConfig` the remediation is per-key (every env var has its own fix), and `reason` already carries the per-key shape ("expected `\"1\"` or unset, got `\"true\"`") that tells the operator what to flip. Adding a separate `fix` field would duplicate.

The `errors` import is added to `cmd/pyrycode-relay/main.go` (not currently imported there).

### Contract table population at merge time

Verified by grep against `main` at spec-write time:

```
$ grep -rn "os\.Getenv\|os\.LookupEnv" cmd/ internal/
cmd/pyrycode-relay/main.go:45:    if err := relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv); err != nil {
internal/relay/production.go:28:    // The env var is read on every call via getenv. Pass os.Getenv at call sites;
internal/relay/production.go:39:    // getenv is the env-var lookup function; pass os.Getenv at the call site,
```

The only read of an env var (the `main.go:45` line passes `os.Getenv` *as a function value* into `CheckInsecureListenInProduction`, which calls it with `envProductionMode` inside `IsProductionMode`) is `PYRYCODE_RELAY_PRODUCTION`. The two `production.go` matches are doc-comment references. The table has one row.

If a sibling ticket (e.g. #81) lands an additional env-var read between now and #80's merge, the developer extends `envContracts` to cover it. Code review enforces the "every env var registered" invariant by re-running the grep.

### Why not a `Config` struct (yet)

A natural extension is to build a `relay.Config` struct populated from the env at boot, validated as a single step, and threaded through the rest of the binary. **Deferred.** Today the relay has one env var; the ticket body promises "future env-var additions register here", which means the contract table is the registry, not a typed struct. When env-var count crosses ~3 *and* a downstream caller wants a typed snapshot (rather than a per-call `getenv` closure), a follow-up ticket consolidates. Doing it now is premature.

## Concurrency model

None. `CheckEnvConfig` runs on the main goroutine after `flag.Parse` and before any listener is started. No locks, no channels, no goroutines.

## Error handling

`CheckEnvConfig` returns either `nil` (full pass) or `*ErrInvalidConfig` (first failure). No wrapping, no retry, no fallback. The main wiring exits 2 on any non-nil return — boot-time refusal is total.

The error message format (`"relay: invalid env-var config: <KEY>: <REASON>"`) is stable and tested. Downstream callers that branch on this failure mode use `errors.Is(err, ErrInvalidConfigSentinel)`; callers that need the offending key use `errors.As(err, &cfgErr)` and read `cfgErr.Key`.

## Testing strategy

All tests live in `internal/relay/env_config_test.go`, are `t.Parallel()`-safe, and never call `os.Setenv` / `t.Setenv`. Helper for building the fake lookup (adapted from `production_test.go`'s `fakeGetenv` to the `LookupEnv` shape):

```go
func fakeLookup(env map[string]string) func(string) (string, bool) {
    return func(k string) (string, bool) {
        v, ok := env[k]
        return v, ok
    }
}
```

### Test 1 — Valid env returns nil (AC.a)

`PYRYCODE_RELAY_PRODUCTION` set to `"1"` and `PYRYCODE_RELAY_PRODUCTION` unset are both valid (the var is optional). Two cases in one table:

| `PYRYCODE_RELAY_PRODUCTION` | want |
|---|---|
| unset | `nil` |
| `"1"` | `nil` |

### Test 2 — Malformed value returns `*ErrInvalidConfig` with key + reason (AC.c)

Table over the malformed-value space. Each row: set `PYRYCODE_RELAY_PRODUCTION` to a non-`"1"`, non-empty value; assert:
- `errors.Is(err, ErrInvalidConfigSentinel)` is true
- `errors.As(err, &cfgErr)` is true
- `cfgErr.Key == "PYRYCODE_RELAY_PRODUCTION"`
- `cfgErr.Reason` starts with `"malformed-value: "`

Cases:

| value | reason contains |
|---|---|
| `"true"` | `expected "1" or unset, got "true"` |
| `"0"` | `expected "1" or unset, got "0"` |
| `"yes"` | `expected "1" or unset, got "yes"` |
| `" 1"` | `expected "1" or unset, got " 1"` |
| `"1 "` | `expected "1" or unset, got "1 "` |
| `"PRODUCTION"` | `expected "1" or unset, got "PRODUCTION"` |

The cases mirror the `IsProductionMode` value matrix from `production_test.go:21-29`. The doubling is intentional: if a future refactor relaxes the `"1"`-exact rule to `strconv.ParseBool`, both tests fail at once, making the contract violation impossible to miss.

### Test 3 — Missing required key returns `*ErrInvalidConfig` with `Reason: "missing"` (AC.b)

Today no env var is `required: true`, so this case is not exercisable through the production `envContracts` table. To still test the code path, the test injects a *test-local* `envContracts` table via a small package-private seam — the simplest version is a `checkEnvConfigWith` helper that takes the table as an argument:

```go
// in env_config.go, package-private
func checkEnvConfigWith(lookup func(string) (string, bool), contracts []envContract) error {
    // body identical to CheckEnvConfig's loop
}

func CheckEnvConfig(lookup func(string) (string, bool)) error {
    return checkEnvConfigWith(lookup, envContracts)
}
```

Then in `env_config_test.go`:

```go
func TestCheckEnvConfig_MissingRequiredKey(t *testing.T) {
    t.Parallel()
    contracts := []envContract{{name: "X", required: true, validate: func(string) error { return nil }}}
    err := checkEnvConfigWith(fakeLookup(map[string]string{}), contracts)
    var cfgErr *ErrInvalidConfig
    if !errors.As(err, &cfgErr) {
        t.Fatalf("err = %v, want *ErrInvalidConfig", err)
    }
    if cfgErr.Key != "X" || cfgErr.Reason != "missing" {
        t.Errorf("got Key=%q Reason=%q, want Key=%q Reason=%q", cfgErr.Key, cfgErr.Reason, "X", "missing")
    }
    if !errors.Is(err, ErrInvalidConfigSentinel) {
        t.Error("err should satisfy errors.Is(err, ErrInvalidConfigSentinel)")
    }
}
```

This is the same shape as `caps_test.go:108-155` (exercise the inner check function with a synthetic input the outer wrapper can't construct).

### Test 4 — Unregistered env vars do NOT fail the validator (AC.d)

```go
func TestCheckEnvConfig_IgnoresUnregisteredKeys(t *testing.T) {
    t.Parallel()
    env := map[string]string{
        "FOO_BAR_BAZ":       "garbage",
        "PYRYCODE_UNKNOWN":  "anything",
        "PATH":              "/usr/bin",
        // PYRYCODE_RELAY_PRODUCTION intentionally absent (optional, unset is valid)
    }
    if err := CheckEnvConfig(fakeLookup(env)); err != nil {
        t.Errorf("got err %v, want nil — unregistered keys must be ignored", err)
    }
}
```

This locks in the "validator polices its declared contract, not arbitrary process env" invariant. Without it a future refactor that walks `os.Environ()` for unknown keys would break the contract silently.

### Test 5 — Sentinel is branchable

```go
func TestErrInvalidConfigSentinel_IsBranchable(t *testing.T) {
    t.Parallel()
    err := &ErrInvalidConfig{Key: "X", Reason: "missing"}
    if !errors.Is(err, ErrInvalidConfigSentinel) {
        t.Error("*ErrInvalidConfig should satisfy errors.Is(_, ErrInvalidConfigSentinel)")
    }
    var cfgErr *ErrInvalidConfig
    if !errors.As(error(err), &cfgErr) {
        t.Error("*ErrInvalidConfig should satisfy errors.As(_, &*ErrInvalidConfig)")
    }
}
```

Locks in the `errors.Is` / `errors.As` dual contract at the type level so future refactors of the `Is` method can't silently break detection.

### What is NOT tested

- The `main.go` wiring. Same rationale as #77: `main` is a coordination function whose behaviour is observable through the unit tests on the package it calls. The `errors.As` extraction in `main` is locally verifiable from the diff; an integration test that fork-execs the binary to assert exit code 2 is over-engineering for one `if err != nil { os.Exit(2) }` block.
- The structured log line's exact field names. The AC requires "a structured log line that names the offending key and the reason"; the spec's wiring satisfies that literally. Asserting on slog handler output requires capturing the handler, which is friction for an operator-facing prose contract.
- The composition of `CheckEnvConfig` + `CheckInsecureListenInProduction`. They are independent checks called in sequence; the per-check tests cover each branch. The ordering invariant ("env-config check runs first") is enforced by code review of `main.go`, not by a unit test.

## Open questions

None. The error type name is fixed by the AC; the sentinel suffix (`Sentinel`) is the smallest naming convention that resolves the AC's collision between "type" and "sentinel" without departing from the AC's literal type name. The getter shape (`(string, bool)`) is fixed by the AC. The contract table is one row at merge time; the population mechanism (grep on landing) is documented above.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings. The only data crossing into the validator is operator-controlled env vars (not network-attacker-controlled). The validator is a fail-closed gate: anything not exactly matching a registered shape aborts boot. The boundary is explicit (the `envContracts` table in `internal/relay/env_config.go`); downstream code does not consume env-var values directly — it consumes typed helpers (`IsProductionMode`) whose inputs have already been validated by the time `main` reaches them. **Important ordering invariant:** the validator must run *before* `CheckInsecureListenInProduction` so that `IsProductionMode`'s silent-non-production fallback for non-`"1"` values cannot be reached with an unvalidated env. The spec's § Wiring order section is explicit about this and code review must enforce it.

- **[Tokens, secrets, credentials]** N/A. The ticket creates no tokens, reads no secrets, writes no credential material. Env vars in the contract table carry configuration signals (`PYRYCODE_RELAY_PRODUCTION="1"`), not credentials. **Forward-looking caveat for the contract table:** if a future env var carries a secret (token, key), its validator must *not* echo the value into the reason string the way the `PYRYCODE_RELAY_PRODUCTION` validator does today (`got %q`). The reason string is logged at boot, so for secret-bearing env vars the validator should describe the shape violation in operator-facing terms ("expected hex-encoded 32-byte value", not "got %q"). Today's only entry is safe because the legitimate value is the literal string `"1"`. The developer must not extend the log line in `main.go` to include the raw `os.LookupEnv` value either — the structured fields are `err` (which includes the reason as composed by the validator), `env_var` (the name, not the value), and `reason` (the validator's prose). No `value` field.

- **[File operations]** N/A. No file is read, written, statted, opened, or unlinked. No path concatenation.

- **[Subprocess / external command execution]** N/A. No `exec.Command`, no `os.StartProcess`, no shell.

- **[Cryptographic primitives]** N/A. No RNG, no hash, no comparison of attacker-controlled values to secrets. The string equality `v == "1"` inside the `PYRYCODE_RELAY_PRODUCTION` validator is operator-controlled-to-operator-controlled (an env var value vs a literal constant) — no timing-side-channel surface.

- **[Network & I/O]** No findings. The validator runs *before* any listener is opened. The whole purpose of the ticket is to prevent a listener from starting in a misconfigured state (specifically, to prevent `--insecure-listen` from being honoured under a typo'd production-mode env). Unrelated network code (`http.Server` timeout configuration in `main.go:84-104`, autocert setup, the `tls.go` machinery) is unchanged.

- **[Error messages, logs, telemetry]** One **SHOULD FIX** noted inline above (Tokens category): the developer must not add a log field that echoes the raw env-var value into a structured log line. The spec's wiring already avoids this (the log fields are `err`, `env_var`, `reason`). For today's one entry (`PYRYCODE_RELAY_PRODUCTION`) the reason itself echoes the value via `got %q`, which is acceptable because the legitimate value is `"1"` and any operator-supplied bytes there are misconfiguration data, not secret data. The rule for future entries is "validator's reason describes shape, not raw value, if the env var could carry a secret".

  No findings for log injection: slog with the default text handler escapes control characters in field values; an attacker (operator) who put a newline into `PYRYCODE_RELAY_PRODUCTION` would get a `\n`-escaped field, not a forged log line.

- **[Concurrency]** N/A. The validator runs on the main goroutine before any goroutine is spawned. No shared mutable state. The `envContracts` table is a package-level `var` written once at init and read-only thereafter; nothing in the spec mutates it at runtime, and code review must keep it read-only (a runtime extension API would invert the trust model).

- **[Threat model alignment]** No findings. `pyrycode/pyrycode/docs/protocol-mobile.md` § Security model and `docs/threat-model.md` both assume the relay is correctly configured for its deployment role (production with TLS; dev with plaintext for local loops). This ticket is the in-binary enforcement of "configuration is well-formed". The complement (a CI / deploy-manifest check that *required* env vars exist in production manifests) is out of scope and tracked separately; today no env var is `required: true` so that future work hooks in naturally when the first required key is introduced.

The validator's adversarial surface is small: it parses no untrusted bytes (env vars are operator-controlled and bounded by the OS env page size, typically 128 KiB total per process), produces no externally-visible artifacts beyond a structured log line, and aborts the process on any contract violation. The fail-closed default is the correct stance for a boot-time gate.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
