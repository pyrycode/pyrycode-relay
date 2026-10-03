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
`https://fcm.googleapis.com`). `(*FCMSender).Send(ctx, deviceToken string) error`
POSTs to `https://fcm.googleapis.com/v1/projects/pyrycode-mobile/messages:send`
a body shaped:

```json
{"message": {"token": "<deviceToken>", "android": {"priority": "HIGH"}}}
```

with no `notification` and no `data` field — the app's
`PyryMessagingService` ignores every field on the message, so nothing from a
session reaches Google. `fcmProjectID` (`pyrycode-mobile`) is a named
constant.

The OAuth access token comes from `jwt.Config.TokenSource`, wrapped by
`golang.org/x/oauth2` in `oauth2.ReuseTokenSource`: fetched once and cached
until shortly before expiry, so two sends in a row make one token request.
The token fetch uses the key's own `token_uri` when present, else
`fcmDefaultTokenURL` (`https://oauth2.googleapis.com/token`, matching
`google.JWTTokenURL`).

Both HTTP calls (the token fetch and the FCM POST) are bounded by the shared
`http.Client{Timeout: fcmSendTimeout}` (10s); the FCM POST additionally
honours the caller's `ctx` via `context.WithTimeout`. The client refuses
redirects (`CheckRedirect` → `http.ErrUseLastResponse`) so a 3xx becomes a
non-2xx error instead of replaying the bearer token and device token to a
second URL. An FCM reply is read into memory up to `fcmMaxDrainBytes` (4 KiB,
same cap as before) then closed; on a non-2xx reply that buffer is decoded
for FCM's error reason (below), otherwise it is discarded.

## Error handling — nothing leaks

| Failure | Returned | Carries |
|---|---|---|
| Bad credentials (env check or `NewFCMSender`) | `ErrFCMCredentials` | nothing from the value |
| Token endpoint non-2xx | `ErrFCMTokenFetch: status N` | HTTP status only |
| Token transport/decode failure | `ErrFCMTokenFetch` | nothing |
| FCM transport failure / cancelled ctx | `ErrFCMSend: <url.Error>` | the constant send URL + net error — the device token is in the body, never the URL |
| FCM non-2xx (incl. 3xx, since redirects are refused) | `ErrFCMSend: status N (REASON[, CODE])` | HTTP status plus FCM's enum-shaped `error.status` and/or FcmError `errorCode`, when present — see below; exactly `status N` otherwise |

`oauth2.RetrieveError` formats the token endpoint's response body into its
`Error()` string, so `Send` never returns or wraps it as-is — it extracts
only `Response.StatusCode`. The sender holds no logger, so it logs nothing;
[`log_allowlist.go`](../../../internal/relay/log_allowlist.go) is unchanged
by this ticket.

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

## Concurrency

No goroutines. `Send` is safe for concurrent use — `ReuseTokenSource`
serialises refreshes under its own mutex, held for at most
`fcmSendTimeout`; `*http.Client` is concurrency-safe. No shutdown hook is
needed.

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
