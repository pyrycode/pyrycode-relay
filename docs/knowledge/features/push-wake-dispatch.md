# Push wake dispatch

`internal/relay/push_wake.go`'s `PushWaker` is what a `push_wake` routing
envelope from a binary turns into: a bounded, asynchronous FCM send. It sits
between [`StartBinaryForwarder`](binary-forwarder.md#push_wake-handling) (the
caller) and [`FCMSender`](fcm-push-sender.md) (the thing that actually talks
to Google), and owns daemon, device and global attempt allowances plus a
retained-device-job cap. Introduced by #133; shared allowances added by
[#172](https://github.com/pyrycode/pyrycode-relay/issues/172), bounded
coalescing by #211, bounded transient retries by #212, correlated invalid-token
feedback by #213 and joined cancellation
by [#169](https://github.com/pyrycode/pyrycode-relay/issues/169).

## Why a waker, not a direct call

`FCMSender.Send` makes a blocking HTTP call (bounded by `fcmSendTimeout`, but
still blocking). The binary read loop (`StartBinaryForwarder`) must not stall
on it — a slow or hung FCM send would delay every subsequent frame the
binary sends, including ordinary phone traffic. `PushWaker.Request` never
blocks on `Send`: it either starts one asynchronous job, accepts a duplicate
without another send, or refuses new work immediately. Delivery is
payload-free and best-effort, with bounded transient retries inside the job;
valid opted-in requests may receive sanitised invalid-token feedback. Neither
wakes nor feedback have a durable delivery guarantee.

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

The public `Request(serverID, raw)` remains source-compatible and supplies no
feedback destination. Production `StartBinaryForwarder` calls the private
origin-aware `request` method with a captured `pushWakeOrigin`: the original
writer and its connection lifetime, independent of registry lookups.

The canonical outer request and result follow the published
[correlated wake opt-in](https://github.com/pyrycode/pyrycode/blob/d400bc3a4ad7e8bba3fffd6be53fc025e7c1f002/docs/protocol-mobile.md#correlated-wake-opt-in)
and [result contract](https://github.com/pyrycode/pyrycode/blob/d400bc3a4ad7e8bba3fffd6be53fc025e7c1f002/docs/protocol-mobile.md#push_wake_result):

```json
{"push_wake":{"platform":"fcm","token":"<fcm-device-token>","request_id":"wake-A-1"}}
```

```json
{"push_wake_result":{"request_id":"wake-A-1","code":"invalid_token"}}
```

`request_id` opts in only when it is 1–64 ASCII bytes from `[A-Za-z0-9_-]`.
The daemon must generate token-independent IDs, fresh for each request on a
connection; they must encode no token, hash, credential or application content.
Omitted, empty, overlong, non-ASCII, disallowed-character, null or wrong-typed
IDs disable feedback without changing ordinary wake admission/coalescing or
closing the connection. Malformed JSON retains ordinary rejection. Unknown
fields remain tolerated; a phone-addressed envelope retains its ordinary routing
even if it also carries `push_wake`.

Only terminal, uncancelled `ErrFCMUnregistered` permits the result above.
It contains only the accepted ID and fixed `invalid_token` code (13 ASCII
bytes), with no `conn_id`, `frame`, token or Noise sealing. It travels only
to the captured daemon `/v1/server` connection, never to phones. Success,
other 4xx, authentication failure, transient failure/exhaustion, cancellation
and admission rejection produce no invalid result. Silence says nothing
about a registration. Old wake-capable relays ignore the new field; pre-wake
relays may drop the request. Neither supplies feedback; no capability
handshake is added.

`NewPushWakerFromEnv` is the boot-time constructor: unset
`PYRYCODE_RELAY_FCM_CREDENTIALS` returns `(nil, nil)` — push is off, not an
error. A set value builds an `FCMSender` and wraps it; a build failure
(unreachable in practice, since `CheckEnvConfig` already validated the value
at boot — see [Env-var config validator](env-config-validator.md)) returns a
wrapped `ErrFCMCredentials` that carries none of the credential.

## `Request`: validate, admit, dispatch

`StartBinaryForwarder` calls `request(serverID, env.PushWake, origin)` once per
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
   - an active job for the complete device token, or one less than ten
     seconds after its first admission → try to bind eligible feedback,
     return `nil` without another send
   - reclaim expired completed jobs; retained jobs at capacity →
     `ErrPushWakeInFlightCap`
   - refill existing daemon/device buckets and the global bucket; touch each
     existing requested key's idle timestamp, including on refusals
   - a missing daemon/device key starts as a temporary full bucket; sweep
     eligible idle state before checking whether either map is at capacity
   - a new key still cannot fit → `ErrPushWakeStateCap`
   - any allowance has no token → `ErrPushWakeRateLimited`
   - a non-blocking acquire of a `slots` token fails → `ErrPushWakeInFlightCap`
   - otherwise: reserve the slot, insert missing buckets, subtract one token
     from all three allowances, insert the active job with a fixed ten-second
     coalescing expiry, capture an immutable 45-second deadline from admission,
     `wg.Add(1)`, spawn the job goroutine, return `nil`

The fresh-job admission also tries to bind eligible feedback under the same
mutex. Feedback refusal never reverses ordinary admission or coalescing.

Coalescing crosses daemon identities and applies regardless of the send's
success or failure. Duplicates never extend the admission window or job
deadline, touch accounting timestamps, charge allowances, create daemon
buckets or emit outcomes. They neither reset attempts nor schedule follow-on
provider work, including during retry backoff. They succeed even when allowances,
accounting maps or retained-job capacity would refuse new work. Each distinct
eligible `(original connection, request_id)` may attach one bounded record,
including across daemon identities. Active repeats allocate nothing and renew
no deadline; equal IDs on different connections are independent.
At the exact ten-second boundary a completed job becomes eligible for
ordinary admission; an active job still suppresses duplicates beyond it.
Different complete device tokens remain independent.

A fresh rate, state-capacity or retained-capacity refusal spends no tokens,
reserves no slot and inserts no new bucket. Refills, idle timestamp updates
and eligible reclamation can still occur after the retained-capacity check. An admitted
attempt stays charged even if authentication or FCM delivery fails;
job completion frees its reserved slot but retains completed metadata only
until the original ten-second window expires. Completion after that window
permits ordinary fresh admission, without refunding allowance debt.
A refused fresh wake is never queued — the caller logs it and moves on.
An admitted job runs on its own goroutine with a deadline context descending
from `w.ctx` (cancelled by `Close`, not the binary connection's context), so
disconnecting the requesting binary does not cancel provider delivery.

Feedback has separate, shared record caps: **128 per original connection,
64 per retained job and 2048 relay-wide**. Pending subscriptions, queued writes
and seen/deduplication metadata all consume the same allowances continuously.
Overflow silently declines only the new binding, preserving accepted records
and ordinary wakes. Every binding expires no later than **60 seconds after
acceptance**, independently of the accounting sweep; repeats/coalescing renew
neither lifetime nor job window.

Each accepted binding gets at most one result attempt in its retained job,
including after a failed write. Before the original ten-second window ends,
a new eligible ID may receive cached invalid feedback without another provider
call; a repeated ID receives none. Completed cache state contains only the
sanitised outcome and bounded correlation/deduplication metadata, never a raw
token. Evict once both provider completion and the original window have occurred.
Late completion may deliver to existing eligible subscribers but opens no new
window. Already-queued original deliveries stay charged to their original job
and connection after cache eviction until delivery completes or is cancelled;
they accept no new subscribers and cannot restore the cache. Eviction,
disconnect and restart grant no replay; a fresh job is a separate ordinary wake.

Delivery runs off the binary read loop and has **five seconds total**, including
queue scheduling and writer wait, starting at job completion for pending
bindings or acceptance for cached results. Subscription expiry, connection
cancellation or shutdown can end it sooner. Disconnect/replacement drops the
original bindings; a reconnect with the same server-id receives no stale result.

Each job invokes `Send` at most three times within its immutable 45-second
budget, covering token waiting, authentication, delivery and retry waits.
Each invocation retains the sender's whole-call 10s cap, limited by the
earlier job deadline. Only the sender's sanitised transient classification
permits another call; see [FCM retry policy](fcm-push-sender.md#retry-classification-and-guidance).
A call's own timeout may retry while the job remains live; job expiry or
cancellation stops it.

After failed attempts 1 and 2, ordinary backoff is respectively 10s and 20s
from call completion, plus an independently sampled jitter in [0, 1s].
The next attempt uses the later of that time and the sender's receipt-anchored
`Retry-After` guidance. A next-attempt time at or beyond the job deadline
ends the job immediately, without an early send. HTTP 429's minimum 60s
wait cannot fit this 45s job. Permanent failure, attempt exhaustion or
cancellation also ends it without scheduling another job.

## Allowances and concurrency cap

All allowances start full. Burst is the maximum stored credit, not a
per-refill-window quota. Refill adds integer tokens at the following rates,
preserves elapsed-time remainder and stops at burst capacity:

| Constant | Burst | Refill | Scope |
|---|---|---|---|
| `pushWakeBurst` / `pushWakeRefillEvery` | 6 | one token per 10s | per daemon server-id, across its devices |
| `pushWakeDeviceBurst` / `pushWakeDeviceRefillEvery` | 6 | one token per 10s | per complete device token, across all daemons |
| `pushWakeGlobalBurst` / `pushWakeGlobalRefillEvery` | 32 | one token per second | all daemons and devices in the relay process |

`pushWakeMaxInFlight` caps retained device jobs at 32, including completed
jobs still inside their window, and bounds active sends via a buffered
channel. Active jobs keep their slot through retry waits, so at most 32
simultaneous sends and 32 retained jobs exist. Tests can supply a smaller
constructor capacity. At capacity a new distinct device returns
`ErrPushWakeInFlightCap`, charges nothing, queues nothing and counts one
`dropped` outcome; an existing coalesced device still
returns `nil`. Expired completed jobs are reclaimed before capacity checks,
on completion after expiry and by the existing periodic sweep. Job expiry
never removes allowance debt. Freed send slots do not replenish allowances
or free completed jobs still in their window. Reconnecting resets neither
daemon, device nor global credit.

Every sender invocation atomically consumes one token from each allowance,
including authentication failures and retries. Retries keep the first
admitting daemon's identity and the complete-device key; a coalesced requester
cannot substitute its credit. An exhausted allowance or accounting-capacity
refusal stops the retry with one `dropped` outcome, without partial charging,
refunds or waiting for refill. Retries reuse the job's reserved slot.

With full production allowances and an available sender, four daemons can
each issue two back-to-back wakes to each of three paired devices (devices
distinct across daemons): 24 accepted requests produce 12 actual sends,
charging three per daemon, one per device and 12 globally before any retries.
A finished turn and a waiting prompt for the same device within the window
share a wake.
Sustained daemon/device refill remains one per 10s, while aggregate refill
is one per second. Coalescing and device refill both take ten seconds, so
fresh same-device admissions alone cannot exhaust production device credit;
each retry still consumes device credit.

Push accounting is local to `PushWaker`, rather than successive
[`IPRateLimiter.Allow`](rate-limit-middleware.md) calls: separate calls cannot
reserve three budgets and a slot atomically. Daemon and device maps each hold
at most `pushWakeMaxBuckets` (1,024) buckets keyed by full 32-byte SHA-256
digests of the complete inputs. Accounting-key size is independent of input
length. The truncated `tokenFingerprint` used in logs is unsuitable for
accounting because distinct tokens can share it. Raw tokens do not enter
accounting state; active job goroutines retain them through delivery/backoff.
Device jobs use the same full digest and retain expiry/completion signals,
the sanitised outcome and bounded origin/ID records. Completed jobs retain no
raw token; feedback records reference their original connection, not a daemon
identity lookup. Accounting and job digests are never logged.

Accounting state becomes eligible for expiry after five minutes without a
request reaching allowance checks; refusals at that stage touch existing
keys, while duplicates and retained-capacity refusals do not. It is reclaimed
by the following five-minute sweep (`pushWakeEvictionInterval`) or when a
request needs a new key, and only if fully refilled at that time. Depleted
accounting state is never evicted to make room, so removal cannot erase debt.
At either map's capacity, requests needing a new key are refused unless eligible state
can first be reclaimed; existing keys remain usable within their allowances.
The global bucket never expires.

All modes (off, observe and enforce) receive these limits when push is
configured. Budgets and coalescing are in-memory for the single relay process
and reset on restart; multiple replicas would need shared accounting and
coalescing. There is no durable or exactly-once guarantee. Only enforce
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
| `ErrPushWakeInFlightCap` | retained-device-job capacity reached (32 in production), or no send slot available | nothing |
| `ErrPushResultFailed` | private WS feedback write failed, expired or was cancelled; discarded by the worker | static text only, no transport cause |

No sentinel here, and no `FCMSender` error surfaced by the send goroutine,
ever carries the device token or the service-account credential. The
forwarder logs a refusal as `binary_forwarder_push_wake_dropped`, using only
`server_id` (or `binary_conn_id`) and `err`.

Invalid correlations, repeats and feedback-cap overflow add no error, log or
metric. Feedback writes discard the static adapter sentinel; results and
feedback diagnostics expose no raw device/admission/access tokens, token hashes,
OAuth credentials, provider reply text or opaque inner frames. No feedback
labels or logs are added. Existing provider-attempt diagnostics below retain
their sanitised fingerprint/message-ID fields and five outcome labels.

A provider error can match `ErrFCMUnregistered` alongside a trailing-read
`context.Canceled` or `context.DeadlineExceeded`. Either context sentinel
suppresses feedback even if the parent job remains live; the existing provider
outcome/log classification is preserved. Feedback is advisory relay testimony,
not proof of device ownership or a generic delivery receipt. The
[Core #3423 family](https://github.com/pyrycode/pyrycode/issues/3423) owns consuming it
and atomically clearing only a still-matching registration snapshot, preserving
pairings and replacements. See the
[coordinated release check](../../deploy.md#coordinated-push-result-release-check)
before activation.

Every sender invocation logs exactly one outcome line from `send`
(#160), attributed to its first admitting daemon. Accepted duplicates log
nothing. A send cancelled by `Close` also logs its outcome — the cancelled
context makes `Send` return an error, so it takes the failure branch:

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
  map's capacity (`ErrPushWakeStateCap`) or retained-job/send capacity
  (`ErrPushWakeInFlightCap`), or when allowance/accounting-capacity checks
  refuse a retry. A retry refusal adds no sender invocation or send log line.
  A malformed `push_wake` object and a push-off refusal (nil or closed
  waker) count nothing — they never reach admission, so they are not a
  "drop" in this counter's sense.
- **`sent` / `unregistered` / `send_failed` / `token_fetch_failed`** — exactly
  one increments per sender invocation, once its `send` goroutine's call to
  `Send` returns. `classifyPushWake(err)` maps the result by error identity,
  never by matching text: `nil` → `sent` (with or without a message id in
  the reply); `errors.Is(err, ErrFCMUnregistered)` → `unregistered`;
  `errors.Is(err, ErrFCMTokenFetch)` → `token_fetch_failed`, including
  cancellation or expiry while waiting for authentication; anything else
  (another FCM error, a transport failure, or cancellation at send entry,
  after token lookup or during FCM delivery) → `send_failed`. Cancellation
  by `Close` follows the phase where it is observed, rather than always
  counting as `send_failed`.

Accepted duplicate requests increment none of these counters. Outcome counts
measure sender invocations and admission/retry refusals, rather than all `nil`
returns from `Request`; each attempt's log uses only its first admitting daemon.
Attempt exhaustion, an excluded backoff/deadline and cancellation between
calls add no synthetic job outcome or log line. Cancellation during a call
still records that invocation's phase-specific outcome once.

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

- One goroutine per admitted job, with at most 32 retained jobs and
  simultaneous sends. Coalescing spawns no additional provider goroutine;
  each newly accepted feedback binding has a bounded worker. The job owns its fixed
  45s deadline, up to three sender invocations and cancellable retry timers;
  each timer is stopped on return. Its reserved slot remains occupied while
  waiting. Each invocation uses one whole-call `fcmSendTimeout` budget in
  production, limited by the job context. Token waiting consumes delivery
  time; authentication does not grant the FCM POST a fresh budget. Shared
  refresh work runs separately, with cancellation owned by its active waiters (see
  [FCM sender concurrency](fcm-push-sender.md#concurrency)). Each invocation logs
  its outcome line before `wg.Done` (deferred first, so it runs last) —
  `Close` returning therefore means
  every invoked attempt's outcome line has already been written (#160).
  Completion releases the slot and marks the job completed under `w.mu`,
  removing it immediately if its fixed window has expired, before `wg.Done`.
- `w.mu` owns all buckets, their refill/idle timestamps, job lookup/expiry,
  sweeping and the admission commit. The closure check, duplicate check,
  all three charges, job/slot reservation, feedback admission/counts and `wg.Add`
  are atomic against other requests and `Close`. Retry charging is atomic under the same mutex,
  after rechecking closure and the job deadline.
  Concurrent requests cannot overspend, and none is admitted after the closed
  transition. No accounting mutex is held across provider or result I/O or waits.
- `push_feedback.go` gives each accepted record one worker (at most 2048
  relay-wide), joined by both its origin and the waker. It waits for completion,
  expiry or cancellation, attempts at most one context-bounded write, then
  retains seen state until eviction/expiry/cancellation. No worker is created
  for repeats or refused bindings. One record keeps its charges through every
  transition, including delivery after cache eviction.
- `WSConn`'s context-aware writer gate includes contention in the feedback
  deadline; wrapping ordinary `Send` in a goroutine would not do so. The private
  result writer checks cancellation before/after gate acquisition and uses the
  remaining budget directly. Ordinary `Send` keeps its independent ten-second
  active-write deadline, preserving graceful shutdown behavior from #207.
- Origin contexts descend from the socket's separate feedback context. Registry
  takeover and grace reclaim cancel the incumbent's feedback under the identity
  lock before publishing the replacement. Waiting for socket close is too late:
  takeover closes it asynchronously and grace reclaim may leave it open.
  This fence only cancels, with no I/O or accounting-lock acquisition. Forwarder
  teardown cancels and joins its origin workers, without cancelling provider jobs.
- One sweep goroutine runs every five minutes, exits on `w.ctx` cancellation
  and has its own `sweepWG`. It also reclaims expired completed jobs; new-job
  admission reclaims them before capacity checks. Feedback workers also arm
  immutable subscription deadlines and a post-completion original-window timer,
  so feedback cleanup does not wait five minutes. The sweep never removes active
  jobs; their deadline ends their goroutine instead. Allowance/coalescing time (`now`) and job
  scheduling time (`jobNow`) are read under `w.mu`; both default to real time.
- `Close` is idempotent: set `closed` under `w.mu`, cancel `w.ctx`,
  `wg.Wait()` for every admitted job and feedback worker, including sends,
  retry waits and result delivery, and
  `sweepWG.Wait()` for the sweep goroutine, then clear retained jobs under
  `w.mu`, releasing their device identities. Repeated and concurrent callers
  all join admitted work;
  none returns early merely because another caller marked the waker closed.
  Cancellation gates refuse new provider invocations and result writes after
  shutdown begins; feedback accounting is released as workers finish.
  With `FCMSender`, the last waiter cancels and joins unfinished token
  refresh work before its send returns, so the wait group joins cancellable
  authentication as well as FCM delivery and stopped retry timers.
  Hijacked WebSocket connections outlive `http.Server.Shutdown`, so a
  forwarder can still call `Request` after `Close` — it gets `ErrPushOff`
  rather than a panic or a send racing shutdown.

`TestPushWaker_CloseCancelsAuthentication` uses a real sender and six distinct
device jobs sharing a held-open local authentication request. Its two-second
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

`push_feedback_test.go` uses literal envelopes, held provider replies,
independent origins and controlled clocks/barriers to distinguish provider
calls, result attempts/deliveries and retained record counts. It covers syntax,
exact expiry/window boundaries, cross-connection IDs, cached/failed delivery,
64/128/2048 caps and queued charges after eviction, retry terminal outcomes,
cancellation, replacement and concurrent close. Real WS writer contention
checks continued reads and joined bounded delivery. Replacement tests keep the
incumbent socket open, so socket-close cancellation cannot mask a missing fence.
Negative-outcome tests join without cancelling the waker: shutdown could hide
an unwanted result. Synthetic secret markers are checked in results, errors,
logs and metrics. These hermetic proofs do not establish real-daemon persistence,
compatibility or production activation.

`push_wake_budget_test.go` uses a controlled clock and fake sender for shared
allowance exhaustion, exact refill boundaries and capped credit, the
24-request/12-send paired-device burst, state saturation and safe idle
reclamation, failed-send charging, concurrent admission and admission racing
concurrent `Close`. Refusals must preserve all token balances, map keys and
slots (apart from time-based refill/reclamation). Zero sender calls alone would
miss consuming a daemon token before discovering a full semaphore. The
complete-token fixture deliberately includes
two distinct tokens sharing a log fingerprint; ordinary distinct-token tests
would also pass with the incorrect truncated accounting key.

Because the coalescing window equals device refill, repeated same-token
requests cannot prove device-budget exhaustion: duplicates spend nothing,
and each eligible fresh admission has had time to refill. Controlled depleted
device buckets prove exhaustion and exact refill instead. Global/daemon
exhaustion fixtures use distinct devices so they still count actual attempts.
Held-authentication shutdown fixtures likewise need distinct device tokens:
six duplicate requests would join only one send, losing the shared-refresh
waiter proof. Both `TestPushWaker_CloseCancelsAuthentication` and
`TestRun_ProcessSIGTERMCompleteShutdown` use six distinct jobs.

`push_wake_coalesce_test.go` holds sends and controls time to cover concurrent
same/cross-daemon duplicates, successful and failed early completion,
just-before/exact window boundaries, active jobs past expiry, retained active
and completed capacity, expiry without erased debt and shutdown races.
Duplicate tests assert unchanged accounting, no outcomes and first-daemon
log attribution even with exhausted allowances and full state.

`daemon_admission_test.go` retains the pre-upgrade rejection proof and checks
initial admitted wakes plus reconnects using a distinct device without clock
advance, so duplicate acceptance cannot hide reset daemon debt. These
tests are hermetic; repository checks establish neither deployment nor live
admission enforcement.

`push_retry_test.go` combines held OAuth/FCM peers with controlled scheduling,
allowance time, waits and jitter to cover transient success, three-attempt
exhaustion, same/cross-daemon duplicates during sends/backoff, immutable
deadlines, guidance exclusion, atomic independent budget refusal and
shutdown while waiting/sending. Authentication-expiry cases distinguish a
surviving refresh waiter from last-waiter cancellation/join. Outcome/log
assertions count invocations and refusals, with no synthetic job outcomes.
Keep scheduling time separate from allowance-test time: historical allowance
fixtures must not immediately expire real deadline contexts. Backoff anchors
at call return before logging, so logging time cannot restart the delay.
Fresh slot reservation must stay after allowance checks and before charging:
moving it earlier skips refused-request idle timestamps; charging earlier
would spend partial credit on refusal.

`push_wake_test.go`: burst exhausted for one server-id → `ErrPushWakeRateLimited`
using distinct devices while a different server-id is still admitted;
a nil `*PushWaker` → `ErrPushOff`; `Close`
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
in-test OAuth and FCM servers drives the four actual-send outcomes off the
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
- [ADR-0013](../decisions/0013-push-wake-async-dispatch-bounded-not-verified.md) — bounded asynchronous attempts, coalescing and unverified device ownership.
- [Connection registry](connection-registry.md) — `SetPushWaker` / the
  `pushWaker` field's lifecycle rules.
- [Rate-limit middleware](rate-limit-middleware.md) — separate upgrade-path
  `IPRateLimiter`; push uses local accounting for atomic admission and key caps.
- [`docs/threat-model.md` § Outbound network calls](../../threat-model.md#outbound-network-calls--fcm-push-wake) —
  the abuse accounting.
- [Codebase note #133](../codebase/133.md) — implementation summary and
  lessons.
