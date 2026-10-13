# Relay restart incident watcher (#198)

## Files read

- `docs/PROJECT-MEMORY.md` — content blindness, loud configuration failure and ownership conventions.
- `docs/knowledge/INDEX.md`, `docs/knowledge/features/fly-deploy.md` — operator-direct deploys, org-scoped read-only access and one-Machine constraint.
- `docs/architecture.md` (Hosting), `docs/threat-model.md` (Deploy security, Log hygiene, Triggers for re-review), `docs/security-followups.md` — operational boundaries and deferred runtime hardening.
- `.github/workflows/security_scan.py` → `discover`, `machine_reference`, `download`; `test_security_scan.py` → `DeployedScanTests` — CodeGraph reading of fixed HTTPS GETs, capped bodies, secret-free subprocesses and sanitized errors. No migration of this helper.
- `Makefile` → `check` — existing Go gate remains clean; the watcher has its own hermetic command.
- [Machines resource API](https://docs.fly.io/machines/api/machines-resource/), [Fly event types](https://github.com/superfly/fly-go/blob/main/machine_types.go) → `MachineEvent`, `MachineRequest`, `MachineExitEvent` — selected-Machine GET, millisecond timestamps, direct/MonitorEvent exit shapes and omitted false values.
- [Wire security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model) — read for security review; this tool never accesses the relay wire protocol or payloads.
- #171 readiness inventory comment — explicit Machine selection, Pyry delivery owner and pending installation evidence.

## Context

Implement one independently checkable operational capability: restart/stopped incidents with durable deduplication and recovery. Runtime, scanner, deployment and #170 monitoring stay outside scope. No decision record is needed.

Size: approximately 740 written lines (350 implementation, 290 tests, 100 plan), two tool files plus this plan, no exported interfaces or updated consumers, four acceptance criteria, at most eight lifecycle rejection/failure categories. Documentation edits belong to the later stage. #176's helper/tests cost 475 lines; this tool replaces scanning/download wiring with incident state. Recount before committing. Fetched in-flight branches #127 and #167: neither overlaps `operations/relay_health.py` or `operations/test_relay_health.py`.

## Design

- Standard-library `operations/relay_health.py`, invoked once per minute with one private JSON configuration path. Required keys: `app`, `machine`, `state_dir`, `token_file`, `notify` (argv array with exactly one `{message_file}` placeholder); optional `maintenance` has finite UTC `start`/`end`. App/Machine identifiers are shape-checked. The operator supplies a dedicated expiring org-scoped read-only token file; neither token nor endpoint is configurable through API data.
- `fetch(config)` makes exactly one GET to `api.machines.dev/v1/apps/<app>/machines/<machine>`. No redirect following, retries or mutation. Socket timeout 10s, whole request deadline 20s, response cap 1 MiB. Read only Machine identity/state/events, discarding configuration and other fields.
- `normalize(events)` validates events, normalizes both exit shapes with omitted-false defaults, and identifies exits by normalized `exited_at` evidence, independent of wrapper shape, ordering and duplicate records. Conflicting evidence fails monitoring. Count only exit evidence `restarting=true`, `requested_stop=false`; use exit event time, never retrieval time. Non-exit records anchor retained-history continuity, never count as crashes.
- `evaluate(config, state, observation, now)` considers `(now-600s, now]`. Full coverage requires an API event at/before the window boundary, a retained launch covering a newly created Machine's lifetime, or overlap with previously proven persisted coverage. A retrieved/persisted gap within the window fails monitoring. A local marker is never treated as an API cursor. Retain the current window plus one older anchor, and its proven coverage boundary.
- Five automatic exits or an unmaintained `stopped` observation latches one incident, with safe reason/count/UTC window. Maintenance suppresses stopped only at `start <= observation < end`, never automatic exits or requested-stop evidence classification.
- Persist state before delivery. Retry pending delivery on subsequent valid polls, including when the Machine has recovered. A successfully delivered incident suppresses all further reasons until recovery rearms. `notify(config, incident)` creates a 0600 temporary message in the private state directory, substitutes only its path into trusted argv, executes without a shell, limits execution to 15s, discards output and uses only a small environment allowlist (no Fly credentials). Message names app/Machine, UTC observation/window, reason/count and Pyry as read-only investigator/escalation owner. Remove message on every outcome.
- `poll(config)` stores one versioned, app/Machine-scoped JSON record. Validate loaded state before use. Directory 0700, files 0600, current UID ownership, no final-component symlinks, descriptor-based reads, capped local input, atomic replacement with file/directory fsync. Other selected Machines use separate state filenames. Insecure existing storage is rejected, never silently repaired.

## Concurrency model

One synchronous process per poll; a nonblocking `flock` in the private directory covers load, fetch, evaluation, delivery and save. Concurrent invocation fails safely. HTTP connections close in `finally`; the bounded notification child is killed/waited on timeout. No daemon goroutines or scheduler. A send succeeding before delivery state is saved can be retried after interruption; exactly-once delivery is promised only after both succeed, as in the ticket.

## State transitions and identity reuse

Python has no Go race-detector surface; each transition below is covered hermetically, with a real lock test for overlapping processes.

| Event | Contract / test |
| --- | --- |
| First install / boot / four versus five exits / boundary expiry | Only current-window automatic exits count; `test_threshold_and_window`, `test_first_boot`. |
| Equivalent, duplicated, reordered or late exits / repeated polls | Same physical exit counted once; `test_exit_variants`, `test_late_and_maintenance`. |
| Maintenance enters/expires or planned stop | Half-open interval; automatic crashes still count; `test_late_and_maintenance`. |
| Restart incident becomes stopped / watcher restarts | One saved delivered latch; `test_delivery_latch_and_restart`. |
| Failed send / subsequent retry after recovery | Pending incident retained and delivered once saved; `test_pending_retry`. |
| Healthy run / failure / missing minute / ten-minute recovery / second incident | Recovery alone resets latch, interruptions reset recovery; `test_rearm_and_interruptions`. |
| Truncated history / persisted overlap / history gap | Failure never advances healthy recovery or fabricates counts; `test_history_coverage`. |
| Machine replacement / overlapping polls / interrupted write | Scope isolation, exclusive lock, atomic old state; `test_private_storage_and_scope`, `test_lock_and_atomic_failure`. |

## Error handling

Configuration/storage, auth/API, deadline/oversize, schema, incomplete coverage, delivery and save failures emit only fixed `relay monitoring failure` diagnostics and nonzero exit. No remote body, exception/child text, credentials or raw configuration is printed. Poll failures clear only recovery progress where safely writable; preserve event history, last successful observation and incident/delivery state. Clock rollback rejects the observation. Missed polls (more than 90s between successes) interrupt recovery. Pending delivery cannot be cleared by rearming.

## Testing strategy

Write tests first, run them red before implementation. `python3 -m unittest discover -s operations -p 'test_relay_health.py'` is the one offline command. Controlled UTC clock, fake API responses, fake subprocess and a real private-file notifier cover classification, coverage, lifecycle, caps, deadlines, permissions, locks and secret hygiene. Run this command, `make check` (per workspace AGENTS.md) and `go build ./cmd/pyrycode-relay` before push. No Fly access, real posts or deployment.

## Open questions

None. API history has no completeness/pagination guarantee used here; insufficient retained evidence is explicitly a monitoring failure. Allow 90s between successful observations for the one-minute cadence, not a missed full poll.

## Documentation handoff

Pending for the documentation stage:
- `docs/deploy.md`, new **Restart incident monitoring** and **Restart incident response**: `/home/pyry/pyry-workspace/crons/relay-health/` installation, one-minute schedule, configuration/token provisioning/expiry, private state, explicit Machine selection/replacement, argv/message-file contract, finite maintenance, threshold/window/dedup/rearm, incomplete-history/failure limits and the local test command. Operator delivery: `pyry channel post --name "Pyrybox board watch" --file <message-file>`. Pyry investigates read-only and escalates to Juhana; no mutation authority.
- `docs/deploy.md`, **Steady-state flow**: register bounded maintenance before operator deploy; unexpected failures still report. Keep installation/delivered evidence pending; link #197 recovery/stop/certificate-snapshot procedures without duplicating them.
- `docs/knowledge/features/fly-deploy.md`: watcher ownership, separate from #170 secure exchange/certificate/capacity/push monitoring.
- `docs/threat-model.md`, **Deploy security**, **Log hygiene**, **Triggers for re-review**: outbound operational Fly/notification boundary, org-wide read-only credential exposure, private state and fixed diagnostics. No relay payload access, public endpoint or new relay deployment target.
- All four issue operator checkboxes remain pending: installation, isolated delivered restart/stopped/maintenance/dedup/rearm evidence, usable Pyry/escalation procedure, and verified activation coordinated with #197 while retaining one active production Machine.

## Security review

**Verdict:** PASS

- [Trust boundaries] `normalize` projects API data to lifecycle evidence; no relay payload or API configuration reaches messages/state. Trusted operator argv never comes from Fly data.
- [Credentials] `fetch` reads only a private token file, sends it only to a fixed TLS host without redirects. Provisioning, expiry, rotation and revocation are operator-owned; org-scoped exposure is documented above.
- [Files] `poll` rejects insecure ownership/modes/symlinks, locks state, caps reads and replaces atomically. Persistent state belongs to an external operational tool, preserving stateless relay runtime.
- [Subprocesses] `notify` uses argv, private temporary files, a bounded child, suppressed output and an explicit credential-free environment.
- [Cryptography] Platform TLS and standard-library defaults; no custom crypto or new keys.
- [Network & I/O] `fetch` enforces whole-request and socket deadlines, fixed one-request budget and 1 MiB cap; `notify` has a 15s bound.
- [Errors/logs] Fixed diagnostics only; messages expose validated app/Machine and lifecycle numbers/times. No new relay logger keys.
- [Concurrency] `poll` holds one lock through delivery/save; failures preserve the latch. Crash between successful send and durable save can duplicate once; no exactly-once transport is claimed.
- [Threat model] Operational outbound boundary triggers documentation re-review. Runtime protocol security is unchanged. OUT OF SCOPE: #170 monitoring and #197 operator recovery/rehearsal; installation and delivered evidence stay operator-owned.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-13
