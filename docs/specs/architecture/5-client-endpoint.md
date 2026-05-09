# Spec — `/v1/client` WS upgrade, header gate, phone register/unregister (#5)

## Files to read first

- `internal/relay/server_endpoint.go` — full file (122 lines). The phone handler is a structural twin: same shape (validate → upgrade → wrap → register → defer-unregister → CloseRead/wait). Reuse `randHex8` and `remoteHost` verbatim — they live in this same package and need no duplication.
- `internal/relay/registry.go:18-21` — `ErrNoServer` sentinel mapped to WS close code 4404; the handler branches on this with `errors.Is`.
- `internal/relay/registry.go:186-228` — `RegisterPhone` / `UnregisterPhone` semantics. Note: `RegisterPhone` returns `ErrNoServer` (not `ErrServerIDConflict`); does NOT deduplicate by `ConnID` (caller's responsibility — random 32-bit suffix makes accidental collision negligible). `UnregisterPhone` is a no-op on unknown `(serverID, connID)`, so a defer that fires after the registry has already torn down the entry (e.g. via `handleGraceExpiry` orphan-close at line 181) is safe.
- `internal/relay/registry.go:148-184` — `ScheduleReleaseServer` + `handleGraceExpiry`. Critical for the phone handler: when the binary's grace window expires, the registry calls `Close()` on every registered phone (line 181-183) and removes them from the map. The phone handler's `CloseRead`-blocked goroutine then unblocks (because `WSConn.Close` cancels the underlying conn's context), runs its defer, and calls `UnregisterPhone` — which no-ops because the entry was already removed. This interaction is structurally safe; the spec re-confirms it under § Concurrency.
- `internal/relay/ws_conn.go` — full file (83 lines). Phone wraps `*websocket.Conn` with `NewWSConn(c, connID)` exactly like #16. The 4404 close path uses the same "stillborn WSConn — close directly on `*websocket.Conn` so the application close code reaches the wire" pattern documented in PROJECT-MEMORY § "Application WS close codes are emitted on the underlying `*websocket.Conn`, not via `WSConn`."
- `internal/relay/server_endpoint_test.go` — full file (357 lines). The phone test file mirrors `startServer`, `dialWith`, `validHeaders` pattern verbatim with phone-side header set; the close-code assertion shape (`errors.As(_, &websocket.CloseError)`) is reused.
- `cmd/pyrycode-relay/main.go:48-50` — current mux registration site. One new line slots in next to `/v1/server`.
- `docs/threat-model.md:42-55` § "Log hygiene" — enumerates what MUST NOT be logged. **Tokens are top of that list.** The phone gate sees a token; the handler must never put it in a log line, error message, or response body. `device_name` is in the MAY-be-logged list (advertised by the binary, not user-secret).
- `docs/threat-model.md:77-85` § "Error response leakage" — public-facing handlers return generic messages.
- `docs/PROJECT-MEMORY.md:36-38` — the four patterns this handler inherits: handler-factory shape, validate-before-upgrade, app-close-codes-on-underlying-conn, defer-after-successful-claim ordering.
- [`pyrycode/pyrycode/docs/protocol-mobile.md` § Phone → relay → binary](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#phone--relay--binary) — authoritative spec for the phone-side headers and the `4404` close code. The relay does NOT validate the token; the binary does.
- `nhooyr.io/websocket` package docs — `Accept`, `AcceptOptions.OriginPatterns`, `StatusCode`, `Conn.CloseRead`, `Conn.Close`. Same calls as #16.

## Context

Implements the phone-side handshake. The endpoint accepts an inbound WebSocket from a mobile client on `/v1/client`, validates three required headers BEFORE upgrading, upgrades on success, looks up the binary holding `x-pyrycode-server`, and registers the phone connection in the registry. If no binary holds the slot, the WS is closed with application code `4404`.

The relay is **not** the trust boundary for the phone token: per the protocol spec, `x-pyrycode-token` is opaque to the relay — presence-checked only, never parsed, never compared, never logged. The binary owns token verification. The relay's authorization model on `/v1/client` is purely: "is there a binary holding this server-id?"

This is the phone-ingress only. Frame forwarding is #6, heartbeat is #7, the 30-second grace window on disconnect is #8 (registry-side already shipped via #20; phone-side close code on grace expiry is registry-handled — see § "Phone close on binary grace-expiry"). Out of scope here: any reading from the phone WS beyond `CloseRead`'s drain-and-discard goroutine.

## Design

### Package & files

- New file: `internal/relay/client_endpoint.go` (`package relay`).
- New test file: `internal/relay/client_endpoint_test.go` (`package relay`).
- Edit: `cmd/pyrycode-relay/main.go` adds one line registering the handler on the mux.

No changes to `registry.go`, `ws_conn.go`, `envelope.go`, `tls.go`, `healthz.go`, `server_endpoint.go`, `doc.go`, `go.mod`, or `Makefile`. `randHex8` and `remoteHost` are package-private helpers in `server_endpoint.go` and are reused as-is.

### Public API

```go
package relay

// ClientHandler returns the http.Handler for /v1/client: the phone-side
// WebSocket upgrade endpoint. It validates required headers, upgrades the
// connection, registers the phone in reg under the requested server-id, and
// holds the connection open until the phone closes it (or the registry
// tears it down on binary-grace expiry).
//
// Close codes used by this endpoint:
//   - 1000 (StatusNormalClosure)        clean close on shutdown / unregister.
//   - 4404 (no server with that id)     no binary currently holds the slot.
//
// The relay treats x-pyrycode-token as opaque — its presence is required,
// its value is never parsed, compared, or logged. Token verification is the
// binary's responsibility (per protocol spec § Phone → relay → binary).
func ClientHandler(reg *Registry, logger *slog.Logger) http.Handler
```

One exported symbol. No new types, no new sentinel errors. Note: no `grace` parameter — phone disconnect is immediate (`UnregisterPhone`); no policy decision is wired through this handler. The grace concept on `/v1/client` lives entirely in the registry's `handleGraceExpiry` (it closes orphan phones on binary-grace expiry); see § Concurrency.

### Required headers (validated pre-upgrade)

| Header (canonical)        | Required | Source                | Enforcement                                               |
|---------------------------|----------|-----------------------|-----------------------------------------------------------|
| `X-Pyrycode-Server`       | Yes      | Phone's target id     | non-empty                                                 |
| `X-Pyrycode-Token`        | Yes      | Phone-side device tok | non-empty (presence only — never parsed by the relay)     |
| `User-Agent`              | Yes      | HTTP standard         | non-empty                                                 |
| `X-Pyrycode-Device-Name`  | No       | User-supplied label   | logged verbatim if present; absent → log empty string     |

`r.Header.Get` performs canonicalisation (case-insensitive). Missing or empty among the required set → `http.Error(w, "", http.StatusBadRequest)` with no body. Registry untouched, `Accept` not called, no log line emitted (gate is silent — same hygiene rationale as #16's gate).

### Algorithm

```
1. Validate the three required headers. Any missing/empty → 400, return.
   Read x-pyrycode-device-name; "" if absent (no validation; informational).
2. websocket.Accept(w, r, &websocket.AcceptOptions{
       OriginPatterns: []string{"*"},
   })
   - On error: the library has already written a 4xx; just return.
     Registry untouched.
3. connID := "client-" + serverID + "-" + randHex8()
4. wsconn := NewWSConn(c, connID)
5. err := reg.RegisterPhone(serverID, wsconn)
   - errors.Is(err, ErrNoServer):
       _ = c.Close(websocket.StatusCode(4404), "no server with that id")
       logger.Info("phone_register_no_server",
           "server_id",   serverID,
           "remote",      remoteHost(r))
       return
   - other err (not produced by current registry, but for completeness):
       wsconn.Close()
       return
6. logger.Info("phone_registered",
       "server_id",   serverID,
       "conn_id",     connID,
       "device_name", deviceName,
       "remote",      remoteHost(r))
7. defer:
       reg.UnregisterPhone(serverID, connID)
       wsconn.Close()
       logger.Info("phone_unregistered",
           "server_id", serverID,
           "conn_id",   connID)
8. // Hold the connection open until the peer closes it. Frame loop (#6)
   // replaces this block with a real read loop later.
   readCtx := c.CloseRead(r.Context())
   <-readCtx.Done()
```

Three design notes inside this algorithm:

**Why `c.Close` directly on the no-server path instead of `wsconn.Close()`.** Same reasoning as #16's `4409` path: `WSConn.Close()` always sends `StatusNormalClosure`; the no-server path needs application-defined `4404`. The WSConn here is stillborn — no `Send` was attempted, no concurrent goroutine holds `writeMu` — so the WSConn doc invariant is preserved in spirit. Pattern is documented in PROJECT-MEMORY § "Application WS close codes are emitted on the underlying `*websocket.Conn`, not via `WSConn`."

**Why the defer is registered AFTER `RegisterPhone` succeeds, not before.** Mirrors #16 exactly. If `RegisterPhone` returns `ErrNoServer`, the handler must NOT call `UnregisterPhone` on a slot it never registered to (registry tolerates it as a no-op, but the structural rule from PROJECT-MEMORY § "`defer { … }` is registered AFTER the successful claim" applies: never schedule cleanup on resources we don't own — the rule sharpens once any cleanup grows side effects beyond a no-op, e.g. metrics increment in a future ticket).

**Why `CloseRead` instead of `for { c.Read() }` or a bare block.** Same reasoning as #16: keep the goroutine alive, drain control frames so the conn observes peer-close, defer to #6 for the real read loop. Swap site is `<-readCtx.Done()` → real loop body in the same goroutine.

### Phone close on binary grace-expiry — interaction with #20

When a binary disconnects, #21's handler arms `ScheduleReleaseServer(serverID, 30s)`. If no reconnect arrives within 30s, the registry's `handleGraceExpiry` (registry.go:169-184):

1. removes the binary entry,
2. snapshots the phones slice,
3. deletes the phones entry from the map,
4. calls `Close()` on every snapshotted phone (each phone is a `*WSConn`; its `Close` cancels its context and closes the underlying `*websocket.Conn`).

The phone handler's `CloseRead`-blocked goroutine sees its context cancelled (the read goroutine errors out, `readCtx.Done()` fires), runs the defer:

- `reg.UnregisterPhone(serverID, connID)` — no-op (entry already deleted in step 3).
- `wsconn.Close()` — no-op (sync.Once already fired in step 4).
- log line `phone_unregistered` — fires.

This interaction is structurally safe: every step is idempotent. The phone observes the close on its socket as `StatusNormalClosure` (registry's `Close()` path through `WSConn.Close`). No 4404 / 4409 / other code is sent on grace-expiry — that's a registry-side concern that registry-#20 explicitly chose to leave at `StatusNormalClosure`. Out of scope to change in this ticket.

### Helper functions

None new. `randHex8` and `remoteHost` already exist in `server_endpoint.go` (same package); reuse verbatim. The 32-bit suffix gives collision odds of ~negligible per server-id; `RegisterPhone` does not dedupe by `ConnID` (the registry's contract notes this), so colliding ids would manifest as two phones with the same key — `UnregisterPhone` removes the first match by linear scan, leaving the second registered until its own defer runs and finds nothing to unregister. At v1 scale (≤ tens of phones per server-id), collision is statistically irrelevant.

### `cmd/pyrycode-relay/main.go` edit

One line, inserted after the `/v1/server` registration (`main.go:50`):

```go
mux.Handle("/v1/client", relay.ClientHandler(reg, logger))
```

`logger` and `reg` are already in scope. No other edits.

### Concurrency model

The handler runs in the http.Server's per-request goroutine. The `WSConn` it constructs is one of N entries in `r.phones[serverID]`; broadcasts (future tickets) reach it through `reg.PhonesFor`. There is one `CloseRead`-spawned read-discard goroutine per accepted connection.

| Method / step | Goroutine | Lock | Lifecycle |
|---|---|---|---|
| Header validation | request goroutine | none | pre-upgrade; no resources held |
| `websocket.Accept` | request goroutine | none | conn allocated on success |
| `RegisterPhone` | request goroutine | registry write lock (held internally) | one-shot |
| `CloseRead` | spawns one read-discard goroutine | none | terminates on conn close / ctx cancel |
| `<-readCtx.Done()` | request goroutine, blocking | none | unblocks on (a) peer close, (b) registry-driven `wsconn.Close` from grace expiry, (c) `r.Context()` cancel from server shutdown |
| `defer { Unregister; wsconn.Close; log }` | request goroutine | `wsconn`'s `closeOnce` | runs on every successful register path; idempotent |

Three unblock paths feed `<-readCtx.Done()`:

- **Peer close.** Phone closes the socket; `CloseRead`'s read errors; ctx cancels.
- **Binary grace-expiry close.** Registry's `handleGraceExpiry` calls `wsconn.Close()` on this phone; `WSConn.Close` cancels the per-conn ctx and closes the `*websocket.Conn`; `CloseRead`'s read errors; ctx cancels.
- **Server shutdown.** http server cancels `r.Context()`; `CloseRead`'s read returns; ctx cancels. Defer runs, unregister no-ops if other phones beat it (or if grace-expiry already cleared the slice).

All three paths converge on the same defer; all defer steps are idempotent.

### Lock-order / deadlock

The handler takes no lock of its own. Registry methods take the registry's `mu` internally. `WSConn.Close`'s `closeOnce` is a `sync.Once`, not a held lock. The `CloseRead` goroutine never touches the registry. **No lock-order risk.**

The one subtlety the spec calls out for clarity: `UnregisterPhone` calls `c.ConnID()` under the registry write lock (registry.go:215). For a `*WSConn`, `ConnID()` is a pure getter on the `connID` field — non-blocking, fits the registry's interface contract. No new constraint.

### Error handling

- **Missing/empty required header** → `400 Bad Request`, empty body. No log line. Registry untouched.
- **`websocket.Accept` error** — library writes its own 4xx; handler returns silently. Registry untouched.
- **`RegisterPhone` returns `ErrNoServer`** — close 4404, log `event=phone_register_no_server server_id=<id> remote=<host>`. Header values are NOT logged on this path; in particular **the token never appears in a log line** (it does not appear on the success path either). This event includes `server_id` because the no-server signal is operationally useful (a phone targeting a binary that disconnected); it's the same field the threat model already names as MAY-be-logged.
- **`RegisterPhone` returns any other error** — not produced by the current registry. Defensively: `wsconn.Close()` and return without logging. If a future registry change introduces such an error, that diff adds the log.
- **`CloseRead` / `<-readCtx.Done()`** does not fail — returns when the conn ends.
- **No panics from this handler.** `randHex8`'s panic-on-RNG-failure is fatal-by-design; same as #16.

### Logging — exact field set

Per `docs/threat-model.md` § "Log hygiene", this handler emits three event types:

| event                       | fields                                                |
|-----------------------------|-------------------------------------------------------|
| `phone_registered`          | `server_id`, `conn_id`, `device_name`, `remote`       |
| `phone_register_no_server`  | `server_id`, `remote`                                 |
| `phone_unregistered`        | `server_id`, `conn_id`                                |

Explicitly **NOT** logged on any path:

- `x-pyrycode-token` — never appears as a field, never appears inside a formatted error string. The token is read into a local variable for the presence-check, then goes out of scope unread.
- `User-Agent` — validated for presence, discarded (same as #16; matches the protocol-spec posture: phone identifies itself via `device_name`, not via `User-Agent`).
- Any full-request-header dump.

`device_name` is the user-supplied label (e.g. `"Juhana's iPhone"`); the threat model's MAY-be-logged list names it explicitly. Empty string on absence; logged literally — slog's text/JSON handlers escape control characters, so log-line forging is structurally blocked. `remote` is the IP host portion of `r.RemoteAddr`, no port (`remoteHost` strips it).

`conn_id` is logged on register and unregister so operators can correlate a phone session across the two events. `phone_register_no_server` deliberately omits `conn_id` because no `WSConn` was constructed before the close on that path (handler returns inside the error branch before line 4 of the algorithm).

## Testing strategy

`internal/relay/client_endpoint_test.go`, `package relay`. Same harness shape as `server_endpoint_test.go`: `httptest.NewServer(ClientHandler(reg, logger))` with `websocket.Dial` clients.

### Test harness

```go
// startClient spins up an httptest.NewServer running ClientHandler against
// a fresh registry. Returns the registry (for assertions), the WS URL, and
// a cleanup. Tests that need a binary registered call seedBinary on the
// returned registry before dialling.
func startClient(t *testing.T) (*Registry, string, func()) {
    t.Helper()
    reg := NewRegistry()
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    srv := httptest.NewServer(ClientHandler(reg, logger))
    wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
    return reg, wsURL, srv.Close
}

func dialWithClient(t *testing.T, wsURL string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
    t.Helper()
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
}

func validClientHeaders(serverID string) http.Header {
    h := http.Header{}
    h.Set("X-Pyrycode-Server", serverID)
    h.Set("X-Pyrycode-Token", "phone-token-opaque")
    h.Set("User-Agent", "pyry-phone-test/0.1.0")
    return h
}

// seedBinary places a no-op fakeConn (defined in registry_test.go) as the
// binary for serverID, so RegisterPhone succeeds. Tests that exercise the
// 4404 path skip this.
func seedBinary(t *testing.T, reg *Registry, serverID string) {
    t.Helper()
    if err := reg.ClaimServer(serverID, &fakeConn{id: "bin-" + serverID}); err != nil {
        t.Fatalf("seed ClaimServer: %v", err)
    }
}
```

`fakeConn` already exists in `registry_test.go` (same package, same build) — reuse it.

### Tests (1:1 with AC bullets)

1. **`TestClientEndpoint_ValidUpgrade_RegistersPhone`** — seed binary on `"s1"`; dial with `validClientHeaders("s1")`; assert `Dial` returns no error; poll `reg.PhonesFor("s1")` (bounded retry, ~1 s, 10 ms sleep) until a phone appears; assert exactly one phone present, its `ConnID()` starts with `"client-s1-"` and ends with 8 hex chars (regex / `len`+`hex.DecodeString`).

2. **`TestClientEndpoint_HeaderGate_400`** — table-driven over the three required headers; each row builds `validClientHeaders` and either deletes the header (via `req.Header[name] = nil`, the same trick #16's test uses for `User-Agent`'s default) or sets it to `""`. Use a plain `http.Client.Do` with the standard upgrade preamble. Expect `StatusCode == 400`. After: `reg.Counts() == (0, 0)` (registry was empty to start; no binary was seeded).

3. **`TestClientEndpoint_NoBinary_4404`** — do NOT seed binary. Dial with `validClientHeaders("s1")`. Read once on the connection; expect `*websocket.CloseError` with `Code == websocket.StatusCode(4404)` and `Reason == "no server with that id"`. Assert with `errors.As`. After: `reg.Counts() == (0, 0)`.

4. **`TestClientEndpoint_PeerClose_UnregistersPhone`** — seed binary; dial; wait until registered; call `c.Close(websocket.StatusNormalClosure, "")` on the client; poll `reg.PhonesFor("s1")` (bounded retry ~2 s) until empty. The binary remains claimed (the test does not touch it).

5. **`TestClientEndpoint_MultiplePhones_IndependentLifecycle`** — seed binary; dial three phones in sequence (`c1`, `c2`, `c3`); wait until `len(reg.PhonesFor("s1")) == 3`. Close `c2`; poll until `len(reg.PhonesFor("s1")) == 2` AND the remaining two phones' `ConnID()`s match the expected `c1` and `c3` ids (capture on register). Close `c1`; poll until `len == 1`. Close `c3`; poll until `len == 0`. Verifies (a) `RegisterPhone` is order-preserving / additive, (b) `UnregisterPhone` removes only the closed phone, (c) the registry's slice mechanics in `UnregisterPhone` (swap-with-last-then-truncate, registry.go:218-225) does not corrupt other entries.

6. **`TestClientEndpoint_DeviceNameOptional_HandlerAccepts`** — seed binary; dial with `validClientHeaders("s1")` plus NO `X-Pyrycode-Device-Name`; assert success (phone registers). Then dial again with `X-Pyrycode-Device-Name: "Juhana's iPhone"`; assert success. Single test asserts the device-name header is treated as optional — required by the AC ("Read optional header... not required").

### Edit fan-out check

Files: 2 new + 1 line edit in `main.go`. Helpers (`randHex8`, `remoteHost`) reused by name — no rename, no signature change. `fakeConn` reused by name. **Zero consumer call sites need updating.** Well below the 10-call-site red line.

### What this test file deliberately does not do

- **Mock the registry.** Use the real one — `Registry` is race-tested in #3 and grace-extended in #20.
- **Mock `nhooyr.io/websocket`.** Same reasoning as #15.
- **Test the binary-grace-expiry close path.** Registry-internal; covered by #20's tests. The phone handler's behaviour under that path (defer no-ops cleanly) is structural; asserting it would require dragging `ScheduleReleaseServer` timing into a `/v1/client` test, which would re-test #20.
- **Assert exact logger output.** `io.Discard` in tests, same as #16.
- **Test `x-pyrycode-token` value handling.** The relay does not parse, compare, or otherwise act on the token's value — only on its presence. The `HeaderGate_400` table covers presence (via the empty / missing rows for `X-Pyrycode-Token`). Asserting "token is not logged" would require diverting slog output to a buffer; the spec relies on code review + the security-review section to enforce the no-log invariant.

### Lint expectations

`make vet`, `make test` (under `-race`), `make build` all clean. `gosec ./...` and `govulncheck ./...` clean. No new dependencies.

## Open questions

1. **Should the 4404 close-reason mention the requested server-id?** No. The protocol-spec close-reason is a literal generic string. Echoing the server-id back invites probing: an attacker enumerating server-ids gets confirmation in the close frame. The current message — `"no server with that id"` — leaks nothing. The AC mandates this exact string; preserve it.

2. **Should `RegisterPhone` be retried after `ErrNoServer` if the binary just disconnected (registry in grace window)?** No. During the grace window `BinaryFor` returns the (closed) binary, so `RegisterPhone` succeeds and the phone is registered to a binary that may never reclaim — the registry's `handleGraceExpiry` then closes it on expiry. This is the registry's deliberate behaviour (#20); the phone handler does not retry, does not poll, does not double-check. If the binary's slot is empty when `RegisterPhone` runs, that's a true 4404 — no other binary will ever pick up this phone's claim before the phone re-dials.

3. **Should the handler reject `device_name` with control characters / very long values?** No. slog escapes control chars; `device_name` is informational only; v1 has no length cap on any header in either endpoint. If the relay grows a per-IP rate limit later, oversized headers are caught upstream of this handler. Per evidence-based defence, no observed failure motivates a check here.

4. **Should `phone_registered` log the device-name even when empty?** Yes — keep the field present so structured-log consumers see consistent shape. `slog`'s text handler emits `device_name=""` for an empty string, which is unambiguous.

## Out of scope (re-stated, for the developer)

- Frame forwarding loop (`#6`) — `CloseRead` discards frames; replace with the real loop in #6.
- Heartbeat / ping-pong (`#7`).
- Per-phone reconnect grace (the binary-side grace from #20 ALREADY closes phones cleanly on expiry; phone-side grace is not in the protocol spec and is not in scope).
- `/v1/server` endpoint — already shipped in #16, grace-extended in #21.
- Per-IP / global connection caps — deferred to the DoS-hardening ticket (`docs/threat-model.md` § "DoS resistance").
- Token validation — by protocol design, the relay does not see token semantics; the binary owns it.
- Updating `docs/PROJECT-MEMORY.md` and `docs/knowledge/` — owned by the documentation step in the pipeline.

## Done means

- `internal/relay/client_endpoint.go` exists with `ClientHandler` and a top-of-file comment block listing close codes.
- `internal/relay/client_endpoint_test.go` covers the six tests above.
- `cmd/pyrycode-relay/main.go` registers the handler on `/v1/client`.
- `make vet`, `make test` (under `-race`), `make build`, `gosec ./...`, `govulncheck ./...` all clean.
- One commit on `feature/5`: `feat(relay): /v1/client WS upgrade with header gate and phone register (#5)`.

---

## Security review (security-sensitive label)

### Threat surface for THIS ticket

This is the phone-side ingress — the public, internet-exposed endpoint that mobile clients connect to. The peer is *less* trusted than the binary (`/v1/server` peer is at least running operator-issued software; `/v1/client` peer is anyone on the internet who learned the relay hostname). Every byte of every frame the relay later forwards FROM the phone arrives through this WS upgrade.

The endpoint sees `x-pyrycode-token` for the first and only time inside the relay process. The relay does not validate the token — the binary does — but the relay handles it: reads it from the header into a local string, presence-checks it, lets it go out of scope. A misstep here (logging it, putting it in an error response, propagating it into a frame) would be a credential leak.

### Categories walked

#### 1. Trust boundaries

The handler's input is `*http.Request`. (a) `serverID` from `X-Pyrycode-Server` is treated as an opaque map key by the registry — no parsing, no filesystem, no command construction. (b) `token` from `X-Pyrycode-Token` is read for presence-check only; never logged, never compared, never forwarded by this handler (frame-forwarding ticket #6 may forward it to the binary inside an envelope — that's #6's contract, not this one's). (c) `userAgent` is presence-checked and discarded. (d) `deviceName` is logged literally; slog escaping is the boundary. (e) `r.RemoteAddr` is parsed by `net.SplitHostPort` for log-only use.

The trust boundary is explicit: a single function, the `r.Header.Get` block at the top of the handler. Nothing past the gate calls `r.Header` again — the validated locals are the only surface that leaves the boundary.

**Finding:** No trust boundary widened beyond what the registry already accepts. The token's narrow lifetime (read-once, discarded) is the design's primary defence against credential leakage.

#### 2. Tokens, secrets, credentials

- `x-pyrycode-token`: read into a local string, presence-checked, never used after that. **MUST NOT** appear in any log line — the spec calls this out explicitly under § "Logging — exact field set" with `User-Agent` deliberately omitted alongside it. The 4404 close path takes the token's presence as already-validated; an attacker who omits the token gets 400, not 4404, so 4404 cannot be used as a token-presence oracle either way (token-absent → 400; token-present + no-binary → 4404; token-present + binary-present → success).
- The relay does not store, hash, or compare tokens. There is no on-disk artefact for this ticket.
- Token rotation/revocation is owned by the binary (per protocol spec). The relay has no role.
- `conn_id` (32-bit `crypto/rand` suffix) is NOT a security token — same as #16's analysis. It's an opaque routing key; an attacker who knew it could not impersonate the connection (the connection IS the trust). Using `crypto/rand` is principled defence-in-depth: if a future ticket exposes it, an unguessable id avoids creating an oracle.

**Finding:** Token handling is minimal-scope by design — the relay is not the trust boundary, and the spec keeps the token's lifetime inside the handler as short as possible. The only failure mode is "developer adds a debug log statement that includes the token"; the spec, the threat model, and the security-review section all name this. Defence is layered: doc, code review, and the structural absence of any code path that uses the token after presence-check.

#### 3. File operations

N/A — handler does no file I/O.

#### 4. Subprocess / external command execution

N/A — handler runs no subprocess.

#### 5. Cryptographic primitives

- `randHex8` uses `crypto/rand` (reused from `server_endpoint.go`). Correct primitive.
- TLS termination is upstream (autocert, #9). No TLS handling here.
- No comparison of attacker-controlled values to secrets — the relay does not have the secret to compare against. `crypto/subtle.ConstantTimeCompare` not applicable.

**Finding:** Inherits #16's RNG-failure-is-fatal posture. No new crypto code.

#### 6. Network & I/O

- **Input size limits.** No explicit length cap on header values. `http.Server.ReadHeaderTimeout: 10 s` (`cmd/pyrycode-relay/main.go:53-95`) bounds total header-read duration. Per-header byte cap is not enforced; the threat model already accepts this in v1 (`docs/threat-model.md` § "DoS resistance"). **Finding:** Inherited residual; same posture as #16.
- **Header validation pre-upgrade.** All three required headers checked before `websocket.Accept`. A request missing any header exits with 400 and zero allocation beyond the response. Token is presence-checked, not value-checked (per protocol spec). **Finding:** Matches AC.
- **Timeout discipline.** Inherited from `http.Server` config in `main.go`. No new timeouts in this handler. WSConn's 10s `Send` deadline (#15) bounds the write side once forwarding ticket lands.
- **Slow-loris on upgrade.** Same as #16: peer that completes handshake but never sends frames consumes one goroutine; `CloseRead` waits. Connection caps deferred.
- **Resource exhaustion.** No phone-count-per-server-id cap. The registry's `phones[serverID]` slice grows unbounded under attack. **Finding:** Inherited DoS gap, named in the threat model. Mitigation deferred.
- **TLS.** Inherited from #9. `MinVersion: tls.VersionTLS12`. No change.

#### 7. Error messages, logs, telemetry

- **Response bodies.** 400 body is empty. 4404 close-reason is the literal `"no server with that id"` — protocol-defined string, no internal-state leakage. `websocket.Accept` errors let the library write its own short generic 4xx.
- **Logged fields:** `server_id`, `conn_id`, `device_name`, `remote` on success; `server_id`, `remote` on no-server. **Never logged:** token, full headers, user-agent, payloads. The spec § "Logging" enumerates this in a table.
- **No `err.Error()` written to response.** All 4xx bodies are empty or library-defaulted.
- **Conflict-style version-disclosure oracle (#16's concern).** Not applicable: this endpoint does not log binary version anywhere; only the bare minimum identifiers go to logs.

**Finding:** Matches `docs/threat-model.md` § "Log hygiene" exactly. Token never reaches a log statement on any code path the spec defines.

#### 8. Concurrency

- **Lock-order.** Handler takes no lock of its own. Registry methods take `mu` internally. WSConn's `closeOnce` is a sync.Once, not a held lock. **Finding:** No lock-order risk.
- **TOCTOU on shared state.** `RegisterPhone` checks-and-writes under the write lock atomically (registry.go:191-198). The handler does not pre-check via `BinaryFor` then call `RegisterPhone` — it calls `RegisterPhone` directly and branches on the returned error. **Finding:** No TOCTOU window.
- **Defer-before-success race.** Defer is registered AFTER `RegisterPhone` returns nil — same ordering rule as #16 (PROJECT-MEMORY pattern). **Finding:** Safe by construction.
- **Goroutine lifecycle.** One request goroutine + one `CloseRead` read-discard goroutine per accepted connection. The read-discard goroutine terminates on conn close OR `r.Context()` cancel; the request goroutine terminates on `<-readCtx.Done()`; defer runs; goroutines exit. **No leak path** — including under (a) peer close, (b) registry-driven `Close` from binary grace-expiry, (c) server shutdown.
- **Concurrent close races.** `WSConn.Close` is `sync.Once`-guarded; `UnregisterPhone` is no-op-on-miss. The registry's `handleGraceExpiry` calling `Close()` (registry.go:181) racing with the handler's deferred `Close()` is benign — first one wins, second one no-ops.

**Finding:** No deadlock, no goroutine leak, no TOCTOU.

#### 9. Threat model alignment

- **Phone is hostile by default.** The spec assumes this and validates pre-upgrade.
- **Token never logged.** The protocol spec implicitly requires this (the relay is a courier; couriers don't read the contents). The threat model § "Log hygiene" makes it explicit. The spec § "Logging" makes it explicit one level deeper.
- **No amplification.** The relay forwards 1:1; this handler cannot create amplification.
- **First-claim-wins under contention is registry-internal.** Phone-side has no equivalent of `4409` because phones do not "claim" anything — they register additively. The registry's slice is unbounded under attack; that's the residual DoS named in the threat model.
- **Connection caps.** Deferred to a future DoS-hardening ticket.

### Adversarial framings considered

- *"Attacker sends `X-Pyrycode-Token: <very-long-blob>` to crash slog or fill up logs."* Token is never logged. Header read into a string of size ~RAM (bounded by `ReadHeaderTimeout` and Go's net/http internal limits — `http.DefaultMaxHeaderBytes` is 1 MB). Not used after the presence-check; goes out of scope. **Finding:** No path to log volume or memory exhaustion via the token.

- *"Attacker sends `X-Pyrycode-Device-Name: <NUL bytes>\n<forged log line>` to spoof a log entry."* slog's text handler quotes string values; control characters are escaped. JSON handler escapes them too. **Finding:** Log-line forging is structurally blocked.

- *"Attacker enumerates server-ids by dialling /v1/client with each candidate and observing 4404 vs success."* The 4404 path tells the attacker "no binary has that id." The success path tells them "a binary has that id, AND my opaque token was accepted by the relay (presence-only)." Both are protocol-spec-defined responses. The relay cannot withhold this signal without breaking phones. **Mitigation lives at the protocol layer:** the binary, on receiving the phone's first frame post-upgrade, validates the token; an invalid token leads to a 4401 from the binary (forwarded by the relay in #6's frame-forwarding logic). At the relay layer, server-id existence is observable; this is by design. **Finding:** Acceptable; named here so a reader doesn't think it's an oversight.

- *"Attacker dials /v1/client with valid headers, holds the connection open without sending frames."* `CloseRead` blocks reading; per-conn cost is one goroutine + one slot in `phones[serverID]`. Without a connection cap, a botnet can fill the registry. Same residual as binary-side; deferred to DoS ticket.

- *"Attacker exhausts the per-server phones slice, then a real phone tries to register."* `RegisterPhone` always succeeds on a valid serverID (it appends; no cap). The attacker's phones outnumber the legitimate one but each legitimate phone still registers and still gets frames forwarded by #6. The DoS shape is "fan-out cost on broadcast" — owned by future #6 / DoS ticket, not this one.

- *"Attacker tries CSWSH from a browser."* Same as #16: browser raw-WebSocket cannot set custom request headers, fetch with custom headers triggers CORS pre-flight that the relay does not satisfy. The custom-header gate above the upgrade structurally excludes browser attackers. `OriginPatterns: ["*"]` is safe for this reason.

- *"Attacker uses POST /v1/client to confuse the upgrade path."* `websocket.Accept` rejects non-GET upgrades with a library 4xx. Registry untouched. Test coverage explicitly exercises this on `/v1/server`; the same library behaviour applies here, but covering it for this endpoint is left implicit (the underlying library is the same).

- *"Attacker re-dials thousands of times to grow the phones slice forever."* Each successful register appends; each disconnect removes via `UnregisterPhone`. The slice does not leak across connections. Steady state is "concurrent live phones" — bounded by file descriptors / OS / connection cap (deferred). No memory leak on the phones map specifically.

- *"Attacker sends valid headers, gets 4404, retries with the same headers in a tight loop."* No registry state per failed attempt — `RegisterPhone` failed before any side effect. Attack costs one accept + one close per iteration. Same shape as any unauthenticated 4xx-response endpoint; bounded by DoS ticket.

- *"Race: binary disconnects between the phone's `RegisterPhone` and the phone observing its conn live."* `RegisterPhone` succeeds (returns nil). The handler logs `phone_registered`. The defer is registered. The grace timer is armed by the binary handler's defer (in parallel). If grace expires, `handleGraceExpiry` closes this phone; the handler's defer runs cleanly. Phone observes a `StatusNormalClosure` and reconnects. **Finding:** Safe by construction; documented under § "Phone close on binary grace-expiry".

- *"`crypto/rand` panic."* Same posture as #16: panic terminates the connection, http server recovers, defer runs, registry untouched (panic fires inside `randHex8`, which is called BEFORE `RegisterPhone`, so no entry to clean up). Loud failure is correct.

### Findings

- **[Trust boundaries]** No findings — single explicit gate at the top of the handler; downstream code holds presence-checked locals, never re-reads `r.Header`.
- **[Tokens, secrets, credentials]** No MUST FIX. **SHOULD FIX (advisory):** the token's no-log posture relies on developer discipline. The spec itself enforces it via the explicit Logging table; code-review on the implementation diff must verify no log statement, no `fmt.Errorf`, and no debug `Printf` includes the token-bearing local.
- **[File operations]** N/A — no file I/O.
- **[Subprocess]** N/A — no exec.
- **[Crypto]** No findings — `crypto/rand` only, reused from #16.
- **[Network & I/O]** No findings on this ticket. Connection caps and per-header byte caps are residual risks already named in `docs/threat-model.md` § "DoS resistance" — out of scope, deferred to a future DoS ticket.
- **[Error messages, logs, telemetry]** No findings — log-field set explicitly enumerated; token excluded by name. The 4404 close-reason matches the AC's literal string with no internal-state leakage.
- **[Concurrency]** No findings — the binary-grace / phone-close interaction is documented and structurally safe; all defer steps are idempotent; no lock-order risk.
- **[Threat model alignment]** No findings — server-id-existence-disclosure on 4404 is by design (named under "Adversarial framings"). DoS gaps inherited.

### Verdict: PASS

The handler reuses #16's audited shape (validate-pre-upgrade, defer-after-success, application-close-codes-on-underlying-conn), narrows the token's lifetime to a single presence-check inside the handler, and excludes the token from every log path by explicit field-set enumeration. The phone/binary lifecycle interactions with #20's grace machinery are walked end-to-end and shown to be idempotent on every path. Connection caps and per-header byte caps are documented residual risks, named in the threat model, and explicitly out of scope. The `security-sensitive` label is justified — this is the public, internet-facing, token-bearing ingress.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-09
