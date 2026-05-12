# Spec: host-specific deploy manifest — Fly.io + CI auto-deploy (#38)

## Files to read first

- `Dockerfile:46` — `VOLUME ["/var/lib/relay/autocert"]`; the destination path the Fly volume mount must match.
- `Dockerfile:53` — `EXPOSE 80 443`; the two ports `[[services]]` blocks in `fly.toml` must publish.
- `Dockerfile:55` — `ENTRYPOINT ["/pyrycode-relay"]`; the relay is invoked directly, with no shell wrapper. There is no `sh` or `bash` in the runtime image — env-var substitution into argv is not available; flag values are passed as literal strings via Fly's `[processes]` block.
- `cmd/pyrycode-relay/main.go:23-43` — the required-flag check (`--domain` xor `--insecure-listen`). The manifest invokes the `--domain` arm; `--insecure-listen` is irrelevant on Fly (TLS terminates here).
- `cmd/pyrycode-relay/main.go:76-117` — autocert wiring: HTTP-01 challenge on `:80` (`httpSrv`), WSS on `:443` (`httpsSrv`). Both listeners are mandatory in `--domain` mode; the fly.toml must publish both with TCP passthrough.
- `cmd/pyrycode-relay/main.go:120-125` — `defaultCertCache()` returns `$HOME/.pyrycode-relay/certs`. Inside the container that's `/home/nonroot/.pyrycode-relay/certs` (a writeable scratch path under the nonroot user's home) — **not** the volume mount. The manifest must pass `--cert-cache /var/lib/relay/autocert` explicitly so autocert writes into the persistent volume.
- `.github/workflows/ci.yml` (entire file, 85 lines) — the workflow the deploy job is appended to. Note the existing `permissions: contents: read` at workflow level, the `actions/checkout@v6` / `actions/setup-go@v6` major-version pins (a Renovate-managed convention this repo uses for first-party actions), and the explicit-SHA pin on the Trivy action (the convention for third-party actions).
- `docs/specs/architecture/68-trivy-image-scan.md` § *Pinning the action* — the SHA-pin + `# Tracks:` comment convention `setup-flyctl` must follow.
- `docs/specs/architecture/32-dockerfile-base-hardening.md` § *On `.dockerignore`*, § *Stage 2: runtime* — confirms the image is distroless (no shell), so the manifest cannot rely on `sh -c` env substitution.
- `docs/architecture.md` § *Single-instance constraint (v1)* (lines 33-63) — the single-machine cap is **load-bearing for correctness**, not a cost optimisation. The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass is the documented escape hatch; the manifest is not allowed to set it.
- `docs/threat-model.md` § *Deploy security — VPS compromise* and § *Cert & key handling* — the operational threat-model entries this ticket lands defence-in-depth on (managed-host substrate replacing bare-VPS; persistent volume backing the autocert cache the `0700` check enforces on).
- `docs/knowledge/decisions/0002-autocert-explicit-failure-on-port-80.md` — the `:80` 404 fallback (rather than a 302 → `:443` redirect) means port 80 carries non-trivial application logic the manifest must not strip. Confirms the fly.toml services block must not insert Fly's HTTP handler in front of `:80`.
- `README.md` § *Build* and § *Run* — the docs to cross-link from the new `docs/deploy.md`.

(Codegraph context was queried (`task = "Fly.io deploy manifest…"`) and confirmed no Go code is touched. Falling back to direct file reads above for the docs/CI surfaces codegraph does not parse.)

## Context

The relay has had a portable Docker image since #32, but no host wiring. Without a manifest, every release is a manual `flyctl deploy` from a developer's laptop — non-reviewable, non-repeatable, and gated on whoever holds the Fly API token locally. This ticket lands the thin wrapper that tells Fly how to run the existing image, plus the CI job that re-applies the manifest on every merge to `main`.

Two prior decisions ([#38 operator comment](https://github.com/pyrycode/pyrycode-relay/issues/38#issuecomment-4433395643)) constrain the design:

1. **Host: Fly.io.** Picked over Hetzner Cloud / Railway. The remaining design space is the fly.toml shape.
2. **TLS terminates in the relay.** Autocert (#9, shipped) stays the cert holder; Fly is a TCP-passthrough substrate. This rules out Fly's default HTTP proxy and Fly-managed certificates; it requires a dedicated IPv4 so Let's Encrypt's HTTP-01 challenge resolves deterministically to the running machine.

The single-machine cap is not a cost optimisation — it is a correctness constraint. `internal/relay/registry.go` is in-process; two machines would hold two disjoint registries and silently drop traffic across the split. `docs/architecture.md` already documents this. The manifest must surface the constraint at the platform level (declarative `min_machines_running` / scale guardrails) so that a future operator running `flyctl scale count 2` is at least a deliberate act, not an autoscaler accident.

## Design

### Scope — files touched

1. **`fly.toml`** (new, repo root) — the Fly Apps v2 manifest.
2. **`.github/workflows/ci.yml`** (edit) — one new `deploy` job appended after `image-scan`.
3. **`docs/deploy.md`** (new) — bootstrap, steady-state, and rollback procedures.
4. **`docs/architecture.md`** (edit) — single-line hosting + TLS-termination decision under a new § *Hosting* heading, between § *Single-instance constraint (v1)* and § *Threat model*. (AC #5 offered `docs/PROJECT-MEMORY.md` or `docs/architecture.md`; the architect "Never Update" rule excludes PROJECT-MEMORY, so architecture.md is the right home. AC #5 carved an explicit exception for this single edit; documenting the choice of file here avoids the developer having to re-litigate it.)

No Go production code. No test code. No new types or interfaces.

### `fly.toml` shape

```toml
# Fly.io manifest for pyrycode-relay.
#
# Decisions encoded here (see docs/architecture.md § Hosting):
#   - Fly is a TCP-passthrough substrate; TLS terminates in the relay
#     binary via autocert (#9). NO Fly HTTP proxy or Fly-managed certs.
#   - Port 80 carries ACME HTTP-01 challenge traffic and the explicit
#     404 fallback for non-challenge requests (ADR-0002). Both arrive at
#     the relay verbatim — Fly must not insert any HTTP handler.
#   - Single-machine hard cap. The relay's connection registry is
#     in-process; multi-instance silently routes phones to the wrong
#     replica. See docs/architecture.md § Single-instance constraint.

app = "pyrycode-relay"
primary_region = "<REGION>"   # operator-chosen at bootstrap; see docs/deploy.md.

[build]
  dockerfile = "Dockerfile"

# --domain and --cert-cache passed as literal argv (distroless has no
# shell; env-var expansion into argv is not available). A domain change
# is a one-line edit to this file — config, not code — and rides the
# next deploy.
[processes]
  app = "--domain <DOMAIN> --cert-cache /var/lib/relay/autocert"

[[mounts]]
  source = "relay_autocert"
  destination = "/var/lib/relay/autocert"
  # initial_size omitted; default is sufficient for autocert's
  # account-key + per-domain cert (≤ a few KiB). Resize is operator-side.

# Raw TCP services. NO `handlers = ["http"]` / `handlers = ["tls"]` —
# either would terminate at Fly's edge and break autocert.
[[services]]
  protocol = "tcp"
  internal_port = 80
  auto_stop_machines = "off"
  auto_start_machines = false
  min_machines_running = 1

  [[services.ports]]
    port = 80
    # handlers = [] — explicit no-op. Fly's default for an unset handlers
    # list IS pass-through, but the explicit empty list is reviewable
    # against a future Fly default change that might add an implicit "http".

[[services]]
  protocol = "tcp"
  internal_port = 443
  auto_stop_machines = "off"
  auto_start_machines = false
  min_machines_running = 1

  [[services.ports]]
    port = 443
    # handlers = [] — see :80 block above. Crucially NOT ["tls"], which
    # would steal cert handling from autocert and require a Fly-managed
    # cert.

[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"

[deploy]
  # Single machine — rolling/canary are inapplicable. immediate replaces
  # in place; brief drop in availability during deploy is acceptable for
  # this binary (clients reconnect; no state to drain).
  strategy = "immediate"
  # max_unavailable = 1 documents intent even though it's redundant with
  # a 1-machine fleet.
  max_unavailable = 1
```

**Placeholders the developer fills at bootstrap time (NOT in this ticket's PR):**

- `<REGION>` — e.g. `ams`, `arn`, `fra`. Operator picks at `flyctl apps create`.
- `<DOMAIN>` — e.g. `relay.pyrycode.dev`. The actual production domain.

For the initial commit landing this ticket: ship `fly.toml` with `<REGION>` and `<DOMAIN>` set to operator-supplied real values, OR keep them as recognisable placeholders (`__REGION__` / `__DOMAIN__`) and have the developer set them via a follow-up commit before CI's first deploy run. Spec recommendation: ship with real values — a placeholder that escapes review is the failure mode (#9-style: a silently-misconfigured cert dir is worse than a loudly missing one). The first CI deploy then succeeds end-to-end.

#### `max_machines = 1` — what AC #1 actually asks for

The AC names `max_machines = 1` as a fly.toml key. As of the current Fly Apps v2 generation, **there is no `max_machines` key in fly.toml** — the hard cap is enforced via the combination of:

- `[[services]]` `min_machines_running = 1` (don't drop below one),
- `auto_start_machines = false` (don't let Fly create new machines on demand),
- `auto_stop_machines = "off"` (don't stop the one machine),
- `[deploy] strategy = "immediate"` (don't create a second machine during deploy for blue/green),
- operator discipline at `flyctl scale count` (no platform-level "ceiling" knob exists).

Spec choice: encode all four declarative knobs above (they are what the AC's *intent* requires), and add a comment block at the top of `fly.toml` calling out that `flyctl scale count > 1` would violate the single-instance constraint. The platform itself does not enforce the ceiling. The runtime self-check from #65 (`PYRYCODE_RELAY_SINGLE_INSTANCE` bypass) is the in-binary backstop; the manifest does **not** set the bypass.

The developer should verify the exact stanza names against current Fly docs (`https://fly.io/docs/reference/configuration/`) at implementation time and `flyctl validate` the resulting file before commit. If Fly has introduced a `max_machines` key by then, set it. Don't invent a key that errors out at deploy.

### CI deploy job

Appended to `.github/workflows/ci.yml`, after `image-scan`. Sketch:

```yaml
  deploy:
    # Runs only on push to main. PRs (including forks) never trigger this
    # job, structurally preventing FLY_API_TOKEN exposure to untrusted code.
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    needs: [test, security, image-scan]
    runs-on: ubuntu-latest
    permissions:
      contents: read
    # No environment: protection — gating is via branch (main is
    # protected) and the fact that PRs from forks cannot reach this job.
    # If a deploy-approval step is wanted later, an `environment:` block
    # with required reviewers is the lever; deferred (out of scope).
    steps:
      - uses: actions/checkout@v6
      # Tracks: superfly/flyctl-actions/setup-flyctl@v1.5
      # Pinned by commit SHA so a tag-swap upstream cannot change what
      # holds FLY_API_TOKEN during the next deploy. Refresh the comment
      # in lockstep with the SHA. Same convention as the Trivy pin in
      # image-scan (#68) and govulncheck pin (#41).
      - uses: superfly/flyctl-actions/setup-flyctl@<SHA-AT-IMPL-TIME>
      - name: flyctl deploy
        run: flyctl deploy --remote-only
        env:
          FLY_API_TOKEN: ${{ secrets.FLY_API_TOKEN }}
```

Three structural defences against the AC's "untrusted PR code" concern:

1. **Branch gate (`if:`).** `pull_request` events never satisfy the condition. Fork PRs cannot mutate `main`, so they cannot reach this job by any path other than the maintainer merging — at which point the code is no longer untrusted.
2. **`needs:` chain.** `test` + `security` + `image-scan` must pass. A malicious PR that slips through review still has to pass the existing gates before deploy runs.
3. **`permissions: contents: read`.** Locks the job down to the workflow-level baseline. `FLY_API_TOKEN` is the only privileged input; it's accessed via `${{ secrets.… }}` (the standard pattern) and is not exposed to any earlier job.

#### Why `--remote-only`

The AC pins `flyctl deploy --remote-only`. Fly's remote builder rebuilds the image from `Dockerfile` against the pushed commit. The image-scan job's locally-built image is **not** reused — exporting it cross-job would require an artifact upload + `packages:write` on the scanner job (regressing #68's `contents: read` posture) or a push to GHCR (same regression). The rebuild cost is paid once per deploy (~minutes on Fly's builders, cached aggressively across deploys via layer dedup); the privilege-minimisation win is permanent. The issue body's "Deploy should reuse that same artifact" is aspirational; the AC's pinned command is the load-bearing constraint and is what the spec implements.

Reusing the scan-time image to deploy the bit-identical bytes (instead of rebuilding) is filed as a follow-up under § *Open questions* — it requires GHCR wiring that is well out of scope here.

#### `setup-flyctl` pin

`superfly/flyctl-actions/setup-flyctl` ships a managed `flyctl` binary; the SHA pin is on the **action**, not on `flyctl` itself. The action will install whichever `flyctl` version it bundles. If a specific `flyctl` version is required (it isn't today — `flyctl deploy --remote-only` is stable across recent releases), it's pinnable via a `with: version: …` input on the action.

Developer step at implementation time: visit `https://github.com/superfly/flyctl-actions`, pick the latest tagged release (currently `v1.5` at time of writing), record its commit SHA via `git ls-remote https://github.com/superfly/flyctl-actions refs/tags/v1.5`, and paste into the pin with a matching `# Tracks:` comment. Renovate keeps this fresh thereafter, same as the Trivy pin.

### `docs/deploy.md` shape

~50 lines of prose split into three sections:

```markdown
# Deploy

The relay deploys to a single Fly.io machine. CI deploys on every merge to
`main`; manual deploys are needed only for the one-time bootstrap and for
rollbacks.

## One-time bootstrap (per environment)

Done once per Fly app — typically only the production app.

1. `flyctl apps create pyrycode-relay` (matches `app =` in `fly.toml`).
2. `flyctl ips allocate-v4 --app pyrycode-relay` — a dedicated IPv4 is
   **required**, not optional, for the autocert HTTP-01 challenge to
   resolve deterministically to the running machine on port 80. Shared
   IPv4 + TCP passthrough is not a supported combination on Fly. Billable;
   call it out at provisioning review.
3. `flyctl volumes create relay_autocert --region <region> --size 1
   --app pyrycode-relay` (matches `source =` in `fly.toml [[mounts]]`).
4. DNS: point `<domain>` (A record) at the IPv4 from step 2. Let's
   Encrypt resolves the domain via HTTP-01 on first deploy; without DNS,
   the relay's first WSS request hangs ~minutes while autocert retries.
5. GitHub repo secret: `FLY_API_TOKEN` = `flyctl auth token` output.
   Settings → Secrets and variables → Actions → New repository secret.
   The token holds deploy access to the entire Fly org — scope it to a
   `pyrycode-relay`-only deploy token if Fly's tokens UI offers that
   today.

## Steady-state flow

1. Open a PR. CI runs `test`, `security`, `image-scan` on the PR HEAD.
2. Merge to `main`. CI re-runs the three jobs against `main`, then runs
   `deploy` (which is gated on all three passing).
3. `deploy` invokes `flyctl deploy --remote-only`. Fly's remote builder
   rebuilds the image from `Dockerfile` and rolls the single machine to
   the new image in place.

Logs: `flyctl logs --app pyrycode-relay`. Status: `flyctl status`.

## Rollback

Two paths, in increasing order of disruption.

1. **By image digest (preferred).** `flyctl releases list` shows recent
   release digests. `flyctl deploy --image <prior-digest> --remote-only`
   pins to the prior image without rebuilding. The autocert cache
   persists across rollbacks (it's on the volume); no LE re-issuance is
   triggered.
2. **By release number.** `flyctl releases rollback` rolls back the
   *most recent* release. Available when a rollback is needed
   immediately and the digest of the prior release isn't to hand.

A rollback does NOT revert the `main` commit. To prevent CI's next
deploy from immediately re-rolling the broken release forward,
either revert the offending PR before the next merge to `main` or
disable the `deploy` job temporarily via a workflow_dispatch toggle.
```

The doc cross-links to `docs/architecture.md` § *Hosting* (the decision record) and to `docs/threat-model.md` § *Deploy security* (the operational threat surface the deploy substrate sits on).

### `docs/architecture.md` edit

Insert a new heading `## Hosting` between the existing § *Single-instance constraint (v1)* and § *Threat model*. Two-paragraph body:

```markdown
## Hosting

Production deploys to a single **Fly.io** machine in one region. TLS
terminates in the relay binary (autocert, #9) — Fly runs the substrate
in raw-TCP passthrough mode on `:80` and `:443`, with a dedicated IPv4
so Let's Encrypt's HTTP-01 challenge resolves deterministically. The
autocert cache lives on a Fly volume at `/var/lib/relay/autocert`.

The single-machine cap is platform-enforced via `min_machines_running
= 1` and `auto_start_machines = false` in `fly.toml`, and binary-enforced
via the `PYRYCODE_RELAY_SINGLE_INSTANCE` self-check (#65). Multi-instance
scaling is out of scope for v1 — see § *Single-instance constraint*
above.

Bootstrap and rollback procedures: [`docs/deploy.md`](deploy.md). The
manifest itself: [`fly.toml`](../fly.toml). CI deploy job:
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml).
```

This satisfies AC #5 ("a single line lands in PROJECT-MEMORY or equivalent recording the hosting + TLS-termination decision") — the AC permits architecture.md as the equivalent location. The "single line" target is the spirit; the prose above is three short paragraphs because the decision has three load-bearing parts (Fly + relay-TLS + single-machine-enforcement) that all want naming. A literal one-line note ("Host: Fly.io; TLS terminates in relay") would be technically AC-compliant but would force the next cold reader to chase three other docs to reconstruct the call. The AC's intent — *"the next agent reading the repo cold sees the call without scrolling this issue"* — is what this satisfies.

## Concurrency model

Not applicable at the manifest level. The relay's existing concurrency model (per-conn goroutines, `errgroup` fan-out in handlers) is unchanged; the manifest does not introduce new processes or coordination points.

The CI deploy job is single-step and serial; no parallel deploy paths exist.

## Error handling

**Manifest-level failure modes the design accepts:**

- **Deploy fails (Fly builder error, network glitch, FLY_API_TOKEN expired).** The job's step fails loud; the workflow run is red; the operator sees it in the Actions tab and on the GitHub commit. No partial state on the running machine — `flyctl deploy` only swaps the image atomically once the build succeeds.
- **`flyctl validate` would reject the manifest.** Caught at PR time via the `deploy` job's first run after merge — but the AC requires the developer to run `flyctl validate fly.toml` locally before the PR lands. Add to the developer's verification checklist (§ Testing strategy below).
- **Autocert cannot issue cert on first deploy (DNS not pointed, IPv4 not allocated, port 80 unreachable).** The relay logs an autocert error and the first WSS request hangs ~minutes. Caught at bootstrap-time, not at every deploy; the deploy.md procedure orders the steps so DNS + IPv4 are in place before the first deploy runs.
- **Volume not yet created (first deploy bootstrap order skipped).** Fly refuses to start the machine without the named volume. Loud failure, operator intervenes.

**Failure modes the design does NOT introduce:**

- The relay binary's own startup failure modes (`ErrCacheDirInsecure`, autocert mismatch) carry through unchanged. The Fly machine surfaces them as a non-zero exit; Fly retries the machine; eventually the deploy is marked failed and the prior release stays live.

## Testing strategy

No automated tests — the artifact is a YAML manifest + a workflow file + prose docs. Verification is via:

1. **`flyctl validate fly.toml`** (run locally by the developer before commit). Catches typos, unknown keys, syntactically-invalid TOML. Fly's CLI ships this; no extra dependency.
2. **CI workflow lint.** `actionlint` (already not wired into this repo, but trivially runnable: `actionlint .github/workflows/ci.yml`) catches YAML schema errors and pinning convention violations. Optional; the AC does not require it. The developer can run it locally if uncertain about the YAML edit.
3. **First real deploy.** The bootstrap procedure in `docs/deploy.md` IS the end-to-end test. After the PR merges and CI runs `deploy` against `main` for the first time, the developer checks:
   - `flyctl status` shows one machine, running, healthy.
   - `curl -i https://<domain>/healthz` returns `200 ok`.
   - `curl -i http://<domain>/anything-not-an-acme-challenge` returns `404` (ADR-0002).
   - `flyctl ssh console` (if available; distroless has no shell, so this will fail — that's expected and a structural defence-in-depth, not a regression).
   - `flyctl volumes list` shows `relay_autocert` mounted on the machine.

These are AC verification steps for the developer's PR — not test code to land in the repo. There is no test harness for fly.toml or workflow files; the manifest's first deploy is its smoke test.

## Open questions

- **Reuse the image-scan-built image at deploy time.** Currently `--remote-only` rebuilds. Reusing the scanned image requires (a) `image-scan` pushing to GHCR (regresses its `contents: read` → `contents: read, packages: write` permission posture), or (b) `actions/upload-artifact` of a `docker save` tarball + `docker load` + `flyctl deploy --local-only --image …` in `deploy`. Both are deferred. The Fly remote builder's layer cache makes the rebuild cost small in steady state.
- **Region choice (`primary_region`).** Operator decision at bootstrap time. The relay has no opinion — single-instance, no latency-sensitive routing. The developer picks one when filling the `<REGION>` placeholder.
- **Domain in fly.toml vs Fly secret.** Spec ships the domain as a literal in `fly.toml`'s `[processes]` block. Treating the domain as a secret was considered and rejected — the domain is not secret (it's published in DNS), and a literal config value is reviewable in the diff. Changing the domain is a one-line PR; the AC's intent ("a domain change does not require a code edit") is satisfied because TOML is config, not Go source.
- **Deploy approval gate (`environment:` with required reviewers).** Would add a human-in-the-loop click between merge and deploy. Out of scope; can be added later as a one-line `environment: production` addition with reviewer config in GitHub Settings. Not gating this ticket on it.
- **Token scope minimisation.** `flyctl auth token` issues an org-wide deploy token. Fly's recent (2026-Q1) deploy-token UI may now support per-app tokens; the developer should check at bootstrap time and use the narrower token if available. Not a manifest-level concern.

## Security review

**Verdict:** PASS

### Trust boundaries

- **Substrate ↔ binary boundary is unchanged.** Fly is a TCP passthrough on `:80` and `:443`; bytes arrive at the relay binary verbatim, gated by the existing chokepoints (`internal/relay/tls.go` for TLS handshake, `relay.EnforceHost` for SNI/Host mismatch, the WS adapter for frame size). The manifest does not add a new untrusted-data ingress.
- **CI ↔ deploy substrate boundary** is new and narrow: `FLY_API_TOKEN` flows from GitHub Actions secrets to a single step's `env:`, never to `with:` inputs that get logged, never to a file written on the runner. The step is in a job whose `permissions:` is `contents: read` — no `issues: write`, no `packages: write`, no `id-token: write`. A compromised step cannot escalate within GitHub; it can only deploy a bad image to Fly (an action that requires merging code to `main`, which has its own protections).

### Tokens, secrets, credentials

- **`FLY_API_TOKEN`** — held as a GitHub repo secret. Never logged. Never echoed (the step's `run:` block does not `echo` it). The `setup-flyctl` action receives it via `env:`, the standard secure pattern. Rotation: operator-driven (`flyctl auth token` → update repo secret); the relay does not need to know the token rotated.
- **Autocert account key + per-domain cert** — sit on the Fly volume at `/var/lib/relay/autocert`. The relay's existing `0700` permission check (`internal/relay/tls.go:16-55`) gates startup on the directory permissions; this remains the load-bearing control. Fly volumes are per-machine; a compromised Fly org would expose the volume. Threat-model match: same as `docs/threat-model.md` § *Cert & key handling*, residual-risk paragraph (same-UID compromise reads the cache).
- **No new secrets introduced by this ticket.** The domain is config, not secret. The region is config, not secret.

### File operations

- The manifest declares a volume mount; it does not write files. The relay's autocert writes inside the volume; that's covered by the existing tls.go permission check (separate code path, not introduced here).
- No path traversal surface — the volume destination is a constant string in `fly.toml`, not user-controlled.

### Subprocess / external command execution

- The CI deploy step runs `flyctl deploy --remote-only` — a single command with a single flag, no user-controlled interpolation. No `sh -c`, no shell-form `run:` block ambiguity.
- `setup-flyctl` action is pinned by commit SHA (the convention from #41 and #68) — a tag-swap upstream cannot change what code holds the token. `# Tracks:` comment makes a malicious SHA swap reviewable.

### Cryptographic primitives

- Not applicable — manifest contains no crypto. The relay's TLS posture is unchanged (`MinVersion: tls.VersionTLS12`, Go defaults for cipher suites). The substrate change (VPS → Fly) does not move TLS termination.

### Network & I/O

- **Port 80 and 443 published.** Both required (autocert HTTP-01 on `:80`, WSS on `:443`). The manifest's `[[services]]` blocks use `protocol = "tcp"` with no Fly-managed handlers, so Fly inserts no MITM-shaped middleware. ADR-0002's explicit 404 on non-challenge `:80` traffic reaches the public unchanged.
- **Slow-loris / timeout discipline** is unchanged — handled in `cmd/pyrycode-relay/main.go` (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` on both HTTP and HTTPS servers, `:53-95` and `:82-102`).
- **Rate limiting** (#34, shipped) reads the socket peer IP directly. TCP passthrough preserves the real peer IP — the rate limiter sees the actual client address, not a Fly proxy. Validated by the AC's "TLS terminates in the relay" decision; the alternative path (Fly-terminated TLS) would have required the rate limiter to trust `Fly-Client-IP`, which is the security-sensitive follow-up that was explicitly deferred ([§ Out of scope](https://github.com/pyrycode/pyrycode-relay/issues/38)).
- **No new ingress surface.** The manifest publishes only the two ports the binary already binds.

### Error messages, logs, telemetry

- The deploy step's output (`flyctl deploy` log lines) appears in the GitHub Actions run log. `flyctl` does not echo `FLY_API_TOKEN` in its logs. GitHub Actions automatically masks values that match registered secrets even if a step did echo them — defence in depth.
- The relay binary's logging is unchanged. The Fly substrate does not introduce new log producers; `flyctl logs` is just a passthrough to the binary's stderr.

### Concurrency

- Single CI job, sequential steps. No new goroutines in the relay; the manifest doesn't touch Go code.
- The deploy strategy `immediate` replaces the single machine in place — there is no overlap window where two relay instances run simultaneously. The single-instance constraint is preserved across deploys (not just in steady state).

### Threat model alignment

- **`docs/threat-model.md` § *Deploy security — VPS compromise*.** The "operator-owned VPS" assumption is now "operator-owned Fly account." Threat surface shifts from "Linux VPS hardening" (SSH keys, fail2ban, auto-updates) to "Fly account hardening" (org-wide MFA, deploy-token scoping, no shared accounts). Worth a threat-model update; that's a follow-up doc PR, not blocking on this ticket. Flag added to § *Open questions* of `docs/threat-model.md`-equivalent if the developer prefers — but the AC does not require a threat-model edit, so this spec does not mandate one.
- **`docs/threat-model.md` § *Supply chain — Go dependencies*.** Unchanged: `go.mod` is identical, the same `go build` runs on Fly's remote builder.
- **`docs/threat-model.md` § *Cert & key handling*.** Improved: the autocert cache now lives on a persistent Fly volume, surviving machine recycles and reducing the LE-re-issuance churn that the `0700`-check guarantees we don't silently degrade through.
- **Protocol-spec security model** is unchanged — no wire-protocol surface is touched.

### Findings

- [Trust boundaries] No findings — the manifest preserves the single explicit boundary at the binary's TLS terminator and the existing header-gate.
- [Tokens] No findings — `FLY_API_TOKEN` is held only in GitHub secrets, scoped to a job with `contents: read`, never echoed.
- [File operations] No findings — manifest declares, does not write.
- [Subprocess] No findings — `flyctl deploy --remote-only` is a fixed command; action pinned by SHA.
- [Crypto] N/A — no crypto introduced.
- [Network & I/O] No findings — TCP passthrough preserves the real peer IP for #34 rate limiting; no Fly-inserted handler on either port.
- [Errors / logs] No findings — `FLY_API_TOKEN` is masked by GitHub Actions; relay logging unchanged.
- [Concurrency] No findings — single CI job, `immediate` deploy strategy preserves single-instance invariant.
- [Threat model alignment] SHOULD FIX (out of scope for this ticket) — `docs/threat-model.md` § *Deploy security* should be updated to reflect "operator-owned Fly account" alongside (or instead of) "operator-owned VPS." Flag as a follow-up; not gating.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-12
