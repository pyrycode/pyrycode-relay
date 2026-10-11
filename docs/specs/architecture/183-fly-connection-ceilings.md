# Fly connection ceilings (#183)

## Files read

- `fly.toml` → both `[[services]]` blocks; raw TCP mappings, PROXY v2 pairing and the 8080 TCP check must be preserved.
- `cmd/pyrycode-relay/main.go` → `defaultMaxListenerConnections` and `run`; each public listener defaults to 32 and the Fly argv leaves that default intact.
- `docs/PROJECT-MEMORY.md` → content blindness, scope and Go conventions.
- `docs/knowledge/INDEX.md`, `docs/knowledge/features/connection-cap.md` → raw admission is independent per listener; queued TCP dial success does not prove admission.
- `docs/knowledge/features/fly-deploy.md` → TCP passthrough, single-machine intent and operator-owned deployment; existing health-check behavior from #118.
- `docs/architecture.md` → Public listener admission and Hosting; relay-owned TLS and independent listener capacity.
- `docs/threat-model.md` → DoS resistance and re-review triggers; raw caps do not bound the edge or kernel backlog.
- `docs/security-followups.md` → Fly Machines `services.concurrency`; the existing immediate-drop wording needs correction by documentation.
- `docs/deploy.md` → fly.toml gotchas and Verify a deploy; isolated operator verification precedes production deployment.
- [Fly configuration reference, services.concurrency](https://docs.fly.io/reference/configuration#servicesconcurrency) → connection counting and stopped forwarding at `hard_limit`, rather than guaranteed immediate client drops.
- [Wire protocol Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model) → DoS mitigation is separate from payload encryption; no wire changes are needed.
- `Makefile` → `check` runs vet and the full-module race suite, required by the supplied AGENTS.md before push.

## Change

Add `[services.concurrency]` with `type = "connections"` and `hard_limit = 32` to each existing raw-TCP service, immediately before its port table. Each block carries a comment identifying `--max-listener-connections` and its default of 32. This matches the binary's independent listener ceilings and tells Fly to stop forwarding new traffic at saturation. Leave the optional soft limit unset. Preserve every existing setting, including passthrough on 80, PROXY v2 on 443, health checks, argv and machine lifecycle. No new type, relay state, failure branch, dependency or wire behavior; no decision record is needed. No overlapping in-flight branch touches `fly.toml` after fetching origin. Re-count: one deliverable, one acceptance criterion, about 60 total added lines including this plan, zero exports, zero consumer updates and zero new state-machine reject branches; below all five limits. The #118 analogue added 21 manifest lines; this change adds 10.

## Testing strategy

Before editing config, run a temporary Python standard-library TOML assertion for both service ceilings and observe failure due to the absent concurrency tables. After editing, parse again and assert exact connection type/limit and both comments. Compare the parsed config against the pre-change config after removing only the new concurrency tables to prove all other settings are preserved. Keep this one-off acceptance check under `/tmp/builder-relay-183/`; no permanent test or parser dependency is needed for two literal configuration blocks. Run `make check` and `go build ./cmd/pyrycode-relay`, plus `git diff --check`. Live Fly forwarding at saturation and recovery cannot be proven by offline config parsing; the operator owns that check before production deployment.

## Documentation handoff

Pending for the documentation stage:

- `docs/security-followups.md`, Fly Machines `services.concurrency`: mark both limits configured at 32; describe stopped forwarding, without promising immediate client drops.
- `docs/threat-model.md`, DoS resistance: name Fly forwarding ceilings alongside raw listener admission.
- `docs/deploy.md`, fly.toml gotchas and Verify a deploy: record matching limits and require an isolated Fly app forwarding check at saturation and recovery before production deployment; operator-owned.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — static operator-owned `fly.toml` policy bounds forwarding before the machine; raw acceptance in `run` remains the independent process boundary. No payload or header inspection changes.
- [Tokens, secrets and credentials] No findings — the two tables introduce only a count and a metric name, with no credential handling.
- [File operations] No findings — manifest edits add no runtime filesystem operations or persistent user state; the autocert mount remains intact.
- [Subprocesses] No findings — production argv and process wiring are preserved; no subprocess is introduced.
- [Cryptography] No findings — TCP passthrough and relay-owned TLS remain intact, including the 443 PROXY v2 pairing.
- [Network and I/O] No findings — both services count connections at 32; `run` retains listener bounds and parsing deadlines. Offline structural checks cannot establish Fly's deployed saturation/recovery behavior, so isolated operator validation is explicitly required before deployment.
- [Errors, logs and telemetry] No findings — no logger, response body, public error or metric changes.
- [Concurrency] No findings — no new relay state or goroutine; connection occupancy and recovery at the Fly boundary are operator-verified, not inferred from successful TCP dials.
- [Threat model alignment] No findings — strengthens DoS resistance without a new dependency, endpoint or deploy target. One source can still consume all capacity; per-IP concurrent allowances remain deferred under that heading in `docs/security-followups.md`, outside #183. The documentation stage records the new ceilings; deployment remains operator-owned.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11
