# FCM push sender

`internal/relay/push.go` gives the relay an `FCMSender` that wakes a
backgrounded Android app by sending it a data-only FCM message. Introduced by
\#132. #133 wires the daemon's `push_wake` routing envelope
(`protocol-mobile.md` § `push_wake`) to `Send` and constructs the sender
from `main` — see [Push wake dispatch](push-wake-dispatch.md) for the
dispatch path (structural validation, per-server-id rate limit, relay-wide
in-flight cap, async send off the binary read loop).

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
A failed attempt sends nothing to FCM and its waiters do not retry in turn;
a later send can start a new attempt.
The token fetch uses the key's own `token_uri` when present, else
`fcmDefaultTokenURL` (`https://oauth2.googleapis.com/token`, matching
`google.JWTTokenURL`).

`Send` starts one `context.WithTimeout(ctx, fcmSendTimeout)` budget (10s) at
call entry, respecting an earlier caller deadline. Token waiting,
authentication and the FCM POST consume that same budget: time spent
obtaining a token leaves less time for delivery. An already-cancelled caller
starts neither authentication nor delivery, even with a cached token, and
`Send` rechecks cancellation after token lookup. Each refresh has its own
10s context so one caller's deadline cannot cancel work another waiter needs
(see [Concurrency](#concurrency)). The HTTP client retains its 10s timeout
as well. The client refuses
redirects (`CheckRedirect` → `http.ErrUseLastResponse`) so a 3xx becomes a
non-2xx error instead of replaying the bearer token and device token to a
second URL. An FCM reply is read into memory up to `fcmMaxDrainBytes` (4 KiB,
same cap as before) then closed; the buffer is decoded for FCM's error
reason on a non-2xx reply, or for the message id on a 2xx (both below).

## Error handling — nothing leaks

| Failure | Returned | Carries |
|---|---|---|
| Bad credentials (env check or `NewFCMSender`) | `ErrFCMCredentials` | nothing from the value |
| Token endpoint non-2xx | `ErrFCMTokenFetch: status N` | HTTP status only |
| Token transport/decode failure | `ErrFCMTokenFetch` | nothing |
| Cancellation/deadline while waiting for a token | `ErrFCMTokenFetch` wrapping `context.Canceled` or `context.DeadlineExceeded` | safe context sentinel only |
| Caller already done at entry, or cancellation detected after token lookup | `ErrFCMSend` wrapping the context error | safe context sentinel only; no FCM request |
| FCM transport failure / cancellation during delivery | `ErrFCMSend: <url.Error>` | the constant send URL + net error — the device token is in the body, never the URL |
| FCM non-2xx (incl. 3xx, since redirects are refused) | `ErrFCMSend: status N (REASON[, CODE])`, also matching `ErrFCMUnregistered` when `errorCode` is `UNREGISTERED` | HTTP status plus FCM's enum-shaped `error.status` and/or FcmError `errorCode`, when present — see below; exactly `status N` otherwise |
| FCM 2xx | `nil`, plus the message id (below) or `""` | nothing beyond the validated id |

`oauth2.RetrieveError` formats the token endpoint's response body into its
`Error()` string, so `fcmTokenCache.fetch` never publishes or wraps it as-is
— it extracts only `Response.StatusCode`. Other token-fetch errors, which
can include a credential-bearing URL, become the bare `ErrFCMTokenFetch`.
Sanitisation happens once before the attempt's result is shared with its
waiters. Cancellation classification follows the phase, including shutdown:
token waiting is `ErrFCMTokenFetch`; entry and delivery are `ErrFCMSend`.
The sender holds no logger, so it logs nothing;
logging happens in the caller (below, and [Push wake dispatch](push-wake-dispatch.md)
§ Error handling).

### Naming FCM's reason (#150)

On a non-2xx reply, `fcmErrorReason` decodes the drained buffer into an
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
unexported `fcmUnregisteredError` that wraps the usual
`fmt.Errorf("%w: status %d (%s)", ErrFCMSend, ...)` error unchanged —
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
admitted wake).

## Concurrency

`Send` is safe for concurrent use. `fcmTokenCache` protects the cached token,
current attempt and waiter counts with one mutex, never held across HTTP I/O
or channel waits. One goroutine fetches each refresh and publishes an
immutable token/error result before closing its `done` channel. The JWT
source's reuse mutex alone would share success but make failed waiters fetch
again serially; the relay cache shares the failed attempt too.

An attempt owns an independent context capped at `fcmSendTimeout`, rather
than the starter's context. Cancelling or timing out any one send, including
the starter, returns promptly while other waiters keep authentication alive.
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
ends the refresh work.

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

## Dependency

New direct dep `golang.org/x/oauth2 v0.37.0`, importing only the `jwt`
subpackage rather than `oauth2/google` — see
[ADR-0011](../decisions/0011-oauth2-jwt-not-google-for-fcm.md) for why, and
[`docs/threat-model.md` § Supply chain](../../threat-model.md) for the
residual-risk accounting.

## Out of scope

- APNs (iOS push).
- Telling the daemon that a device token is dead (no retry, no dead-token
  callback).
- A global wakes-per-second ceiling beyond the per-server-id rate and
  relay-wide in-flight cap — see
  [`docs/threat-model.md` § Outbound network calls](../../threat-model.md#outbound-network-calls--fcm-push-wake).

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
