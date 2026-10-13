# Complete-process shutdown budget (#200)

## Files read

- `cmd/pyrycode-relay/main.go` → `run`, `signalContextFor`, `runServers`, `drainDeadline`: production signal entry, deferred cleanup order and ten-second drain.
- `cmd/pyrycode-relay/main_e2e_test.go` → `dialServer`, `dialPhone`, `readPump`, `sendAndExpectAtBinary`: real socket admission and opaque routing fixtures.
- `internal/relay/push_refresh.go` → `fcmTokenCache.token`, `leave`, `fetch`: shared refresh waiters, cancellation and authentication join under the cache mutex.
- `internal/relay/push_refresh_test.go` → `heldOAuth`, `awaitFCMWaiters`: held local authentication and actual waiter synchronization.
- `internal/relay/push_wake.go` → `NewPushWakerFromEnv`, `send`, `Close`: six-request burst, outcome classification and send/limiter join.
- `internal/relay/push_wake_test.go` → `TestPushWaker_CloseCancelsAuthentication`: separate cleanup evidence does not prove process exit.
- `internal/relay/ratelimit.go` → `IPRateLimiter.Close`, `evictLoop`: stop channel and worker join, no external I/O.
- `internal/relay/metrics_push.go` → `NewPushMetrics`: existing `token_fetch_failed` counter proves final outcomes.
- `fly.toml`: passthrough services, volume, concurrency, rolling strategy and #199 explicit app restart policy.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`: conventions and feature reading map.
- `docs/knowledge/features/graceful-shutdown.md`, `docs/knowledge/features/fly-deploy.md`: admission must be proved after upgrade; stop settings are deployment intent.
- `docs/architecture.md`, `docs/threat-model.md` (DoS resistance, log hygiene, cert/key handling): complete cleanup follows drain; no payload/secret inspection.
- [Wire contract](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#push_wake) and Security model: unchanged push envelope and opaque forwarding.
- [Fly runtime options](https://docs.fly.io/reference/configuration/#runtime-options): top-level seconds, 300-second maximum and best-effort allowance.

## Context

Fly's default five-second stop allowance is shorter than the ten-second drain alone. Existing tests cover synthetic cancellation and push cleanup separately. One deliverable couples an explicit complete-exit budget to a hermetic process regression; no decision record is needed.

Size: approximately 600 written lines across four code/config files and this plan, zero new exported types/interfaces, two existing call expressions updated, three acceptance criteria, no new production reject branches. #184's 683 insertions include documentation outside this builder's scope; our independent sketch fits 800 lines. Remote overlaps #127 (already merged historical test additions) and #167 (different boot/admission blocks in `main.go`) need no dependency; edits stay local.

## Design

Set top-level `kill_signal = "SIGTERM"`, `kill_timeout = 30`; preserve all other manifest values. Define the regression's complete-process budget as 25 seconds: ten seconds for `runServers` drain, ten seconds conservatively for subsequent push operation/refresh cancellation and join (existing send/refresh deadlines, not a new cleanup deadline), and five seconds scheduling/limiter/exit headroom. Both limiter joins stop local eviction workers with no network I/O. Push sends run concurrently, so their deadlines are not multiplied by six. The earlier two-second unit assertion is evidence, not a production bound. This is a conservative configured budget under responsive local cancellation, not a hard guarantee against runtime/platform stalls.

Add two grouped unexported boot factory variables in `main.go`, defaulting to `relay.NewPushWakerFromEnv` and `relay.NewMetricsRegistry`. The child alone wraps these factories to retain the actual production waker and metrics; it delegates construction unchanged. Add `PushWaker.RefreshState() (int, <-chan struct{})` beside private refresh code: a read-only mutex-guarded snapshot of FCM waiters and the current attempt's completion channel, no credentials or state mutation. These seams have one consuming regression and no HTTP endpoint.

The regression re-execs the current test executable with one exact helper-test name, minimal environment and synthetic RSA service-account credentials. The helper calls production `run` and `signalContextFor(SIGTERM, SIGINT)` and exits with run's code after assertions. Parent and child exchange fixed JSON events/control over pipes. All outbound child HTTP uses a deny-by-default transport: only the parent's exact loopback OAuth endpoint is allowed, no proxy; FCM/other calls are counted and refused. OAuth remains held until request cancellation. The transport also holds its return after cancellation behind parent control, proving the child cannot finish before authentication cleanup is joined.

Parent admits a binary and phone with readers plus native ping/pong and byte-exact opaque round trips. Six push envelopes use the existing burst. Child readiness requires six actual joined refresh waiters and an unfinished refresh; parent also proves exactly one OAuth request is held and continued traffic in both directions. Continuous traffic/readers remain active through real SIGTERM. Signal before natural ten-second deadlines using a bounded readiness phase and a final child snapshot/control barrier. After cancellation, prove `run` has not returned while transport cleanup is held, release it, then require both wire closes 1001, OAuth cancellation, zero exit, one OAuth/no FCM, six `token_fetch_failed` counters and the retained refresh completion channel closed inside the child before exit. Record signal-to-exit and signal-to-auth-cancellation durations.

## Concurrency model

Production workers and deadlines are unchanged. Only the isolated helper replaces boot factories/default transport before serving. Snapshot reads take only the cache mutex; no I/O or callback occurs under it. Parent peer reader/writer goroutines stop on socket closure or test context cancellation and are joined. Child event encoding is serialized. Parent always closes controls, cancels contexts, closes peers/server, and kills/reaps an unfinished child on failure; the helper's pipe reader exits on EOF.

## State transitions and identity reuse

| Event | Race test |
| --- | --- |
| Binary/phone upgrade reaches admission before traffic and signal | `TestRun_ProcessSIGTERMCompleteShutdown` native ping and round trips |
| Six cold sends join one refresh; signal while all remain joined | `TestRun_ProcessSIGTERMCompleteShutdown` guarded snapshot + held OAuth |
| Last waiter cancels authentication, joins transport and records final outcomes | `TestRun_ProcessSIGTERMCompleteShutdown` cleanup barrier + retained done + final metrics |
| SIGTERM drains responsive peers while bidirectional writes race shutdown | `TestRun_ProcessSIGTERMCompleteShutdown` two wire 1001 closes + process exit |
| Failed setup/child exit/control EOF | `TestRun_ProcessSIGTERMCompleteShutdown` bounded waits and unconditional cleanup |

No production identity reuse or new lifecycle state is introduced; every execution uses a fresh process, routing id, cache and ephemeral ports.

## Error handling

No new production failures or recovery policy. Child asserts locally and exits nonzero on failed synchronization/cleanup; parent retains diagnostics and bounds every wait. Unexpected outbound requests fail locally without contacting Google. Early timeout cannot satisfy shutdown cancellation: assert held request and unfinished refresh immediately before signal within the readiness budget.

## Testing strategy

Write the regression before seams/configuration and observe failure. Focused race run proves the process lifecycle, then `make check` (user-required full race suite and vet before push), `go build ./cmd/pyrycode-relay`. Offline Python `tomllib` parsing compares all manifest data except the two added top-level keys against committed baseline and checks 30 >= 25 + 5 and <= 300; no parser dependency. Keep durations and parse evidence in the PR. No Linux-only production file changes.

## Open questions

None. If implementation changes this contract, record a Revisions entry with the implementation commit.

## Documentation handoff

All pending for the documentation stage; no operational checkbox is completed by local tests or merge:

- `docs/deploy.md`, **fly.toml gotchas**, **Verify a deploy**, **Rollback**: explicit SIGTERM/30s stop allowance versus 10s drain and complete 25s budget; best-effort platform allowance, effective-settings checks, operator checklist, previous immutable image rollback (observed digest in #200; refresh then-current digest).
- `docs/architecture.md`, **Hosting**, **The PYRYCODE_RELAY_SINGLE_INSTANCE bypass**; `docs/knowledge/features/fly-deploy.md`, **Single-machine hard cap**: env assertion records operator intent, not Machine inventory; disabled autostart prevents starts, not creation; no maximum is enforced, inspect actual inventory and preserve #199 restart wording.
- `docs/deploy.md`, new **Certificate snapshot recovery**: operator local Fly login; refresh snapshots for source `vol_rkggyj5jel8o0xk4`, mounted at `/var/lib/relay/autocert`; observed `vs_ov9G6nK906KS2JD4ZZJ11Y` (2026-10-12T14:50:17Z, five-day retention) is expiring, not pinned. Restore current snapshot into a new encrypted isolated volume at least source size and 1 GiB, preserve permissions, never expose keys or replace live volume; record IDs, isolation, recovery and elapsed time per Fly restore procedure linked in #200.
- `docs/knowledge/features/graceful-shutdown.md`, **Contract**, **Wiring**: separate drain from deferred complete cleanup and describe real SIGTERM regression.
- Preserve #200's four pending operator rehearsals: isolated shutdown under held synthetic auth; isolated current snapshot recovery; actual before/after production Machine inventory and exactly one active relay; clean-main activation only after isolated shutdown and #199 eleven-crash recovery checks. Record effective settings/image/digest; alerts remain #198. No live deploy in builder.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — production envelope handling/opaque `json.RawMessage` forwarding is unchanged; child reads only guarded refresh counts and completion, never payloads or credentials.
- [Tokens, secrets & cryptography] SHOULD FIX — generate synthetic RSA credentials locally, pass only to the minimal child environment and never include credentials/request bodies in events or diagnostics. No production key/cache is accessed.
- [File operations] No findings — only test temp files/pipes and plan/config changes; autocert cache permissions and storage are unchanged.
- [Subprocesses] SHOULD FIX — re-exec exact executable/helper name without shell, scrub deployment/credential/proxy environment, bound all waits and always kill/reap unfinished child.
- [Network & I/O] SHOULD FIX — deny every outbound URL except the exact local OAuth endpoint, disable proxy and bound peer/control reads; existing public server timeouts, frame caps and admission limits remain intact.
- [Errors, logs & telemetry] No findings — use existing fixed outcome metrics; add no production log keys; events hold only counts, stages and timings.
- [Concurrency] SHOULD FIX — snapshot under cache mutex, capture boot values before serving, join parent goroutines and assert refresh completion after actual `run` defers; socket cancellation alone is insufficient.
- [Threat model alignment] No findings — no new dependency, public endpoint, deploy target or outbound production request. Best-effort Fly allowance is not a guarantee. Snapshot rehearsal and live inventory/deploy remain operator-owned #200 follow-ups; recovery policy stays #199.

**Reviewer:** builder (self-review per security-review checklist)
**Date:** 2026-10-13
