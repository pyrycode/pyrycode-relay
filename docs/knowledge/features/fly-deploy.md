# Fly.io deploy — production host wiring

The relay's production host is a single [Fly.io](https://fly.io) machine in one region. `fly.toml` at the repo root tells Fly how to run the portable image from #32; `.github/workflows/ci.yml`'s `deploy` job re-applies the manifest on every push to `main`. Operator-facing procedures live in [`docs/deploy.md`](../../deploy.md); the decision record lives in [`docs/architecture.md` § Hosting](../../architecture.md#hosting).

## What it does

- **`fly.toml`** declares a Fly Apps v2 app with TCP passthrough on `:80` + `:443`, a persistent volume at `/var/lib/relay/autocert`, and a single-machine hard cap.
- **CI `deploy` job** runs `flyctl deploy --remote-only` on push to `main`, gated on `test` + `security` + `image-scan` passing. Fly's remote builder rebuilds the image from `Dockerfile` against the merged commit and rolls the single machine in place.
- **Bootstrap is one-time:** `flyctl apps create` → `flyctl ips allocate-v4` → `flyctl volumes create relay_autocert` → DNS → set the `FLY_API_TOKEN` GitHub repo secret → fill `__REGION__` / `__DOMAIN__` placeholders in `fly.toml`.

## Why this shape

### TCP passthrough, not Fly's HTTP proxy

TLS terminates **in the relay** via autocert (#9, shipped). Fly is a raw-TCP substrate; the binary holds the cert. Three load-bearing consequences:

- `[[services]]` blocks use `protocol = "tcp"` with no `handlers = ["http"]` / `handlers = ["tls"]`. An explicit handler list would either steal `:80` from autocert's HTTP-01 listener (breaks cert issuance) or terminate TLS at Fly's edge (bypasses autocert entirely and would force a Fly-managed cert).
- A **dedicated IPv4** is required, not optional. Shared IPv4 + TCP passthrough is not a supported combination on Fly; Let's Encrypt's HTTP-01 challenge needs a deterministic resolution from `<domain>:80` to the running machine. `flyctl ips allocate-v4` is part of the bootstrap.
- The real socket peer IP reaches the relay verbatim. #34's IP rate limiter reads the socket peer; the TCP-passthrough decision is what keeps that working without needing to trust an `X-Forwarded-For` / `Fly-Client-IP` header. Any future move to platform-terminated TLS would require a security-sensitive follow-up against #34.

ADR-0002's `:80` 404 fallback (explicit failure on non-ACME `:80` requests, rather than a 302 → `:443`) means `:80` carries non-trivial application logic. The TCP-passthrough manifest preserves that — Fly inserts no MITM-shaped middleware.

### Single-machine hard cap

The relay's connection registry is **in-process** (`internal/relay/registry.go`); two machines hold two disjoint registries and silently drop phones routed to the "wrong" replica. This is a correctness constraint, not a cost optimisation — `docs/architecture.md § Single-instance constraint (v1)` is canonical.

Fly Apps v2 has **no `max_machines` key**. The cap is encoded via four declarative knobs:

| Knob | Setting | Why |
| --- | --- | --- |
| `[[services]] min_machines_running` | `1` | Don't drop below one |
| `[[services]] auto_start_machines` | `false` | Don't let Fly create new machines on demand |
| `[[services]] auto_stop_machines` | `"off"` | Don't stop the one machine |
| `[deploy] strategy` | `"immediate"` | Don't create a second machine during deploy for blue/green |

Operator discipline at `flyctl scale count` is the platform-level ceiling — Fly itself does not enforce one. The in-binary `PYRYCODE_RELAY_SINGLE_INSTANCE` self-check (#65) is the load-bearing backstop for an operator mistake.

### Argv, not shell

The distroless runtime image has no shell, so `[processes] app = "..."` is treated as **final argv** by Fly, not as a shell line. Env-var expansion (`${DOMAIN}`) is not available; the manifest passes literal values:

```toml
[processes]
  app = "--domain __DOMAIN__ --cert-cache /var/lib/relay/autocert"
```

`--cert-cache /var/lib/relay/autocert` is explicit because the binary's default (`defaultCertCache()` returns `$HOME/.pyrycode-relay/certs`, i.e. `/home/nonroot/.pyrycode-relay/certs`) is **not** the volume mount. Without the flag, autocert would write into an unmounted scratch path and lose the cache on every machine recycle.

### Loud placeholders

`primary_region = "__REGION__"` and `--domain __DOMAIN__` ship as placeholders. The first deploy fails loudly if the operator forgets to fill them — preferable to a plausible-but-wrong real value escaping review (cf. the silently-misconfigured-cert-dir failure mode the autocert work guarded against).

## CI deploy job — privilege model

`deploy` runs in `.github/workflows/ci.yml` after `image-scan`. Three structural defences keep `FLY_API_TOKEN` away from untrusted code:

1. **Branch gate.** `if: github.event_name == 'push' && github.ref == 'refs/heads/main'`. PRs (including from forks) never satisfy this condition.
2. **`needs:` chain.** `test` + `security` + `image-scan` must pass. A bad PR that slips through review still has to pass the existing gates.
3. **Job-level `permissions: contents: read`.** Belt-and-suspenders on the workflow-level header. `FLY_API_TOKEN` flows only as `env:` on the deploy step.

`superfly/flyctl-actions/setup-flyctl` is pinned by commit SHA with a `# Tracks: superfly/flyctl-actions/setup-flyctl@<tag>` comment alongside — same convention as the Trivy pin (#68) and govulncheck pin (#41). A tag-swap upstream cannot change what code holds the token between Renovate bumps.

### Why `--remote-only`, not scan-image reuse

The `image-scan` job (#68) builds the image locally as part of its scan. Reusing it at deploy time would require either:

- Pushing to GHCR — regresses `image-scan`'s `contents: read` posture to `contents: read, packages: write`.
- An `actions/upload-artifact` tarball handoff + `docker load` in `deploy` — adds a new artifact channel and CI complexity.

Both are deferred. Fly's remote builder rebuilds from `Dockerfile` on every deploy; layer-cache dedup makes the rebuild cost small in steady state. The privilege-minimisation win is permanent.

## Rollback

Two paths in `docs/deploy.md`, both operator-driven:

1. **By image digest (preferred).** `flyctl releases list` → `flyctl deploy --image <prior-digest> --remote-only`. Autocert cache persists across rollbacks (it's on the volume) — no Let's Encrypt re-issuance triggered.
2. **By release number.** `flyctl releases rollback` rolls back the most recent release. Use when the prior digest isn't to hand.

A rollback does **not** revert the `main` commit. To prevent CI's next deploy from immediately re-rolling the broken release forward, revert the offending PR before the next merge — or temporarily disable the `deploy` job via a revert PR.

## What this feature deliberately does NOT do

- **No Fly-managed TLS, no Fly HTTP proxy.** Both would break autocert and silently change the rate-limiter's view of the peer IP.
- **No platform-level enforcement of single-machine cap.** Fly has no ceiling knob; operator discipline + the `PYRYCODE_RELAY_SINGLE_INSTANCE` self-check (#65) is the load-bearing combination.
- **No deploy-approval gate.** `environment: production` with required reviewers is a one-line addition for later; not wired in v1.
- **No multi-region / multi-instance scaling.** Blocked on the in-process-registry constraint; separate prerequisite ticket.
- **No image-reuse from the scan job.** Always `--remote-only` rebuild; privilege-minimisation over rebuild-time.

## Cross-links

- [Ticket #38 codebase notes](../codebase/38.md) — what landed in this ticket.
- [Architect spec](../../specs/architecture/38-fly-deploy-manifest.md) — full design rationale + security review.
- [`docs/deploy.md`](../../deploy.md) — operator-facing bootstrap / steady-state / rollback.
- [`docs/architecture.md` § Hosting](../../architecture.md#hosting) — the decision record.
- [Feature: Docker image](docker-image.md) — the portable artifact this manifest wires.
- [Feature: Autocert TLS](autocert-tls.md) — the in-binary TLS termination this substrate is shaped around.
- [ADR-0002](../decisions/0002-autocert-explicit-failure-on-port-80.md) — the `:80` 404 fallback the TCP-passthrough services preserve.
- [Threat model](../../threat-model.md) — § *Deploy security* (substrate shifts from VPS to Fly account; threat-model update flagged as follow-up).
