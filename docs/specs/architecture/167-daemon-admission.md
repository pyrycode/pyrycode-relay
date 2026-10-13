# Identity-bound daemon admission (#167)

## Files read

- `internal/relay/env_config.go` → `envContracts`, `CheckEnvConfig`: startup registry and structured, value-free errors.
- `internal/relay/server_endpoint.go` → `ServerHandler`, `serverHandler`: pre-upgrade validation, claims, incumbent probes and grace cleanup; preserve its 11 callers.
- `cmd/pyrycode-relay/main.go` → `run`: startup checks, private metrics registry, rate/source/global wrappers and shutdown.
- `internal/relay/push.go` → `parseFCMCredentials`: discard parser errors instead of wrapping secret-bearing excerpts.
- `internal/relay/metrics_upgrade.go`, `metrics.go` → `NewUpgradeMetrics`, `NewMetricsRegistry`: private collectors and fixed outcomes.
- `internal/relay/server_endpoint_test.go`, `client_endpoint_test.go`, `push_wake_test.go` → endpoint peers, seeded connections, push spy and thread-safe log capture.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`, features `server-endpoint.md`, `env-config-validator.md`, `metrics-registry.md`, `push-wake-dispatch.md`: preserve opaque forwarding and bounded push dispatch; admission is distinct from phone authorization.
- `docs/architecture.md`, `docs/threat-model.md`, `docs/security-followups.md`: current listener limits, log hygiene and deferred raw-source protections.
- [Protocol: Daemon admission](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#daemon-admission-declared), its Security model and Core architecture plan #3196: authoritative decision table, rollout and threat boundary.

## Context

Public headers currently permit anonymous identity reservation and daemon-originated wakes. Implement the declared gate; production activation remains operator work. No new dependency or decision record is needed. One deliverable, five criteria, one production consumer update, one exported type, five admission outcome paths. Forecast: about 700 written lines including this plan (approximately 220 production and 400 tests/helpers). The #132 analogue includes 588 comparable credential code/test lines; this parser uses no external credential service or transport. No overlapping in-flight branch touches the planned files.

## Design

Add `internal/relay/daemon_admission.go` with `DaemonAdmission` (private mode and credentials), `NewDaemonAdmissionFromEnv(lookup) (*DaemonAdmission, error)` and `Wrap(next http.Handler, reg prometheus.Registerer) http.Handler`. Constructor parses a fresh owned mapping once for the handler; there are no exposed maps or reload hooks. Register both environment variables in `envContracts`; all supplied values validate even in off, and `CheckEnvConfig` also refuses enforce without supplied credentials. Constructor independently enforces the same rules and returns structured static config errors.

Parse the JSON object with decoder tokens so decoded duplicate identity keys cannot be overwritten, including escaped spellings. Reject null/non-object roots, null/non-array entries, trailing data, non-string/malformed tokens and cross-identity token reuse; empty arrays are valid. Return only `ErrDaemonAdmissionCredentials`, never a JSON error. Same-identity repeated tokens may remain accepted. Token shape is exactly 64 lowercase hex bytes.

`Wrap` runs inside the existing rate/source/global wrappers immediately before unchanged `ServerHandler`. Off delegates unchecked. Observe/enforce use a shared decision function: identity missing/empty, duplicate values or a comma list is malformed; token absence is missing; duplicate or invalid token values are malformed. Identity matching is exact, with no UUID validation newly imposed on the legacy nonempty routing key. Compare every candidate using `crypto/subtle.ConstantTimeCompare`; an exact pair is valid, a token belonging to another identity is mismatch, otherwise unknown. No trimming, normalization or list selection occurs. Only enforce rejects, using 401, no-store and exactly `daemon admission rejected\n`, before any endpoint work.

Create a private `pyrycode_relay_daemon_admission_total{outcome}` counter with exactly five prebound fixed labels. Observe and enforce increment once per evaluated request; off does not evaluate or increment. No admission logger, identity labels or dynamic label input. Existing upgrade metrics continue to describe the legacy endpoint path.

`run` constructs the snapshot after env validation and before workers/listeners, then wraps only the production daemon endpoint with the private metrics registry. No changes to phone headers, envelopes or payloads.

## Concurrency model

No new goroutine, mutex or lifecycle state. Owned credential maps and arrays are read-only after construction. Prebound Prometheus counters support concurrent increments. Existing handlers own heartbeat/forwarder cleanup; existing shutdown closes connections and discards process-local claims/grace state on restart.

## State transitions and identity reuse

| Event | Race-detector proof |
|---|---|
| Repeated requests and concurrent exact-identity reconnects | `TestDaemonAdmission_ConcurrentReconnects`; existing registry locking/conflict proofs |
| Initial admission and grace reclaim under same identity | `TestDaemonAdmission_ReconnectForwardPush`; existing `TestClaimServer_ReclaimDuringGrace_StaleFireTouchesNothing` |
| Unauthorized claim against incumbent or grace reservation | `TestDaemonAdmission_RejectedStateUntouched` checks no probe, eviction, timer replacement or push |
| Provisioning edit, overlap rotation, token/identity removal and restart | `TestDaemonAdmission_SnapshotRestart` plus existing `TestShutdown_AdmittedPeersGoingAway` (connection close) |
| Authorized conflict retains 4409 | `TestDaemonAdmission_AuthorizedConflict`; existing incumbent liveness tests |

## Error handling

Static config reasons only; parser sentinel has no secret-bearing cause. Enforce uses one public response for all failures. Observe introduces no reject path; legacy header checks and conflicts still apply. No raw headers or credentials flow to logs, metrics, status or frames.

## Testing strategy

Write table tests first and record their failure before implementation. Cover every mode/config decision, duplicate/escaped JSON keys, token shapes and duplicate/coalesced HTTP values. Use httptest real WebSocket peers, registry fakes and push spy for ordering, occupied/gracing identities, reconnect forwarding/push and restart revocation. Capture errors/logs and scrape fixed metrics to check secrecy. Reuse existing grace/shutdown/opaque-frame proofs. Run focused race tests, touched packages, vet and build; user-required `make check` runs before push.

## Open questions

None. Identity syntax stays the existing nonempty key with the contract-required duplicate/comma rejection; no additional UUID policy is introduced.

## Documentation handoff

Pending documentation stage:
- `docs/knowledge/features/server-endpoint.md` and `env-config-validator.md`: gate ordering, exact header/mapping rules, modes, secret-safe validation and restart snapshots; replace unconditional anonymous-admission descriptions.
- `docs/architecture.md` — Authoritative reference / What this binary does / What this binary does NOT do: admission versus daemon-owned phone authentication and Noise; content remains opaque.
- `docs/threat-model.md` — Deploy security / Supply chain / Outbound network calls / Log hygiene / Error response leakage: Noise ciphertext and metadata, enforce protection, stolen bound tokens/push abuse without Noise keys, relay-compromise credential/metadata capture and DoS, visible opaque phone header; off/observe remain anonymous.
- `docs/knowledge/decisions/0013-push-wake-async-dispatch-bounded-not-verified.md` — Context/Consequences; `docs/knowledge/features/push-wake-dispatch.md`: enforce narrows abuse to credential holders; unverified device-token residual and limits remain.
- `docs/deploy.md` — Secrets/Steady-state flow: ship both implementations off, privately provision distinct random tokens, restart into observe and confirm intended identities through controlled reconnects/aggregate counters, explicitly restart into enforce and verify authorized/anonymous behavior; overlap rotation, removal-plus-restart revocation, rollback restores anonymous admission, terminal daemon 401 needs correction and daemon restart. Production deploy/activation/live verification pending operator; synthetic examples only.

## Security review

**Verdict:** PASS

**Findings:**
- [Trust boundaries] Gate in `DaemonAdmission.Wrap` precedes all daemon-handler side effects; payloads remain `json.RawMessage`; phone credential handling is unchanged.
- [Tokens/secrets] Startup-owned map is private; operators generate tokens, overlap is permitted and restart revokes. Parser errors are discarded, not wrapped. No credential generation or persistence in Relay.
- [Files] No runtime file operations or new disk state; provisioning is environment-only.
- [Subprocesses] No subprocess or shell command introduced.
- [Cryptography] `ConstantTimeCompare` after exact length/alphabet validation, scanning accepted tokens without early return; no custom crypto or Noise change.
- [Network/I/O] Existing header cap, server timeouts, rate/source/global caps and frame caps remain. Reject before upgrades, probes, claims and wakes; no new socket read.
- [Errors/logs/telemetry] Fixed rejection and static config errors; prebound five aggregate labels; no new log keys or admission logs. Tests must check duplicate/escaped-key parser failure does not expose input.
- [Concurrency] Immutable owned map and safe counters; no new goroutine/lock; restart uses existing shutdown boundary.
- [Threat alignment] Only enforce addresses anonymous claims/wakes. Stolen credentials and malicious Relay do not confer Noise keys; operator/documentation handoff carries these limits. Raw-source and proxy-trust mitigations remain deferred in `docs/security-followups.md` under Deferred from threat-model.

**Reviewer:** builder (self-review per security-review checklist)
**Date:** 2026-10-13
