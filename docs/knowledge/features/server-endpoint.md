# `/v1/server` — binary-side WebSocket upgrade

`/v1/server` is the relay's ingress for pyry binaries. A binary opens an outbound WSS to the relay with routing/version headers and, when configured, an identity-bound admission token. The production admission gate runs before upgrade; an admitted binary — if no other binary holds the same `serverID` — gets registered in the connection registry and held there until the connection ends. The endpoint enforces first-claim-wins via `Registry.ClaimServer`; a claim that conflicts with a held `serverID` no longer closes immediately with `4409` — the handler first pings the incumbent for up to `conflictProbeTimeout` (10s, #112). An incumbent that answers keeps the slot and the claim is closed with `4409`, exactly as before. An incumbent that does not answer — typically a crash-restarted daemon's silently-dropped predecessor, which the heartbeat alone would take up to 60s to notice — is taken over: the new binary gets the slot via `Registry.TakeoverServer` with the same reclaim semantics as a grace-window reclaim (phones evicted with `4404`), and the unresponsive incumbent is closed with `1011 "heartbeat timeout"`. On disconnect (clean close, network error, ping timeout, or being taken over), the slot is scheduled for release after a 30-second grace window — scoped to the handler's own connection via `ScheduleReleaseServerIfHeld` — so a quick reconnect reclaims the slot and evicts its old phones with `4404`, and a displaced handler's own release defer cannot delete its successor's slot.

This is the binary side only. Phone ingress (`/v1/client`) is #5; phone-side frame forwarding is #25; heartbeat is #7. The binary-side data path is wired here via `StartBinaryForwarder` (#26) — the handler hands the conn to the forwarder after the claim and heartbeat are in place, and the forwarder unwraps each routing envelope and writes the inner frame to the addressed phone.

## Wire shape

```http
GET /v1/server HTTP/1.1
Host: relay.example.com
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <base64>
X-Pyrycode-Server: <server-id>
X-Pyrycode-Admission-Token: <configured credential; required in enforce>
X-Pyrycode-Version: <binary version string>
User-Agent: <binary UA>
```

| Header (canonical) | Source | Enforcement |
|---|---|---|
| `X-Pyrycode-Server` | binary's server-id | non-empty; observe/enforce evaluate exactly one value, no comma list and exact mapping lookup |
| `X-Pyrycode-Admission-Token` | private daemon startup configuration | in enforce exactly one 64-character lowercase hex token accepted for that exact identity |
| `X-Pyrycode-Version` | binary's version string | non-empty |
| `User-Agent` | HTTP standard | non-empty |

The legacy `ServerHandler` uses `r.Header.Get` for the three non-token headers. Its validation runs **before** `websocket.Accept`: a missing header returns `400 Bad Request` with a newline body, no upgrade attempted, registry untouched. In production, the admission wrapper runs first and can return the uniform 401 below. Requests reaching the legacy handler with a wrong method (`POST`) or missing `Upgrade` headers fall through to `websocket.Accept`, which writes the library's standard 4xx. The handler returns silently in both cases.

## Daemon admission before upgrade

The [shared admission contract](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#daemon-admission-declared)
is authoritative. `DaemonAdmission` in `daemon_admission.go` owns a private,
immutable startup snapshot built by `NewDaemonAdmissionFromEnv`. Configuration
and mapping validation are described in [Env-var config validator](env-config-validator.md#daemon-admission-configuration).
The production path is metrics observer → attempt-rate limiter → source cap →
global cap → `DaemonAdmission.Wrap` → `ServerHandler`. Outer limits can return
429/503 before admission is evaluated. Direct `ServerHandler` callers retain
legacy behavior; production opts in through the wrapper without changing that API.

| Mode | Admission behavior |
|---|---|
| `off` (unset default) | Delegates without checking credentials or incrementing admission outcomes. |
| `observe` | Evaluates and counts outcomes, then delegates every request to existing header validation and conflict handling. |
| `enforce` | Only `valid` proceeds to existing validation and conflict handling; every other outcome returns 401. |

Only enforce protects against anonymous identity claims and daemon-originated
wakes. A valid credential does not promise upgrade or ownership: version/UA
validation, WebSocket handshake, draining and incumbent conflict handling still
apply. An authorized conflicting claimant can still receive `4409`.

Observe/enforce evaluate the following in order, using header values as delivered
by `net/http`, with no trimming, normalization or first/last list selection:

1. Require exactly one nonempty `X-Pyrycode-Server` value without a comma;
   otherwise `malformed`. Duplicate values are malformed even when identical.
   There is no additional UUID syntax check; the routing identity remains opaque.
2. No `X-Pyrycode-Admission-Token` value → `missing`. Multiple values, empty,
   wrong-length, uppercase, non-hex, whitespace or comma-list values → `malformed`.
   Tokens are exactly 64 lowercase hexadecimal characters.
3. Identity absent from the mapping or mapped to `[]` → `unknown`, even if
   another identity owns the supplied token.
4. Compare all configured candidates with `crypto/subtle.ConstantTimeCompare`,
   without stopping at a match. Exact identity/token pair → `valid`; a token
   accepted only for another identity → `mismatch`; otherwise `unknown`.

All enforce failures at this gate return HTTP 401 with `Cache-Control: no-store`
and exactly `daemon admission rejected\n` (one trailing LF). No rejection reason
or value is echoed. The wrapper never calls the endpoint on rejection: no
WebSocket acceptance, claim, incumbent probe, eviction, grace reservation or
push access occurs, including attempts against an occupied or gracing identity.
Observe records duplicate/coalesced values as malformed without introducing a
401; legacy validation and conflicts still decide what happens afterward.

`pyrycode_relay_daemon_admission_total{outcome}` has five prebound aggregate
labels: `valid`, `missing`, `malformed`, `unknown`, `mismatch`. Observe/enforce
increment once per evaluated request; off exposes the same five series at zero.
The counter has no mode, claimed-identity or token label and no per-identity
rollout proof. `valid` counts admission only, before downstream success or conflict.
There are no admission logs; maps, credentials and raw headers never enter
errors, status, metrics, URLs, routing envelopes or application frames.

The snapshot has no reload hook, goroutine or mutable request state; concurrent
requests read owned maps/slices and increment concurrency-safe counters. Editing
provisioning does not change a running decision. Old/new tokens can overlap for
one identity. Removing a token or identity takes effect only after Relay restart,
which closes existing connections and discards claims/grace state. Authorized
initial connections and reconnects retain forwarding, push and grace reclaim.
Phone authentication and Noise encryption remain daemon-owned; admission gives
neither Noise keys nor device-token ownership proof. See the
[operator rollout and rotation checklist](../../deploy.md#daemon-admission-activation).

## Close codes

| Code | Meaning |
|---|---|
| `1000` (`StatusNormalClosure`) | ordinary handler cleanup via `WSConn.Close()`. |
| `1001` (`StatusGoingAway`) | shutdown fan-out for admitted connections, or handler-owned close when registry admission returns `ErrRegistryDraining`. Reason: `"shutting down"`. |
| `4409` | `serverID` already claimed by another binary **and the incumbent answered the conflict liveness probe** (#112). Reason: `"server-id already claimed"`. |
| `1011` (`StatusInternalError`) | sent to an incumbent that did **not** answer the probe and was taken over (#112). Reason: `"heartbeat timeout"` — the same code and reason the heartbeat itself would eventually send; the probe just gets there sooner. |

`4409` is application-defined per RFC 6455 (4000–4999 range). Future tickets layer `4401` (token) and `4404` (no server) on `/v1/client`, plus frame-forward errors here. `4404` is also what a taken-over incumbent's phones receive, via the same `reclaimCloseCode` a grace-window reclaim uses — see [Connection registry § Grace-period reclaim](connection-registry.md).

## API

Package `internal/relay` (`server_endpoint.go`):

```go
func ServerHandler(reg *Registry, logger *slog.Logger, grace time.Duration, maxFrameBytes int64, metrics *UpgradeMetrics) http.Handler
```

`ServerHandler` retains its existing signature; admission is a separate wrapper. `maxFrameBytes` supplies the read cap and `metrics` is nil-safe. `grace` is the duration the server-id slot is held after disconnect before the registry releases it (see [Connection registry § grace-period reclaim](connection-registry.md)). Wired in `cmd/pyrycode-relay/main.go` next to `/healthz`:

```go
// Inside the existing rate/source/global wrappers:
admission.Wrap(relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes, upgradeMetrics), metricsReg)
```

The `30*time.Second` literal lives at the wiring site, not as a package-level constant: it appears exactly once, the value is policy (matches protocol spec § Authentication → Binary → relay), and inlining it keeps the protocol-spec linkage visible in the file that wires the relay together. Tests pass ms-scale durations.

## Algorithm

Before this algorithm, production evaluates the admission wrapper above.

1. Validate the three legacy required headers. Any missing/empty → `400`, return.
2. `websocket.Accept` with `OriginPatterns: []string{"*"}`. On error, the library has already written a 4xx; return silently.
3. Construct `connID = "server-" + serverID + "-" + randHex8()` (8 hex chars from `crypto/rand`).
4. Wrap with `NewWSConn(c, connID, maxFrameBytes)`.
5. `reg.ClaimServer(serverID, wsconn)`:
   - On `ErrRegistryDraining` from the claim or takeover → `wsconn.CloseWithCode(StatusGoingAway, "shutting down")`, return without success cleanup, heartbeat or forwarder startup. Draining takes precedence over conflicts and grace reclaim.
   - On `ErrServerIDConflict` → call `takeoverUnresponsive(r.Context(), reg, serverID, wsconn, conflictProbeTimeout)` (#112), which pings the incumbent (`BinaryFor` + the conn's `Ping`, outside any registry lock):
     - Incumbent answers, or isn't pingable (test fakes only) → `ErrServerIDConflict` again: close the underlying `*websocket.Conn` with code `4409`, log `server_id_conflict`, return. Unchanged from before #112.
     - Incumbent doesn't answer within `conflictProbeTimeout`, and the request context is still live → call `reg.TakeoverServer(serverID, incumbent, wsconn)`. Only on success does it hand over the slot with grace-reclaim semantics (phones evicted with `4404`); then close the incumbent with `1011 "heartbeat timeout"` in a background goroutine and log `server_id_takeover`. A failed probe begun before draining cannot install a replacement after the boundary; `ErrRegistryDraining` takes the 1001 path above.
     - The probe is cut short by the request context ending (server shutdown) → propagate that error; `wsconn.Close()`, return without logging, same as any other shutdown-time teardown.
   - On any other error → `wsconn.Close()`, return (defensive).
6. Log `server_claimed`.
7. `defer { reg.ScheduleReleaseServerIfHeld(serverID, wsconn, grace); wsconn.Close; log server_released }`. The defer is registered **after** the successful claim so a missed/conflicted claim never tries to schedule a release for a slot we don't hold — and never arms a stray grace timer for an id it never owned. Scoping the schedule to `wsconn` (rather than the unscoped `ScheduleReleaseServer`, #112) matters specifically for a handler that was just displaced by a takeover: its release defer still runs, but as a no-op, because by then the slot belongs to a different conn.
8. Launch the heartbeat goroutine via `hbCtx, cancelHB := context.WithCancel(r.Context()); defer cancelHB(); go runHeartbeat(hbCtx, wsconn, …)` (#7).
9. `_ = StartBinaryForwarder(r.Context(), reg, serverID, wsconn, logger)` (#26) — synchronously runs the per-binary read pump. Returns on `binary.Read` error or ctx cancel; per-frame errors (malformed envelope, unknown `conn_id`, phone `Send` failure) `log + continue` without ending the loop. Return value is observability-only.

## Logging

Four structured event types after admission. Field set is fixed; nothing else (full headers, payloads, conn-id internals) is logged.

| event | fields |
|---|---|
| `server_claimed` | `server_id`, `binary_version`, `remote` |
| `server_id_conflict` | `server_id`, `remote` |
| `server_id_takeover` | `server_id`, `remote` (#112, fires once the unresponsive incumbent has been replaced, before the new binary's `server_claimed` line) |
| `server_released` | `server_id` |

`server_released` fires at the moment the disconnect `defer` runs — i.e. when the slot is *scheduled* for release, not when the grace timer fires. The wording is preserved despite the slight semantic shift (the existing string is what operators search for; renaming would ripple into log queries with no observable benefit). If a future ticket needs to distinguish "scheduled" from "fired," that's a separate event on the registry's expiry path, not a rename here.

`binary_version` is the value of `X-Pyrycode-Version`, advertised by the binary itself — informational. `remote` is the IP host portion of `r.RemoteAddr`, no port (via `net.SplitHostPort`, with a verbatim fallback). `User-Agent` is required for entry but never logged (no operational use).

The conflict and takeover paths **deliberately omit** `binary_version`: a probe surface (anonymous in off/observe) that echoed the version of the legitimate squatter (or of the binary that just displaced it) would be a version-disclosure oracle. Header gate failures are not logged at all — avoids amplifying header-floods into log volume.

## Concurrency

| Step | Goroutine | Lock | Lifecycle |
|---|---|---|---|
| Header validation | request goroutine | none | pre-upgrade; no resources held |
| `websocket.Accept` | request goroutine | none | conn allocated on success |
| `ClaimServer` | request goroutine | registry write lock (held internally) | one-shot |
| `runHeartbeat` | sibling goroutine | none (control frames serialised by the library) | exits on `hbCtx` cancel or pong timeout |
| `StartBinaryForwarder` | request goroutine, blocking | none (per-frame `PhonesFor` RLock, snapshot) | unblocks on `binary.Read` error or ctx cancel; per-frame errors continue |
| `defer cancelHB` | request goroutine | none | LIFO: runs before the release defer |
| `defer` (schedule-release/close/log) | request goroutine | `wsconn.closeOnce` | runs only on the success path; idempotent |

Shutdown first closes registry admission and snapshots handles under the
registry mutex, then closes admitted WebSockets concurrently with 1001.
Their reads unblock, heartbeat cancellation runs and the identity-scoped
release defer unwinds. HTTP shutdown does not wait for hijacked WebSockets.
An already-upgraded handler reaching claim or takeover after the boundary
closes its own socket with 1001 outside the registry lock and shutdown wait
group, even if shutdown has already returned. This preserves the bounded,
best-effort drain without joining paused handlers; see
[graceful shutdown](graceful-shutdown.md#concurrency-model).

Dial success proves upgrade only. With a reader active, a native ping/pong
round trip proves this endpoint reached its post-admission forwarder read
loop, without an application acknowledgement. Process exit drops pending
grace timers and registry state.

## Design notes

- **`OriginPatterns: ["*"]`.** `nhooyr.io/websocket.Accept` rejects cross-origin upgrades by default. Programmatic clients don't have to send `Origin`; setting `["*"]` says "this endpoint is not browser-facing." Safe because the custom-header gate one layer above structurally excludes browser-driven CSWSH: raw browser WebSocket cannot set custom request headers, and `fetch` with custom headers triggers a CORS pre-flight this endpoint does not satisfy.
- **Direct `c.Close` on conflict, not `wsconn.Close()`.** `WSConn.Close` always emits `StatusNormalClosure`; the conflict path needs `4409`. Adding a custom-code close to `WSConn` would inflate its surface for one caller. The conflict case is a stillborn WSConn — no `Send` was attempted, no goroutine holds `writeMu` — so the WSConn invariant ("reach the connection only through WSConn methods") is preserved in spirit. The handler comments name this exception so a future reader doesn't generalise it. See [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md).
- **`StartBinaryForwarder` is the sole reader (#26).** The earlier `CloseRead` + `<-readCtx.Done()` placeholder is deleted, not retained alongside the forwarder: keeping it would spawn a second drain-and-discard goroutine that races the forwarder for the `*websocket.Conn` sole-reader contract. The forwarder processes control frames inline with data reads, so peer close still reaches the handler promptly. See [Binary-side forwarder](binary-forwarder.md) for the read-loop design.
- **`crypto/rand`-backed `connID` suffix.** 32 bits is sufficient: scoped per server-id, used only as an opaque map key in `UnregisterPhone`. Birthday bound non-trivial only at ~2¹⁶ live conns under one server-id — far beyond v1 scale. Using `crypto/rand` over `math/rand` is forward-compat hardening: if a future ticket exposes the conn-id (e.g. echoing it in an error envelope), unguessable bytes avoid creating an oracle by accident.
- **`crypto/rand` failure is fatal-by-design.** `randHex8` panics on RNG read failure; on Linux/macOS the OS RNG does not block once boot-initialised, so a failure here means a broken host. `http.Server` panic recovery terminates the connection; the slot is not yet claimed when `randHex8` runs, so the registry stays clean.
- **`grace` is a constructor parameter, not a package-level var or `Options` struct.** Honest about the dependency; consistent with how the handler already takes `reg` and `logger`; the value is policy and policy belongs at the wiring site (`cmd/pyrycode-relay/main.go`). A package-level override (e.g. `setGracePeriod(t, d)`) would be action-at-a-distance and force test serialisation. An `Options` struct would be premature for a single field.
- **The handler does not validate `grace`.** `grace` never crosses the network boundary — it's a compile-time literal in `main.go`. The registry tolerates degenerate values safely (`d <= 0` fires immediately, `d` near `MaxInt64` never fires before exit). Adding a clamp/panic at this layer would be defensive code with no observed failure mode.
- **`conflictProbeTimeout = 10 * time.Second` is a package constant, not a constructor parameter (#112).** Unlike `grace`, it isn't protocol-mandated policy — it's derived from an internal bound (`WSConn`'s `writeTimeout`, also 10s): a live incumbent's pong is only read between its forwarder's frames, and one synchronous `phone.Send` can hold the forwarder, and so the pong, for up to `writeTimeout`. A shorter probe would displace live binaries more readily; the chosen value is exactly the ceiling below which a probe cannot distinguish "dead" from "briefly busy." `ServerHandler`'s exported signature is unchanged (9 call sites); an unexported `serverHandler` takes `probeTimeout` as an explicit parameter so tests can shrink it to millisecond scale.
- **The probe runs through a narrow `pinger` interface (`Ping(ctx) error`), not through `Conn`.** `*WSConn` satisfies it; the registry's `Conn` interface does not gain a `Ping` method, so `TakeoverServer` and the rest of the registry stay ignorant of liveness checks — the probe is entirely a handler-layer concern. An incumbent that isn't a `pinger` (only possible with test fakes) is treated as if it had answered: the conservative choice, since the handler cannot tell "doesn't implement probing" from "definitely alive."
- **A takeover never closes the incumbent under the registry lock.** `Registry.TakeoverServer` swaps the binary and evicts phones but deliberately does not close `incumbent` itself — the close code (`1011`, not the registry's phone-eviction `4404`) is a handler concern, and closing inside the lock would violate the same "never block on a `Conn` while holding `mu`" rule that keeps `PhonesFor`/`Send` outside it. The handler closes the incumbent in its own goroutine after `TakeoverServer` returns.

## What this handler deliberately does NOT do

- **No additional UUID or charset policy on `serverID`.** Registry treats it as opaque. The admission wrapper checks value count, nonempty identity, no comma list and exact mapping membership; public HTTP parsing also bounds header size.
- **No `400` log line.** Avoids amplifying header-floods into log volume.
- **No log on `websocket.Accept` errors.** Library writes a 4xx; the failure is visible in the http access log. A per-failure log would invite log-flood from misconfigured clients. A counter (Prometheus, future) is the right shape if observability later wants this.
- **No connection-cap policy inside the handler.** Production supplies the shared rate/source/global wrappers and raw listener caps; see [connection caps](connection-cap.md).
- **No inline frame handling.** `StartBinaryForwarder` owns reading; the handler is a thin upgrade-and-claim shell. See [Binary-side forwarder](binary-forwarder.md).
- **No heartbeat policy in the handler.** The handler launches `go runHeartbeat(...)` after the successful claim and registers `defer cancelHB()` so the goroutine exits cleanly under handler unwind (#7). The heartbeat policy itself — 30s interval, 30s pong timeout, `1011 "heartbeat timeout"` close — lives in `heartbeat.go`. See [Heartbeat feature](heartbeat.md).
- **No frame-forward error handling during grace.** A phone whose `Send` errors against the closed binary `Conn` during the grace window is the forwarders' territory (#25 returns; #26 logs+continues); the handler does not observe these.
- **No phone-token validation or Noise termination.** The daemon owns those. Production daemon credentials are checked by `DaemonAdmission.Wrap`; direct handler callers remain unchecked.
- **No log-line sanitisation.** `slog`'s text handler quotes string values, so a newline in `binary_version` cannot forge a log line. JSON handler in production preserves the property.

## Adversarial framing

The endpoint is internet-exposed. Off/observe retain anonymous admission; enforce admits only holders of credentials for the exact claimed identity. Threats considered:

- **Fake `serverID` claim.** Enforce rejects anonymous and cross-identity claims before registry work. A stolen token still permits claiming its bound identity and requesting wakes, but cannot impersonate the daemon to a paired phone without its Noise private key. Off/observe retain the anonymous first-claim-wins risk.
- **Squatting on a `serverID`.** In enforce the squatter must hold a credential for that identity; off/observe permit anonymous squatting. A squatter that keeps answering the conflict probe cannot be displaced — a live binary is never taken over, so a legitimate binary still sees `4409` for as long as the squatter stays responsive (#112). If the squatter disconnects or goes silent (drops without a clean close, or simply stops answering pings), the legitimate binary no longer has to wait for the heartbeat to notice: the very next conflicting claim probes the incumbent and takes the slot within `conflictProbeTimeout` (≤10s) if it doesn't answer, instead of waiting up to 60s for the heartbeat plus the pre-#112 conflict. After a clean disconnect the slot is additionally held for `grace` seconds — but `ClaimServer` *during* the grace window succeeds (registry's reclaim path), so a competing legitimate binary lands in the slot inside that window rather than being blocked. A caller admitted to the handler can trigger one probe per conflicting upgrade attempt, but that upgrade already sits behind the per-IP rate limiter, and a probe costs one ping frame — no amplification. Net effect versus pre-#112: the window in which a squatter can hold a `serverID` after going silent only shrinks, it never grows. Existing rate/source/global and raw caps bound connection resources.
- **Grace-window map growth.** `binaries` and `timers` map sizes scale as `grace × accept-rate` in the worst case (an attacker rapidly connect-disconnects with distinct server-ids, leaving a 30 s residual entry per id). Each entry is bounded — the timer eventually fires and reclaims it — so growth is not unbounded. Existing rate and connection caps bound admission pressure; enforce additionally restricts claimable identities to the configured mapping.
- **Crafted headers (NULs, very long values).** `slog` quotes string values; `net.SplitHostPort` runs on `r.RemoteAddr` (set by `net/http`, not attacker-controlled); `r.Header.Get` does no parsing. No fragile parse step the attacker controls.
- **Whitespace handling.** Legacy version/UA checks remain presence-only. Admission matches the identity exactly as supplied by `net/http` and requires the token's exact lowercase-hex shape; it adds no trimming or normalization.
- **Frame floods after upgrade.** `StartBinaryForwarder` processes each envelope: `Unmarshal` (structural), bounded routing lookup and nonblocking opaque-frame enqueue to the phone outbox. Per-envelope work is bounded; production supplies `WSConn`'s 256 KiB read cap. One goroutine per connection. Acceptable for v1.
- **Browser-driven CSWSH via `OriginPatterns: ["*"]`.** Browsers cannot set custom request headers on raw WebSocket; `fetch` with custom headers triggers CORS pre-flight. The header gate structurally excludes browser attackers; `OriginPatterns: ["*"]` is safe because of the gate, not despite it.
- **`POST /v1/server` to confuse the upgrade.** `websocket.Accept` rejects non-GET with a library 4xx. Registry untouched. Tested.
- **Conn-id collision.** Birthday bound is fine at v1 scale. Widen the suffix if scale ever demands; public API doesn't change.
- **`crypto/rand` failure.** Panic terminates the connection; the http server recovers; other connections continue. A failing host at this depth is operator-visible; refusing to serve is correct (loud failure over silent correction).
- **Shutdown admission race.** Upgrade precedes registry admission. The registry drain guard rejects late claims, grace reclaims and takeovers; the handler owns their bounded 1001 close. Admitted sockets close through the concurrent shutdown fan-out; the forwarder returns and existing defers cancel the heartbeat and schedule identity-scoped release.

Verdicts from #16's, #21's, and #112's security reviews: **PASS**. No new defences invented; no new threats opened. #21 inherits #20's well-tested registry-side semantics for grace; the residual `grace × accept-rate` map-size scaling is named here and tracked to the per-IP/total-cap tickets that own it. #112's review recorded one residual risk out of scope for that ticket: a *live* incumbent could miss the conflict probe if its forwarder was stalled delivering to a slow phone (pong handling shares the same read pump), so a genuine duplicate binary claim could displace it — tracked as issue #140. Since [#113](phone-outbox.md), that residual no longer holds: delivery to a phone goes through its own bounded queue, so the forwarder's read pump — and the incumbent's pong handling with it — never waits on a phone write. See [`docs/threat-model.md`](../../threat-model.md) § DoS resistance.

## Testing

`daemon_admission_test.go` covers configuration in every mode, decoded duplicate
JSON keys (including escaped spellings), cross-identity token reuse, exact header
values, uniform responses and the five fixed counter series. Its occupied/grace
cases assert no probe, eviction, timer replacement, log or push on rejection.
Authorized conflict/concurrent reconnect cases retain 4409; snapshot/restart
cases prove overlap, revocation, 1001 shutdown and fresh registry state. A real
WebSocket reconnect reclaims grace, evicts old phones with 4404, then forwards
synthetic opaque ciphertext unchanged and reaches a push spy.
`cmd/pyrycode-relay/daemon_admission_test.go` checks startup refusal before listeners.
These are hermetic proofs, not production activation evidence.

Test exact header values directly as well as through real HTTP peers: Go parses
surrounding HTTP field whitespace before the handler sees it. Direct requests
can exercise unchanged-value validation; internal whitespace and duplicate values
survive network parsing and exercise the real gate. Reuse existing conflict,
grace, shutdown and opaque-forwarding tests for those unchanged boundaries.

`internal/relay/server_endpoint_test.go`, `package relay`. End-to-end against a real `*websocket.Conn` via `httptest.NewServer(ServerHandler(reg, logger, grace, 256*1024, nil))`. Logger is `slog.New(slog.NewTextHandler(io.Discard, nil))` — log-field assertions are out of scope; future tickets that need them swap in a JSON handler over a `bytes.Buffer`.

The shared `startServer(t, grace)` helper threads the grace duration through. Tests that don't exercise the disconnect path pass any small value (e.g. `100*time.Millisecond`); the assertion-rich grace test passes `200*time.Millisecond` so its early-window check has comfortable margin against scheduler jitter on slow CI runners.

Tests (1:1 with AC bullets):

- `TestShutdown_LateUpgradeGoingAway` and `TestShutdown_ConflictProbeDuringDrain` in `shutdown_admission_test.go` pause after upgrade or during an incumbent probe, complete shutdown, then resume. The late binary must receive wire 1001 without a claim, reclaim or takeover; see [shutdown testing](graceful-shutdown.md#testing).
- `TestServerEndpoint_ValidUpgrade_RegistersBinary` — valid headers; assert dial succeeds, `reg.BinaryFor(...)` returns a non-nil `Conn` whose `ConnID()` is `"server-<id>-<8 hex>"`.
- `TestServerEndpoint_HeaderGate_400` — table-driven over the three required headers (each row deletes or empties one); plain `http.Client.Do` with standard upgrade headers; expect `400`; `reg.Counts() == (0, 0)`.
- `TestServerEndpoint_DuplicateClaim_4409` — first dial succeeds and calls `CloseRead` so it answers the conflict probe (#112: a client that never reads is unresponsive by the ticket's own definition); second dial with same headers; one `Read` on the second conn returns a `*websocket.CloseError` with `Code == 4409` and `Reason == "server-id already claimed"` (asserted via `errors.As`). First conn stays registered, and a phone registered against it stays open — a refused duplicate claim must not touch the incumbent's phones.
- `TestServerEndpoint_UnresponsiveIncumbent_TakenOver` (#112) — via the unexported `serverHandler` with a short `probeTimeout`: dial an incumbent that never reads, register a phone against it, dial a second claim. Asserts the registry's binary becomes the second conn within roughly `probeTimeout`, the phone is closed with `4404`, the incumbent's own `Read` returns a close error with code `1011`, the takeover is logged as `server_id_takeover`, and — critically — once the displaced handler's `server_released` log line proves its disconnect defer has run, waiting a further `3×grace` still finds the *new* binary holding the slot (the regression `TestScheduleReleaseServerIfHeld_DisplacedConn_NoOp` in `registry_test.go` pins the same property at the registry layer).
- `TestServerEndpoint_PeerClose_ReleasesSlot` — dial, assert registered; client `Close(StatusNormalClosure, "")`; poll `reg.BinaryFor` (~1s, 10ms ticks); assert eventually unregistered; re-claim succeeds. End-to-end "disconnect → eventual release → re-claim" property; the short grace makes the existing 2-second wait still meaningful.
- `TestServerEndpoint_PeerClose_SchedulesGraceRelease` (#21) — proves *scheduled, not immediate*. Two phases against a 200 ms grace: phase 1 polls `reg.BinaryFor` for `grace/4` after the client close and fails if the entry disappears (a buggy immediate-release path would flip it within milliseconds); phase 2 polls for `2*grace + 500ms` and fails if the entry is still there. Inspects the registry directly because `ScheduleReleaseServer` returns nothing.
- `TestServerEndpoint_WrongMethod_NoPanic` — `GET` without upgrade headers and `POST` with valid pyrycode headers; both expect a 4xx, no panic, registry empty.

Not tested: registry mocks (use the real one — it's race-tested in #3); library mocks; `randHex8` distribution (stdlib); exact log output.

## Related

- [Env-var config validator](env-config-validator.md#daemon-admission-configuration) — exact provisioning and static startup errors.
- [Binary-side forwarder](binary-forwarder.md) — the read pump wired in step 9; replaces the `CloseRead` placeholder.
- [Connection registry](connection-registry.md) — `ClaimServer` / `ReleaseServer` / `ErrServerIDConflict` are the primitives this handler routes through; `TakeoverServer` and `ScheduleReleaseServerIfHeld` (#112) are the identity-scoped primitives the conflict-probe path adds.
- [Heartbeat feature](heartbeat.md) — the probe's close code and reason (`1011 "heartbeat timeout"`) are borrowed verbatim from the heartbeat's own timeout close, since a probe timeout is exactly what the heartbeat would have produced given more time.
- [WSConn adapter](ws-conn-adapter.md) — what the handler hands to `ClaimServer`. The handler's direct-`c.Close` on conflict is the documented exception to the WSConn-only invariant.
- [`/healthz`](healthz.md) — sibling unauthenticated endpoint; mirrors the handler-factory shape.
- [ADR-0005](../decisions/0005-application-close-codes-via-underlying-conn.md) — close-code emission goes through the underlying `*websocket.Conn`, not WSConn.
- [ADR-0006](../decisions/0006-grace-period-as-reclaim-path.md) — the grace window IS the reclaim path; the handler's disconnect defer hooks into this via `ScheduleReleaseServerIfHeld` (identity-scoped since #112). The probe-driven takeover is a second route into the same reclaim semantics, documented in the ADR's 2026-09-24 amendment.
- [ADR-0003](../decisions/0003-connection-registry-passive-store.md) — narrow `ReleaseServer` left room for the grace-period wrapper this handler now uses.
- [Threat model](../../threat-model.md) § DoS resistance — connection-cap residual is named there.
- [Protocol spec § Authentication → Binary→relay](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#binary--relay) — authoritative wire shape.
