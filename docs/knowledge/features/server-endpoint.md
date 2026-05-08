# `/v1/server` — binary-side WebSocket upgrade

`/v1/server` is the relay's ingress for pyry binaries. A binary opens an outbound WSS to the relay, sends three required headers, and — if no other binary holds the same `serverID` — gets registered in the connection registry and held there until the connection ends. The endpoint enforces first-claim-wins via `Registry.ClaimServer`; a duplicate claim is closed with WS code `4409`.

This is the binary side only. Phone ingress (`/v1/client`) is #5; frame forwarding is #6; heartbeat is #7; the 30-second grace period on disconnect is #8. The handler currently holds the connection open by draining-and-discarding frames — #6 swaps in the real read loop in the same call site.

## Wire shape

```http
GET /v1/server HTTP/1.1
Host: relay.example.com
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <base64>
X-Pyrycode-Server: <server-id>
X-Pyrycode-Version: <binary version string>
User-Agent: <binary UA>
```

| Header (canonical) | Source | Enforcement |
|---|---|---|
| `X-Pyrycode-Server` | binary's server-id | non-empty |
| `X-Pyrycode-Version` | binary's version string | non-empty |
| `User-Agent` | HTTP standard | non-empty |

`r.Header.Get` canonicalises case. Validation runs **before** `websocket.Accept`: a missing header returns `400 Bad Request` with an empty body, no upgrade attempted, registry untouched. Wrong method (`POST`) or missing `Upgrade` headers fall through to `websocket.Accept`, which writes the library's standard 4xx. The handler returns silently in both cases.

## Close codes

| Code | Meaning |
|---|---|
| `1000` (`StatusNormalClosure`) | clean close on shutdown / release. |
| `4409` | `serverID` already claimed by another binary. Reason: `"server-id already claimed"`. |

`4409` is application-defined per RFC 6455 (4000–4999 range). Future tickets layer `4401` (token) and `4404` (no server) on `/v1/client`, plus frame-forward errors here.

## API

Package `internal/relay` (`server_endpoint.go`):

```go
func ServerHandler(reg *Registry, logger *slog.Logger) http.Handler
```

One exported symbol. No new types, no new sentinel errors. Wired in `cmd/pyrycode-relay/main.go` next to `/healthz`:

```go
mux.Handle("/v1/server", relay.ServerHandler(reg, logger))
```

## Algorithm

1. Validate the three required headers. Any missing/empty → `400`, return.
2. `websocket.Accept` with `OriginPatterns: []string{"*"}`. On error, the library has already written a 4xx; return silently.
3. Construct `connID = "server-" + serverID + "-" + randHex8()` (8 hex chars from `crypto/rand`).
4. Wrap with `NewWSConn(c, connID)`.
5. `reg.ClaimServer(serverID, wsconn)`:
   - On `ErrServerIDConflict` → close the underlying `*websocket.Conn` with code `4409`, log `server_id_conflict`, return.
   - On any other error → `wsconn.Close()`, return (defensive; not currently reachable).
6. Log `server_claimed`.
7. `defer { reg.ReleaseServer; wsconn.Close; log server_released }`. The defer is registered **after** the successful claim so a missed/conflicted claim never tries to release a slot we don't hold.
8. `readCtx := c.CloseRead(r.Context()); <-readCtx.Done()` — block until the peer closes.

## Logging

Three structured event types. Field set is fixed; nothing else (full headers, payloads, conn-id internals) is logged.

| event | fields |
|---|---|
| `server_claimed` | `server_id`, `binary_version`, `remote` |
| `server_id_conflict` | `server_id`, `remote` |
| `server_released` | `server_id` |

`binary_version` is the value of `X-Pyrycode-Version`, advertised by the binary itself — informational. `remote` is the IP host portion of `r.RemoteAddr`, no port (via `net.SplitHostPort`, with a verbatim fallback). `User-Agent` is required for entry but never logged (no operational use).

The conflict path **deliberately omits** `binary_version`: an unauthenticated probe surface that echoed the version of the legitimate squatter would be a version-disclosure oracle. Header gate failures are not logged at all — avoids amplifying header-floods into log volume.

## Concurrency

| Step | Goroutine | Lock | Lifecycle |
|---|---|---|---|
| Header validation | request goroutine | none | pre-upgrade; no resources held |
| `websocket.Accept` | request goroutine | none | conn allocated on success |
| `ClaimServer` | request goroutine | registry write lock (held internally) | one-shot |
| `CloseRead` | spawns one read-discard goroutine | none | terminates on conn close / `r.Context()` cancel |
| `<-readCtx.Done()` | request goroutine, blocking | none | unblocks on peer close |
| `defer` (release/close/log) | request goroutine | `wsconn.closeOnce` | runs only on the success path; idempotent |

Shutdown: `http.Server` cancels `r.Context()` → `CloseRead`'s read errors → `readCtx` cancels → handler unblocks → defer runs → slot released. `ReleaseServer`'s grace-period wrapper from #8 will hook the same defer.

## Design notes

- **`OriginPatterns: ["*"]`.** `nhooyr.io/websocket.Accept` rejects cross-origin upgrades by default. Programmatic clients don't have to send `Origin`; setting `["*"]` says "this endpoint is not browser-facing." Safe because the custom-header gate one layer above structurally excludes browser-driven CSWSH: raw browser WebSocket cannot set custom request headers, and `fetch` with custom headers triggers a CORS pre-flight this endpoint does not satisfy.
- **Direct `c.Close` on conflict, not `wsconn.Close()`.** `WSConn.Close` always emits `StatusNormalClosure`; the conflict path needs `4409`. Adding a custom-code close to `WSConn` would inflate its surface for one caller. The conflict case is a stillborn WSConn — no `Send` was attempted, no goroutine holds `writeMu` — so the WSConn invariant ("reach the connection only through WSConn methods") is preserved in spirit. The handler comments name this exception so a future reader doesn't generalise it. See [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md).
- **`CloseRead` instead of `for { c.Read() }`.** The handler must keep the goroutine alive until the peer disconnects, but reading frames is #6's job. `CloseRead` spawns a goroutine that drains-and-discards frames (including control frames — pings must be processed for the connection to observe a peer-side close) and returns a context cancelled on close. When #6 lands, `<-readCtx.Done()` is replaced with a real frame loop in the same call site.
- **`crypto/rand`-backed `connID` suffix.** 32 bits is sufficient: scoped per server-id, used only as an opaque map key in `UnregisterPhone`. Birthday bound non-trivial only at ~2¹⁶ live conns under one server-id — far beyond v1 scale. Using `crypto/rand` over `math/rand` is forward-compat hardening: if a future ticket exposes the conn-id (e.g. echoing it in an error envelope), unguessable bytes avoid creating an oracle by accident.
- **`crypto/rand` failure is fatal-by-design.** `randHex8` panics on RNG read failure; on Linux/macOS the OS RNG does not block once boot-initialised, so a failure here means a broken host. `http.Server` panic recovery terminates the connection; the slot is not yet claimed when `randHex8` runs, so the registry stays clean.

## What this handler deliberately does NOT do

- **No length cap or charset check on `serverID`.** Registry treats it as opaque; the protocol spec is the right layer to define shape.
- **No `400` log line.** Avoids amplifying header-floods into log volume.
- **No log on `websocket.Accept` errors.** Library writes a 4xx; the failure is visible in the http access log. A per-failure log would invite log-flood from misconfigured clients. A counter (Prometheus, future) is the right shape if observability later wants this.
- **No connection caps (per-IP or global).** Documented residual in `docs/threat-model.md` § DoS resistance; the WS upgrade path is named there. Inherited gap, not widened.
- **No frame loop.** `CloseRead` discards frames until #6 replaces it.
- **No heartbeat / ping-pong.** #7.
- **No grace-period release.** Immediate release; #8 wraps `ReleaseServer` in a deferred timer.
- **No token validation.** `/v1/server` is unauthenticated by design — the binary owns the trust relationship with phones.
- **No log-line sanitisation.** `slog`'s text handler quotes string values, so a newline in `binary_version` cannot forge a log line. JSON handler in production preserves the property.

## Adversarial framing

The handler is internet-exposed and unauthenticated. Threats considered:

- **Fake `serverID` claim.** First-claim-wins is the trust primitive at this layer. Mitigation lives at the protocol layer (binary validates phone tokens; phones validate the binary out-of-band). Out of scope here.
- **Squatting on a `serverID`.** Legitimate binary sees `4409` until the squatter disconnects. Phones treat unresponsive binaries as offline. Connection caps are a future DoS ticket.
- **Crafted headers (NULs, very long values).** `slog` quotes string values; `net.SplitHostPort` runs on `r.RemoteAddr` (set by `net/http`, not attacker-controlled); `r.Header.Get` does no parsing. No fragile parse step the attacker controls.
- **Space-only headers.** `!= ""` accepts them. Hardening to "trim then non-empty" would be invented enforcement; the fields are informational, not enforcement, and no observed failure motivates it.
- **Frame floods after upgrade.** `CloseRead` discards. One goroutine per connection. No amplification. Per-frame work is a header read + body drop. Acceptable for v1.
- **Browser-driven CSWSH via `OriginPatterns: ["*"]`.** Browsers cannot set custom request headers on raw WebSocket; `fetch` with custom headers triggers CORS pre-flight. The header gate structurally excludes browser attackers; `OriginPatterns: ["*"]` is safe because of the gate, not despite it.
- **`POST /v1/server` to confuse the upgrade.** `websocket.Accept` rejects non-GET with a library 4xx. Registry untouched. Tested.
- **Conn-id collision.** Birthday bound is fine at v1 scale. Widen the suffix if scale ever demands; public API doesn't change.
- **`crypto/rand` failure.** Panic terminates the connection; the http server recovers; other connections continue. A failing host at this depth is operator-visible; refusing to serve is correct (loud failure over silent correction).
- **Goroutine leak on shutdown.** `r.Context()` cancellation propagates through `CloseRead` to `readCtx`; defer runs; no leak.

Verdict from #16's security review: **PASS**. No new defences invented; no new threats opened; the residual risks are the same connection-cap and slow-peer gaps `docs/threat-model.md` already names.

## Testing

`internal/relay/server_endpoint_test.go`, `package relay`. End-to-end against a real `*websocket.Conn` via `httptest.NewServer(ServerHandler(reg, logger))`. Logger is `slog.New(slog.NewTextHandler(io.Discard, nil))` — log-field assertions are out of scope; future tickets that need them swap in a JSON handler over a `bytes.Buffer`.

Tests (1:1 with AC bullets):

- `TestServerEndpoint_ValidUpgrade_RegistersBinary` — valid headers; assert dial succeeds, `reg.BinaryFor(...)` returns a non-nil `Conn` whose `ConnID()` is `"server-<id>-<8 hex>"`.
- `TestServerEndpoint_HeaderGate_400` — table-driven over the three required headers (each row deletes or empties one); plain `http.Client.Do` with standard upgrade headers; expect `400`; `reg.Counts() == (0, 0)`.
- `TestServerEndpoint_DuplicateClaim_4409` — first dial succeeds; second dial with same headers; one `Read` on the second conn returns a `*websocket.CloseError` with `Code == 4409` and `Reason == "server-id already claimed"` (asserted via `errors.As`). First conn stays registered.
- `TestServerEndpoint_PeerClose_ReleasesSlot` — dial, assert registered; client `Close(StatusNormalClosure, "")`; poll `reg.BinaryFor` (~1s, 10ms ticks) because release races the client close; assert eventually unregistered; re-claim succeeds.
- `TestServerEndpoint_WrongMethod_NoPanic` — `GET` without upgrade headers and `POST` with valid pyrycode headers; both expect a 4xx, no panic, registry empty.

Not tested: registry mocks (use the real one — it's race-tested in #3); library mocks; `randHex8` distribution (stdlib); exact log output.

## Related

- [Connection registry](connection-registry.md) — `ClaimServer` / `ReleaseServer` / `ErrServerIDConflict` are the primitives this handler routes through.
- [WSConn adapter](ws-conn-adapter.md) — what the handler hands to `ClaimServer`. The handler's direct-`c.Close` on conflict is the documented exception to the WSConn-only invariant.
- [`/healthz`](healthz.md) — sibling unauthenticated endpoint; mirrors the handler-factory shape.
- [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md) — close-code emission goes through the underlying `*websocket.Conn`, not WSConn.
- [ADR-0003](../decisions/0003-connection-registry-passive-store.md) — narrow `ReleaseServer` so #8's grace logic can wrap it.
- [Threat model](../../threat-model.md) § DoS resistance — connection-cap residual is named there.
- [Protocol spec § Authentication → Binary→relay](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#binary--relay) — authoritative wire shape.
