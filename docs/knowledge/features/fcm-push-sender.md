# FCM push sender

`internal/relay/push.go` gives the relay an `FCMSender` that wakes a
backgrounded Android app by sending it a data-only FCM message. Introduced by
\#132. #133 wires the daemon's `push_wake` routing envelope
([protocol § `push_wake`](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#push_wake)) to `Send` and constructs the sender
from `main` — see [Push wake dispatch](push-wake-dispatch.md) for the
dispatch path (structural validation, daemon/device/global attempt allowances,
bounded device coalescing/retained-job cap, async send off the binary read loop).

## Why

The Android app closes its WebSocket connection when backgrounded and
reconnects for 30 seconds on an FCM data message (pyrycode-mobile #361,
\#685). Decided 2026-09-24: the FCM sender lives in the relay, not in every
copy of `pyry`, so the Firebase service-account credentials sit in one relay
secret instead of being distributed to every daemon install.

## Credentials

`PYRYCODE_RELAY_FCM_CREDENTIALS` holds a whole Google service-account JSON
key. It is registered in `envContracts`
([env-config validator](env-config-validator.md)) as `required: false` —
**unset is valid and means push is off.** A value that does not parse as a
usable service-account key fails `CheckEnvConfig` with fixed text
(`"not a service-account JSON key (value withheld)"`); the reason string
never quotes the value, unlike the package's other validators, because this
one is a private key.

`parseFCMCredentials` (shared by the env validator and `NewFCMSender`, so
there is exactly one place that accepts or rejects a key) requires
`type == "service_account"`, a non-empty `client_email`, and a `private_key`
that parses as an RSA key — PEM-unwrapped, then PKCS#8, then PKCS#1, the same
order `golang.org/x/oauth2`'s internal JWT signer uses. Checking the key
shape at boot (or at env-check time) means a bad key fails loud immediately
rather than at the first send. Every failure returns the bare
`ErrFCMCredentials` sentinel — never a wrapped `json`/`x509` error, since
those can echo fragments of the input.

## Sending a message

`NewFCMSender(credentialsJSON []byte) (*FCMSender, error)` builds a sender
against the production FCM endpoint (`fcmDefaultBaseURL`,
`https://fcm.googleapis.com`).
`(*FCMSender).Send(ctx, deviceToken string) (string, error)` POSTs to
`https://fcm.googleapis.com/v1/projects/pyrycode-mobile/messages:send` a body
shaped:

```json
{"message": {"token": "<deviceToken>", "android": {"priority": "HIGH"}}}
```

with no `notification` and no `data` field — the app's
`PyryMessagingService` ignores every field on the message, so nothing from a
session reaches Google. `fcmProjectID` (`pyrycode-mobile`) is a named
constant.

The OAuth access token comes from `jwt.Config.TokenSource`, created per
refresh attempt. The sender's `fcmTokenCache` (`push_refresh.go`) caches a
successful token in memory while `oauth2.Token.Valid()` accepts it, including
the library's pre-expiry margin; the next send refreshes an invalid token.
Concurrent sends waiting on one attempt share its token or sanitised failure.
A failed refresh sends nothing to FCM and its waiters do not start serial
refreshes inside the same call; a later `Send` can start a new refresh.
`PushWaker` may make that later call as a bounded job retry.
The token fetch uses the key's own `token_uri` when present, else
`fcmDefaultTokenURL` (`https://oauth2.googleapis.com/token`, matching
`google.JWTTokenURL`).

`Send` starts one `context.WithTimeout(ctx, fcmSendTimeout)` budget (10s) at
call entry, respecting an earlier caller deadline. Token waiting,
authentication and the FCM POST consume that same budget: time spent
obtaining a token leaves less time for delivery. This is a per-call cap,
distinct from [the waker's immutable 45s job budget](push-wake-dispatch.md#request-validate-admit-dispatch),
which covers up to three calls and their retry waits. `Send` itself never
retries. An already-cancelled caller starts neither authentication nor
delivery, even with a cached token, and
`Send` rechecks cancellation after token lookup. Each refresh has its own
10s context so one caller's deadline cannot cancel work another waiter needs
(see [Concurrency](#concurrency)). The HTTP client retains its 10s timeout
as well. The client refuses
redirects (`CheckRedirect` → `http.ErrUseLastResponse`) so a 3xx becomes a
non-2xx error instead of replaying the bearer token and device token to a
second URL. An FCM reply is read into memory up to `fcmMaxDrainBytes` (4 KiB,
same cap as before) then closed; the buffer is decoded for FCM's error
reason before deciding success, or for the message id on a successful reply
(both below). `UNREGISTERED` overrides even a contradictory 2xx status.

## Error handling — nothing leaks

| Failure | Returned | Carries |
|---|---|---|
| Bad credentials (env check or `NewFCMSender`) | `ErrFCMCredentials` | nothing from the value |
| Token endpoint rejection / body-read failure | `ErrFCMTokenFetch: status N`, optionally wrapping a safe context sentinel | HTTP status and safe identity, no body or raw read cause |
| Malformed/invalid successful token reply | `ErrFCMTokenFetch: status N` | HTTP status only; permanent failure |
| Token transport failure | `ErrFCMTokenFetch`, optionally wrapping a safe context sentinel | no original transport cause |
| Cancellation/deadline while waiting for a token | `ErrFCMTokenFetch` wrapping `context.Canceled` or `context.DeadlineExceeded` | safe context sentinel only |
| Caller already done at entry, or cancellation detected after token lookup | `ErrFCMSend` wrapping the context error | safe context sentinel only; no FCM request |
| FCM transport failure / cancellation during delivery | `ErrFCMSend`, optionally wrapping a safe context sentinel | no original transport cause or URL |
| FCM rejected reply or body-read failure | `ErrFCMSend: status N (REASON[, CODE])`, also matching `ErrFCMUnregistered` when `errorCode` is `UNREGISTERED`; optionally wrapping a safe context sentinel | HTTP status plus validated FCM enum reasons, exactly `status N` when absent; no body or raw read cause |
| FCM 2xx, read successfully without `UNREGISTERED` | `nil`, plus the message id (below) or `""` | nothing beyond the validated id |

`fcmRetryError` carries a sanitised cause, a retry flag and an absolute
not-before time. `errors.As` preserves this typed guidance through wrapping;
`errors.Is` preserves `ErrFCMTokenFetch`, `ErrFCMSend`, `ErrFCMUnregistered`
and safe cancellation/deadline identities. No consumer parses error strings.
Network boundaries discard raw transport errors, credential-bearing URLs,
endpoint bodies and `Retry-After` text before returning or sharing a result.
Only validated classifications/guidance survive. Cancellation classification
follows the phase, including shutdown:
token waiting is `ErrFCMTokenFetch`; entry and delivery are `ErrFCMSend`.
The sender holds no logger, so it logs nothing;
logging happens in the caller (below, and [Push wake dispatch](push-wake-dispatch.md)
§ Error handling).

### Retry classification and guidance

Retryable HTTP statuses are FCM 429/500/503 and OAuth
429/500/502/503/504. Transport failures retry only with positive evidence
from a wrapped `net.Error` whose `Timeout()` or `Temporary()` is true;
unclassified failures stop. A sender call's own timeout can retry while its
parent job remains live. Job cancellation/expiry always stops further calls.
`UNREGISTERED` at any HTTP status, OAuth credential rejection
(`invalid_client`, `invalid_grant`, `unauthorized_client`), other HTTP
statuses and malformed/invalid successful token replies are permanent.

Both phases capture response receipt before reading the body. Valid
`Retry-After` delta-seconds or HTTP dates become an absolute not-before time
anchored there; past dates give zero additional delay, malformed/negative
values leave ordinary backoff in effect. Very large or overflowing positive
seconds saturate beyond the job horizon instead of wrapping into an early
retry. HTTP 429 imposes at least 60s from receipt, excluding another call
within the 45s job. The waker takes the later of this guidance and its
completion-anchored backoff plus jitter; a time at or beyond its deadline
ends the job without sending early.

When a body read fails, complete permanent rejection JSON takes precedence
over HTTP status, and HTTP status takes precedence over transport retryability.
Only a 2xx status defers retryability to the sanitised read failure.
Cancellation always stops; a safe deadline identity alone does not override
received status or guidance. Thus a trailing timeout cannot turn HTTP 401
into a retry, erase a 429 floor or bypass `Retry-After` on a 503.

The pinned JWT implementation discards response status/body on read failure,
formats transport/read errors with `%v` and does not populate the OAuth
rejection code. `fcmRefreshTransport` captures sanitised status/guidance and
transport classification before that boundary. `fcmRefreshBody` retains at
most the JWT reader's existing 1 MiB cap locally to recognise complete
credential-rejection JSON; those bytes are discarded after `fetch`, never
published. `http.Client` annotates body timeouts outside this wrapper, so a
failed read also captures the original request's timeout context signal to
preserve safe identity before JWT formatting loses it. The shared refresh
publishes only the sanitised result, retaining the original receipt time
instead of restarting the delay when each waiter receives it.

### Naming FCM's reason (#150)

On each reply, `fcmErrorReason` decodes the drained buffer into an
unexported struct (`fcmErrorReply`) that declares only two fields:
`error.status` and the `errorCode` of any `error.details[]` entry whose
`@type` is `type.googleapis.com/google.firebase.fcm.v1.FcmError`
(`fcmErrorType`). `error.message` is not a field in that struct — FCM's free
text is never held in a typed value, let alone returned — because it can
echo request data (2026-09-24: every wake to `pyrycode-mobile` came back
`status 404`, from both an ATD and a Play emulator, and the log alone
couldn't say `UNREGISTERED` from a project mismatch; pyrycode-mobile #955).

Each of the two values is kept only when `isFCMEnum` accepts it — 1–64 bytes
of `[A-Z0-9_]` — so even a hostile or misconfigured-proxy reply that echoed
request data into `error.status`/`errorCode` can't smuggle it into the log;
a decode failure, an empty body, or fields missing/malformed is silently the
same status-only error as before. When both values are present the error
reads `relay: fcm send failed: status 404 (NOT_FOUND, UNREGISTERED)`; with
only one, only that one appears in the parentheses. No new log key —
[`push_wake.go`](../../../internal/relay/push_wake.go)'s `PushWaker.send`
still logs the whole error under the already-allowlisted `err` key.

**`ErrFCMUnregistered` (#161).** `fcmErrorReason` additionally reports
whether the `FcmError` detail's `errorCode` is exactly `UNREGISTERED` — the
same validated, enum-shaped field the reason string already exposes, never
the HTTP status and never `error.message`. When it is, `Send` returns an
unexported `fcmUnregisteredError` that wraps the sanitised status/retry error —
`Error()` and the `ErrFCMSend` match are both preserved — and additionally
unwraps (via `Unwrap() []error`) to the sentinel `ErrFCMUnregistered`. A
caller distinguishes a dead token from any other send failure by
`errors.Is(err, ErrFCMUnregistered)`, never by matching text; see
[Push wake dispatch § Metrics](push-wake-dispatch.md#metrics) for the
counter this drives. `UNREGISTERED` means the device token is permanently
invalid on Google's side — telling the daemon so (and letting it stop
retrying that token) is still out of scope; see below.

### Returning FCM's message id (#160)

On a 2xx reply, `fcmMessageID` decodes the same drained buffer into
`fcmSendReply`, a struct declaring only `name` — the field a successful
reply carries as `projects/<project>/messages/<id>`. The id is returned
only when `isFCMMessageID` accepts it: 1 to `fcmMaxMessageIDLen` (256)
bytes of `[A-Za-z0-9/:%_-]`, a charset that excludes space, `=`, `"` and
control bytes — the same log-injection concern `isFCMEnum` guards against
for the error reason, since a hostile or misconfigured-proxy reply could
otherwise plant forged `key=value` fields in a `slog` text line. A missing,
malformed, or over-long `name` is silently `""`; `Send` never fails because
of what a 2xx body contains, and extra fields alongside `name` are ignored.

`tokenFingerprint(token string) string`, also in `push.go`, returns the
first 8 hex characters of the device token's SHA-256 — stable per token,
one-way, safe to log in the token's place. It exists for the caller: see
[Push wake dispatch](push-wake-dispatch.md) § Error handling for the full
logging contract (`push_wake_sent` / `push_wake_send_failed`, one line per
sender invocation attributed to its first admitting daemon; accepted
coalesced requests produce no line).

## Concurrency

`Send` is safe for concurrent use. `fcmTokenCache` protects the cached token,
current attempt and waiter counts with one mutex, never held across HTTP I/O
or channel waits. One goroutine fetches each refresh and publishes an
immutable token/error result before closing its `done` channel. Retry flags
and receipt-anchored guidance are shared unchanged, not recalculated for
each waiter. The JWT source's reuse mutex alone would share success but make failed waiters fetch
again serially; the relay cache shares the failed attempt too.

An attempt owns an independent context capped at `fcmSendTimeout`, rather
than the starter's context. Cancelling or timing out any one send, including
the starter or an expiring 45s job, returns promptly while other waiters
keep authentication alive.
When the last waiter leaves an unfinished attempt, it detaches the attempt,
cancels it and waits for `done` before returning from `Send`. This keeps
authentication cleanup inside the send lifetime, so
[`PushWaker.Close`](push-wake-dispatch.md#concurrency-model) joins it through
its existing send wait group; the sender needs no separate public close API.

A new send can start a replacement after detachment. Completion only updates
the cache or clears the current attempt when that attempt's pointer still
matches, preventing abandoned work from overwriting a newer token or losing
the replacement. Old waiters retain their own attempt's result even if a
later send starts another attempt.

The pinned JWT source uses its context to select the HTTP client, but calls
`http.Client.PostForm` without attaching that context to the request.
`fcmRefreshTransport` therefore clones each authentication request with the
refresh context before delegating to the original/default transport. A
cancellable token-source context alone would leave network work running
after send cancellation. The copied client preserves redirect refusal and
the HTTP timeout; refresh completion, its deadline or last-waiter cancellation
ends the refresh work. Capturing response/read classification changes neither
this cancellation ownership nor the last-waiter join.

## Testing

`push_refresh_test.go` uses controlled local OAuth and FCM endpoints to cover
shared success/failure, a later retry, repeated cache expiry, independent
starter/other-waiter cancellation and expiry, all-waiter cleanup and reuse,
whole-send and earlier caller deadlines, and pre-cancelled callers with cold
and warm caches. Shared-attempt tests inspect the waiter count under the
cache mutex before releasing or cancelling authentication; starting six send
goroutines alone does not prove they joined the same attempt. Cleanup checks
assert both endpoint cancellation and attempt completion, since prompt send
returns alone could hide authentication left running.

`push_retry_test.go` covers both HTTP allowlists, wrapped temporary/timeout
transport classification, permanent replies even on transient/successful
statuses, Retry-After dates/seconds/overflow and shared receipt anchoring.
Combined response/read-failure tests preserve permanent rejection, HTTP
status, quota floors, guidance and safe phase/context identities in both
phases. A real local body-timeout peer complements injected read failures:
the client can add timeout identity outside the OAuth wrapper, which a fake
reader alone would miss. Tests also assert that raw causes, URLs and endpoint
text never enter returned errors.

## Dependency

New direct dep `golang.org/x/oauth2 v0.37.0`, importing only the `jwt`
subpackage rather than `oauth2/google` — see
[ADR-0011](../decisions/0011-oauth2-jwt-not-google-for-fcm.md) for why, and
[`docs/threat-model.md` § Supply chain](../../threat-model.md) for the
residual-risk accounting.

## Out of scope

- APNs (iOS push).
- Telling the daemon that a device token is dead (no dead-token callback).

Shared global/device attempt budgets are implemented by
[`PushWaker`](push-wake-dispatch.md#allowances-and-concurrency-cap), alongside
the daemon allowance and concurrency cap (#172). Bounded transient retries
(#212) consume those same budgets; dead-token notification remains separate.

## Cross-links

- [ADR-0011: `oauth2/jwt` not `oauth2/google`](../decisions/0011-oauth2-jwt-not-google-for-fcm.md)
- [Push wake dispatch](push-wake-dispatch.md) — the caller: validates the
  wake, rate-limits and caps it, calls `Send` off the binary read loop.
- [Env-var config validator](env-config-validator.md) — the registry
  `PYRYCODE_RELAY_FCM_CREDENTIALS` joins.
- [Codebase note #132](../codebase/132.md) — implementation summary and
  lessons for the sender itself; [#133](../codebase/133.md) for the
  dispatch wiring; [#150](../codebase/150.md) for naming FCM's error reason.
- [`internal/relay/push.go`](../../../internal/relay/push.go) — implementation.
