# `/v1/client` — phone-side WebSocket upgrade

`/v1/client` is the relay's ingress for mobile phones. A phone opens an outbound WSS to the relay, sends three required headers (one of which is the device token, opaque to the relay), and — if a binary currently holds the requested `serverID` — gets registered on that binary's phone slice in the connection registry. If no binary holds the slot, the WS is closed with application code `4404`.

This is the public, internet-exposed endpoint. The peer is *less* trusted than `/v1/server`'s peer (which at least runs operator-issued software); anyone on the internet who learns the relay hostname can connect. Header validation runs **before** `websocket.Accept`; the token is presence-checked only and never logged.

This is the phone side only. After header validation and `RegisterPhone`, the handler hands the connection to `StartPhoneForwarder` ([phone-forwarder.md](phone-forwarder.md), #25), which is the read pump for the data path. The heartbeat goroutine ([Heartbeat feature](heartbeat.md), #7) runs alongside the handler — see § Concurrency below for the LIFO defer ordering.

## Wire shape

```http
GET /v1/client HTTP/1.1
Host: relay.example.com
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <base64>
X-Pyrycode-Server: <server-id>
X-Pyrycode-Token: <opaque device token>
User-Agent: <phone UA>
X-Pyrycode-Device-Name: <optional user-supplied label>
```

| Header (canonical) | Required | Source | Enforcement |
|---|---|---|---|
| `X-Pyrycode-Server` | Yes | phone's target server-id | non-empty |
| `X-Pyrycode-Token` | Yes | phone-side device token | **non-empty (presence only — never parsed by the relay)** |
| `User-Agent` | Yes | HTTP standard | non-empty |
| `X-Pyrycode-Device-Name` | No | user-supplied label | logged verbatim if present; absent → empty string |

`r.Header.Get` canonicalises case. Missing or empty among the required set → `400 Bad Request`, empty body, no upgrade, registry untouched, no log line. Wrong method or missing `Upgrade` headers fall through to `websocket.Accept`, which writes the library's standard 4xx.

The relay does not validate the token — the binary does. The token is read into a local string for the presence-check, then goes out of scope unread. It is **never** logged, never compared, never propagated by this handler. Token verification is the binary's responsibility (per protocol spec § Phone → relay → binary).

## Close codes

| Code | Meaning |
|---|---|
| `1000` (`StatusNormalClosure`) | clean close on shutdown / unregister / binary-grace expiry. |
| `4404` | no binary currently holds the requested `serverID`. Reason: `"no server with that id"`. |

`4404` is application-defined per RFC 6455 (4000–4999 range); mirrors the `4409` pattern on `/v1/server`. The close-reason is the protocol-spec literal — it deliberately does not echo the requested server-id back (that would invite probing).

## API

Package `internal/relay` (`client_endpoint.go`):

```go
func ClientHandler(reg *Registry, logger *slog.Logger) http.Handler
```

One exported symbol. No new types, no new sentinel errors, no new package-level constants. No `grace` parameter — phone disconnect is immediate (`UnregisterPhone`); the only grace concept on this endpoint lives entirely inside the registry's `handleGraceExpiry`, which closes orphan phones when a binary's grace window expires (see § Concurrency below).

Wired in `cmd/pyrycode-relay/main.go` next to `/v1/server`:

```go
mux.Handle("/v1/client", relay.ClientHandler(reg, logger))
```

## Algorithm

1. Validate the three required headers. Any missing/empty → `400`, return.
2. Read `X-Pyrycode-Device-Name` (optional; `""` if absent; informational only).
3. `websocket.Accept` with `OriginPatterns: []string{"*"}`. On error, the library has already written a 4xx; return silently.
4. Construct `connID = "client-" + serverID + "-" + randHex8()` (8 hex chars from `crypto/rand`).
5. Wrap with `NewWSConn(c, connID)`.
6. `reg.RegisterPhone(serverID, wsconn)`:
   - On `ErrNoServer` → close the underlying `*websocket.Conn` with code `4404` and reason `"no server with that id"`, log `phone_register_no_server`, return.
   - On any other error → `wsconn.Close()`, return (defensive; not currently reachable).
7. Log `phone_registered`.
8. `defer { reg.UnregisterPhone(serverID, connID); wsconn.Close(); log phone_unregistered }`. Registered **after** the successful `RegisterPhone` so a no-server path never tries to unregister a slot we never owned.
9. `_ = StartPhoneForwarder(r.Context(), reg, serverID, wsconn, logger)` — synchronous read pump that wraps inbound frames and `Send`s them to the binary holding `serverID`. Returns on phone close, ctx cancel, missing binary, or `Send` failure. The handler discards the return; the forwarder logs the cause. See [phone-forwarder.md](phone-forwarder.md).

`randHex8` and `remoteHost` are reused verbatim from `server_endpoint.go` — same package, no duplication.

## Logging

Three structured event types. Field set is fixed; nothing else (token, full headers, payloads) is logged.

| event | fields |
|---|---|
| `phone_registered` | `server_id`, `conn_id`, `device_name`, `remote` |
| `phone_register_no_server` | `server_id`, `remote` |
| `phone_unregistered` | `server_id`, `conn_id` |

Explicitly **not logged on any path:**

- `X-Pyrycode-Token` — the relay never parses, compares, or propagates it; logging it would be a credential leak. The token is read into a local once, presence-checked, and discarded. No `fmt.Errorf`, no debug `Printf` references it — code review enforces this in addition to the handler structurally never reaching for it after the gate.
- `User-Agent` — validated for presence, then discarded (no operational use).
- Any full request-header dump.

`device_name` is the user-supplied label (e.g. `"Juhana's iPhone"`); the threat model's MAY-be-logged list names it explicitly. Empty string on absence; logged literally — slog's text/JSON handlers escape control characters, so log-line forging via crafted device names is structurally blocked. `remote` is the IP host portion of `r.RemoteAddr`, no port.

`conn_id` is logged on register and unregister so operators can correlate a session across both events. `phone_register_no_server` deliberately omits `conn_id` because no `WSConn` was constructed before the close on that path.

Header gate failures (`400`) are not logged — same hygiene rationale as `/v1/server`: avoid amplifying header-floods into log volume.

## Concurrency

| Step | Goroutine | Lock | Lifecycle |
|---|---|---|---|
| Header validation | request goroutine | none | pre-upgrade; no resources held |
| `websocket.Accept` | request goroutine | none | conn allocated on success |
| `RegisterPhone` | request goroutine | registry write lock (held internally) | one-shot |
| `StartPhoneForwarder` | request goroutine, blocking; sole reader of the WS | per-frame `BinaryFor` RLock + `WSConn.writeMu` on the binary | returns on (a) phone-side close, (b) `r.Context()` cancel, (c) `Send` failure to the binary, (d) registry-driven `Close` from binary-grace expiry |
| `defer` (unregister/close/log) | request goroutine | `wsconn.closeOnce` | runs only on the success path; idempotent |

The handler takes no lock of its own. `WSConn.Close`'s `closeOnce` is a `sync.Once`, not a held lock. `UnregisterPhone` is a no-op on unknown `(serverID, connID)`. **No lock-order risk; no goroutine-leak path.**

### Phone close on binary-grace expiry

When a binary disconnects, `/v1/server`'s defer arms `ScheduleReleaseServer(serverID, 30s)`. If no reconnect arrives, the registry's `handleGraceExpiry`:

1. removes the binary entry,
2. snapshots the phones slice,
3. deletes the phones entry from the map,
4. calls `Close()` on every snapshotted phone.

The phone handler's in-flight `WSConn.Read` then aborts with the library's close error (the underlying `*websocket.Conn.Close` was invoked by `WSConn.Close`), `StartPhoneForwarder` returns, the defer runs, and:

- `reg.UnregisterPhone(serverID, connID)` no-ops (entry already deleted in step 3).
- `wsconn.Close()` no-ops (`sync.Once` already fired in step 4).
- `phone_unregistered` log line fires.

The phone observes the close on its socket as `StatusNormalClosure` — by deliberate design, no `4404` / `4409` is sent on grace expiry. Every step is idempotent; the interaction is structurally safe.

## Design notes

- **`OriginPatterns: ["*"]`.** Same rationale as `/v1/server`: the custom-header gate one layer above structurally excludes browser-driven CSWSH. Browser raw WebSocket cannot set custom request headers, and `fetch` with custom headers triggers a CORS pre-flight this endpoint does not satisfy.
- **Direct `c.Close` on the no-server path, not `wsconn.Close()`.** `WSConn.Close` always emits `StatusNormalClosure`; `4404` requires the underlying `*websocket.Conn`. The no-server case is a stillborn WSConn — no `Send` was attempted, no goroutine holds `writeMu` — so the WSConn invariant is preserved in spirit. See [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md).
- **`defer` after `RegisterPhone` succeeds.** If `RegisterPhone` returns `ErrNoServer`, the handler must NOT call `UnregisterPhone` on a slot it never claimed. The registry tolerates it as a no-op today, but the structural rule sharpens once any cleanup grows side effects beyond a no-op (e.g. metrics increment in a future ticket).
- **No `RegisterPhone` retry on `ErrNoServer` even during a binary's grace window.** During grace, `BinaryFor` still returns the (closed) binary, so `RegisterPhone` succeeds and `handleGraceExpiry` later closes the phone cleanly on expiry. If the slot is empty when `RegisterPhone` runs, that's a true 4404 — no other binary will pick up this phone's claim before the phone re-dials. The handler does not retry, does not poll, does not double-check.
- **`StartPhoneForwarder` is the sole reader; `CloseRead` is gone.** Pre-#25 the handler used `c.CloseRead(r.Context())` to drain control frames pending the real read loop. With the forwarder in place, the read loop processes control frames inline with data; retaining `CloseRead` would race the sole-reader contract. See [phone-forwarder.md](phone-forwarder.md).
- **`crypto/rand`-backed `connID` suffix.** 32 bits is sufficient — scoped per server-id, used only as an opaque map key in `UnregisterPhone`. `RegisterPhone` does not dedupe by `ConnID` (registry contract); collision odds at v1 scale are negligible. Using `crypto/rand` over `math/rand` is forward-compat hardening: if a future ticket exposes the conn-id, unguessable bytes avoid creating an oracle.
- **No length cap or charset check on `device_name`.** Informational only; slog escapes control characters; no observed failure motivates a check. If oversized headers ever become an issue, mitigation lives upstream (per-IP rate limit, per-header byte cap) — both deferred to the DoS ticket.

## What this handler deliberately does NOT do

- **No token validation.** The relay is not the trust boundary for the phone token; the binary owns it. The relay's authorization model on `/v1/client` is purely "is there a binary holding this server-id?"
- **No connection caps (per-IP or global).** Documented residual in `docs/threat-model.md` § DoS resistance. Same gap as `/v1/server`, named there, not widened.
- **No phone-count-per-server-id cap.** `phones[serverID]` grows under attack; the broadcast cost (when #6 lands) is the DoS shape that owns it.
- **No inner-frame parsing.** `StartPhoneForwarder` wraps each frame in the routing envelope and forwards opaque bytes; the binary owns inner-frame validation.
- **No heartbeat policy in the handler.** The handler launches `go runHeartbeat(...)` after the successful register and registers `defer cancelHB()` so the goroutine exits cleanly under handler unwind (#7). The heartbeat policy itself — 30s interval, 30s pong timeout, `1011 "heartbeat timeout"` close — lives in `heartbeat.go`. See [Heartbeat feature](heartbeat.md).
- **No phone-side reconnect grace.** The binary-side grace from #20 already closes orphan phones cleanly on expiry; phone-side grace is not in the protocol spec and is out of scope.
- **No `400` log line.** Avoids amplifying header-floods into log volume.
- **No log on `websocket.Accept` errors.** Library writes a 4xx; the failure is visible in the http access log.

## Adversarial framing

- **Server-id enumeration via 4404 vs success.** `4404` confirms "no binary holds that id"; success confirms "a binary holds that id, and the relay accepted my token's *presence*." Both are protocol-spec-defined responses; the relay cannot withhold the signal without breaking phones. The token's *value* is validated by the binary on the first frame post-upgrade — an invalid token surfaces as `4401` from the binary forwarded by the relay (#6's territory). At the relay layer, server-id existence is observable by design.
- **Token leak via crafted log line.** The token is never logged, never put into an error string, never compared with `fmt.Errorf`. Log-field set is enumerated; structurally the handler does not reach for the token after the presence-check. The defence is layered: spec, code review, and the absence of any code path that uses the token after the gate.
- **Crafted `X-Pyrycode-Device-Name` to forge log lines.** slog's text and JSON handlers quote string values and escape control characters. Log-line forging is structurally blocked.
- **Slow-loris on upgrade.** Peer that completes handshake but never sends frames parks one goroutine in `WSConn.Read` indefinitely. Connection caps deferred.
- **Race: binary disconnects between phone's `RegisterPhone` and phone observing its conn live.** `RegisterPhone` succeeds; the binary's grace timer arms in parallel; if grace expires, `handleGraceExpiry` closes this phone; the handler's defer runs cleanly. Phone observes `StatusNormalClosure` and reconnects.
- **Phone re-dials thousands of times to grow the phones slice forever.** Each successful register appends; each disconnect removes via `UnregisterPhone`. The slice does not leak across connections. Steady state is "concurrent live phones" — bounded by file descriptors / connection cap (deferred).
- **Token-presence oracle via 4404.** Token-absent → 400; token-present + no binary → 4404; token-present + binary → success. The 400 vs 4404 difference is gated on token presence, but the *validity* of the token is not testable through the relay (the binary owns that). 4404 is not a presence oracle — it confirms the gate passed, which an attacker already knew because they sent a non-empty value.
- **`crypto/rand` panic.** Same posture as `/v1/server`: panic terminates the connection, http server recovers, defer is not yet registered (panic fires inside `randHex8`, before `RegisterPhone`), registry stays clean.

Verdict from the security review: **PASS**. Reuses `/v1/server`'s audited shape (validate-pre-upgrade, defer-after-success, application-close-codes-on-underlying-conn), narrows the token's lifetime to a single presence-check, excludes the token from every log path by explicit field-set enumeration. The phone/binary lifecycle interactions with #20's grace machinery are walked end-to-end and idempotent on every path.

## Testing

`internal/relay/client_endpoint_test.go`, `package relay`. Same harness as `server_endpoint_test.go`: `httptest.NewServer(ClientHandler(reg, logger))` with `websocket.Dial` clients, `slog.NewTextHandler(io.Discard, nil)` for logs.

`fakeConn` is reused from `registry_test.go` to seed a binary on tests that exercise the success path; tests for the 4404 path skip the seed.

Tests (1:1 with AC bullets):

- `TestClientEndpoint_ValidUpgrade_RegistersPhone` — seed binary; dial with valid headers; poll `reg.PhonesFor` until populated; assert `ConnID()` matches `"client-<id>-<8 hex>"`.
- `TestClientEndpoint_HeaderGate_400` — table-driven over the three required headers; expect `400`; registry empty.
- `TestClientEndpoint_NoBinary_4404` — no seed; dial; one `Read` returns `*websocket.CloseError` with `Code == 4404` and `Reason == "no server with that id"`; registry empty.
- `TestClientEndpoint_PeerClose_UnregistersPhone` — seed; dial; close client; poll `reg.PhonesFor` until empty; binary remains claimed.
- `TestClientEndpoint_MultiplePhones_IndependentLifecycle` — seed; dial three phones; close them in non-FIFO order; assert removal order does not corrupt other entries (covers `UnregisterPhone`'s swap-with-last-then-truncate).
- `TestClientEndpoint_DeviceNameOptional_HandlerAccepts` — dial without and with `X-Pyrycode-Device-Name`; both succeed.

Not tested: registry mocks (use the real one — race-tested in #3); library mocks; exact log output; token-not-logged invariant (would require diverting slog to a buffer; defended by code review + spec).

## Related

- [`/v1/server`](server-endpoint.md) — sibling ingress; this handler reuses its `randHex8`, `remoteHost`, validate-pre-upgrade shape, and stillborn-WSConn close-code pattern.
- [Phone-side frame forwarder](phone-forwarder.md) — the data-path read pump this handler hands the conn to after registration.
- [Connection registry](connection-registry.md) — `RegisterPhone` / `UnregisterPhone` / `ErrNoServer` are the primitives this handler routes through. Its `handleGraceExpiry` closes phones registered here when a binary's grace window expires.
- [WSConn adapter](ws-conn-adapter.md) — what the handler hands to `RegisterPhone`. The direct-`c.Close` on no-server is the documented exception to the WSConn-only invariant.
- [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md) — close-code emission on the underlying `*websocket.Conn`.
- [ADR-0006](../decisions/0006-grace-period-as-reclaim-path.md) — phones registered here are torn down by the registry on binary-grace expiry; this handler's defer is idempotent against that path.
- [Threat model](../../threat-model.md) — log hygiene (token never logged), DoS resistance (connection-cap residual), error-response leakage (generic 4404 reason).
- [Protocol spec § Phone → relay → binary](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#phone--relay--binary) — authoritative wire shape and close-code set.
