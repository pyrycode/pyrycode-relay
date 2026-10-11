# Per-source upgraded connection allowance (#180)

## Files read

- `internal/relay/conncap.go` → `ConnCap.Wrap`: nonblocking global admission and handler-return release.
- `internal/relay/conncap_test.go` → `startCapped`, `TestConnCap_SharedSlotReleasedOnEveryExit`: real WebSocket lifecycle harness to extend with source accounting.
- `internal/relay/client_ip.go` → `ClientIP`: exact host/XFF extraction, fallback and quoting contract; do not canonicalize.
- `internal/relay/ratelimit_middleware.go` → `NewRateLimitMiddleware`: empty-source and exhausted-bucket denial precede reservations.
- `internal/relay/server_endpoint.go` → `serverHandler`: header/handshake exits, conflict probe and deferred cleanup.
- `internal/relay/client_endpoint.go` → `ClientHandler`: phone registration, outbox teardown and 4404/4429 exits.
- `internal/relay/heartbeat.go` → `runHeartbeat`: shorten timing in tests without changing production policy.
- `cmd/pyrycode-relay/main.go` → `run`: validate flags before workers, share caps across upgrade routes only.
- `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_ConnCapGatesUpgradesNotHealthz`: replace invalid global=1 configuration with distinct-source saturation.
- `cmd/pyrycode-relay/listener_cap_test.go` → `startCappedRelay`, `capProxyHeader`: reuse local TLS/cache and PROXY peers for actual run wiring.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`, `docs/knowledge/features/connection-cap.md`, `docs/knowledge/features/rate-limit-middleware.md`: admission conventions, separate raw/session lifetime and source trust.
- `docs/architecture.md`, `docs/threat-model.md` (DoS resistance, Log hygiene), `docs/security-followups.md`: single-instance scope, permitted remote logging, remaining raw-source risk.
- [Protocol Authentication and Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md): existing headers and close semantics remain authoritative.

## Context

One source can currently occupy all 20 upgraded slots in a burst. Limit each source to eight handlers: one daemon, six phones and one desktop behind a NAT, leaving twelve global slots. This is one admission behaviour, with configuration and lifecycle proofs. No decision record is needed; middleware composition follows the existing global cap.

Size sketch and recount: approximately 100 production, 430 test/helper and 100 plan lines, below 800; one new exported type, two route call sites, four acceptance criteria, two new rejection branches (configuration and allowance). Analogue #114/PR #143 added 384 production/test lines. Reference docs are a later-stage handoff. Branch #127 overlaps `main_e2e_test.go` additively in different tests; no dependency or redesign is needed.

## Design

Add `SourceConnCap` in `internal/relay/source_conncap.go`, with `NewSourceConnCap(max int, logger *slog.Logger, trustForwardedFor bool) (*SourceConnCap, error)` and `Wrap(http.Handler) http.Handler`. The constructor rejects non-positive maxima with wrapped `ErrInvalidSourceConnCap`. A mutex-protected map records positive counts by the exact `ClientIP` result. Admission checks and increments under one lock; refusal never inserts an entry. Release decrements and deletes the last entry.

`Wrap` reserves before calling next and defers release until next's entire cleanup returns. Over allowance: `http.Error(w, "", 503)` and exactly one `Warn("source_conn_cap_reached", "remote", strconv.Quote(source))`; only the existing allowlisted, safe source-host key is logged. No routing headers, tokens, payloads or metrics labels are added.

`run` constructs one source cap after validating the global cap and before any workers/listeners. Effective source maximum must satisfy `0 < source < global`, including the omitted default. Default eight and the coupled constraint are explained beside `defaultMaxConnections` and in flag help. Trust-XFF help covers both source limits. Compose both routes as metrics observer → rate limiter → source cap → global cap → endpoint. A global rejection releases its temporary source reservation on return. Health and loopback metrics bypass upgraded caps. PROXY source rewriting remains below HTTP and is reused without new parsing.

## Concurrency model

Only existing request goroutines access the map; no new goroutine/timer. Hold the source mutex only for checking/updating counts, never during next, response writes or logging. Thus it never nests with the global semaphore or registry locks. Handler-return defers release even on panic unwinding. Shutdown uses existing registry/socket cleanup; the wrapper requires no shutdown worker.

## State transitions and identity reuse

| Event | Contract and race test |
|---|---|
| Concurrent server/client admission from one source | Check/increment atomically; bounded accepted population; `TestSourceConnCap_ConcurrentSharedAdmission`. |
| Refused source/global/rate attempt | No retained source entry or global slot; `TestSourceConnCap_DenialAndCleanup`, `TestSourceConnCap_RateLimitOutside`. |
| Last handler exits then same source reconnects repeatedly | Delete zero entry; capacity reused without stale release; `TestSourceConnCap_DenialAndCleanup`, existing `TestConnCap_SharedSlotReleasedOnEveryExit` extended with source assertions. |
| Header or handshake failure | Return releases source reservation; `TestSourceConnCap_FailureAndTeardown`. |
| Peer/abrupt close, heartbeat timeout | Reservation survives cleanup then releases; existing normal-close test plus `TestSourceConnCap_FailureAndTeardown`. |
| Grace expiry then same server-id reclaimed | Phones release and fresh connections can claim capacity; existing grace-expiry test with source assertions. |
| 4404/4409/4429 rejection | Stillborn handler releases; existing 4404 test plus `TestSourceConnCap_FailureAndTeardown`. |
| Shutdown | Registry closure returns handlers and empties source map; `TestSourceConnCap_FailureAndTeardown`. |
| Source ports/XFF change; PROXY sources share socket peer | Exact extraction determines identity; `TestSourceConnCap_ClientIP`, `TestRun_SourceAllowanceProxyHTTPS`. |

## Error handling

Bad configuration returns exit 2 before background workers or listening. Source saturation is immediate 503, distinct from rate denial 429 and existing WebSocket application close codes. No new socket read or dependency is added; existing frame caps, HTTP limits, TLS and PROXY deadlines remain in force.

## Testing strategy

Write source-wrapper tests and boot/wiring assertions first, observe compilation/assertion failure, then implement. Use blocking handlers for concurrent admission, cleanup lifetime, panic release, map deletion and denial logs; real WebSockets for endpoint exits and capacity reuse. Extend existing cap harness assertions rather than copy lifecycle scenarios. Reuse local TLS/PROXY fixture for separate declared sources with one socket peer. Assert metrics remain available and 503 does not increment 429 counters. Run touched packages under race, `go vet ./...`, build; also run `make check` before push as the user explicitly requires.

## Open questions

None.

## Documentation handoff

Pending for documentation stage:
- `docs/knowledge/features/connection-cap.md`, Behaviour/Wiring/Default derivation/Out of scope: flag/default, NAT rationale, shared endpoint accounting, handler lifetime, coupled startup constraint; retire upgraded single-source gap, retain raw saturation.
- `docs/threat-model.md`, DoS resistance: upgraded source allowance mitigation; residual many-source exhaustion (including operator daemons on different addresses), raw/pre-header saturation and XFF/PROXY trust risks; future allowances distinguish raw sources.
- `docs/security-followups.md`, Per-IP concurrent connection cap: upgraded allowance shipped, raw listener slots remain unprotected per source.
- `docs/deploy.md`, fly.toml gotchas/Verify a deploy: 8-source/20-global/32-per-listener defaults on 256 MB, coupled validation and isolated non-production same-source refusal/different-source admission/recovery operator check before production deploy.
Operator owns isolated Fly validation and manual deploy; neither is performed by this build.

## Security review

**Verdict:** PASS

**Findings:**
- [Trust boundaries] `SourceConnCap.Wrap` uses only `ClientIP` with the same immutable trust setting as rate limiting; it never reads routing headers or inner frames. Payloads remain opaque.
- [Tokens, secrets and credentials] No credentials introduced; denial never inspects or reports token/header values.
- [File operations] No new runtime disk state; counts are process-local and discarded on restart.
- [Subprocesses] None introduced.
- [Cryptography] No cryptographic changes; existing TLS settings retained.
- [Network & I/O] SHOULD FIX — enforce source check before global admission and after rate limiting, with tests for empty/rate-limited sources and denial without global occupancy. Existing size/time limits bound all reads; no new socket read.
- [Errors, logs and telemetry] SHOULD FIX — quote even hostile XFF source strings with `strconv.Quote`; verify one Warn line, newline-only public response and no secrets. Reuse `remote`, without source metric labels.
- [Concurrency] SHOULD FIX — atomic check/increment, defer release through cleanup, and delete zero counts; exercise concurrent endpoints and repeated source reuse under race.
- [Threat model alignment] Single-process allowance matches registry scope. OUT OF SCOPE — raw-source saturation and XFF/PROXY spoofing trust remain deferred under the respective headings in `docs/security-followups.md`; documentation stage carries these residuals forward. No new dependency, endpoint or deploy target.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11
