# Push wake dispatch

`internal/relay/push_wake.go`'s `PushWaker` is what a `push_wake` routing
envelope from a binary turns into: a bounded, asynchronous FCM send. It sits
between [`StartBinaryForwarder`](binary-forwarder.md#push_wake-handling) (the
caller) and [`FCMSender`](fcm-push-sender.md) (the thing that actually talks
to Google), and owns daemon, device and global attempt allowances plus a
concurrent-send cap. Introduced by #133; shared allowances added by #172.

## Why a waker, not a direct call

`FCMSender.Send` makes a blocking HTTP call (bounded by `fcmSendTimeout`, but
still blocking). The binary read loop (`StartBinaryForwarder`) must not stall
on it — a slow or hung FCM send would delay every subsequent frame the
binary sends, including ordinary phone traffic. `PushWaker.Request` never
blocks on `Send`: it either admits the wake and returns immediately (the
send runs on its own goroutine) or refuses it immediately.

The relay also cannot verify that a binary's claimed device token belongs to
one of its own phones. In [daemon admission enforce](server-endpoint.md#daemon-admission-before-upgrade),
claiming a server-id first requires its exact identity-bound credential; the gate
rejects anonymous requests before upgrade, claim or push access. Off/observe
retain anonymous claims and wakes. An admitted holder, including one with a
stolen token, can still request a wake for an arbitrary device token: admission
proves daemon identity, not device-token ownership. It grants neither Noise keys
nor phone authorization. `PushWaker` is the point that accepts
this and bounds the resulting risk with shared attempt budgets, on the reasoning
that a payload-free wake (see [FCM push sender](fcm-push-sender.md)) makes
the worst case "an app reconnects more than it should," not data exposure.
See [`docs/threat-model.md` § Outbound network calls](../../threat-model.md#outbound-network-calls--fcm-push-wake)
for the full accounting.

## API

Package `internal/relay` (`push_wake.go`):

```go
type pushSender interface {
    Send(ctx context.Context, deviceToken string) (string, error)
}

type PushWaker struct { /* unexported */ }

func NewPushWaker(sender pushSender, logger *slog.Logger) *PushWaker
func NewPushWakerFromEnv(lookup func(string) (string, bool), logger *slog.Logger) (*PushWaker, error)
func (w *PushWaker) Request(serverID string, raw json.RawMessage) error
func (w *PushWaker) Close()
```

`*FCMSender` satisfies `pushSender`; tests substitute a fake
(`fakePushSender` in `push_wake_test.go`) that can block until released, so
tests can assert the binary read loop keeps forwarding while a send is still
in flight.

`NewPushWakerFromEnv` is the boot-time constructor: unset
`PYRYCODE_RELAY_FCM_CREDENTIALS` returns `(nil, nil)` — push is off, not an
error. A set value builds an `FCMSender` and wraps it; a build failure
(unreachable in practice, since `CheckEnvConfig` already validated the value
at boot — see [Env-var config validator](env-config-validator.md)) returns a
wrapped `ErrFCMCredentials` that carries none of the credential.

## `Request`: validate, admit, dispatch

`StartBinaryForwarder` calls `Request(serverID, env.PushWake)` once per
`push_wake` envelope. Three things happen in order, any of which can end the
call with an error and no send:

1. **Nil receiver.** `w == nil` means push was never configured — `Request`
   returns `ErrPushOff` immediately. The registry holds a concrete
   `*PushWaker` (not an interface), so there is no typed-nil-interface trap
   here.
2. **Structural validation.** `parsePushWake(raw)` (`envelope.go`) — platform
   must be exactly `"fcm"`, token must be non-empty. See
   [Routing envelope § `push_wake` shape](routing-envelope.md#push_wake-shape-relay-addressed).
3. **Admission, under `w.mu`:**
   - closed (`Close` already called) → `ErrPushOff`
   - refill existing daemon/device buckets and the global bucket; touch each
     existing requested key's idle timestamp, including on refusals
   - a missing daemon/device key starts as a temporary full bucket; sweep
     eligible idle state before checking whether either map is at capacity
   - a new key still cannot fit → `ErrPushWakeStateCap`
   - any allowance has no token → `ErrPushWakeRateLimited`
   - a non-blocking acquire of a `slots` token fails → `ErrPushWakeInFlightCap`
   - otherwise: reserve the slot, insert missing buckets, subtract one token
     from all three allowances, `wg.Add(1)`, spawn the send goroutine, return `nil`

A rate, state-capacity or concurrency refusal spends no tokens, reserves no
slot and inserts no new bucket. Refills, idle timestamp updates and eligible
reclamation can still occur. An admitted attempt stays charged even if
authentication or FCM delivery fails; completing a send returns only its slot.
A refused wake is never queued or retried — the caller logs it and moves on.
An admitted wake's send runs on its own goroutine against `w.ctx` (cancelled
by `Close`, not the binary connection's context), so a wake requested just
before the binary disconnects still goes out.

## Allowances and concurrency cap

All allowances start full. Burst is the maximum stored credit, not a
per-refill-window quota. Refill adds integer tokens at the following rates,
preserves elapsed-time remainder and stops at burst capacity:

| Constant | Burst | Refill | Scope |
|---|---|---|---|
| `pushWakeBurst` / `pushWakeRefillEvery` | 6 | one token per 10s | per daemon server-id, across its devices |
| `pushWakeDeviceBurst` / `pushWakeDeviceRefillEvery` | 6 | one token per 10s | per complete device token, across all daemons |
| `pushWakeGlobalBurst` / `pushWakeGlobalRefillEvery` | 32 | one token per second | all daemons and devices in the relay process |

`pushWakeMaxInFlight` separately retains the 32-send concurrency cap via a
buffered channel. Freed slots do not replenish attempt allowances. Without
clock advance, the seventh wake to one device is refused even across fresh
daemon identities, and the 33rd wake to distinct devices across identities is
refused even if previous sends have completed. Reconnecting resets neither
daemon, device nor global credit.

With full production allowances and an available sender, four daemons can
each issue two back-to-back wakes to each of three paired devices (devices
distinct across daemons): 24 wakes, charging six per daemon, two per device
and 24 globally. The burst accommodates a finished turn and a waiting prompt
for three devices; sustained daemon/device refill remains one per 10s, while
aggregate refill is one per second.

Push accounting is local to `PushWaker`, rather than successive
[`IPRateLimiter.Allow`](rate-limit-middleware.md) calls: separate calls cannot
reserve three budgets and a slot atomically. Daemon and device maps each hold
at most `pushWakeMaxBuckets` (1,024) buckets keyed by full 32-byte SHA-256
digests of the complete inputs. Accounting-key size is independent of input
length. The truncated `tokenFingerprint` used in logs is unsuitable for
accounting because distinct tokens can share it. Raw tokens do not enter
accounting state; admitted sends retain them for delivery. Accounting digests
are never logged.

Keyed state becomes eligible for expiry after five minutes without a request,
including refused requests touching existing keys. It is reclaimed by the
following five-minute sweep (`pushWakeEvictionInterval`) or when a request
needs a new key, and only if fully refilled at that time. Depleted or active
state is never evicted to make room, so removal cannot erase debt. At either
map's capacity, requests needing a new key are refused unless eligible state
can first be reclaimed; existing keys remain usable within their allowances.
The global bucket never expires.

All modes (off, observe and enforce) receive these limits when push is
configured. Budgets are in-memory for the single relay process and reset on
restart; multiple replicas would need shared accounting. Only enforce
authenticates daemon identities. Device-token ownership remains unverified,
and wakes remain payload-free under the
[wire protocol](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md).

## Error handling

| Error | Meaning | Carries |
|---|---|---|
| `ErrPushOff` | push not configured, or `Close` already called | nothing |
| `ErrMalformedPushWake` / `ErrUnsupportedPushPlatform` / `ErrEmptyPushToken` | `parsePushWake` rejection | nothing (see [Routing envelope](routing-envelope.md)) |
| `ErrPushWakeRateLimited` | daemon, device or global allowance exhausted | nothing — not even which allowance |
| `ErrPushWakeStateCap` | a new daemon/device key cannot fit after safe reclamation | nothing |
| `ErrPushWakeInFlightCap` | relay-wide 32 in-flight sends already running | nothing |

No sentinel here, and no `FCMSender` error surfaced by the send goroutine,
ever carries the device token or the service-account credential. The
forwarder logs a refusal as `binary_forwarder_push_wake_dropped`, using only
`server_id` (or `binary_conn_id`) and `err`.

Every wake *admitted* by `Request` logs exactly one outcome line from `send`
(#160), including a send cancelled by `Close` — the cancelled context makes
`Send` return an error, so it takes the failure branch with no special case:

| Line | Level | Keys |
|---|---|---|
| `push_wake_sent` | Info | `server_id`, `token_fp`, `duration`, and `fcm_message_id` when the reply carried a valid one |
| `push_wake_send_failed` | Warn | `server_id`, `token_fp`, `duration`, `err` |

`token_fp` (`tokenFingerprint`, `push.go`) is the first 8 hex characters of
the device token's SHA-256 — stable per token, one-way, never the token
itself. `duration` is the elapsed time of the `Send` call. All three keys
are in `allowedLogKeys` (`internal/relay/log_allowlist.go`), each with a
comment explaining why it's safe to log.

## Metrics

`internal/relay/metrics_push.go`'s `NewPushMetrics(reg, w)` registers
`pyrycode_relay_push_wakes_total{outcome}` with exactly five pre-bound label
values, all exposed at 0 from boot: `sent`, `unregistered`, `send_failed`,
`token_fetch_failed`, `dropped`. It follows the `NewGraceMetrics` hook shape
([Frame-forwarded and grace-expiry counters](frame-and-grace-counters.md)):
prometheus stays out of `push_wake.go`, and `setOutcomeHook` installs a
nil-safe hook that `record` calls. Introduced by #161, after a 2026-10-03
incident where server-id `d12a1a37` got `UNREGISTERED` from FCM four times in
20 minutes and only a log line recorded it.

- **`dropped`** — increments exactly once when `Request` refuses a wake for
  any daemon/device/global allowance (`ErrPushWakeRateLimited`), either keyed
  map's capacity (`ErrPushWakeStateCap`) or concurrency (`ErrPushWakeInFlightCap`).
  A malformed `push_wake` object and a push-off refusal (nil or closed
  waker) count nothing — they never reach admission, so they are not a
  "drop" in this counter's sense.
- **`sent` / `unregistered` / `send_failed` / `token_fetch_failed`** — exactly
  one increments per admitted wake, once its `send` goroutine's call to
  `Send` returns. `classifyPushWake(err)` maps the result by error identity,
  never by matching text: `nil` → `sent` (with or without a message id in
  the reply); `errors.Is(err, ErrFCMUnregistered)` → `unregistered`;
  `errors.Is(err, ErrFCMTokenFetch)` → `token_fetch_failed`, including
  cancellation or expiry while waiting for authentication; anything else
  (another FCM error, a transport failure, or cancellation at send entry,
  after token lookup or during FCM delivery) → `send_failed`. Cancellation
  by `Close` follows the phase where it is observed, rather than always
  counting as `send_failed`.

[`ErrFCMUnregistered`](fcm-push-sender.md) is the sentinel `FCMSender.Send`
now also wraps, alongside the existing `ErrFCMSend`, when FCM's reply's
`errorCode` is `UNREGISTERED` — so `errors.Is(err, ErrFCMSend)` still
matches and the error's text is unchanged; only the classification gains a
second, checkable identity.

The five label values are hard-coded constants from `pushWakeOutcome` — none
carries a server id, a device token, a fingerprint or any text from Firebase's
reply, so the series count is fixed at five regardless of traffic.

Wired in `main` right after `NewUpgradeMetrics`, before any listener serves
— see § Wiring. Push off (`pushWaker == nil`) means no
`pyrycode_relay_push_wakes_total` series at all, not five series stuck at
zero.

## Concurrency model

- One goroutine per admitted wake, capped at `pushWakeMaxInFlight` by the
  buffered `slots` channel. Each goroutine exits when `Send` returns —
  using one whole-send `fcmSendTimeout` budget in production, and honouring
  `w.ctx` cancellation on `Close`. Token waiting consumes delivery time;
  authentication does not grant the FCM POST a fresh budget. Shared refresh
  work runs separately, with cancellation owned by its active waiters (see
  [FCM sender concurrency](fcm-push-sender.md#concurrency)). Each send logs
  its one outcome line before `wg.Done`
  (deferred first, so it runs last) — `Close` returning therefore means
  every admitted wake's outcome line has already been written (#160).
- `w.mu` owns all buckets, their refill/idle timestamps, sweeping and the
  admission commit. The closure check, all three charges, non-blocking slot
  reservation and `wg.Add` are atomic against other requests and `Close`.
  Concurrent requests cannot overspend, and none is admitted after the closed
  transition. No mutex is held across the blocking `Send` call.
- One sweep goroutine runs every five minutes, exits on `w.ctx` cancellation
  and has its own `sweepWG`. The injected clock used by budget tests is read
  under `w.mu`, as are the timestamps it supplies.
- `Close` is idempotent: set `closed` under `w.mu`, cancel `w.ctx`,
  `wg.Wait()` for every admitted send to return and `sweepWG.Wait()` for the
  sweep goroutine. Repeated and concurrent callers all join admitted work;
  none returns early merely because another caller marked the waker closed.
  With `FCMSender`, the last waiter cancels and joins unfinished token
  refresh work before its send returns, so the wait group joins cancellable
  authentication as well as FCM delivery.
  Hijacked WebSocket connections outlive `http.Server.Shutdown`, so a
  forwarder can still call `Request` after `Close` — it gets `ErrPushOff`
  rather than a panic or a send racing shutdown.

`TestPushWaker_CloseCancelsAuthentication` uses a real sender and six admitted
wakes sharing a held-open local authentication request. Its two-second
push-cleanup budget starts at entry to `Close`: it checks joined refresh
completion, endpoint cancellation, six outcome lines, no further
authentication/FCM requests and `ErrPushOff` after close. This is a regression
test budget, not a total process-shutdown deadline. `runServers` separately
allows `drainDeadline` (10s) for HTTP/WebSocket draining; the deferred waker
close runs after that drain.

## Wiring (`main.go`)

```go
pushWaker, err := relay.NewPushWakerFromEnv(os.LookupEnv, logger)
if err != nil {
    logger.Error("refusing to start: cannot build the FCM push sender", "err", err)
    return 2
}
if pushWaker == nil {
    logger.Info("push off: PYRYCODE_RELAY_FCM_CREDENTIALS unset")
} else {
    reg.SetPushWaker(pushWaker)
    defer pushWaker.Close()
}
```

`Registry.SetPushWaker` (`registry.go`) is the same "set once at boot,
`nil` = off" optional-collaborator shape as `SetForwarderHooks` — see
[Connection registry](connection-registry.md). `StartBinaryForwarder` reads
`reg.pushWaker` directly (an unexported field on the same package), so no
interface or extra parameter was threaded through the forwarder's existing
signature.

The waker is built here, before `metricsReg` exists. Its metrics hook is
installed later, in the same `main.go` block as the other counters:

```go
upgradeMetrics := relay.NewUpgradeMetrics(metricsReg)
if pushWaker != nil {
    relay.NewPushMetrics(metricsReg, pushWaker)
}
```

Guarding on `pushWaker != nil` keeps "with push on" (the AC's condition)
structural — push off means `NewPushMetrics` is never called, not called
against a nil-op waker. Both wiring points still run before any listener
serves, so no `Request` can reach the waker before its hook is in place.

## Testing

`push_wake_budget_test.go` uses a controlled clock and fake sender for shared
device/global exhaustion after slots free, exact refill boundaries and capped
credit, the 24-wake paired-device burst, state saturation and safe idle
reclamation, failed-send charging, concurrent admission and admission racing
concurrent `Close`. Refusals must preserve all token balances, map keys and
slots (apart from time-based refill/reclamation). Zero sender calls alone would
miss consuming a daemon token before discovering a full semaphore. The
complete-token fixture deliberately includes
two distinct tokens sharing a log fingerprint; ordinary distinct-token tests
would also pass with the incorrect truncated accounting key.

`daemon_admission_test.go` retains the pre-upgrade rejection proof and checks
initial admitted wakes plus reconnects without resetting allowances. These
tests are hermetic; repository checks establish neither deployment nor live
admission enforcement. Later retries (#173) must consume these same attempt
budgets.

`push_wake_test.go`: burst exhausted for one server-id → `ErrPushWakeRateLimited`
while a different server-id is still admitted; a blocking fake sender fills
`maxInFlight` → the next `Request` → `ErrPushWakeInFlightCap`, and releasing
one send readmits the next; a nil `*PushWaker` → `ErrPushOff`; `Close`
cancels a blocked send, waits for it, and a `Request` afterward →
`ErrPushOff`; `NewPushWakerFromEnv` returns `(nil, nil)` when unset and a
non-nil waker for a valid key. Since #160: exactly one outcome line for each
of success-with-id, success-without-id, failure and cancelled-by-`Close`,
each checked for its keys and for the raw token's absence; and an
end-to-end run of a real `FCMSender` through `PushWaker` against an
httptest FCM server, proving neither the device token, the access token,
nor FCM reply text beyond a validated id ever reaches the log — including a
hostile `name` and a 404 error body that both echo a marker and the tokens.

`forward_test.go` covers the forwarder's side of the contract: a wake with a
blocking sender followed by an ordinary `conn_id` frame proves the phone
still receives the frame while the send is still blocked (the loop never
stalls); every refusal case (unsupported platform, empty token, malformed
object, push off) logs `binary_forwarder_push_wake_dropped`, sends nothing,
and leaves a later frame still deliverable; a `conn_id` envelope that also
carries a `push_wake` is forwarded to the phone exactly as before, with no
send triggered. Captured logs across every case are asserted to never
contain the token string.

`metrics_push_test.go` (#161) scrapes a fresh registry per case and asserts
all five outcome cells together (the one expected, the rest at 0), plus that
the scrape body contains no server id or token. A real `FCMSender` against
in-test OAuth and FCM servers drives the four admitted-wake outcomes off the
errors `Send` actually returns (2xx with and without an id, `UNREGISTERED`,
another FCM error, a failed token fetch); a fake sender covers
cancellation-by-`Close` (→ `send_failed`), rate-limit and in-flight-cap
drops, and that a malformed object or a push-off `Request` counts nothing.
**A real-`FCMSender` case cannot scrape right after `PushWaker.Close`**:
`Close` cancels the in-flight send, potentially recording `token_fetch_failed`
or `send_failed` instead of the intended endpoint outcome — a version of the
test that scraped only after `Close` would have passed while classifying
nothing correctly. It instead polls for the send's one outcome log line
(recorded before the line is logged, so the poll and the counter agree) and
closes the waker afterward. The fake-sender cases, which need no real HTTP
round trip, still scrape right after `Close`.

## Related

- [Daemon admission](server-endpoint.md#daemon-admission-before-upgrade) — enforce narrows wake abuse to admitted credential holders; limits and unverified device-token risk remain.
- [Binary-side frame forwarder](binary-forwarder.md#push_wake-handling) — the
  sole caller.
- [Routing envelope](routing-envelope.md#push_wake-shape-relay-addressed) —
  the wire shape and `parsePushWake`'s structural checks.
- [FCM push sender](fcm-push-sender.md) — what `Send` actually does.
- [Connection registry](connection-registry.md) — `SetPushWaker` / the
  `pushWaker` field's lifecycle rules.
- [Rate-limit middleware](rate-limit-middleware.md) — separate upgrade-path
  `IPRateLimiter`; push uses local accounting for atomic admission and key caps.
- [`docs/threat-model.md` § Outbound network calls](../../threat-model.md#outbound-network-calls--fcm-push-wake) —
  the abuse accounting.
- [Codebase note #133](../codebase/133.md) — implementation summary and
  lessons.
