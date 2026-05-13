# Production-mode contract & `--insecure-listen` startup refusal

Single-env-var production-mode signal for the relay, plus the first boot-time check that consumes it. Defined by #77; sibling startup checks (#78 = refuse-to-run-as-uid-0; future others) reuse the contract rather than re-reading the env var.

## The contract

- **Env var:** `PYRYCODE_RELAY_PRODUCTION`
- **On:** the exact string `"1"`. Anything else — unset, `"0"`, `"true"`, `"yes"`, `" 1"`, `"1 "`, `"PRODUCTION"` — means non-production.
- **Read lazily** on every call. No init-time cache. Tests inject a getter; production wires `os.Getenv`.

Strict-equality (not `strconv.ParseBool` "truthy" parsing) is intentional and mirrors `PYRYCODE_RELAY_SINGLE_INSTANCE` (#64/#65). An exact-string contract is harder to misread in a deploy manifest than a "what does this parser accept?" question, and an operator who believes production mode is on but the relay disagrees is exactly the silent-misconfiguration class the contract exists to prevent.

## API (`internal/relay/production.go`)

- `IsProductionMode(getenv func(string) string) bool` — reports whether `getenv("PYRYCODE_RELAY_PRODUCTION") == "1"`. Exported so sibling checks (#78+) compose on the same predicate.
- `CheckInsecureListenInProduction(insecureListen string, getenv func(string) string) error` — returns `ErrInsecureListenInProduction` when production mode is on AND `insecureListen != ""`, nil otherwise. Intended to run after `flag.Parse()`, before any listener is started.
- `ErrInsecureListenInProduction` — exported sentinel, branchable via `errors.Is`. Message names both inputs (`"relay: --insecure-listen is set with PYRYCODE_RELAY_PRODUCTION=1; refusing to start"`) so a log line is self-documenting.
- `envProductionMode` (unexported) — the env-var-name constant. In-package siblings reuse it; out-of-package callers go through `IsProductionMode`.

## Why this shape (test seam)

The check takes a `func(string) string` rather than calling `os.Getenv` directly. The seam is the smallest design that satisfies the AC's "do not mutate process env" — no interface, no struct, no package-level mutable variable. `os.Getenv` satisfies the signature literally at the call site; tests build a closure over a `map[string]string`. Process env is never touched, so the tests are safe under `t.Parallel()` and `go test -race -count=N`.

## Wiring (`cmd/pyrycode-relay/main.go`)

After flag-parse and after the existing "either `--domain` or `--insecure-listen`" guard, before `relay.NewRegistry()`:

```go
if err := relay.CheckInsecureListenInProduction(*insecureListen, os.Getenv); err != nil {
    logger.Error("refusing to start: production-mode misconfiguration",
        "err", err,
        "env_var", "PYRYCODE_RELAY_PRODUCTION",
        "fix", "remove --insecure-listen and set --domain, or unset PYRYCODE_RELAY_PRODUCTION")
    os.Exit(2)
}
```

- **Exit code 2** matches the flag-validation guard immediately above. Exit 1 is reserved for runtime failures in this binary (listener died, autocert failed); exit 2 = configuration-rejected-at-boot. Splitting the codes lets ops dashboards distinguish "deploy never started" from "deploy started and crashed."
- **`fix` field** lists both valid resolutions (remove the flag OR unset the env var). The operator picks whichever input was wrong.
- **`env_var` field carries the name, never the value.** Even if a confused operator put a secret in `PYRYCODE_RELAY_PRODUCTION`, it would not be logged. Do not extend this log line to include the env var value.

## Behaviour matrix

| `PYRYCODE_RELAY_PRODUCTION` | `--insecure-listen` | Result |
|---|---|---|
| unset / not `"1"` | any | nil (no check fires) |
| `"1"` | empty (autocert path) | nil |
| `"1"` | non-empty | `ErrInsecureListenInProduction` → `os.Exit(2)` |

Autocert (`--domain`) is not inspected — the contract is purely about plaintext-in-prod. Setting `--domain` with `PYRYCODE_RELAY_PRODUCTION=1` is the happy path.

## Threat model alignment

`pyrycode/pyrycode/docs/protocol-mobile.md` § Security model assumes TLS for all production traffic. This is the in-binary enforcement of that assumption *when production mode is explicitly tagged*. The complement — a CI / deploy-manifest check that prod manifests actually set `PYRYCODE_RELAY_PRODUCTION=1` — is out of scope and the responsibility of the deploy layer.

The check is fail-closed: if the env var is on AND plaintext is requested, the relay refuses to boot. There is no degradation path, no fallback, no retry. Boot-time refusal is total.

## Out of scope (deferred)

- **No `Config` struct.** A bundled `relay.Config` with `Validate()` returning a multi-error is a natural extension once 3+ startup checks exist. With one check today (this one) and one more queued (#78), the consolidation is premature.
- **No fork-exec integration test on `main.go`.** The `main` wiring is observable via package-level unit tests; one `if err != nil { os.Exit(2) }` block does not warrant a binary-spawning test.

## Cross-links

- ADR: none filed; the shape is precedent-following (mirrors `PYRYCODE_RELAY_SINGLE_INSTANCE`), not a new architectural choice.
- [`internal/relay/tls.go`](../../../internal/relay/tls.go) — `ErrCacheDirInsecure` is the canonical boot-time-refusal sentinel this one models.
- [Single-instance constraint (v1)](../../architecture.md#single-instance-constraint-v1) — sibling env-var contract (`PYRYCODE_RELAY_SINGLE_INSTANCE`), shape precedent.
- [Codebase ticket note #77](../codebase/77.md) — per-ticket implementation detail.
