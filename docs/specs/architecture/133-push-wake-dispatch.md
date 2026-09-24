# #133 — Send an FCM wake when the binary asks for one

## Files read

- `internal/relay/envelope.go` → `Envelope`, `Unmarshal`, `hasFrame` — today a `{push_wake}` envelope fails `Unmarshal` with `ErrMissingConnID`; `CloseCode` (#119) is the precedent for one relay-interpreted control field.
- `internal/relay/forward.go` → `StartBinaryForwarder` — the binary read loop; the only consumer of `Unmarshal` in production. Per-frame errors log + continue and never end the loop.
- `internal/relay/push.go` → `FCMSender.Send`, `envFCMCredentials`, `NewFCMSender` — the sender from #132: data-only, high-priority, no payload; each HTTP call bounded by `fcmSendTimeout`; returned errors carry at most an HTTP status, never the device token, access token, credentials or a body.
- `internal/relay/ratelimit.go` → `IPRateLimiter` — string-keyed token bucket with bounded memory and a `Close`d eviction goroutine; reused keyed by server-id.
- `internal/relay/registry.go` → `Registry.SetForwarderHooks`, `onBinaryForwarded` — the "set once at boot, nil = off" optional-collaborator pattern the push waker follows.
- `internal/relay/env_config.go` → `envContracts` entry for `envFCMCredentials` — boot validation already rejects an unusable (or set-but-empty) value, so the from-env constructor only sees unset or a parseable key.
- `internal/relay/log_allowlist.go` → `allowedLogKeys` — every new log call in this design uses only `server_id`, `binary_conn_id` and `err`, all already allowlisted. No new key.
- `internal/relay/forward_test.go` → `fakePhone`, `newFakeBinarySource`, `claimAndRegister`, `runBinaryForwarder`, `waitForPhoneSent` — the harness the new forwarder tests extend.
- `internal/relay/push_test.go` → `testServiceAccountJSON` — valid key fixture for the from-env constructor test.
- `cmd/pyrycode-relay/main.go` → `run` — where `limiter` is built and `defer`-closed; the waker is built and wired alongside.
- protocol-mobile.md § `push_wake` — the envelope shape `{"push_wake":{"platform":"fcm","token":"…"}}`, no `conn_id`, no `frame`; `apns` is skipped; token never logged; older relays log and drop.
- `docs/knowledge/features/binary-forwarder.md`, `routing-envelope.md`, `fcm-push-sender.md` — per-frame-error-never-kills-the-loop invariant; `#119` close-code precedent.

In-flight overlap: `feature/127` edits `registry.go` (`ClaimServer`); this ticket only adds a field and a setter next to `SetForwarderHooks`, so a later merge is additive.

## Context

The daemon (pyrycode/pyrycode#2564) asks the relay for a phone wake with a second routing-envelope shape that addresses the relay itself. Today the relay drops it as an unmarshal error — the documented older-relay behaviour. This ticket makes the relay recognise it, validate it structurally at the envelope boundary, and hand the token to `FCMSender.Send` asynchronously under two abuse limits. The relay still never reads `frame`; `push_wake` is the second relay-interpreted control field after `close_code`. An ADR is probably warranted ("relay originates outbound calls to Google on the binary's behalf; limits and payload-free wake") — the documentation phase decides.

## Design

### Envelope (`envelope.go`)

- `Envelope` gains `PushWake json.RawMessage \`json:"push_wake,omitempty"\``. It stays raw at this layer so a conn_id envelope that also carries a `push_wake` of any shape (even malformed) decodes exactly as today; `omitempty` keeps `Marshal` output byte-identical.
- `Unmarshal`: when `ConnID` is empty **and** `PushWake` is present (non-empty, non-`null` — the `hasFrame` presence rule), return the envelope with a nil error. Otherwise the existing checks run unchanged (`ErrMissingConnID`, `ErrMissingFrame`). A conn_id envelope never takes the wake branch.
- New unexported `pushWake struct{ Platform, Token string }` and `parsePushWake(raw json.RawMessage) (pushWake, error)`:
  - decode failure (not an object, wrong field types) → bare `ErrMalformedPushWake` (the decoder error is not wrapped, so no fragment of input reaches error text);
  - `Platform != "fcm"` (including `apns`, empty) → `ErrUnsupportedPushPlatform`;
  - empty `Token` → `ErrEmptyPushToken`.
- Error text never includes the platform string or token.

### Waker (`push_wake.go`, new)

```go
type pushSender interface { Send(ctx context.Context, deviceToken string) error }

type PushWaker struct { /* sender, limiter *IPRateLimiter, slots chan struct{},
                           ctx+cancel, mu, closed, wg, logger */ }

func NewPushWaker(sender pushSender, logger *slog.Logger) *PushWaker
func NewPushWakerFromEnv(lookup func(string) (string, bool), logger *slog.Logger) (*PushWaker, error)
func (w *PushWaker) Request(serverID string, raw json.RawMessage) error
func (w *PushWaker) Close()
```

- `NewPushWaker` uses the named constants; an unexported `newPushWaker(sender, refillEvery, burst, maxInFlight, logger)` lets tests shrink them.
- `NewPushWakerFromEnv`: unset → `(nil, nil)` (push off); set → `NewFCMSender` then `NewPushWaker`; builder failure → wrapped `ErrFCMCredentials` (unreachable after `CheckEnvConfig`, loud if it happens).
- `Request` is the forwarder's single entry point:
  1. nil receiver → `ErrPushOff` (nil `*PushWaker` means push is off; the registry holds the concrete pointer, so there is no typed-nil interface trap);
  2. `parsePushWake` errors returned as-is;
  3. under `w.mu`: closed → `ErrPushOff`; `limiter.Allow(serverID)` false → `ErrPushWakeRateLimited`; non-blocking acquire of a `slots` token fails → `ErrPushWakeInFlightCap`; else `wg.Add(1)`;
  4. spawn one goroutine: `sender.Send(w.ctx, token)`; on error log `push_wake_send_failed` with `server_id`, `err`; release the slot; `wg.Done`.
  Never blocks on the sender; never queues.
- Constants, each with its reason in a comment:
  - `pushWakeRefillEvery = 10s`, `pushWakeBurst = 6` — per server-id; wakes are human-paced (a finished turn, a waiting prompt) for a handful of paired devices; burst 6 covers both events for three devices back to back, and 6/min sustained is far above that while bounding how fast one binary can push at Google.
  - `pushWakeEvictionInterval = 5 * time.Minute` — same bounded-memory sweep as the upgrade limiter.
  - `pushWakeMaxInFlight = 32` — global; each send holds a goroutine for at most two `fcmSendTimeout`-bounded calls; 32 bounds goroutines and outbound sockets across every binary regardless of how many server-ids they claim.

### Registry (`registry.go`)

- Field `pushWaker *PushWaker` and `SetPushWaker(w *PushWaker)`, same lifecycle doc as `SetForwarderHooks` (set once at boot; nil = push off).

### Forwarder (`forward.go`)

After a successful `Unmarshal`, before the phone lookup:

```go
if env.ConnID == "" {   // only reachable for a push_wake envelope
    if err := reg.pushWaker.Request(serverID, env.PushWake); err != nil {
        logger.Warn("binary_forwarder_push_wake_dropped", "server_id", …, "binary_conn_id", …, "err", err)
    }
    continue
}
```

Every other path is unchanged.

### Wiring (`main.go`)

After `reg` is built: `NewPushWakerFromEnv(os.LookupEnv, logger)`; error → `refusing to start`, return 2; nil → `logger.Info("push off: PYRYCODE_RELAY_FCM_CREDENTIALS unset")` once; non-nil → `reg.SetPushWaker(w)` and `defer w.Close()`.

## Concurrency model

- One goroutine per admitted wake; count ≤ `pushWakeMaxInFlight` by the buffered `slots` channel. Each exits when `Send` returns: bounded by `fcmSendTimeout` per HTTP call in production, and by `w.ctx` cancellation on `Close`.
- `w.mu` guards `closed` and makes admission (`closed` check, `Allow`, slot acquire, `wg.Add`) atomic against `Close`, so no `wg.Add` races `wg.Wait` and `Allow` is never called after `limiter.Close`. Lock order: `w.mu` → `limiter.mu` only; neither is held across `Send`.
- `Close`: set `closed` under `w.mu` (idempotent), cancel `w.ctx`, `wg.Wait()`, `limiter.Close()`. Hijacked WS connections outlive `http.Server.Shutdown`, so a forwarder may still call `Request` after `Close`; it gets `ErrPushOff`.
- Sends use the waker's own context, not the binary connection's: a wake requested just before the binary disconnects still goes out.

## Error handling

Every drop is one `Warn` line from the forwarder with a sentinel in `err`; the binary loop continues. Send failures are logged from the send goroutine by `FCMSender`'s error, whose text is at most the status (`relay: fcm send failed: status 404`) or a transport error naming only the send URL. No error in this design carries the token.

Sentinels (new, in `envelope.go` / `push_wake.go`): `ErrMalformedPushWake`, `ErrUnsupportedPushPlatform`, `ErrEmptyPushToken`, `ErrPushOff`, `ErrPushWakeRateLimited`, `ErrPushWakeInFlightCap`.

## Testing strategy

Fake sender (`fakePushSender`) records tokens, can block until released or its ctx is cancelled, can return an error.

- `envelope_test.go` (table): wake envelope → nil error, `ConnID==""`, `PushWake` raw; `{"push_wake":null}` and `{}` → `ErrMissingConnID`; conn_id + malformed `push_wake` decodes as today. `parsePushWake` table: fcm ok; apns / other / missing platform → `ErrUnsupportedPushPlatform`; empty token → `ErrEmptyPushToken`; `5`, `"x"`, `{"token":7}` → `ErrMalformedPushWake`; no error text contains the token.
- `push_wake_test.go`: burst exhausted for one server-id → `ErrPushWakeRateLimited`, another server-id still admitted; blocking sender fills `maxInFlight` → next → `ErrPushWakeInFlightCap`, releasing one slot readmits; nil waker → `ErrPushOff`; `Close` cancels a blocked send, waits, later `Request` → `ErrPushOff`; send failure is logged with `server_id`/`err` and the log never contains the token; `NewPushWakerFromEnv` unset → nil, valid key → non-nil.
- `forward_test.go`:
  - AC1: wake with a blocking sender, then a conn_id frame → the phone receives the frame while the send is still blocked; sender saw the token exactly once; no `binary_forwarder_unmarshal_err` in captured logs.
  - AC2: each drop case (apns, other platform, empty token, malformed, push off) → a `binary_forwarder_push_wake_dropped` line, no send, later frame still delivered; conn_id + `push_wake` envelope → forwarded to the phone, no send.
  - Token absence: captured logs across all the above never contain the token string.
- `TestLogKeysAreAllowlisted` covers the new call sites unchanged.

## Open questions

- None blocking. Whether dropped wakes deserve a metric is out of scope per the ticket.

## Documentation handoff (pending — documentation stage)

- `docs/architecture.md` § What this binary does NOT do: replace "Implement push notifications. The binary calls APNs/FCM directly." with the relay sending FCM wakes on request; describe the outbound calls to Google (OAuth token endpoint, FCM send); state that the wake carries no payload.
- `docs/threat-model.md`: new outbound network call (a "Triggers for re-review" item) — the call, the credential secret (never logged), the per-server-id rate (`pushWakeRefillEvery`/`pushWakeBurst`) and the in-flight cap (`pushWakeMaxInFlight`).
- `docs/deploy.md`: secret `PYRYCODE_RELAY_FCM_CREDENTIALS` — whole service-account JSON, optional (unset = push off), set with `fly secrets set PYRYCODE_RELAY_FCM_CREDENTIALS="$(cat key.json)"`.
- `docs/knowledge/features/routing-envelope.md` and `binary-forwarder.md`: the `push_wake` shape and forwarder handling.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the binary is untrusted input; the wake crosses into the relay in exactly one place, `parsePushWake`, called only from `PushWaker.Request`. `Unmarshal` admits a no-conn_id envelope only when `push_wake` is present, and conn_id envelopes never enter that branch, so the phone routing path is unchanged. `frame` stays `json.RawMessage` and is never read on the wake path; `push_wake` is a relay-addressed control object defined by protocol-mobile.md § `push_wake`, not a payload.
- [Trust boundaries] No findings — any authenticated-by-claim binary can supply an arbitrary token (the relay cannot verify it belongs to that daemon's phones). That makes the relay a potential push-spam proxy; bounded by the two limits below, and the push itself carries no payload so a spoofed wake only makes an app reconnect.
- [Tokens, secrets, credentials] No findings — the device token is held only in the send goroutine's closure and the FCM request body; it is never logged (every log call carries only `server_id`, `binary_conn_id`, `err`) and no sentinel's text includes it. `parsePushWake` does not wrap the decoder error. The service-account key is read once from env in `NewPushWakerFromEnv` and handed to `NewFCMSender`; `ErrFCMCredentials` carries no part of it. Tested: captured logs never contain the token.
- [File operations] No findings — no file is read or written; credentials come from env.
- [Subprocess] No findings — none spawned.
- [Crypto] No findings — no new primitives; OAuth JWT signing is `golang.org/x/oauth2/jwt` from #132.
- [Network & I/O] No findings — the envelope is already capped by `maxFrameBytes` on the binary read, which also bounds the token length sent to Google. Outbound calls use `FCMSender`'s client with `fcmSendTimeout` and no redirects. Amplification: one inbound envelope produces at most one outbound send, admitted only under the per-server-id bucket and the global `pushWakeMaxInFlight` cap; excess is dropped, never queued, so memory and goroutines stay bounded.
- [Network & I/O] OUT OF SCOPE — the in-flight cap bounds concurrency, not total throughput across many claimed server-ids; server-id claims are themselves behind the per-IP upgrade limiter. A global wakes-per-second ceiling is not asked for; revisit if abuse is observed (documentation stage to note in `docs/threat-model.md`).
- [Error messages, logs] No findings — no new log key; drops log a sentinel, send failures log `FCMSender`'s status-only error. Nothing is written back to the binary.
- [Concurrency] SHOULD FIX (address in Phase B) — admission must be atomic with `Close` so `wg.Add` never races `wg.Wait` and `IPRateLimiter.Allow` is never called after `IPRateLimiter.Close`; the design holds `w.mu` across admission. Every send goroutine exits on `Send` return, bounded by `fcmSendTimeout` and by the waker context cancelled in `Close`. A test covers `Close` with a blocked send.
- [Threat model] Trips "new outbound network call" — carried in the Documentation handoff. Single instance: the rate buckets are in-memory per process, consistent with the single-instance design.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
