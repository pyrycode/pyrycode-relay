# #132 — FCM sender built from the service-account secret

## Files read

- `internal/relay/env_config.go` → `envContracts`, `checkEnvConfigWith`, `ErrInvalidConfig`. This is the registry the new env var joins. The walk passes the value to `validate` whenever it is present, even when it is empty, and it prefixes `err.Error()` into `Reason`. So the validator's error text must not contain the value.
- `internal/relay/env_config_test.go` → `fakeLookup`, `TestCheckEnvConfig_MalformedValueReturnsStructuredError`. These are the harness the new env rows mirror.
- `internal/relay/single_instance.go` → `envSingleInstanceBypass`. The precedent for keeping the env-name constant in the feature's own file, with the `#nosec G101` comment.
- `internal/relay/log_allowlist.go`. The sender logs nothing, so the allowlist is unchanged.
- `docs/knowledge/features/env-config-validator.md` (via INDEX). Registry-row pattern; exit-2 boot refusal comes from `main` (#133, not here).
- `docs/threat-model.md` § Supply chain — Go dependencies. The documentation handoff target.
- `golang.org/x/oauth2@v0.37.0` → `jwt.Config.TokenSource`, `jwtSource.Token`, `oauth2.ReuseTokenSource`, `oauth2.RetrieveError`, `internal.ParseKey`. `TokenSource` wraps the JWT source in `ReuseTokenSource`, which caches until `Expiry - 10s`. The JWT source takes its HTTP client from `ctx.Value(oauth2.HTTPClient)` and caps the token response read at 1 MiB. On a non-2xx reply it returns `*RetrieveError`, whose `Error()` includes the response body.

## Context

The Android app reconnects for 30 s when it receives an FCM data message. Decided 2026-09-24: the relay owns the FCM sender, so the Firebase credentials live in one relay secret. They are not copied into every `pyry`. This ticket ships the sender and its credential check only. #133 wires the `push_wake` envelope and constructs the sender from `main`.

**Dependency choice (deviates from the ticket's suggestion; recorded for the ADR the documentation phase may want).** The ticket suggests `golang.org/x/oauth2/google`. Its `JWTConfigFromJSON` does five things: it parses the key JSON, checks `type == "service_account"`, and maps `client_email`, `private_key`, `private_key_id` and `token_uri` onto a `jwt.Config`, defaulting the token URI. The `google` package also imports `cloud.google.com/go/compute/metadata`, `authhandler`, `externalaccount`, `impersonate` and `stsexchange`. None of these is used on this path. The plan instead imports only `golang.org/x/oauth2/jwt` and does that mapping itself. The build graph gains only packages from the `golang.org/x/oauth2` module (`oauth2`, `oauth2/internal`, `oauth2/jws`, `oauth2/jwt`). `cloud.google.com/go/compute/metadata` appears in `x/oauth2`'s own `go.mod` but is not compiled into the binary. The token caching and `token_uri` behaviour the ticket relies on come from `jwt.Config.TokenSource`, not from `google`, so nothing is lost. Doing the parse ourselves also means we control every byte of the credential-error text, which AC 4 needs anyway.

New module: `golang.org/x/oauth2 v0.37.0` (latest; stdlib-only transitive imports on this path).

## Design

New file `internal/relay/push.go`. It adds no new goroutines and no logger.

Constants:
- `envFCMCredentials = "PYRYCODE_RELAY_FCM_CREDENTIALS"` (`// #nosec G101 -- env var name, not a credential`).
- `fcmProjectID = "pyrycode-mobile"`.
- `fcmDefaultBaseURL = "https://fcm.googleapis.com"`. The send URL is `base + "/v1/projects/" + fcmProjectID + "/messages:send"`.
- `fcmScope = "https://www.googleapis.com/auth/firebase.messaging"`.
- `fcmDefaultTokenURL = "https://oauth2.googleapis.com/token"`, used when the key has no `token_uri`, matching `google.JWTTokenURL`.
- `fcmSendTimeout = 10 * time.Second`.
- `fcmMaxDrainBytes = 4 << 10`, the cap on draining an FCM reply body before close.

Sentinels:
- `ErrFCMCredentials`: the credential JSON is not a usable service-account key.
- `ErrFCMTokenFetch`: the OAuth access-token fetch failed.
- `ErrFCMSend`: the FCM request failed (transport error or non-2xx).

Contract:
- `parseFCMCredentials(raw []byte) (*jwt.Config, error)` unmarshals `{type, client_email, private_key, private_key_id, token_uri}`. It requires `type == "service_account"` and a non-empty `client_email`. The `private_key` must parse as an RSA key: PEM, then PKCS#8, then PKCS#1, the same order as `internal.ParseKey`, which we can't import. That way a bad key fails at boot rather than at the first send. On any failure it returns **only** `ErrFCMCredentials`, never a wrapped parse error. `json` and `x509` errors can echo fragments of the input. It sets `Scopes = {fcmScope}` and `TokenURL = token_uri`, falling back to `fcmDefaultTokenURL`.
- `NewFCMSender(credentialsJSON []byte) (*FCMSender, error)` is the production constructor and uses `fcmDefaultBaseURL`. `newFCMSender(credentialsJSON []byte, baseURL string)` is the test seam.
- `FCMSender` fields:
  - `client *http.Client`, with `Timeout: fcmSendTimeout` and a `CheckRedirect` that returns `http.ErrUseLastResponse`, so a 3xx is a non-2xx error and the body and bearer token are never replayed elsewhere.
  - `tokens oauth2.TokenSource`, from `cfg.TokenSource(ctx)`, where `ctx = context.WithValue(context.Background(), oauth2.HTTPClient, client)`. The token fetch therefore uses the same bounded client.
  - `sendURL string`.
- `(*FCMSender).Send(ctx context.Context, deviceToken string) error` runs these steps:
  1. `s.tokens.Token()`. On an error it returns `ErrFCMTokenFetch`. If the error is a `*oauth2.RetrieveError` with a non-nil `Response`, it becomes `fmt.Errorf("%w: status %d", ErrFCMTokenFetch, code)`. The oauth2 error itself is never wrapped.
  2. It marshals `{"message":{"token":<deviceToken>,"android":{"priority":"HIGH"}}}` through unexported structs that have no `notification` or `data` fields.
  3. It builds a POST with `context.WithTimeout(ctx, fcmSendTimeout)` and sets `Authorization: Bearer <access>` and `Content-Type: application/json`.
  4. A transport error returns `fmt.Errorf("%w: %w", ErrFCMSend, err)`. The `*url.Error` carries only our constant URL and the net error; the device token is in the body, not the URL. The test asserts this.
  5. It drains at most `fcmMaxDrainBytes` of the reply, then closes it. It never keeps the body.
  6. A non-2xx reply returns `fmt.Errorf("%w: status %d", ErrFCMSend, resp.StatusCode)`.

Each send is bounded by `fcmSendTimeout`, because both HTTP calls use `client.Timeout`. The FCM POST additionally honours the caller's `ctx`. The token fetch can't take a per-call ctx (the `jwtSource` holds its construction ctx), so the client timeout is what bounds it.

`env_config.go` gains one `envContracts` row: `{name: envFCMCredentials, required: false, validate: …}`. It calls `parseFCMCredentials` and on failure returns a fixed-text error, `"not a service-account JSON key (value withheld)"`. The value is never quoted. An unset variable is valid, meaning push is off. Present-but-empty is malformed, consistent with the other rows.

## Concurrency model

The sender spawns no goroutines. `Send` is safe for concurrent use. `ReuseTokenSource` serialises refreshes under its own mutex, and a refresh holds that mutex for at most `fcmSendTimeout`. `http.Client` is concurrency-safe. Shutdown needs nothing, because there are no background refreshes.

## Error handling

| Failure | Returned | Contains |
|---|---|---|
| Bad credentials (constructor / env check) | `ErrFCMCredentials` / `ErrInvalidConfig{Reason: fixed}` | nothing from the value |
| Token endpoint non-2xx | `ErrFCMTokenFetch: status N` | status only |
| Token transport / decode failure | `ErrFCMTokenFetch` | nothing |
| FCM transport failure / timeout | `ErrFCMSend: <url.Error>` | constant URL + net error |
| FCM non-2xx / 3xx | `ErrFCMSend: status N` | status only |

A failed send doesn't retry. Retry policy and telling the daemon about dead tokens belong to #133 and later tickets.

## Testing strategy

`internal/relay/push_test.go` (`package relay`). The helper `testServiceAccountJSON(t, tokenURI)` builds the JSON around a 2048-bit RSA key. The key is generated once per test binary (`sync.OnceValue`) and never committed. Fakes are `httptest.Server`s: the OAuth fake counts requests and returns `{"access_token":"at-…","token_type":"Bearer","expires_in":3600}`, and the FCM fake records the request.

Scenarios:
- **Happy path (AC 1).** Checks the request's path `/v1/projects/pyrycode-mobile/messages:send`, method POST and `Authorization: Bearer <fake access token>`. The body decodes to a map whose `message` has exactly the keys `{token, android}`, with `token` equal to the device token, and `android` has exactly `{priority: "HIGH"}`. The test also asserts that `NewFCMSender` targets `fcmDefaultBaseURL` (sendURL prefix).
- **Token reuse (AC 2).** Two sends make one OAuth request and two FCM requests.
- **Non-2xx FCM (AC 3).** A table of 400, 404, 500 and 307 (with `Location`). The FCM fake's body contains a marker, and it echoes the device token and the Authorization header. The error `errors.Is(ErrFCMSend)` and contains the status number. It must not contain the device token, the access token, the private key, the client email or the marker.
- **Token fetch failure (AC 3).** The OAuth fake returns 400 with a marker body. The error `errors.Is(ErrFCMTokenFetch)`, contains no secrets and no marker, and the FCM fake gets zero hits.
- **Timeout constant (AC 3).** `s.client.Timeout == fcmSendTimeout`. A cancelled caller ctx returns `ErrFCMSend` without the device token.
- **Credential rejection.** A table: not JSON, wrong `type`, missing `client_email`, missing `private_key`, garbage PEM, and an ECDSA key. Each case yields `errors.Is(ErrFCMCredentials)`, and the error string does not contain the input's planted marker.

`env_config_test.go`:
- A valid key passes `CheckEnvConfig`, and unset passes (the existing empty-env row already covers unset).
- Malformed values (not JSON with a marker, wrong type with a marker, empty) fail with `Key == envFCMCredentials` and a `malformed-value: ` prefix. The `Reason` does not contain the value or the marker.

The sender has no logger, so "logs nothing" holds by construction. `TestLogKeysAreAllowlisted` keeps covering the package.

## Open questions

- None blocking. Should the sender drop a cached token after a 401 from FCM? That is out of scope; the token expires on its own within an hour.

## Documentation handoff (pending — documentation stage)

- `docs/threat-model.md` § Supply chain — Go dependencies: add `golang.org/x/oauth2 v0.37.0` (the `jwt` subpackage only). It is used to mint the FCM access token from the service-account key. It is not on the frame path, and on this path it pulls in no packages outside the `x/oauth2` module; `cloud.google.com/go/compute/metadata` is listed in its `go.mod` but not built. Residual risk: a compromised release sees the FCM service-account private key and the device tokens it sends to. This change trips § Triggers for re-review ("A new dependency is added to `go.mod`").

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings. The credential JSON is operator-supplied and trusted. It enters through one parser, `parseFCMCredentials`, shared by `CheckEnvConfig` and `NewFCMSender`. The device token is opaque here: it is copied into the JSON body by `encoding/json`, so it can't inject structure, and it is never logged or put in an error. No payload is read. The sender takes a token string, not a frame, and #133 owns extracting it from the `push_wake` envelope.
- [Trust boundaries] OUT OF SCOPE. `token_uri` is taken from the operator's key and is not restricted to Google or to HTTPS. A hostile `token_uri` would need control of the relay's own secret, which already hands over the key. Only the operator can set it; there is no follow-up ticket.
- [Secrets] No findings. The private key and access token live only in memory (`jwt.Config`, `ReuseTokenSource`), with no disk writes. Every error path returns sentinel text plus, at most, an HTTP status or a transport `*url.Error` over our constant URL. The oauth2 `RetrieveError` (which embeds the response body) and the `json`/`x509` parse errors (which can echo input) are never wrapped. Tests plant markers to prove this. Rotation: the operator replaces the Fly secret and restarts the relay.
- [Secrets] SHOULD FIX (addressed in design). `CheckEnvConfig` builds `Reason` from `err.Error()`, so the validator returns fixed text only. `TestCheckEnvConfig_*` asserts that the value and a planted marker are absent.
- [File operations] No findings. The design adds no file I/O; the credentials arrive through the env var.
- [Subprocess] No findings. The design adds no subprocesses.
- [Crypto] No findings. RS256 JWT signing is done by `x/oauth2/jws` with `crypto/rsa`, with no hand-rolled crypto. The test keys come from `crypto/rand` at test time and are never committed.
- [Network & I/O] No findings. Outbound only. `client.Timeout` bounds both calls, and the FCM POST also carries a `context.WithTimeout`. The FCM reply drain is capped at `fcmMaxDrainBytes`, and the token reply is capped at 1 MiB inside `jwtSource.Token`. Redirects are refused (`CheckRedirect` → `ErrUseLastResponse`), so the bearer token and device token go only to the configured URL. TLS uses Go's default verifying transport.
- [Network & I/O] OUT OF SCOPE. Fan-out and amplification: one `push_wake` triggers one outbound POST. Rate-limiting wakes per server-id is #133's concern, because only #133 connects inbound frames to `Send`.
- [Errors/logs] No findings. The sender holds no logger and adds no allowlist keys.
- [Concurrency] No findings. There are no goroutines, the `ReuseTokenSource` mutex is the only lock, and it is held for at most `fcmSendTimeout`.
- [Threat model] Trips "new `go.mod` dependency", which is recorded in the Documentation handoff. Single-instance assumption: none; the sender is stateless apart from a cached access token that is re-minted after a restart.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
