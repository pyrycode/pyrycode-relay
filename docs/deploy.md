# Deploy

The relay deploys to a single [Fly.io](https://fly.io) machine. **Every deploy is
operator-run. Nothing deploys on merge.** The repo has no deploy workflow at all —
only `security-scan.yml` and the dependency graph — so a merged fix stays out of
production until somebody runs `flyctl deploy` by hand. See
[*Steady-state flow*](#steady-state-flow) below for the procedure.

> **This has bitten us.** The 2026-08-03 CVE patch (#122) merged and then sat
> undeployed while the live machine kept serving a 2026-07-03 build, because the
> daily security scan went green and read as "handled". That scan builds a fresh
> image from `main` and inspects **that**, so it reports what *would* ship and
> never what is serving. A green scan is not evidence that production is patched.
> After any merge you care about, verify the **running** artifact per
> [*Verify a deploy*](#verify-a-deploy).

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

## fly.toml gotchas

Non-obvious requirements that surface as `flyctl deploy` failures or
boot-time errors. Both are encoded in the checked-in `fly.toml`; the
notes here exist so an operator editing the manifest (first bootstrap,
re-cutover, porting to a new app) doesn't trip them again.

1. **`[env] PYRYCODE_RELAY_SINGLE_INSTANCE = "1"` is required.** Fly's
   substrate exposes `FLY_APP_NAME`, which the #65 self-check reads as
   a multi-instance-capable platform; without this assertion the binary
   refuses to boot. See
   [`docs/architecture.md` § *The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass*](architecture.md#the-pyrycode_relay_single_instance-bypass)
   for the rationale.
2. **`processes = ["app"]` on each `[[services]]` block.** Fly's schema
   requires every service to name its process whenever `[processes]` is
   defined, and `[processes]` is always defined here because the
   distroless image has no shell to expand env vars into argv — the
   relay receives `--domain` / `--cert-cache` via the `[processes]`
   argv string.
3. **High-port pattern: bind `:8080`/`:8443` internally; Fly forwards
   external `:80`/`:443`.** The distroless `:nonroot` Dockerfile runs the
   binary as uid `65532`, which can't bind privileged ports (`<1024`) on
   Linux without `CAP_NET_BIND_SERVICE`. The relay's `--http-listen` and
   `--https-listen` flags (added in
   [#96](https://github.com/pyrycode/pyrycode-relay/issues/96)) take
   operator-supplied addresses; the checked-in `fly.toml` passes
   `--http-listen=:8080 --https-listen=:8443` in the `[processes]` argv,
   and the matching `[[services]].internal_port` values let Fly's TCP
   passthrough forward:
   - `[[services]] internal_port = 8080` ← external `port = 80`
     (ACME HTTP-01 challenge listener + ADR-0002 `404` fallback for
     non-challenge requests).
   - `[[services]] internal_port = 8443` ← external `port = 443`
     (autocert TLS terminator; cert lives in the binary, not at Fly's
     edge).

   The external ports must stay at `80` / `443` so Let's Encrypt's HTTP-01
   reaches autocert on the standard challenge port of the public IP, and
   clients reach TLS on the standard HTTPS port. The flags default to
   `:80` / `:443` for substrates that grant the bind capability (root,
   K8s with `NET_BIND_SERVICE`, systemd `AmbientCapabilities`); pass the
   override on any nonroot substrate. The full rationale, mutex with
   `--insecure-listen`, and listener-allowlist interaction live in
   [`docs/specs/architecture/96-autocert-configurable-listener-addrs.md`](specs/architecture/96-autocert-configurable-listener-addrs.md).

## Steady-state flow

Deploys are **operator-direct from a clean `main`** since 2026-05-24,
when the convenience CI workflow (`.github/workflows/ci.yml`) was
removed across the org (commit
[`0b987e2`](https://github.com/pyrycode/pyrycode-relay/commit/0b987e2)).
Pre-merge correctness is handled by the dispatcher pipeline (PO →
architect → developer → code-review → docs stages, each running
`make check` = `go vet + go test -race`); the daily
[`security-scan.yml`](../.github/workflows/security-scan.yml) workflow
keeps the `govulncheck` + image-scan guards firing against `main`. No
GHA workflow auto-deploys on push.

1. Land changes on `main` (dispatcher pipeline for tickets;
   operator-direct PR for runner/release-adjacent or other
   pipeline-incompatible work).
2. From a clean local checkout of `main`, run `make check` for the
   final pre-deploy gate, then `flyctl deploy --remote-only -a
   pyrycode-relay`. Fly's remote builder rebuilds the image from
   `Dockerfile` and replaces the single machine in place via the
   `immediate` deploy strategy.
3. Verify post-deploy: `flyctl status -a pyrycode-relay` (machine
   `started`), `curl -sS https://pyrycode-relay.pyryco.de/healthz`
   (`200`), and a tail of `flyctl logs -a pyrycode-relay` for any
   startup-time errors.

Observability:

- `flyctl status -a pyrycode-relay` — machine health.
- `flyctl logs -a pyrycode-relay` — relay stderr in real time.
- Local deploy output records the build + roll progress (the GHA
  Actions log no longer exists for deploys).

## Verify a deploy

Answering "is the fix actually live?" means asking the **running** machine, not
the repo and not CI. Two independent readings, and they should agree:

1. `flyctl status -a pyrycode-relay` — the `LAST UPDATED` column is when the
   running machine was last replaced. If it predates your merge, the fix is not
   live no matter how green everything else looks.
2. `curl -sS https://pyrycode-relay.pyryco.de/healthz` — the `uptime_seconds`
   field independently dates the running process. Convert it and check it lands
   on the same moment as the machine's `LAST UPDATED`.

On 2026-08-05 those two agreed to the minute on a build 33 days old, which is
what established that the CVE patch had never shipped. Note `version` in the
health payload is the build-stamp string and reads `dev` on every deploy, so it
distinguishes nothing — do not use it as a version check.

Two flyctl gotchas on the operator MacBook, both of which look like something
worse than they are:

- `flyctl` is installed at `~/.fly/bin/flyctl` and is **not on `PATH`**, so
  `which flyctl` comes back empty and reads as "not installed". Call it by full
  path.
- In a non-interactive shell it does not pick up the stored login and fails with
  `no access token available`, which reads as an expired session. The credential
  is fine, in `~/.fly/config.yml`; pass it through for the one command:
  `FLY_ACCESS_TOKEN="$(sed -n 's/^access_token: *//p' ~/.fly/config.yml | tr -d '"')"`.

Restarting the relay drops every connected binary. They reconnect on their own
through the daemon's backoff ladder — measured at 13 s for two binaries on
2026-08-05 — so a deploy is disruptive but self-healing.

## Rollback

Two paths, in increasing order of disruption.

1. **By image digest (preferred).** `flyctl releases list -a
   pyrycode-relay` shows recent release digests. `flyctl deploy --image
   <prior-digest> --remote-only -a pyrycode-relay` pins to the prior
   image without rebuilding. The autocert cache persists across
   rollbacks (it's on the volume), so no Let's Encrypt re-issuance is
   triggered.
2. **By release number.** `flyctl releases rollback -a pyrycode-relay`
   rolls back the *most recent* release. Use when the prior release's
   digest isn't to hand and a rollback is needed immediately.

A rollback does **not** revert the `main` commit. Because deploys are
operator-direct, there is no auto-deploy that would re-roll a broken
release forward — but the next manual `flyctl deploy --remote-only`
WILL rebuild from `main`'s current HEAD. Revert the offending PR on
`main` before the next deploy, or you'll re-ship the bad change.
