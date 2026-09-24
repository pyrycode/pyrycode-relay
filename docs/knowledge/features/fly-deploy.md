# Fly.io deploy — production host wiring

The relay's production host is a single [Fly.io](https://fly.io) machine in one region. `fly.toml` at the repo root tells Fly how to run the portable image from #32. Deploys are **operator-direct from a clean `main`** (`flyctl deploy --remote-only`) since 2026-05-24, when the convenience CI deploy workflow (`.github/workflows/ci.yml`) was removed across the org — no GHA workflow auto-deploys on push. Operator-facing procedures live in [`docs/deploy.md`](../../deploy.md); the decision record lives in [`docs/architecture.md` § Hosting](../../architecture.md#hosting).

## What it does

- **`fly.toml`** declares a Fly Apps v2 app with TCP passthrough on `:80` + `:443`, a persistent volume at `/var/lib/relay/autocert`, and a single-machine hard cap.
- **Operator-direct deploy:** from a clean checkout of `main`, run `make check` then `flyctl deploy --remote-only -a pyrycode-relay`. Fly's remote builder rebuilds the image from `Dockerfile` against `main`'s HEAD and rolls the single machine in place via the `rolling` strategy, which waits for the `internal_port = 8080` service's TCP check before finishing — a relay that refuses to boot fails the deploy rather than leaving production down ([#118](../codebase/118.md)). No GHA workflow auto-deploys.
- **Bootstrap is one-time:** `flyctl apps create` → `flyctl ips allocate-v4` → `flyctl volumes create relay_autocert` → DNS → fill `__REGION__` / `__DOMAIN__` placeholders in `fly.toml`. The operator authenticates `flyctl` locally (`flyctl auth login` / `FLY_API_TOKEN` in the local environment); no GitHub repo secret is involved anymore.

## Why this shape

### TCP passthrough, not Fly's HTTP proxy

TLS terminates **in the relay** via autocert (#9, shipped). Fly is a raw-TCP substrate; the binary holds the cert. Three load-bearing consequences:

- `[[services]]` blocks use `protocol = "tcp"` with no `handlers = ["http"]` / `handlers = ["tls"]`. An explicit handler list would either steal `:80` from autocert's HTTP-01 listener (breaks cert issuance) or terminate TLS at Fly's edge (bypasses autocert entirely and would force a Fly-managed cert).
- A **dedicated IPv4** is required, not optional. Shared IPv4 + TCP passthrough is not a supported combination on Fly; Let's Encrypt's HTTP-01 challenge needs a deterministic resolution from `<domain>:80` to the running machine. `flyctl ips allocate-v4` is part of the bootstrap.
- The client's real IP reaches the relay's rate limiter, but not by default and not as the raw socket peer. On Fly's raw-TCP services the socket peer the relay sees is Fly's edge proxy, not the client — the real address travels only via the `proxy_proto` handler on the `443` port, which prepends a PROXY protocol v2 header. `--https-proxy-protocol` ([feature doc](proxy-protocol-listener.md), #110) makes the autocert HTTPS listener require and parse that header before the TLS handshake, so `RemoteAddr` — and therefore #34's `ClientIP` and the per-IP rate limiter — carry the real client address. TCP passthrough (as opposed to Fly-terminated TLS) is still what makes this possible at all: a `handlers = ["tls"]` or `["http"]` port would terminate at Fly's edge and there would be no relay-owned TLS handshake to attach the wrapper to. Any future move to platform-terminated TLS would require a security-sensitive follow-up against both #34 and #110.

ADR-0002's `:80` 404 fallback (explicit failure on non-ACME `:80` requests, rather than a 302 → `:443`) means `:80` carries non-trivial application logic. The TCP-passthrough manifest preserves that — Fly inserts no MITM-shaped middleware.

### Single-machine hard cap

The relay's connection registry is **in-process** (`internal/relay/registry.go`); two machines hold two disjoint registries and silently drop phones routed to the "wrong" replica. This is a correctness constraint, not a cost optimisation — `docs/architecture.md § Single-instance constraint (v1)` is canonical.

Fly Apps v2 has **no `max_machines` key**. The cap is encoded via four declarative knobs:

| Knob | Setting | Why |
| --- | --- | --- |
| `[[services]] min_machines_running` | `1` | Don't drop below one |
| `[[services]] auto_start_machines` | `false` | Don't let Fly create new machines on demand |
| `[[services]] auto_stop_machines` | `"off"` | Don't stop the one machine |
| `[deploy] strategy` | `"rolling"` | Updates the one machine in place and waits for its health check; never creates a second machine |

Operator discipline at `flyctl scale count` is the platform-level ceiling — Fly itself does not enforce one. The in-binary `PYRYCODE_RELAY_SINGLE_INSTANCE` self-check (#65) is the load-bearing backstop for an operator mistake.

### Deploy health check — TCP on 8080, `rolling` waits for it ([#118](../codebase/118.md))

`[deploy] strategy` is `"rolling"`, not `"immediate"`. `rolling` replaces machines one at a time and waits for each machine's checks before finishing; `immediate` replaces machines without waiting for any check, even when one exists. On this one-machine fleet `rolling` still updates in place — it creates no second machine, so the single-machine hard cap above is unaffected; only `immediate` vs. `rolling` changed, not `min_machines_running` / `auto_start_machines` / `auto_stop_machines`.

The `internal_port = 8080` service carries a `[[services.tcp_checks]]` block (`grace_period = "30s"`, `interval = "15s"`, `timeout = "5s"`). The `internal_port = 8443` service has no check. The check is TCP, not HTTP, and lives on 8080 rather than 8443, because no port a Fly check can reach serves `/healthz`:

- On 8080 everything except the ACME HTTP-01 challenge gets a 404 fallback ([ADR-0002](../decisions/0002-autocert-explicit-failure-on-port-80.md)) — there is no `/healthz` route to probe here, but a bare TCP connect is enough to prove the listener is up.
- On 8443, `NewProxyProtoListener` ([PROXY protocol listener](proxy-protocol-listener.md), #110) requires a PROXY v2 header before the TLS handshake even starts; Fly's checker connects directly with no such header, so every HTTP or TCP probe there would fail in the TLS path. That server has no `ErrorLog` set, so each failed probe would also log a line to stderr — a TCP check on 8443 would just add log noise for a check that can never pass.
- Checking 8080 still covers 443: a boot refusal exits before either listener binds, and `runServers` drains and exits if any listener fails, so both ports go down together. One check on 8080 is sufficient signal for both.

Serving `/healthz` on a new surface reachable by Fly's checker would be a new public endpoint and a threat-model re-review — out of scope here; see [`docs/security-followups.md` § *`/healthz` exposure*](../../security-followups.md).

Risk accepted: a spurious check failure pulls port 80 off Fly's proxy on this single-machine fleet (443 has no check, so the WebSocket surface is unaffected by a spurious 8080 failure). The timings above are deliberately generous — boot is local-only and sub-second on a shared-cpu-1x / 256 MB machine, so 30s grace is roughly a 30× margin. Operator follow-up after each deploy: `flyctl checks list -a pyrycode-relay` should show the check passing.

### Argv, not shell

The distroless runtime image has no shell, so `[processes] app = "..."` is treated as **final argv** by Fly, not as a shell line. Env-var expansion (`${DOMAIN}`) is not available; the manifest passes literal values:

```toml
[processes]
  app = "--domain __DOMAIN__ --cert-cache /var/lib/relay/autocert"
```

`--cert-cache /var/lib/relay/autocert` is explicit because the binary's default (`defaultCertCache()` returns `$HOME/.pyrycode-relay/certs`, i.e. `/home/nonroot/.pyrycode-relay/certs`) is **not** the volume mount. Without the flag, autocert would write into an unmounted scratch path and lose the cache on every machine recycle.

### Loud placeholders

`primary_region = "__REGION__"` and `--domain __DOMAIN__` ship as placeholders. The first deploy fails loudly if the operator forgets to fill them — preferable to a plausible-but-wrong real value escaping review (cf. the silently-misconfigured-cert-dir failure mode the autocert work guarded against).

### Manifest gotchas surfaced by first-deploy bootstrap (#97)

Two non-obvious `fly.toml` requirements were captured after the 2026-05-24 first-deploy bootstrap. Both are now encoded in the checked-in manifest; the operator-facing notes live in [`docs/deploy.md` § *fly.toml gotchas*](../../deploy.md#flytoml-gotchas).

- **`[env] PYRYCODE_RELAY_SINGLE_INSTANCE = "1"` is required on Fly.** Fly's substrate exposes `FLY_APP_NAME`, which the #65 self-check reads as a multi-instance-capable platform. Without this env var asserted in the manifest, the relay refuses to boot. The `[env]` block sits immediately under the manifest's single-machine-cap header comment with an inline rationale that names the self-check by issue number, so a future reader doesn't mistake the assertion for dead config. This is the deploy-time half of the [Single-instance startup self-check](single-instance-check.md) belt-and-suspenders pair — the binary-side gate plus the manifest-side assertion together close the "operator deployed to Fly and never read `architecture.md`" failure mode.
- **`processes = ["app"]` on each `[[services]]` block.** Fly's schema requires every service to name its process whenever `[processes]` is defined, and `[processes]` is always defined here (argv lives in `[processes]` because distroless has no shell to expand env vars into argv — see *Argv, not shell* above). Adding a third `[[services]]` block in a future ticket means adding `processes = ["app"]` as the first line; `flyctl config validate` is the structural backstop but PR-time correctness is cheaper than a failed `flyctl deploy`.

### Listen-port pinning is provisional

The current manifest pins `internal_port = 80` / `internal_port = 443`, matching autocert's hardcoded defaults from the pre-#96 era. [Ticket #96](https://github.com/pyrycode/pyrycode-relay/issues/96) — [Autocert TLS (configurable HTTP-01 and TLS listener addresses)](autocert-tls.md) — has now shipped the binary-side capability for high-port substrates (`--http-listen` / `--https-listen` flags, defaults preserve the low-port binding exactly). Landing the canonical high-port Fly recipe (`internal_port = 8080` / `internal_port = 8443` + `--http-listen=:8080 --https-listen=:8443` in `[processes]`, with external `port = 80` / `port = 443` untouched on the `[[services.ports]]` blocks) is a deferred doc-only follow-up that owns the operator-side verification dance against a real Fly deploy. The #97 forward pointer in [`docs/deploy.md` § *fly.toml gotchas*](../../deploy.md#flytoml-gotchas) is the institutional-memory seam that keeps the follow-up findable.

## Deploy privilege model — operator-direct

Deploys run from an operator's local checkout of clean `main`, not from CI. The deploy credential (`FLY_API_TOKEN`, or a `flyctl auth login` session) lives only in the operator's local environment — it never touches GitHub Actions. PR code, including code from forks, has no path to a deploy credential because no workflow holds one.

The pre-merge correctness gates that the old CI `deploy` job depended on still run, just decoupled from the deploy step:

1. **Dispatcher pipeline.** Tickets land via the PO → architect → developer → code-review → docs stages, each running `make check` (`go vet` + `go test -race`). Runner/release-adjacent work lands via operator-direct PR.
2. **Final pre-deploy gate.** The operator runs `make check` from the clean checkout before `flyctl deploy --remote-only` — the local equivalent of the old `needs: [test, security, image-scan]` chain.
3. **Daily security scan.** [`security-scan.yml`](../../../.github/workflows/security-scan.yml) keeps `govulncheck` + image-scan firing against `main` on a cron, so disclosed CVEs against unchanged deps still surface within ≤24h.

### Historical note — the removed CI `deploy` job

Before 2026-05-24 this wiring lived in `.github/workflows/ci.yml`'s `deploy` job, which ran `flyctl deploy --remote-only` on push to `main`. Its privilege model kept `FLY_API_TOKEN` away from untrusted code via a branch gate (`github.ref == 'refs/heads/main'`), a `needs: [test, security, image-scan]` chain, and job-level `permissions: contents: read` with the token flowing only as `env:` on the deploy step; `superfly/flyctl-actions/setup-flyctl` was SHA-pinned with a `# Tracks:` comment. That entire workflow was removed across the org (commit [`0b987e2`](https://github.com/pyrycode/pyrycode-relay/commit/0b987e2)); the GHA-token privilege model is moot now that the token never enters CI. Only [`security-scan.yml`](../../../.github/workflows/security-scan.yml) remains in `.github/workflows/`.

### Why `--remote-only`

Fly's remote builder rebuilds the image from `Dockerfile` on every deploy rather than reusing a locally-built or scan-built image. Reusing the `image-scan` artifact (#68) would have required either pushing to GHCR (regressing that job's `contents: read` posture) or an artifact tarball handoff — both deferred. Layer-cache dedup makes the rebuild cost small in steady state.

## Rollback

Two paths in `docs/deploy.md`, both operator-driven:

1. **By image digest (preferred).** `flyctl releases list` → `flyctl deploy --image <prior-digest> --remote-only`. Autocert cache persists across rollbacks (it's on the volume) — no Let's Encrypt re-issuance triggered.
2. **By release number.** `flyctl releases rollback` rolls back the most recent release. Use when the prior digest isn't to hand.

A rollback does **not** revert the `main` commit. Because deploys are operator-direct, no auto-deploy re-rolls a broken release forward — but the next manual `flyctl deploy --remote-only` rebuilds from `main`'s current HEAD. Revert the offending PR on `main` before the next deploy, or it ships again.

## What this feature deliberately does NOT do

- **No Fly-managed TLS, no Fly HTTP proxy.** Both would break autocert and silently change the rate-limiter's view of the peer IP.
- **No platform-level enforcement of single-machine cap.** Fly has no ceiling knob; operator discipline + the `PYRYCODE_RELAY_SINGLE_INSTANCE` self-check (#65) is the load-bearing combination.
- **No deploy-approval gate.** Deploys are operator-direct; the gate is operator discipline (clean `main` + `make check` before `flyctl deploy`), not a CI approval step.
- **No multi-region / multi-instance scaling.** Blocked on the in-process-registry constraint; separate prerequisite ticket.
- **No image-reuse from the scan job.** Always `--remote-only` rebuild; privilege-minimisation over rebuild-time.

## Cross-links

- [Ticket #38 codebase notes](../codebase/38.md) — what landed for the initial Fly wiring.
- [Ticket #97 codebase notes](../codebase/97.md) — fly.toml gotchas captured after the 2026-05-24 first-deploy bootstrap (the `[env]` block and per-`[[services]]` `processes = ["app"]` mapping).
- [Ticket #118 codebase notes](../codebase/118.md) — the `internal_port = 8080` TCP health check and the `immediate` → `rolling` deploy-strategy switch.
- [Architect spec](../../specs/architecture/38-fly-deploy-manifest.md) — full design rationale + security review.
- [`docs/deploy.md`](../../deploy.md) — operator-facing bootstrap / steady-state / rollback.
- [`docs/architecture.md` § Hosting](../../architecture.md#hosting) — the decision record.
- [Feature: Docker image](docker-image.md) — the portable artifact this manifest wires.
- [Feature: Autocert TLS](autocert-tls.md) — the in-binary TLS termination this substrate is shaped around.
- [Feature: PROXY protocol v2 on the HTTPS listener](proxy-protocol-listener.md) — the `proxy_proto` handler on `443` and `--https-proxy-protocol`, so the rate limiter sees the real client IP through TCP passthrough.
- [ADR-0002](../decisions/0002-autocert-explicit-failure-on-port-80.md) — the `:80` 404 fallback the TCP-passthrough services preserve.
- [Threat model](../../threat-model.md) — § *Deploy security* (substrate shifts from VPS to Fly account; threat-model update flagged as follow-up).
