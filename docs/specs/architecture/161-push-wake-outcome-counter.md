# #161 — Count push wakes by outcome

## Files read

- `internal/relay/push_wake.go` → `PushWaker`, `Request`, `send`, `Close`: the admission path (rate limit, in-flight cap) and the one-outcome-per-admitted-wake send goroutine #160 shaped. The hook fires from the same two places.
- `internal/relay/push.go` → `FCMSender.Send`, `fcmErrorReason`, `ErrFCMSend`, `ErrFCMTokenFetch`: where `UNREGISTERED` must become an error identity instead of text only.
- `internal/relay/metrics_grace.go` → `NewGraceMetrics`: the hook shape to follow (constructor registers, installs `func` hook on the source; prometheus stays out of the source file).
- `internal/relay/metrics_upgrade.go` → `NewUpgradeMetrics`: the labelled-vector shape with every label value pre-bound at boot and hard-coded.
- `internal/relay/metrics_counters_test.go` → `assertCounter`: scrape assertion helper reused by the new test.
- `internal/relay/push_wake_test.go` → `fakePushSender`, `wakeJSON`, `discardLogger`; `internal/relay/push_test.go` → `fakeOAuth`, `testServiceAccountJSON`, `TestFCMSender_NonSuccessReplyNamesFCMReason`.
- `cmd/pyrycode-relay/main.go` → push block and `metricsReg` block: the waker is built before `metricsReg` exists.
- `docs/knowledge/features/push-wake-dispatch.md` § Error handling, § Concurrency model: `Close` returning means every admitted wake's outcome has been recorded (`wg.Done` runs last) — the metrics test relies on this to scrape without polling.
- `docs/threat-model.md` push section: names "a metric on dropped/refused wakes" as future hardening; this ticket delivers it.

No overlapping in-flight branches.

## Context

On 2026-10-03 server id d12a1a37 got `UNREGISTERED` four times in 20 minutes and only a log line saw it. The operator wants `/metrics` to show dead tokens, failed sends and relay-side drops. No decision record needed: this follows the existing counter pattern.

## Design

**Error identity (`push.go`).** New sentinel `ErrFCMUnregistered`. `fcmErrorReason` additionally reports whether the FcmError detail's `errorCode` is `UNREGISTERED` (signature becomes `fcmErrorReason(reply []byte) (reason string, unregistered bool)`; one caller). On that branch `Send` returns an unexported `fcmUnregisteredError` that wraps the existing `fmt.Errorf("%w: status %d (%s)", ErrFCMSend, ...)` error, keeps its exact `Error()` text, and `Unwrap() []error` yields both the inner error and `ErrFCMUnregistered`. So `errors.Is(err, ErrFCMSend)` and `errors.Is(err, ErrFCMUnregistered)` both hold, and log text is unchanged. Detection uses the parsed, enum-validated `errorCode` only — the same field the reason string already exposes, not the HTTP status and not `error.message`.

**Hook (`push_wake.go`).** Unexported `type pushWakeOutcome int` with five constants: `pushWakeSent`, `pushWakeUnregistered`, `pushWakeSendFailed`, `pushWakeTokenFetchFailed`, `pushWakeDropped`. `PushWaker` gains field `onOutcome func(pushWakeOutcome)` set by unexported `setOutcomeHook(fn)` (boot-time only, nil = no-op; the only caller is in-package). A private `record(o)` calls the hook if non-nil.

- `Request`: on `ErrPushWakeRateLimited` and `ErrPushWakeInFlightCap` refusals, `record(pushWakeDropped)`. Nil waker, malformed object and closed waker record nothing.
- `send`: after `Send` returns, `classifyPushWake(err) pushWakeOutcome`: nil → sent; `errors.Is ErrFCMUnregistered` → unregistered; `errors.Is ErrFCMTokenFetch` → token_fetch_failed; anything else (other FCM error, transport, cancelled by `Close`) → send_failed. Recorded once, alongside the existing log line.

**Metrics (`metrics_push.go`, new).** `NewPushMetrics(reg prometheus.Registerer, w *PushWaker)` registers `pyrycode_relay_push_wakes_total{outcome}` and pre-binds all five label values (`sent`, `unregistered`, `send_failed`, `token_fetch_failed`, `dropped`) from a fixed `[...]string` indexed by `pushWakeOutcome`, then installs the hook as an index into the pre-bound counters. Only one new exported identifier (`NewPushMetrics`) plus one exported sentinel.

**Wiring (`main.go`).** After the existing `NewUpgradeMetrics` line: `if pushWaker != nil { relay.NewPushMetrics(metricsReg, pushWaker) }`. That is still before any listener serves, so no `Request` can race the hook install. Push off → no push series, matching "with push on" in the AC.

## Concurrency model

No new goroutines. The hook field is written once at boot before any forwarder runs (same rule as `SetGraceExpiryHook`) and read by `Request` (under `w.mu`) and by `send` goroutines. Prometheus counters are concurrency-safe. The hook must not call back into the `PushWaker`; the metrics hook only does `Counter.Inc`. Shutdown unchanged: `send` records before its deferred `wg.Done`, so `Close` returning means all outcomes are counted.

## Error handling

No new failure modes. A `pushWakeOutcome` outside the five constants cannot be produced (all values come from in-package constants); the metrics hook indexes a fixed-size array keyed by those constants.

## Testing strategy

- `push_test.go`: add an `unregistered bool` column to `TestFCMSender_NonSuccessReplyNamesFCMReason`; assert `errors.Is(err, ErrFCMUnregistered)` matches it while the existing exact-text and `ErrFCMSend` assertions stay unchanged (proves text and the `ErrFCMSend` wrap are preserved). True only for the standard `UNREGISTERED` body; false for `errorCode under another type` and non-enum values.
- `metrics_push_test.go` (new), each case on its own registry, scraping after `w.Close()`, asserting all five cells (the one expected at its count, the rest at 0):
  - real `FCMSender` against an in-test FCM server: 2xx with id → sent; 2xx without id → sent; 404 `UNREGISTERED` → unregistered; 400 `INVALID_ARGUMENT` → send_failed; token endpoint 401 → token_fetch_failed.
  - fake sender blocked + `Close` → send_failed (cancelled).
  - rate limit (burst 1, two requests) → sent 1, dropped 1; in-flight cap (cap 1, blocked sender, second server-id) → dropped 1, then send_failed 1 after `Close`.
  - malformed object and request after `Close` → all zero.
  - fresh metrics with no requests → all five series exposed at 0.
- Existing `PushWaker` tests run with no hook (nil no-op), unchanged.

## Open questions

None.

## Documentation handoff

Pending for the documentation stage:

- `docs/knowledge/features/push-wake-dispatch.md`: a Metrics section naming `pyrycode_relay_push_wakes_total{outcome}`, its five values and when each increments (dropped from `Request` on rate-limit / in-flight-cap refusal; the other four once per admitted wake when `Send` returns; malformed and push-off refusals count nothing).
- `docs/knowledge/INDEX.md`: one line pointing at it.
- `docs/knowledge/features/fcm-push-sender.md`: `ErrFCMUnregistered` alongside the other sentinels.
- `docs/threat-model.md` push section: the "no metric exists yet" residual-risk sentence and the "metric on dropped/refused wakes" future-hardening item are now delivered.

## Revisions

- 2026-10-03 (Phase B): the real-`FCMSender` metrics cases cannot scrape "after `w.Close()`", because `Close` cancels the in-flight send and turns every case into `send_failed`. They instead poll for the send's one outcome log line (recorded before the line is logged) and close afterwards. The fake-sender cases still scrape after `Close`. Design unchanged.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the only new input consulted is FCM's reply, already parsed by `fcmErrorReason` into enum-validated fields (`isFCMEnum`); the new check is an equality test on that validated `errorCode`. No binary payload is read: `Request` still parses only the `push_wake` object it already parsed, and the hook fires after that existing parse.
- [Tokens & secrets] No findings — the outcome label is chosen from five compile-time constants via `pushWakeOutcome`; no server id, device token, token fingerprint or Firebase text reaches a label. `fcmUnregisteredError.Error()` returns the inner error's existing text, which already carries no token.
- [File operations] No findings — no files.
- [Subprocesses] No findings — none.
- [Cryptography] No findings — none added.
- [Network & I/O] No findings — no new reads, endpoints or sockets; `/metrics` stays on the loopback metrics listener. Series cardinality is fixed at five regardless of traffic, so a hostile binary spamming wakes cannot grow the metrics surface.
- [Errors, logs, telemetry] No findings — no new log keys, so `log_allowlist.go` is untouched; existing log lines unchanged in text. Metrics carry no user-identifiable data. SHOULD FIX (implementation check): the metrics test must assert the scrape body contains no server id or token from the test, so a future label regression reddens.
- [Concurrency] No findings — hook installed once at boot before serving; invoked under `w.mu` in `Request` and lock-free in `send`; the hook only increments a counter and never re-enters `PushWaker`, so no lock-order change.
- [Threat model alignment] No findings — no new dependency, endpoint or deploy change. Delivers the "metric on dropped/refused wakes" future hardening in `docs/threat-model.md`; recorded in the Documentation handoff. Telling the daemon a token is dead stays OUT OF SCOPE (a protocol change on `pyrycode/pyrycode`, named in the ticket).

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-03
