# Single-instance startup self-check

Boot-time refusal that fires when the relay detects it is running on a multi-instance-capable platform (today: Fly.io) and the operator has not asserted single-instance intent via `PYRYCODE_RELAY_SINGLE_INSTANCE=1`. Deterministic backstop to the [`docs/architecture.md` § Single-instance constraint](../../architecture.md#single-instance-constraint-v1) prose half (#64). Closes the foot-gun where `fly scale count 3` silently breaks server-id routing because the connection registry (`internal/relay/registry.go`) is in-memory per process — two replicas hold disjoint server-id tables and a phone routed to the wrong replica closes with `4404` even though the binary is online.

## Contract

- **Bypass env var:** `PYRYCODE_RELAY_SINGLE_INSTANCE`. The literal name is pinned by `docs/architecture.md` (the doc and the code agree on the wire). Exact string `"1"` bypasses; anything else is treated as not set.
- **Multi-instance signal:** `FLY_APP_NAME` non-empty. Fly's platform sets this on every machine in an app, which is also the substrate where `fly scale count > 1` is a one-line command. Presence is sufficient evidence we are on a multi-instance-capable platform; the value is not inspected.
- **Decision order:** bypass wins unconditionally. If `PYRYCODE_RELAY_SINGLE_INSTANCE=1`, return nil even when the platform signal is present. Otherwise, if the platform signal is present, return `ErrMultiInstanceDeployDetected`. Otherwise nil.

## API (`internal/relay/single_instance.go`)

- `CheckSingleInstance(getenv func(string) string) error` — exported helper. Signature mirrors `CheckInsecureListenInProduction` / `CheckRunningAsRoot`. Pure: two env reads, one string comparison, no goroutines, no I/O.
- `ErrMultiInstanceDeployDetected` — exported sentinel, branchable via `errors.Is`. Message: `"relay: multi-instance deploy detected; set PYRYCODE_RELAY_SINGLE_INSTANCE=1 to bypass"`. Both the AC-mandated substring `multi-instance deploy detected` AND the literal env-var name `PYRYCODE_RELAY_SINGLE_INSTANCE` are present in the message so `grep -i 'multi-instance deploy detected'` over boot logs hits regardless of structured-field rendering.
- `envSingleInstanceBypass` / `envFlyAppName` (unexported) — the env-var-name constants. The bypass constant carries a `// #nosec G101` comment to silence gosec's "looks like a credential" false-positive on the literal string.

## Wiring (`cmd/pyrycode-relay/main.go`)

Boot-time check ordering, top to bottom:

1. `CheckEnvConfig` (#80)
2. **`CheckSingleInstance` (#65)** — this check
3. `CheckInsecureListenInProduction` (#77)
4. `CheckRunningAsRoot` (#78)
5. `CheckCapabilities` (#79)

Position rationale:

- **After `CheckEnvConfig`** so a typo'd bypass (`=true`, `=0`, `= 1`) fails with the structured per-key config error rather than being silently treated as "not set" and tripping the multi-instance refusal with a misleading log line.
- **Before `CheckInsecureListenInProduction`** because a deploy-shape misconfiguration (multi-instance) is more fundamental than a production-mode misconfiguration (insecure-listen-in-prod). If both fire, the operator should see the multi-instance error first; fixing it may not be possible without changing the deploy substrate, while insecure-listen is a flag flip.

Error-log shape (mirrors the established refuse-to-start pattern):

```go
if err := relay.CheckSingleInstance(os.Getenv); err != nil {
    logger.Error("refusing to start: multi-instance deploy detected",
        "err", err,
        "bypass_env_var", "PYRYCODE_RELAY_SINGLE_INSTANCE",
        "fix", "set PYRYCODE_RELAY_SINGLE_INSTANCE=1 in the deploy manifest (e.g. fly.toml [env]) to assert single-instance intent; see docs/architecture.md § Single-instance constraint")
    return 2
}
```

Exit code 2 matches the sibling boot-time refusals (config-rejected-at-boot vs. exit 1 for runtime failures).

## Env-config registry row

`envContracts` in `internal/relay/env_config.go` gains a row for `PYRYCODE_RELAY_SINGLE_INSTANCE` — `required: false`, validator accepts exact `"1"` or unset, rejects everything else. Same shape as `envProductionMode`. The row references the unexported constant declared in `single_instance.go` so the literal env var name lives in exactly one place. A `=true` typo therefore fails `CheckEnvConfig` with a structured per-key error before `CheckSingleInstance` reads the value.

## Behaviour matrix

| `PYRYCODE_RELAY_SINGLE_INSTANCE` | `FLY_APP_NAME` | Result |
|---|---|---|
| unset | unset | nil (common single-instance case) |
| unset | set | `ErrMultiInstanceDeployDetected` → exit 2 |
| `"1"` | unset | nil (bypass is benign when the check would have passed) |
| `"1"` | set | nil (bypass asserts intent) |
| anything else (`"true"`, `"0"`, `" 1"`, …) | any | caught earlier by `CheckEnvConfig` (exit 2 with structured per-key error) |

The "anything else" row is the defence-in-depth case: `CheckSingleInstance` treats every non-`"1"` value as "not set" even if `CheckEnvConfig` were ever to be reordered or removed. The matrix's bottom row reflects today's wiring; the underlying contract is "exact-`"1"` bypass" at both layers.

## Scope and limits

- **Heuristic, not adversarial.** The check catches "operator deployed to Fly and never read `architecture.md`". It does NOT catch "operator ran `fly scale count 3` after configuring the bypass in `fly.toml [env]`" — once the bypass propagates via the manifest, all replicas inherit it. The constraint to use only env vars exposed by the host (no Fly Machines API client, no DNS query against `<app>.internal`) precludes the only mechanism that could catch the scale-out case at runtime. The honest framing: this is an *explicit deploy-time gate*, not a runtime instance count.
- **Fly.io only today.** Only `FLY_APP_NAME` is in the signal set. Kubernetes (`KUBERNETES_SERVICE_HOST`), ECS (`ECS_CONTAINER_METADATA_URI`), AWS Lambda (`AWS_LAMBDA_FUNCTION_NAME`) would be candidates if the relay is ever ported. Signal-set is intentionally narrow — adding signals on speculation would create false positives for operators running the binary in unusual local-dev shells. Defer until a non-Fly substrate is actually considered.
- **No new dependencies.** No Fly Machines API client, no platform-specific SDK; the detection mechanism is local env-var reads only, per ticket Technical Notes.

## Threat model alignment

The check reads process env vars set by the deploy substrate (Fly's platform sets `FLY_APP_NAME`; the operator sets the bypass). Both are out-of-band relative to the relay's internet-exposed surface — a remote attacker on `/v1/server` or `/v1/client` cannot influence either value. The bypass is fail-open by design: the threat model assumes an adversary capable of setting process env vars already has shell access to the deploy host, at which point the multi-instance refusal is not the load-bearing control. The bypass is an operator-discipline tool, not a security boundary.

No env-var *values* are interpolated into the log line — only the literal bypass-env-var name and the sentinel error. Log-injection is structurally impossible at this site.

## Out of scope (deferred)

- **`fly.toml [env] PYRYCODE_RELAY_SINGLE_INSTANCE = "1"` block.** Once this ticket lands, the production Fly deploy will refuse to start until the manifest sets the bypass. The `__REGION__` / `__DOMAIN__` placeholders in `fly.toml` would fail the first deploy regardless; the bootstrap procedure in `docs/deploy.md` handles both at once.
- **Reconciling `docs/architecture.md`'s "NOT recommended for production" framing.** That section, written before this code spec, characterises the bypass as an emergency / migration-window tool. With `FLY_APP_NAME` as the multi-instance signal, the bypass must be set *permanently* in the Fly manifest for the production deploy to start at all. The framing reconciliation is a separate doc-only follow-up ticket against `docs/architecture.md § Single-instance constraint`; this code-side ticket landed first as the contract source.
- **Shared-registry / sticky-session multi-instance support.** Tracked in `docs/architecture.md § What multi-instance would require`; this ticket is the v1 single-instance backstop.

## Cross-links

- [Architecture § Single-instance constraint (v1)](../../architecture.md#single-instance-constraint-v1) — the doc half (#64); shares the `PYRYCODE_RELAY_SINGLE_INSTANCE` env var contract.
- [Production-mode contract & startup refusals](production-mode.md) — sibling boot-time refusals (#77 / #78); the `func(string) string` getter shape and exact-`"1"` contract this check mirrors.
- [Env-var config validator](env-config-validator.md) — #80; polices the `=true` / `=0` / `= 1` typo cases before this check reads the value.
- [Listener port allowlist](listener-port-allowlist.md) — sibling boot-time refusal (#81); same exit-2-and-structured-log shape.
- [Codebase note #65](../codebase/65.md) — per-ticket implementation detail.
- [`internal/relay/single_instance.go`](../../../internal/relay/single_instance.go) — implementation.
- [Spec: 65-startup-multi-instance-check](../../specs/architecture/65-startup-multi-instance-check.md) — architect's design.
