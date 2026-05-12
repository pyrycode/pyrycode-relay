# Deploy

The relay deploys to a single [Fly.io](https://fly.io) machine. CI deploys on every merge to
`main`; manual deploys are needed only for the one-time bootstrap and for
rollbacks.

See [`docs/architecture.md` § *Hosting*](architecture.md#hosting) for the
hosting + TLS-termination decision record, and
[`docs/threat-model.md` § *Deploy security*](threat-model.md#deploy-security--vps-compromise)
for the operational threat surface the substrate sits on.

## One-time bootstrap (per environment)

Done once per Fly app — typically only the production app.

1. Edit `fly.toml`: replace `__REGION__` with a Fly region code (e.g.
   `ams`, `arn`, `fra` — see `flyctl platform regions`) and `__DOMAIN__`
   with the public domain (e.g. `relay.pyrycode.dev`). These are
   placeholders by design — the first deploy fails loudly if either is
   left unset, which is preferable to a silently-misconfigured production
   relay.
2. `flyctl apps create pyrycode-relay` (must match `app =` in `fly.toml`).
3. `flyctl ips allocate-v4 --app pyrycode-relay` — a dedicated IPv4 is
   **required**, not optional, for autocert's HTTP-01 challenge to
   resolve deterministically to the running machine on port 80. Shared
   IPv4 + TCP passthrough is not a supported combination on Fly. This is
   billable; call it out at provisioning review.
4. `flyctl volumes create relay_autocert --region <region> --size 1
   --app pyrycode-relay` — the volume name must match `source =` in the
   `[[mounts]]` block of `fly.toml`. The autocert cache lives here and
   survives machine recycles, avoiding repeated Let's Encrypt
   re-issuance.
5. DNS: point the production domain (A record) at the IPv4 from step 3.
   Let's Encrypt resolves the domain via HTTP-01 on first deploy;
   without DNS in place, the first WSS request hangs ~minutes while
   autocert retries.
6. GitHub repo secret: `FLY_API_TOKEN` = output of `flyctl auth token`.
   Settings → Secrets and variables → Actions → New repository secret.
   The token grants deploy access to the entire Fly org — scope it to a
   `pyrycode-relay`-only deploy token if Fly's tokens UI offers that at
   bootstrap time.

## Steady-state flow

1. Open a PR. CI runs `test`, `security`, and `image-scan` on the PR HEAD.
2. Merge to `main`. CI re-runs the three jobs against `main`, then runs
   `deploy` (gated on all three passing).
3. `deploy` invokes `flyctl deploy --remote-only`. Fly's remote builder
   rebuilds the image from `Dockerfile` and replaces the single machine
   in place via the `immediate` deploy strategy.

Observability:

- `flyctl status` — machine health.
- `flyctl logs --app pyrycode-relay` — relay stderr in real time.
- The deploy job's GitHub Actions log records the build + roll output.

## Rollback

Two paths, in increasing order of disruption.

1. **By image digest (preferred).** `flyctl releases list` shows recent
   release digests. `flyctl deploy --image <prior-digest> --remote-only`
   pins to the prior image without rebuilding. The autocert cache
   persists across rollbacks (it's on the volume), so no Let's Encrypt
   re-issuance is triggered.
2. **By release number.** `flyctl releases rollback` rolls back the
   *most recent* release. Use when the prior release's digest isn't to
   hand and a rollback is needed immediately.

A rollback does **not** revert the `main` commit. To prevent CI's next
deploy from immediately re-rolling the broken release forward, either
revert the offending PR before the next merge to `main`, or disable the
`deploy` job temporarily by editing `.github/workflows/ci.yml` on a
revert PR.
