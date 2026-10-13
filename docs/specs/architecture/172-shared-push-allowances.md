# Shared push wake allowances (#172)

## Files read

- `internal/relay/push_wake.go` → `PushWaker.Request`, constructors, `send`, `Close`: admission, outcome hook and shutdown ownership.
- `internal/relay/push_wake_test.go` → `fakePushSender`, rate/cap/shutdown and secret-hygiene proofs: reuse the sender and retain these regressions.
- `internal/relay/metrics_push_test.go` → `newPushMetricsBundle`, `assertPushWakeOutcomes`: fixed five-label metric assertions.
- `internal/relay/ratelimit.go` → `IPRateLimiter.Allow`, `sweep`: integer refill remainder and full-refill eviction precedent; leave upgrade limits untouched.
- `internal/relay/forward.go` → `StartBinaryForwarder`: sole production request caller; routing and opaque frames stay unchanged.
- `internal/relay/server_endpoint.go` → `serverHandler`: admitted connections start the forwarder after claiming an identity.
- `internal/relay/daemon_admission_test.go` → `TestDaemonAdmission_RejectedStateUntouched`, `TestDaemonAdmission_ReconnectForwardPush`: existing enforce boundary and reconnect harness.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`, `docs/knowledge/features/push-wake-dispatch.md`: conventions, outcome ordering and cancellation traps.
- `docs/architecture.md` (Outbound calls), `docs/threat-model.md` (Outbound network calls, Log hygiene), `docs/security-followups.md`: single-process bounds, secrets and deferred device ownership.
- [Protocol mobile](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#push_wake), push_wake and Security model: payload-free wakes outside Noise; enforce alone authenticates daemon identity.

## Context

Concurrency alone does not limit outgoing request rate, and fresh daemon identities bypass a per-daemon allowance. #167 is merged and its admission boundary remains unchanged. This repairs one admission budget, in every admission mode, without retries, device ownership checks or wire changes. Existing ADR-0013 needs an update by documentation rather than a new decision record.

Size sketch and plan recount: approximately 200 production, 420 tests/helpers and 100 plan lines, below 800 total; no new exported types/interfaces, no consumer migrations, five criteria and three admission refusal branches (rate, keyed capacity, concurrency). The #133 analogue's dispatcher/sender harness is already present. Planned edits: `push_wake.go`, new `push_wake_budget_test.go`, and a local extension to `daemon_admission_test.go`. Fetched in-flight branches: no overlapping files found.

## Design

Keep public constructors and `Request` compatible, including `newPushWaker`'s existing daemon-limit test parameters. Replace only the push use of `IPRateLimiter` with local accounting under `w.mu`. One unexported bucket type holds integer tokens, refill timestamp and last-request timestamp. Separate maps keyed by full SHA-256 arrays hold daemon and exact device-token identities; no raw keys or truncated logging fingerprints. Each map retains at most 1,024 buckets. Global state is a single bucket, initially full.

Production allowances: daemon and device burst 6, one token per 10 seconds; global burst 32, one token per second. Preserve fractional elapsed-time remainder using integer refill steps, capped at burst. Custom constructor parameters affect the daemon allowance only; device/global limits remain production values.

`Request` validates structurally, hashes complete keys, then under `w.mu` checks closure and refreshes existing buckets' refill and last-request timestamps even on admission refusals. Missing keys are temporary full buckets. On a request requiring a new key, sweep eligible idle state before checking map capacity. Existing requested keys have already been touched and cannot be reclaimed during this request. Refuse saturated maps, exhausted allowances or a full send semaphore without decrementing any bucket, inserting temporary state or reserving a slot. Successful admission reserves the slot, inserts missing buckets, decrements all three tokens, adds the send wait group entry and dispatches. A failed send stays charged.

Sweep removes only buckets idle for at least five minutes AND fully refilled at the sweep time. This second condition preserves depleted allowances even with the constructor's slower test refill. No capacity-pressure eviction of active/depleted state. Idle expiry is checked on new-key requests and by one five-minute periodic sweep. Global state never expires. Existing state remains usable at capacity. All state resets on process restart, consistent with this stateless single-process relay.

Use the existing static `ErrPushWakeRateLimited` for any allowance refusal (generalise its text), and add static `ErrPushWakeStateCap` for either keyed map. Each admission refusal records `pushWakeDropped` once. Malformed and push-off requests still count nothing. No logging keys, dependency or payload access is added.

## Concurrency model

One mutex owns keyed/global budgets, sweep and admission versus closure. The send semaphore remains capped at 32; send goroutines release it and finish their outcome before `wg.Done`. No mutex is held through `Send`. A sweep goroutine exits on the waker context cancellation and has a separate wait group. An injected `now` function is read under `w.mu`, allowing controlled-clock tests without a race. `Close` marks closed under the admission mutex, cancels sends/sweep and joins both wait groups; repeated/concurrent Close callers also join admitted work. `wg.Add` cannot occur after closed. No new connection goroutines or locks.

## State transitions and identity reuse

| Event | Race-detector proof |
|---|---|
| Distinct daemons reuse one complete device token | `TestPushWaker_SharedAllowances` |
| Identity changes, completed sends and same-key reuse do not reset budgets | `TestPushWaker_SharedAllowances`, `TestPushWaker_AtomicRefusals` |
| Initial admitted connection and reconnect under the same identity | `TestDaemonAdmission_ReconnectForwardPush` (extend initial wake), existing rejection proof |
| Refill just before/exact boundary; long-idle refill stops at capacity | `TestPushWaker_RefillBoundaries` |
| Refusal renews last request; idle sweep and capacity pressure reclaim only full buckets | `TestPushWaker_StateBoundsAndExpiry` |
| Evicted fully refilled identity returns; depleted slow-refill identity cannot be evicted | `TestPushWaker_StateBoundsAndExpiry` |
| Concurrent requests compete for daemon/device/global tokens and slots | `TestPushWaker_ConcurrentAdmission` |
| Admission races Close, repeated/concurrent Close joins admitted work | `TestPushWaker_AdmissionRacesClose`, existing cancellation/authentication proofs |

## Error handling

No queue or retry on refusal, no refunds on send failure. New errors are static and token-free. Existing send classification and sanitised FCM errors stay intact. At capacity a new key is refused rather than deleting debt. Cancellation joins the sweep and all admitted sends.

## Testing strategy

Write controlled-clock, fake-sender tests first and observe the missing behavior fail. Cover independent daemon/device/global exhaustion, complete-token distinction, normal 24-wake paired-device burst, no partial charging or slot/state leaks on each refusal, failed-send charging, cardinality saturation, refusal timestamps and full-refill reclamation. Scrape the real fixed-label metrics for all refusal kinds. Concurrent admissions test independent allowances and concurrency; race shutdown and verify joining. Retain admission/forwarding, payload-free sender, log allowlist and secret-hygiene suites. Run `make check` as requested and build `./cmd/pyrycode-relay`; all checks are hermetic.

## Open questions

None. Constructor tuning remains daemon-only; production device/global values and safe eviction apply in all modes.

## Documentation handoff

Pending for documentation stage:

- `docs/knowledge/features/push-wake-dispatch.md`, The two limits; Request: validate, admit, dispatch; Metrics; Concurrency model: three exact burst/refill allowances, atomic charging/slot reservation, bounded maps/expiry, saturation refusals and four-daemon/three-device/two-wake example (24).
- `docs/architecture.md`, Outbound calls to Google (FCM push wake); `docs/threat-model.md`, Outbound network calls — FCM push wake; `docs/knowledge/decisions/0013-push-wake-async-dispatch-bounded-not-verified.md`, Decision/Consequences: replace throughput residual/future ceiling with shipped device/global budgets; distinguish burst from sustained refill, process-memory/restart reset, enforce-only daemon authentication, unverified device ownership and payload-free wakes.
- `docs/deploy.md`: retain manual deployment and production admission activation follow-ups; hermetic checks establish neither deployment nor live enforcement.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] `parsePushWake` validates only the relay-addressed control object. `StartBinaryForwarder` preserves opaque frames; budgets apply even when off/observe supply unauthenticated identities. Device-token ownership remains deliberately unverified under ADR-0013.
- [Tokens/secrets] Complete device tokens exist only transiently for hashing and admitted sends. Accounting stores full SHA-256 keys, never raw secrets; admission tokens never enter this path.
- [File operations] No runtime disk state or caller-controlled paths; only the required plan is written.
- [Subprocesses] No production subprocesses introduced.
- [Cryptography] Standard-library SHA-256 over complete input gives bounded identities, avoiding truncated fingerprint collisions; no credential comparison or new crypto protocol.
- [Network/I/O] Existing bounded WebSocket reads and FCM deadlines remain. All three allowances and 32 slots bound attempts, while 1,024 keys per map bound retained accounting memory. Sweeps are bounded by map sizes. One request produces at most one payload-free send.
- [Errors/logs/telemetry] Static sentinels and one dropped count per refusal; existing five fixed labels and allowlisted logs only. Accounting digests never become metric labels or logs.
- [Concurrency] SHOULD FIX — reserving sequential budgets or evicting depleted state would permit overcharging/bypass. Implement the single-mutex commit point and full-refill eviction guard, and prove both with controlled-clock/race tests. Concurrent Close must join admitted sends even when another caller marked closure first.
- [Threat model] Single-process in-memory budgets reset on restart; multiple replicas require the already deferred shared rate-limit design. Retry/coalescing is #173 and must later consume these attempt budgets. No dependency, endpoint or deploy-target trigger added. Operator deployment/enforce activation remain manual.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-13
