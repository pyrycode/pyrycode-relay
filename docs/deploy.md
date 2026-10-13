# Deploy

The relay deploys to a single [Fly.io](https://fly.io) machine. **Every deploy is
operator-run. Nothing deploys on merge.** The repo has no deploy workflow at all —
only `security-scan.yml` and the dependency graph — so a merged fix stays out of
production until somebody runs `flyctl deploy` by hand. See
[*Steady-state flow*](#steady-state-flow) below for the procedure.

> **This has bitten us.** The 2026-08-03 CVE patch (#122) merged and then sat
> undeployed while the live machine kept serving a 2026-07-03 build, because the
> daily security scan went green and read as "handled". The repository scans
> report what `main` *would* ship. The separate `deployed-image-scan` result now
> reports the immutable digest observed on started Fly Machines, with its UTC
> observation time in the discovery log and any finding body. Check that result
> after the deploy: a green repository result or an older deployed observation
> cannot establish that production has the patch. Verify the running artifact per
> [*Verify a deploy*](#verify-a-deploy).

See [`docs/architecture.md` § *Hosting*](architecture.md#hosting) for the
hosting + TLS-termination decision record, and
[`docs/threat-model.md` § *Deploy security*](threat-model.md#deploy-security--vps-compromise)
for the operational threat surface the substrate sits on.

## One-time bootstrap (per environment)

Done once per Fly app — typically only the production app.

1. `fly.toml` is checked in with the production values — `primary_region
   = "arn"` and the public domain (`pyrycode-relay.pyryco.de`) in the
   `[processes]` argv — so the production app needs no edit. When porting
   to a new app or environment, treat both as placeholders: replace the
   region with your own code (see `flyctl platform regions`) and the
   domain with the new app's public domain before the first deploy.
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

The operator authenticates `flyctl` locally (`flyctl auth login`); the
deploy token lives only in the operator's local environment. GitHub
Actions uses a separate read-only scan credential; it holds no deploy
credential.

### Daily security scan credential

Provision this for an existing production app as well as a new bootstrap.
Create a dedicated read-only token with an explicit expiry:

```sh
flyctl tokens create readonly --org <owning-org> --name relay-security-scan --expiry <duration>
```

Replace the placeholders with the app's owning organization and a finite
duration, for example `720h` (30 days); record the expiry and rotate before it.
See Fly's [token creation reference](https://docs.fly.io/flyctl/cmd/fly_tokens_create_readonly).
This token reads the organization and its resources, including other apps,
as documented in [org-scoped tokens](https://docs.fly.io/security/tokens/#org-scoped-tokens).
It cannot deploy or mutate resources. Do not substitute a deploy or personal
token if authentication fails.

Store the token as the **GitHub repository Actions secret** `FLY_READONLY_TOKEN`
in `pyrycode/pyrycode-relay`, rather than as a Fly runtime secret. The deployed
scan receives it only during discovery/download on scheduled or manually
dispatched runs on `main`; Trivy scans an archive after temporary registry
credentials have been removed. The [privilege model](knowledge/features/fly-deploy.md#deploy-privilege-model--operator-direct)
records the isolation details.

Run `security-scan` with `workflow_dispatch` selecting **main** after adding
the secret. Confirm `deployed-image-scan` can discover the started Machine,
download its digest and complete Trivy scanning; read the logged digest and UTC
observation time. This initial live access check is operator follow-up; offline
fixture checks do not establish real Machines or registry access. Until
provisioning, the job intentionally fails and files or deduplicates
`security-scan: scanner failed with no parseable findings`.

To rotate, create another read-only token with an explicit expiry, replace
`FLY_READONLY_TOKEN`, and repeat the manual run on `main`. Once access is
confirmed, revoke the old token in the owning organization's Tokens page (or
with `flyctl tokens revoke <token ID>`). Keep the expiry reminder current.

## Secrets

- **`PYRYCODE_RELAY_DAEMON_ADMISSION_MODE`** — unset defaults to `off`;
  otherwise exactly `off`, `observe` or `enforce`. Off skips credential checks;
  observe records aggregate decisions while allowing legacy admission. Only
  enforce blocks anonymous daemon identity claims and daemon-originated wakes.
- **`PYRYCODE_RELAY_DAEMON_ADMISSION_CREDENTIALS`** — private JSON object
  mapping exact server-ids to arrays of accepted tokens. Unset means an empty
  mapping in off/observe; enforce requires a supplied mapping. `{}` is valid
  and denies all; an identity with `[]` admits nobody. Each token must be
  exactly 64 lowercase hex characters representing 32 cryptographically random
  bytes. Bind each token to one exact identity; no aliasing or normalization.
  Duplicate decoded identity keys, cross-identity token reuse and malformed
  values fail startup even in off. See [configuration validation](knowledge/features/env-config-validator.md#daemon-admission-configuration).

Privately generate distinct tokens and provision the daemon's private
`relay_admission_tokens` configuration and Relay's runtime secret mapping
through trusted operator channels. Follow the [shared storage and admission contract](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#daemon-admission-declared).
Keep real tokens, maps and raw headers out of shell history, logs, GitHub,
checked-in files and public verification evidence. Checked-in examples and test
fixtures must be explicitly synthetic; never deploy their predictable tokens.
Credentials enter no URL, QR payload, routing envelope or application frame.

Mode and mapping are immutable startup snapshots. Provisioning changes apply
only when Relay restarts; daemon token selection also requires daemon restart.
A Relay restart closes existing connections and discards claims/grace state.
Coordinate secret changes with the [activation checklist](#daemon-admission-activation).

- **`PYRYCODE_RELAY_FCM_CREDENTIALS`** — optional. Holds the *entire* Google
  service-account JSON key the relay uses to send FCM push wakes (see
  [Push wake dispatch](knowledge/features/push-wake-dispatch.md)). Unset
  means push is off: the relay starts normally, sends nothing, and logs
  once at startup (`push off: PYRYCODE_RELAY_FCM_CREDENTIALS unset`). Set
  it with:

  ```sh
  fly secrets set PYRYCODE_RELAY_FCM_CREDENTIALS="$(cat key.json)" -a pyrycode-relay
  ```

  Setting a Fly secret triggers a machine restart, which drops every
  connected binary the same way a deploy does (see
  [*Steady-state flow*](#steady-state-flow) below); binaries reconnect on
  their own. A malformed value fails `CheckEnvConfig` at boot with
  value-withheld error text, so a bad key refuses to start rather than
  running with push silently broken.

## fly.toml gotchas

Manifest requirements and resource limits to account for when editing the
deployment (first bootstrap, re-cutover, or porting to a new app).

1. **`[env] PYRYCODE_RELAY_SINGLE_INSTANCE = "1"` is required.** Fly's
   substrate exposes `FLY_APP_NAME`, which the #65 self-check reads as
   a multi-instance-capable platform; without this assertion the binary
   refuses to boot. See
   [`docs/architecture.md` § *The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass*](architecture.md#the-pyrycode_relay_single_instance-bypass)
   for the rationale.
   The assertion does not count Machines. Disabled autostart prevents
   automatic starts, not creation; these settings enforce no maximum of one.
   Verify actual inventory with `flyctl machine list -a pyrycode-relay`
   before and after rehearsals and production activation, preserving exactly
   one active relay.
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
4. **`proxy_proto` on the `443` port and `--https-proxy-protocol` must
   move together — in both directions.** The `443` `[[services.ports]]`
   block carries `handlers = ["proxy_proto"]` /
   `proxy_proto_options = { version = "v2" }`, and `[processes] app`
   carries `--https-proxy-protocol`. If only one changes, `443` fails
   **closed**: handler on / flag off feeds the PROXY header to the TLS
   handshake as garbage; flag on / handler off means every connection
   lacks a header the listener now requires, so every connection is
   refused. Editing either side of this pairing (including a rollback,
   see below) means editing both in the same deploy. See
   [PROXY protocol listener](knowledge/features/proxy-protocol-listener.md)
   ([#110](knowledge/codebase/110.md)) for why the relay needs this at
   all — Fly's TCP passthrough hands the relay its own edge-proxy address
   as the socket peer, not the client's.
5. **Public HTTP headers have a 16 KiB allowance on the 256 MB machine.**
   The binary sets `MaxHeaderBytes: 16 * 1024` on the insecure, autocert
   HTTPS and HTTP-01 servers (#181); this is fixed in `run`, with no
   `fly.toml` setting or flag. It reduces each incomplete request's header
   allowance from Go's 1 MiB default. The allowance includes the request
   line and headers, with Go's buffer slack; concurrent accepted sockets
   are bounded separately below. The 5s header timeout remains in place. See
   [DoS resistance](threat-model.md#dos-resistance--connection-floods-slow-loris-fork-bomb-retry)
   for the limits and residual exposure. Apply the change by deploying
   from clean `main` after merge using the flow below.

6. **Defaults on the 256 MB machine: 8 upgraded handlers per source,
   20 globally, 32 accepted sockets per public listener.**
   `--max-listener-connections` defaults to 32 independently for HTTPS and
   HTTP-01 (or the alternative insecure listener). It wraps TCP acceptance
   before PROXY/TLS/HTTP parsing and retains slots through keep-alive and
   WebSocket hijacking until socket close. At capacity, peers wait outside
   acceptance; successful TCP dial does not mean admission. `/healthz`
   consumes raw slots; loopback metrics remains separate. The existing
   `--max-connections` default of 20 governs upgraded sessions, while
   `--max-connections-per-source` defaults to 8 shared across both upgrade
   endpoints. Eight allows a daemon, six phones and a desktop behind one
   NAT; all clients using that source share the allowance. The source gate
   runs after HTTP headers, so one source can still saturate raw slots.
   None of these defaults requires an argv change.

   All three values must be positive; the effective source allowance must
   also be strictly below the global cap (`0 < source < global`). Invalid
   configuration exits 2 before listeners or workers start, including an
   omitted source default of 8 when the global cap is reduced to 8 or below.
   Explicitly lower the source allowance in that case, for example
   `--max-connections=4 --max-connections-per-source=2`. A global cap of 1
   admits no valid positive source allowance. Raw capacity is independent
   of this coupled validation rule.

   Both raw-TCP services (8080/external 80 and 8443/external 443) also set
   `[services.concurrency]` with `type = "connections"` and `hard_limit = 32`
   (#183), matching the raw listener default; `soft_limit` is left unset.
   Fly [stops forwarding new traffic](https://docs.fly.io/reference/configuration#servicesconcurrency)
   to a Machine at or above the service limit, without guaranteeing
   immediate client drops. Keep both service limits aligned when changing
   `--max-listener-connections` or its default, and repeat the isolated
   forwarding check below. TCP passthrough, 443 PROXY v2 and the 8080 TCP
   health check remain in place.

   The combined 256 MB sizing estimate is 2 × 32 raw slots × ~128 KiB
   (16 KiB headers + 4 KiB Go slack, TLS/PROXY/bufio buffers and stacks) =
   8 MiB, plus 20 × ~4.4 MiB upgraded state = 88 MiB. The upgraded sockets
   are already within the raw slots. About 96 MiB live state × 2 for
   `GOGC=100`, plus 56 MiB runtime/autocert/kernel reserve, totals ~248 MiB.
   This conservatively overlaps some transport storage and is an estimate,
   not a memory guarantee. Raising the global or raw cap changes the budget.
   Complete the isolated non-production check in [Verify a deploy](#verify-a-deploy)
   before production deployment; merging does not perform that check.

7. **Explicit restart recovery for `app`.** `[[restart]]` sets
   `processes = ["app"]` and `policy = "always"`, with no `retries` setting
   (#199). Fly attempts recovery after the main process exits regardless
   of exit code, including a clean exit; see the
   [restart reference](https://docs.fly.io/reference/configuration#the-restart-section).
   This replaces implicit `on-failure` recovery with its retry ceiling.
   Offline manifest validation does not establish the effective Machine
   policy or live recovery. Complete the operator checks below before
   production deployment.

8. **Explicit complete-process stop allowance.** Top-level
   `kill_signal = "SIGTERM"` and integer `kill_timeout = 30` (#200) allow
   five seconds above the conservative 25-second process budget: 10s for
   `runServers`' HTTP/WebSocket drain, 10s for subsequent concurrent push
   cancellation/authentication join, and 5s for local limiter joins,
   scheduling and exit. The ten-second drain alone does not cover deferred
   cleanup. Neither the measured fast regression exit nor the two-second
   push unit-test assertion replaces that budget. No production deadline
   changed. Fly's [stop allowance](https://docs.fly.io/reference/configuration/#runtime-options)
   is best-effort (maximum 300s); shorter stopping times remain possible.
   Recheck the full call path and retain at least five seconds of margin
   when changing shutdown behavior. See [graceful shutdown](knowledge/features/graceful-shutdown.md#contract).

## Steady-state flow

Deploys are **operator-direct from a clean `main`** since 2026-05-24,
when the convenience CI workflow (`.github/workflows/ci.yml`) was
removed across the org (commit
[`0b987e2`](https://github.com/pyrycode/pyrycode-relay/commit/0b987e2)).
Pre-merge correctness is handled by the dispatcher pipeline (PO →
architect → developer → code-review → docs stages, each running
`make check` = `go vet + go test -race`); the daily
[`security-scan.yml`](../.github/workflows/security-scan.yml) workflow
keeps `govulncheck` + `image-scan` firing against `main` and independently
scans the digest observed on started Fly Machines. No GHA workflow
auto-deploys on push.

1. Land changes on `main` (dispatcher pipeline for tickets;
   operator-direct PR for runner/release-adjacent or other
   pipeline-incompatible work).
2. From a clean local checkout of `main`, run `make check` for the
   final pre-deploy gate. Tag the commit and push the tag
   (`git tag -a vX.Y.Z -m "pyrycode-relay vX.Y.Z" && git push origin
   vX.Y.Z`). Before deploying, register a finite UTC planned-release
   interval in the watcher's `maintenance` configuration, covering the
   intended release window; see [Restart incident monitoring](#restart-incident-monitoring).
   This suppresses stopped-state notices only during that interval;
   unexpected automatic restarts still count and report. Installation and
   delivered verification remain pending until the checklist below is complete.
   Then deploy with the tag stamped into the binary:
   `flyctl deploy --remote-only -a pyrycode-relay --build-arg
   VERSION=vX.Y.Z`. Without `--build-arg` the image falls back to
   `VERSION=dev` and the running relay cannot say which build it is.
   Fly's remote builder rebuilds the image from `Dockerfile` and replaces the single machine in place via the
   `rolling` deploy strategy, which waits for the machine to pass its
   `internal_port = 8080` TCP check before finishing — a relay that
   refuses to boot now fails the deploy instead of silently leaving
   production down. `flyctl deploy` exits non-zero if the check never
   passes; see [`docs/knowledge/features/fly-deploy.md` § *Deploy
   health check*](knowledge/features/fly-deploy.md#deploy-health-check--tcp-on-8080-rolling-waits-for-it-118)
   for why the check is TCP on 8080 and not an HTTP probe of
   `/healthz`.
3. Verify post-deploy: `flyctl status -a pyrycode-relay` (machine
   `started`), `curl -sS https://pyrycode-relay.pyryco.de/healthz`
   (`200`, and `version` matches the tag you deployed), and a tail of
   `flyctl logs -a pyrycode-relay` for any startup-time errors.
4. Publish the GitHub release for the tag (`gh release create vX.Y.Z
   --verify-tag`), listing the changes since the previous release and
   the rollback target.

Deploy history: releases are listed on the repo's
[Releases page](https://github.com/pyrycode/pyrycode-relay/releases),
starting with `v0.1.0` (2026-09-24). Earlier deploys were untagged: the
live instance was redeployed 2026-07-03 from commit
[`67ffa46`](https://github.com/pyrycode/pyrycode-relay/commit/67ffa46),
picking up the #108 text-frames fix; before that it ran the initial
2026-05-29 deploy.

Observability:

- `flyctl status -a pyrycode-relay` — machine health.
- `flyctl logs -a pyrycode-relay` — relay stderr in real time.
- Local deploy output records the build + roll progress (the GHA
  Actions log no longer exists for deploys).
- The [restart watcher](#restart-incident-monitoring), once installed and
  verified, reports lifecycle incidents to Pyry; scheduler diagnostics
  identify monitoring or delivery failures separately.

### Daemon admission activation

**Pending operator work:** manual deployment, private provisioning, production
mode activation and authorized/anonymous reconnect verification. Repository
checks and merge establish neither deployment nor live enforcement. Shared
push attempt budgets (#172) apply in every admission mode once deployed; they
do not activate enforce mode. Each relay restart resets the in-memory budgets.
Use the
[shared rollout contract](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#daemon-admission-declared)
and preserve exactly one active relay through each restart.

1. Ship the daemon companion and Relay implementation with Relay **off**.
   Deploy Relay manually from clean `main` through the flow above. Confirm
   intended daemon versions support credential delivery and terminal 401 handling.
2. Privately provision distinct cryptographically random tokens for every exact
   intended identity, putting each selected token in that daemon's private
   configuration and the accepted array in Relay's runtime secret mapping.
   Restart daemons to load their selected credentials. Do not publish the mapping.
3. Set Relay to **observe** and restart. Reconnect every intended daemon under
   operator control and correlate each reconnect with aggregate
   `pyrycode_relay_daemon_admission_total{outcome}` deltas on private metrics.
   Confirm `valid` for all intended identities; resolve missing/malformed,
   unknown and mismatch outcomes before enforcement. Aggregate counts have no
   identity labels and do not prove coverage by themselves; use private
   provisioning records and controlled reconnects. A valid outcome means the
   gate accepted the credential, not that upgrade/claim succeeded.
4. Explicitly set **enforce** and restart Relay. Verify authorized reconnects,
   forwarding and wakes when push is configured. Verify an anonymous daemon
   upgrade attempt that reaches the gate receives HTTP 401,
   `Cache-Control: no-store` and exactly `daemon admission rejected\n`, without
   an upgrade, claim, probe, grace reservation or wake. Existing rate/source/global
   limits can respond first;
   a 429/503 is not admission-rejection evidence. Record non-secret results
   and actual inventory, with exactly one active relay.

**Rotation:** privately add a fresh token alongside the old token in the same
identity's Relay array, then restart Relay to load the overlap. Update the
daemon's selected token and restart that daemon; confirm its authorized
reconnect. Remove the old token and restart Relay again. Removing a token or
whole identity revokes it only after that restart, which also drops the holder's
existing connection and its claims/grace state. Confirm revoked rejection and
remaining authorized reconnects.

**Rollback:** setting off or observe and restarting Relay restores anonymous
admission and removes the enforcement guarantee. These modes still reject
malformed supplied startup configuration; correct provisioning before restarting.
An image rollback also needs review of effective admission mode/configuration.

**Terminal daemon 401:** the first admission 401 stops automatic retries for
that daemon relay leg while preserving local daemon service. Correct private
provisioning/mode as needed, then have the operator restart the daemon to retry.
Changing Relay provisioning alone does not restart that terminal daemon leg.

## Restart incident monitoring

[`operations/relay_health.py`](../operations/relay_health.py) is a standalone
standard-library Python tool (#198). Each invocation polls one explicitly
selected Fly app/Machine and exits; the operator owns installation, scheduling,
credential rotation and delivery. It stores operational history outside the
relay process. [#170](https://github.com/pyrycode/pyrycode-relay/issues/170)
owns secure-exchange, certificate, capacity and push-outcome monitoring.

Install the reviewed `operations/relay_health.py` under
`/home/pyry/pyry-workspace/crons/relay-health/` on pyrybox. Use a directory owned
by the scheduler's UID with mode `0700`; keep `config.json`, `fly-readonly.token`
and state files at `0600`, with a `0700` state directory. The tool requires
current-UID ownership, exact modes and no final-component symlinks for private
inputs/storage, and refuses insecure existing files rather than repairing them.
Keep parent directories and the script under trusted operator control.

Provision a **dedicated expiring org-scoped read-only token**, separate from
deploy credentials and the CI scan secret:

```sh
flyctl tokens create readonly --org <owning-org> --name relay-health --expiry <duration>
```

Choose a finite duration, record expiry and arrange rotation before it. Store
the value only in the private local token file, never in config, cron arguments,
logs, GitHub or relay runtime secrets. Its read scope includes other apps and
resources in the organization; selecting one Machine does not narrow that scope.
Rotate by provisioning another read-only token, replacing the private file,
verifying access and revoking the old token through the operator's Fly login.
Expired/failed authentication is a monitoring failure; never substitute a deploy
or personal token. See [Fly token provisioning](#daily-security-scan-credential).

Example private `config.json` (confirm the Machine against current inventory):

```json
{
  "app": "pyrycode-relay",
  "machine": "7817426a000438",
  "state_dir": "/home/pyry/pyry-workspace/crons/relay-health/state",
  "token_file": "/home/pyry/pyry-workspace/crons/relay-health/fly-readonly.token",
  "notify": ["pyry", "channel", "post", "--name", "Pyrybox board watch", "--file", "{message_file}"]
}
```

The Machine above was observed started in `arn` on 2026-10-13; it is historical
inventory, not automatic discovery. Select the actual Machine explicitly, not
one distinct image digest. After replacement, update `machine` and verify the
new identity and access. State is scoped to `<app>-<machine>.json`; retain the
old Machine's evidence separately. A new scope starts with the current
ten-minute window and does not inherit the old incident or recovery progress.
Verify actual production inventory and preserve exactly one active relay.

Run once per minute as that UID, with a verified Python 3 interpreter and
`PATH`/`HOME` suitable for the existing assistant-channel command. For example,
the operator's crontab can use:

```cron
* * * * * /usr/bin/python3 -B /home/pyry/pyry-workspace/crons/relay-health/relay_health.py /home/pyry/pyry-workspace/crons/relay-health/config.json
```

Arrange visibility of scheduler exit status and stderr; a silent scheduler
cannot establish monitoring coverage. A nonblocking directory lock covers
load, poll, notification and save; overlapping invocations fail safely.
`notify` is trusted argv, executed without a shell, with exactly one standalone
`{message_file}` argument. It configures
`pyry channel post --name "Pyrybox board watch" --file <message-file>`.
The watcher creates a `0600` message file in the private state directory and
removes it after the command returns or fails. The child has a 15s timeout,
discarded stdout/stderr and only `PATH`, `HOME`, `USER`, `LANG`, `LC_ALL` from
the environment; it receives no Fly credential. Verify channel access under
this scheduler environment. Messages name app/Machine, UTC observation/window,
reason, automatic restart count and Pyry as response owner.

### Incident and recovery semantics

Five distinct automatic exits in `(observation UTC minus ten minutes,
observation UTC]` latch an incident; a valid `stopped` observation outside
maintenance also latches one. Only normalized `exit` evidence with
`restarting: true` and `requested_stop: false` counts. Omitted booleans default
to false and omitted exit/signal codes to zero. Four exits, older exits, first
boot and ordinary `started` observations do not trigger an incident.

Both `request.exit_event` and `request.MonitorEvent.exit_event` normalize to
the same evidence. Identity uses nanosecond `exited_at`; window counting uses
its millisecond time, so late retrieval uses exit time rather than retrieval
time. Duplicate/out-of-order wrappers and repeated polls count that exit once.
Duplicates must agree on normalized exit time, `restarting`, `requested_stop`,
`oom_killed`, `exit_code`, `guest_exit_code`, `guest_signal` and `signal`, including
across saved polls. Conflicting evidence fails monitoring even if both records
derive the same crash classification.

For a planned release, add optional `maintenance` with finite UTC `start` and
`end` strings ending in `Z`, for example
`{"start":"2026-10-13T18:00:00Z","end":"2026-10-13T18:15:00Z"}`; replace these
example times with the actual window and require `start < end`. Suppression is
start-inclusive/end-exclusive and applies only to stopped observations while
active. Requested stops never count as automatic crashes; unrequested automatic
restarts still count during maintenance. Expiry cannot suppress a later stopped
observation. Omit `maintenance` when no interval is needed.

Version **2** state persists normalized evidence, proven coverage, the last
successful observation, recovery progress and incident/delivery state through
atomic replacement with file/directory fsync. Version 1 lacks the required
evidence and is rejected unchanged; there is no automatic migration. Investigate
incompatible state without discarding an ongoing incident's evidence.
The incident is saved before notification. Failed delivery remains pending and
is retried on a later valid poll, even after the Machine recovers. Once sending
and saving `delivered` both succeed, the ongoing incident sends no further notice,
including a restart-to-stopped transition. Interruption between send and durable
save can cause a repeat delivery.

Rearm requires ten continuous minutes of successful observations showing
`started` with fewer than five current-window automatic exits, after delivery.
A failed poll, nonhealthy observation, duplicate observation time or gap greater
than 90s interrupts recovery; pending delivery cannot be cleared by recovery.
A later incident after rearming gets a new notice.

### Coverage, bounds and local checks

Each poll makes exactly one read-only HTTPS GET to the selected Machine at
`api.machines.dev`, with no redirects or retries, a 10s socket timeout, 20s
request deadline and 1 MiB response cap. Local input/state sizes and event rows
are bounded too. The tool does not paginate history or remediate production.
History must reach the window boundary, retain a launch covering a newly
created Machine's lifetime, or overlap previously proven persisted coverage.
Persisted overlap additionally requires the earliest incoming platform timestamp
to reach the prior successful observation. A duplicate exit with a later wrapper
timestamp cannot bridge a history gap; a local marker is not a pagination cursor.

Authentication, API, schema, conflicting evidence, incomplete history, clock
rollback, private-storage and delivery failures exit `2` with only
`relay monitoring failure; incident delivery or monitoring coverage unavailable`.
This is distinct from a relay incident, not proof of a crash or healthy recovery.
Polling failures preserve prior history and incident/delivery state and reset
recovery where safely writable; incompatible state remains unchanged. Messages
and diagnostics exclude credentials, relay payloads, raw API configuration and
remote/subprocess error text. Absence of an alert proves neither delivery nor
healthy coverage.

From the repository root, run the offline hermetic check:

```sh
python3 -B -m unittest discover -s operations -p 'test_relay_health.py'
```

It uses controlled UTC time, fake API/notification behavior and temporary private
storage, with no Fly access or real posts. `make check` remains the Go gate.

## Restart incident response

Pyry receives notices in **Pyrybox board watch**, investigates read-only and
escalates to Juhana when operator action is needed. Compare the notice's UTC
observation/window and app/Machine with current read-only lifecycle observations;
check requested-stop evidence, maintenance, repeated exits and stopped state.
Record non-secret event references, current state and delivery time, and distinguish
a monitoring-failure diagnostic from a confirmed relay incident. For monitoring
failures, inspect scheduler execution, private-file access, credential expiry,
API/history coverage and channel access without copying tokens or raw error text
into the channel. A pending notification needs successful delivery, not a state reset.

Escalation should give Juhana the identity, times, restart count/state, safe evidence
and the operator action needed. Monitoring grants no production mutation authority;
restart, stop, deploy, scaling or volume recovery requires separate operator
authorization. Follow [App restart recovery](#app-restart-recovery) and the
[#197 recovery/stop and certificate-snapshot handoff](https://github.com/pyrycode/pyrycode-relay/issues/197#issuecomment-6148485681).
Its complete-shutdown and certificate-snapshot procedures are owned by
[#200](https://github.com/pyrycode/pyrycode-relay/issues/200); use those procedures
when available rather than improvising recovery or restoring over the live cache.

All #198 operator follow-ups remain **pending**; merge and hermetic tests complete none:

- [ ] Install/configure the private pyrybox home/state, expiring read-only credential,
  actual Machine identity, one-minute schedule and assistant-channel command.
- [ ] In isolation, deliver five-unplanned-restart and stopped-state notices to
  **Pyrybox board watch**. Record planned-release exclusion, repeated-poll/watcher-restart
  deduplication and later-incident rearming, with delivery UTC and non-secret
  app/Machine/event references.
- [ ] Verify monitoring failures are distinguishable, Pyry's read-only response is
  usable and escalation reaches Juhana; remediation needs separate authorization.
- [ ] Activate production monitoring only after delivered verification and read-only
  access/current inventory evidence. Coordinate with #197's recovery work and
  preserve exactly one active production relay; keep rehearsals isolated.

## Verify a deploy

### Complete-process shutdown

The #200 operator checklist remains **pending**; local tests and merge
complete none of these operational checkboxes. Use an isolated Fly
deployment with separate domains, clients and synthetic credentials, and
never route rehearsal traffic through production.

- [ ] Under admitted binary/phone traffic and held synthetic authentication,
  send real SIGTERM. Record both peers' close codes (1001), whole-process
  exit time, authentication cancellation time, one OAuth/no FCM request,
  six `token_fetch_failed` outcomes and joined authentication cleanup.
  Inspect `flyctl machine status <machine-id> -d -a <isolated-app>` for
  effective `config.kill_signal`, `config.kill_timeout` and `app` restart
  policy, and compare the observed lifecycle to the 25s budget/30s allowance.
  Socket cancellation alone is insufficient evidence of cleanup completion.
- [ ] Discover a current certificate snapshot and restore to a new encrypted
  isolated volume using [Certificate snapshot recovery](#certificate-snapshot-recovery).
  Record access, source/snapshot/destination IDs, steps, successful cache
  recovery without key disclosure and elapsed recovery time.
- [ ] Before and after both rehearsals and production activation, record actual
  production Machine inventory (`flyctl machine list -a pyrycode-relay`),
  IDs and states; preserve exactly one active production relay.
- [ ] After isolated shutdown and [#199's eleven-crash recovery checks](#app-restart-recovery)
  pass, deploy from clean `main` using the [steady-state flow](#steady-state-flow).
  Verify the production Machine's effective `app`/`always` restart policy,
  SIGTERM/30s stop settings and image digest; retain UTC observations and
  results. Capture the prior immutable image for [rollback](#rollback).
  Alert installation/delivery remains [#198](https://github.com/pyrycode/pyrycode-relay/issues/198).

### App restart recovery

The #199 live checks remain **pending for the operator**. On an isolated
non-production Fly app with separate domain, cache, credentials and clients,
deploy the intended image and manifest before testing recovery:

- Read the effective Machine config with `flyctl machine status <machine-id>
  -d -a <isolated-app>` and confirm `config.restart.policy = "always"` for
  the `app` process group. Inspect the returned retry fields as well;
  `max_retries` governs `on-failure`, while `always` recovers regardless of
  exit code. See [Machine restart policies](https://docs.fly.io/machines/guides-examples/machine-restart-policy/).
- Cause at least eleven consecutive main-process crashes within the former
  five-minute retry window. Confirm automatic recovery after each crash,
  including the eleventh, without manual starts, redeploys or policy changes
  between crashes. Also verify automatic recovery after a clean process exit.
  An explicit Machine restart is not evidence of crash recovery.
- Record lifecycle evidence for deliberate Machine stop/start and a rolling
  redeploy, including state transitions, restored health and client
  reconnection. Confirm these operator actions still complete as intended.
  Retain UTC timestamps, Machine ID, image digest, effective config and
  exit/restart events alongside the recovery observations.

After these recovery checks and the [isolated shutdown check](#complete-process-shutdown)
pass, manually deploy from clean `main` using the
[steady-state flow](#steady-state-flow). Preserve exactly one active
production relay throughout recovery and deployment; do not add a production
replica for testing. Re-read the production Machine's effective restart
policy after deployment and retain that evidence. Offline validation and a
merge complete none of these live checks. Alert installation and delivery
remain [#198](https://github.com/pyrycode/pyrycode-relay/issues/198).

### Connection limits and memory

Before deploying the per-source upgraded allowance, raw listener admission
or Fly forwarding ceilings to production, the operator must verify the
combined memory estimate and saturation/recovery behavior on an
**isolated non-production Fly app**,
using the intended image, 256 MB machine size,
`GOGC=100` and production listener/PROXY wiring. Keep its domain, cache,
credentials and test clients separate from production. The pipeline's local
socket tests do not substitute for this operational check.

- With raw and global capacity available, hold eight upgraded sessions
  from one source across `/v1/server` and `/v1/client`. Confirm a ninth
  attempt on either route receives HTTP 503 with a newline-only body and
  one `source_conn_cap_reached` Warn carrying the quoted source. From a
  different source, verify an upgrade still succeeds. Close a held phone
  session, allow its handler cleanup to finish, then verify the first source can
  reconnect and route a frame. Keep attempts within the rate-limit budget
  so 429 does not mask source admission; verify the 503s leave rate-denial
  counters unchanged. Check `/healthz` and loopback metrics while source
  capacity is full but raw capacity is available. Record the image, source
  addresses, effective source/global/raw limits and refusal/admission/recovery
  evidence. This check must pass before production deployment.
- Confirm the deployed config has `type = "connections"` and
  `hard_limit = 32` on both services, matching the image's independent
  `--max-listener-connections` default of 32. For each service, establish
  32 forwarded connections and keep them open long enough to observe
  saturation; account for phase and idle timeouts. Use Fly connection
  observations together with machine-side socket or traffic observations
  to verify additional attempts are not forwarded to the full service.
  Lack of an HTTP/TLS response alone cannot distinguish Fly withholding
  forwarding from the relay waiting to accept a queued socket. Close a
  held connection and verify forwarding resumes and a new peer receives
  service. Record both saturation and recovery evidence for each service;
  do not require an immediate client drop or infer forwarding from a
  successful TCP dial. This check must pass before production deployment.
- Fill both public listeners to the configured raw cap, including stalled
  pre-header peers, keep-alive sockets and the 20 upgraded sessions within
  those raw slots. Exercise the upgraded worst case with bounded phone
  queues ([budget derivation](knowledge/features/connection-cap.md#default-derivation)).
  Observe machine memory and Go heap/GC behavior over sustained saturation
  and churn; check for OOM kills, restarts and adequate headroom.
- Check that additional peers receive no HTTP/TLS service before a slot
  frees, then recover after close or phase timeout. TCP dial success alone
  proves neither admission nor available memory. Check HTTP-01 saturation
  leaves HTTPS upgrades and loopback metrics accessible, and that a freed
  HTTP-01 slot serves a cached challenge or the non-challenge `404`.
  `/healthz` may wait when its public listener is full.
- Stop the isolated relay at saturation with a queued peer and verify the
  listeners close, registered WebSockets receive `1001`, and shutdown stays
  within the 10s drain budget. Record the image, caps, load and memory
  observations. Adjust caps or machine size and repeat if headroom is
  insufficient; proceed to production only after the check passes.

This verification and deployment are operator-owned and were not performed
by the ticket pipeline. After production deployment, identify the running
artifact as follows.

### Running artifact

Answering "is the fix actually live?" requires identifying the running artifact.
Start with two independent readings, which should agree:

1. `flyctl status -a pyrycode-relay` — the `LAST UPDATED` column is when the
   running machine was last replaced. If it predates your merge, the fix is not
   live no matter how green everything else looks.
2. `curl -sS https://pyrycode-relay.pyryco.de/healthz` — the `uptime_seconds`
   field independently dates the running process. Convert it and check it lands
   on the same moment as the machine's `LAST UPDATED`.

On 2026-08-05 those two agreed to the minute on a build 33 days old, which is
what established that the CVE patch had never shipped.

Since `v0.1.0` there is a third, direct reading: the `version` field in the
same health payload carries the release tag, set by `--build-arg VERSION=…` at
deploy time. It reads `dev` when a deploy left the build arg out, as every
deploy before 2026-09-24 did, so `dev` says the stamp is missing, not which
build is running.

After deploying, manually dispatch
[`security-scan`](../.github/workflows/security-scan.yml) on **main** and inspect
the **`deployed-image-scan` job**, alongside the repository results. Its
`discover started image` log records
`registry.fly.io/pyrycode-relay@sha256:…` and the UTC observation time; compare
the digest with the deployed artifact and ensure the observation follows your
deploy. Any deployed finding body repeats those values and links the run.
Successful discovery alone is insufficient: the download and scan must also
complete. A passing deployed job means no fixable HIGH/CRITICAL finding under
the configured policy for that observed digest, not that it is free of all
vulnerabilities. The daily 06:00 UTC run provides the same result, but a run
before a later deploy describes only its earlier observation. See
[CI image scanning](knowledge/features/docker-image.md#ci-image-scanning) for
the distinction between Go-binary version matches and source call-path evidence.

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

Before activation, capture the currently running immutable image digest
from the Machine's detailed status and retain it with the release evidence.
The #200 inventory observed this rollback target; refresh it if the running
image has changed:

```text
registry.fly.io/pyrycode-relay@sha256:cf2f4d979e111157ca776f771bcd2df0ea1ff7cac24a355763047249d9cde241
```

Roll back with `flyctl deploy --image <previous-immutable-image-digest>
--remote-only -a pyrycode-relay`. This pins the previous artifact without
rebuilding. The autocert cache persists on its volume across image rollbacks.
Retain the explicit SIGTERM/30s settings and #199's `app`/`always` restart
policy, verify their effective Machine values and image after rollback,
and record actual inventory with exactly one active relay.

A rollback does **not** revert the `main` commit. Because deploys are
operator-direct, there is no auto-deploy that would re-roll a broken
release forward — but the next manual `flyctl deploy --remote-only`
WILL rebuild from `main`'s current HEAD. Revert the offending PR on
`main` before the next deploy, or you'll re-ship the bad change.

An image rollback does **not** edit the checked-in `fly.toml`; review its
compatibility with the selected image. A rollback to a pre-#110 image while
the manifest
still carries `handlers = ["proxy_proto"]` on `443` fails `443` closed (the
older binary has no `--https-proxy-protocol` flag and never opens the
proxyproto-wrapped listener, so the PROXY header hits its TLS handshake as
garbage). Rolling back across the #110 boundary means also reverting
`fly.toml`'s `443` port block and `[processes] app` line to their pre-#110
state, then deploying that combination — see the *fly.toml gotchas* item
above.


## Production capacity headroom

The production manifest selects 64 total live WebSocket connections, 48 per source address, and 128 raw connections per public listener. Fly forwarding has the same 128-connection limit on each listener. Upgrade requests refill at one attempt per 10 ms with a burst of 200 per source. The binary defaults remain 20 total, eight per source, 32 raw per listener, one attempt per six seconds, and burst 20.

The operator has kept the production allocation at 256 MiB. These higher allowances give normal live suites room to connect, but do not guarantee that fully occupied connections with full queues fit in that allocation. The selected later allocation is 1024 MiB. The existing conservative budget estimates 4.4 MiB per upgraded session and 128 KiB per raw slot. Charging both full listeners and all upgraded sessions, then allowing twice that live state for garbage collection and the 56 MiB runtime reserve, gives about 684 MiB. The later allocation is a sizing plan, not a completed resize or an unbounded-load guarantee.

Validate changed limits with isolated concurrent fixtures before deployment. Confirm the deployed command, memory, both Fly listener limits and a single active machine after deployment. Retain the previous image digest and machine configuration for rollback.

## Certificate snapshot recovery

This rehearsal is **pending**, using the operator's local Fly login
(`flyctl auth login`), not the CI read-only scan credential. Follow
[Fly's restore procedure](https://docs.fly.io/volumes/volume-manage/).
The source observed for #200 is `vol_rkggyj5jel8o0xk4`, mounted at
`/var/lib/relay/autocert`. Confirm its current identity and provisioned size
with `fly volumes show vol_rkggyj5jel8o0xk4 --app pyrycode-relay`, then
refresh discovery:

```sh
fly volumes snapshots list vol_rkggyj5jel8o0xk4 --app pyrycode-relay
```

The inventory observed `vs_ov9G6nK906KS2JD4ZZJ11Y`, created
2026-10-12T14:50:17Z with five-day retention. It expires; do not pin it in a
recovery command. Select a current completed snapshot and record its creation
time and retention. Use the source's provisioned volume size, not the
snapshot's compressed stored size, for the destination size.

Create a new volume with a distinct recovery name and size at least as large
as the source and at least 1 GiB:

```sh
fly volumes create <recovery-volume-name> --snapshot-id <current-snapshot-id> --size <size-in-GiB> --region <recovery-region> --app <recovery-app>
```

[Volume creation](https://docs.fly.io/flyctl/cmd/fly_volumes_create)
encrypts contents by default; do not use `--no-encryption`, and verify
`Encrypted: true` in the destination metadata. Attach only the new destination
volume to an isolated recovery Machine with no production traffic, DNS or
clients. Never replace or mount over the live volume, and never start a
second active production relay. A restored cache contains production private
keys even when the recovery Machine is isolated.

Preserve cache ownership and permissions for the relay UID (`65532` in the
image), with directory mode `0700` and private cache files. Validate successful
cache recovery in isolation without printing file contents or certificate
keys and without sending ACME requests for the production domain. Record
local-login access success (no token), source/snapshot/destination and recovery
Machine IDs, sizes/encryption, commands, isolation controls, permission and
cache-read results, start/end UTC times and elapsed recovery time. Snapshot
discovery alone does not complete the recovery checkbox.
