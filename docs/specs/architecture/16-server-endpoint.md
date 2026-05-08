# Spec — `/v1/server` WS upgrade, header gate, server-id claim (#16)

## Files to read first

- `internal/relay/registry.go:69-101` — `ClaimServer` / `ReleaseServer` semantics and `ErrServerIDConflict`. The handler is a thin shell around these two calls.
- `internal/relay/registry.go:22-46` — `Conn` interface contract. The handler hands `WSConn` (which already implements it) to `ClaimServer`; nothing about the interface changes.
- `internal/relay/ws_conn.go:1-83` — `WSConn` adapter. The handler constructs one with `NewWSConn(c, connID)`. Note the doc invariant: "callers must reach the connection only through `WSConn` methods" — the conflict path needs a principled exception (see § Design).
- `internal/relay/healthz.go:35-57` — handler-factory shape: `func NewXHandler(...) http.Handler` returning an `http.HandlerFunc` literal. Convention to mirror.
- `internal/relay/ws_conn_test.go:20-54` — `httptest.NewServer` + `websocket.Dial` test harness. The endpoint test reuses this shape (server-side handler is `ServerHandler(reg, logger)` instead of an inline accept).
- `cmd/pyrycode-relay/main.go:45-50` — current mux registration site. One new `mux.Handle("/v1/server", ...)` line slots in next to `/healthz`.
- `docs/threat-model.md` § "Log hygiene" (line 42–55) — what must never be logged: payload bodies, token values, full headers. Headers we DO log are enumerated in the AC.
- `docs/threat-model.md` § "Error response leakage" (line 77–85) — public-facing handlers return generic messages. The `400` body is empty / generic; the conflict close-reason is the literal `"server-id already claimed"` (no internal state).
- `docs/PROJECT-MEMORY.md:30-34` — handler-factory pattern, package-internal tests, "loud failure over silent correction." All three apply.
- [`pyrycode/pyrycode/docs/protocol-mobile.md` § Authentication → Binary→relay](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#binary--relay) — authoritative spec for the headers and the `4409` close code. The relay implements; the spec defines.
- `nhooyr.io/websocket` package docs (godoc — already a direct dep) — `Accept`, `AcceptOptions.OriginPatterns`, `StatusCode`, `Conn.CloseRead`, `Conn.Close`. The handler uses each exactly once.

## Context

Implements the binary-side handshake. The endpoint accepts an outbound WebSocket from a pyry binary on `/v1/server`, validates three required headers BEFORE upgrading, upgrades on success, and registers the connection in the registry under the binary's `x-pyrycode-server` header. First-claim-wins is enforced by `ClaimServer` (atomic in #3); a losing claim sends close code `4409`.

The relay treats every byte from a binary as adversarial: header validation runs before `websocket.Accept` so a malformed request never receives an upgrade response. The registry is untouched on every error path.

This is the binary side only. Phone (`/v1/client`) is #5, frame forwarding is #6, heartbeat is #7, and the 30-second grace period on disconnect is #8.

## Design

### Package & files

- New file: `internal/relay/server_endpoint.go` (`package relay`).
- New test file: `internal/relay/server_endpoint_test.go` (`package relay`).
- Edit: `cmd/pyrycode-relay/main.go` adds one line registering the handler on the mux.

No changes to `registry.go`, `ws_conn.go`, `envelope.go`, `tls.go`, `healthz.go`, `doc.go`, `go.mod`, or `Makefile`.

### Public API

```go
package relay

// ServerHandler returns the http.Handler for /v1/server: the binary-side
// WebSocket upgrade endpoint. It validates required headers, upgrades the
// connection, claims the server-id slot in reg, and holds the connection
// open until the binary closes it (or the slot is released).
//
// Close codes used by this endpoint:
//   - 1000 (StatusNormalClosure)        clean close on shutdown / release.
//   - 4409 (server-id already claimed)  another binary holds the slot.
//
// Future tickets layer 4401 (token), 4404 (no server) on /v1/client and
// frame-forward errors on /v1/server.
func ServerHandler(reg *Registry, logger *slog.Logger) http.Handler
```

One exported symbol. No new types, no new sentinel errors.

### Required headers (validated pre-upgrade)

| Header (canonical) | Source | Enforcement |
|---|---|---|
| `X-Pyrycode-Server` | binary's server-id | non-empty |
| `X-Pyrycode-Version` | binary's version string | non-empty |
| `User-Agent` | HTTP standard | non-empty |

`r.Header.Get` performs canonicalisation (case-insensitive). Missing or empty → `http.Error(w, "", http.StatusBadRequest)` with no body, no `WWW-Authenticate`, no internal-state leakage. Registry untouched, `Accept` not called.

### Algorithm

```
1. Validate the three headers. Any missing/empty → 400, return.
2. websocket.Accept(w, r, &websocket.AcceptOptions{
       OriginPatterns: []string{"*"},
   })
   - On error: the library has already written a 4xx; just return.
     Registry untouched.
3. connID := "server-" + serverID + "-" + randHex8()
4. wsconn := NewWSConn(c, connID)
5. err := reg.ClaimServer(serverID, wsconn)
   - errors.Is(err, ErrServerIDConflict):
       _ = c.Close(websocket.StatusCode(4409), "server-id already claimed")
       logger.Info("server_id_conflict",
           "server_id", serverID,
           "remote",    remoteHost(r))
       return
   - other err (not produced by current registry, but for completeness):
       wsconn.Close()
       return
6. logger.Info("server_claimed",
       "server_id",      serverID,
       "binary_version", versionHeader,
       "remote",         remoteHost(r))
7. defer:
       reg.ReleaseServer(serverID)
       wsconn.Close()
       logger.Info("server_released", "server_id", serverID)
8. // Hold the connection open until the peer closes it. Frame loop (#6)
   // replaces this block with a real read loop later.
   readCtx := c.CloseRead(r.Context())
   <-readCtx.Done()
```

Two design notes inside this algorithm deserve calling out:

**Why call `c.Close` directly on conflict instead of `wsconn.Close()`.** `WSConn.Close()` always sends `StatusNormalClosure`; the conflict path needs the application-defined `4409`. Adding a "close with custom code" method to `WSConn` would inflate #15's surface for one caller. The conflict case is a stillborn WSConn: no `Send` was attempted, no concurrent goroutine holds `writeMu`, so the WSConn doc's "callers must reach the connection only through WSConn methods" invariant is preserved in spirit — we never *use* the WSConn after construction. The handler's comment names this exception so a future reader doesn't generalise the pattern.

**Why `CloseRead` instead of `for { c.Read() }` or a bare block.** The handler must keep the goroutine alive until the peer disconnects, but reading frames is #6's job. `nhooyr.io/websocket.Conn.CloseRead(ctx)` spawns a goroutine that drains-and-discards frames and returns a context cancelled when the conn closes. When #6 lands, the read goroutine is replaced with a real frame loop in the same goroutine — the call site swaps `<-readCtx.Done()` for the loop body. Using `CloseRead` here also satisfies a subtle correctness point: WebSocket pings / control frames need to be processed, otherwise the connection never observes a close from the peer side. `CloseRead` handles that internally.

### Helper functions (file-local, unexported)

```go
// randHex8 returns 8 hex chars (32 bits) from crypto/rand. Panics on a
// crypto/rand read failure — the OS RNG failing is not a recoverable
// error in this process.
func randHex8() string {
    var b [4]byte
    if _, err := rand.Read(b[:]); err != nil {
        panic("crypto/rand: " + err.Error())
    }
    return hex.EncodeToString(b[:])
}

// remoteHost strips the port from r.RemoteAddr. Falls back to RemoteAddr
// verbatim if SplitHostPort fails (handlers behind some proxies set it
// without a port). Used only for logging.
func remoteHost(r *http.Request) string {
    if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
        return host
    }
    return r.RemoteAddr
}
```

`crypto/rand.Read` is documented to never fail on platforms the relay supports (Linux, macOS); the `panic` matches Go-stdlib idiom (`crypto/rand`'s own examples treat it as fatal). 32 bits is sufficient: the conn-id is scoped per server-id, and the registry uses it only as an opaque key for slice lookups (`UnregisterPhone`); collisions inside a single server-id slice are statistically negligible at v1 scale.

### `cmd/pyrycode-relay/main.go` edit

One line, inserted after the `/healthz` registration (`main.go:49`):

```go
mux.Handle("/v1/server", relay.ServerHandler(reg, logger))
```

`logger` is already in scope (`main.go:37`). No other edits.

### Concurrency model

The handler runs in the http.Server's per-request goroutine. The `WSConn` it constructs is the registry's reference for `serverID`; broadcasts (future tickets) reach it through `reg.BinaryFor`. There is one `CloseRead`-spawned read-discard goroutine per accepted connection; it terminates when the peer closes or `r.Context()` is cancelled.

| Method / step | Goroutine | Lock | Lifecycle |
|---|---|---|---|
| Header validation | request goroutine | none | pre-upgrade; no resources held |
| `websocket.Accept` | request goroutine | none | conn allocated on success |
| `ClaimServer` | request goroutine | registry write lock (held internally) | one-shot |
| `CloseRead` | spawns one read-discard goroutine | none | terminates on conn close / ctx cancel |
| `<-readCtx.Done()` | request goroutine, blocking | none | unblocks on peer close |
| `defer { Release; wsconn.Close; log }` | request goroutine | `wsconn`'s `closeOnce` | runs on every successful claim path; idempotent |

Shutdown path: when the http server shuts down, `r.Context()` is cancelled, the `CloseRead` goroutine's read returns an error, `readCtx` cancels, the handler unblocks, the defer runs, the slot is released. Clean. (The grace-period behaviour from #8 will wrap `ReleaseServer` in a deferred timer; nothing about this handler's structure precludes that — `defer` is the same hook.)

### Error handling

- **Missing/empty header** → `400 Bad Request`, empty body. No log line (avoids amplifying header-floods into log volume; the threat model already names "future endpoints may write `err.Error()` to the response" as a leakage risk — generic body is the mitigation).
- **`websocket.Accept` error** (wrong method, missing `Upgrade`, wrong subprotocol) — the library writes its own 4xx response. The handler returns without logging. Registry untouched.
- **`ClaimServer` returns `ErrServerIDConflict`** — close 4409, log `event=server_id_conflict server_id=<id> remote=<host>`. Header values are NOT logged (no `binary_version`, no `user_agent`) — matches the AC and avoids leaking the conflicting binary's version through what is, observationally, an unauthenticated probe surface.
- **`ClaimServer` returns any other error** — not produced by the current registry, but defensively: `wsconn.Close()` and return. Not logged because no such error is reachable from current code; if it becomes reachable, the diff that introduces it should add the log.
- **`CloseRead` / `<-readCtx.Done()`** does not fail — it returns when the conn ends.
- **No panics from this handler.** `randHex8`'s panic on RNG failure is fatal-by-design; if it fired, the http server's panic recovery would terminate the connection, the defer would still run, and the slot would be released.

### Logging — exact field set

Per `docs/threat-model.md` § Log hygiene, the relay must log structured fields only, never full headers or payloads. This handler emits three event types:

| event | fields |
|---|---|
| `server_claimed` | `server_id`, `binary_version`, `remote` |
| `server_id_conflict` | `server_id`, `remote` |
| `server_released` | `server_id` |

`binary_version` is the value of the `X-Pyrycode-Version` header, advertised by the binary itself — informational, not a secret. `remote` is the IP host portion of `r.RemoteAddr`, no port. `User-Agent` is validated for presence but never logged (no operational use; would inflate log volume without value).

## Testing strategy

`internal/relay/server_endpoint_test.go`, `package relay`. End-to-end against a real `*websocket.Conn` via `httptest.NewServer(ServerHandler(reg, logger))`, mirroring the harness in `ws_conn_test.go`.

### Test harness

```go
// startServer spins up an httptest.NewServer running ServerHandler against
// a fresh registry. It returns the registry, the WS URL, and a cleanup.
func startServer(t *testing.T) (*Registry, string, func()) {
    t.Helper()
    reg := NewRegistry()
    logger := slog.New(slog.NewTextHandler(io.Discard, nil)) // silence test output
    srv := httptest.NewServer(ServerHandler(reg, logger))
    wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
    return reg, wsURL, srv.Close
}

// dialWith calls websocket.Dial with the given header set.
func dialWith(t *testing.T, wsURL string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
    t.Helper()
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
}

// validHeaders returns a header set that passes the gate for serverID.
func validHeaders(serverID string) http.Header {
    h := http.Header{}
    h.Set("X-Pyrycode-Server", serverID)
    h.Set("X-Pyrycode-Version", "0.1.0-test")
    h.Set("User-Agent", "pyry-test/0.1.0")
    return h
}
```

### Tests (1:1 with AC bullets)

1. **`TestServerEndpoint_ValidUpgrade_RegistersBinary`** — dial with `validHeaders("s1")`; assert `Dial` returns no error; assert `reg.BinaryFor("s1")` returns a non-nil Conn whose `ConnID()` starts with `"server-s1-"` and ends with 8 hex chars (regex or `len`+`hex.DecodeString` check).

2. **`TestServerEndpoint_HeaderGate_400`** — table-driven over the three required headers; each row builds `validHeaders` and either deletes the header or sets it to `""`. Use a plain `http.Client.Do` (not `websocket.Dial`) with the standard upgrade headers (`Upgrade: websocket`, `Connection: Upgrade`, `Sec-WebSocket-Version: 13`, `Sec-WebSocket-Key: <fixed>`); expect `StatusCode == 400`. After the call, `reg.Counts()` returns `(0, 0)`.

3. **`TestServerEndpoint_DuplicateClaim_4409`** — dial once with `validHeaders("s1")` (success). Dial again with the same headers. Read once on the second connection; expect `websocket.CloseError` whose `Code == websocket.StatusCode(4409)` and `Reason == "server-id already claimed"`. Assert with `errors.As(err, &ce)`. The first conn remains registered.

4. **`TestServerEndpoint_PeerClose_ReleasesSlot`** — dial with valid headers; assert registered; call `c.Close(websocket.StatusNormalClosure, "")` on the client; poll `reg.BinaryFor("s1")` (with a short bounded retry — up to ~1s, sleeping 10ms — because release is observed via the server-side handler returning, which races the client close); assert it eventually returns `(_, false)`. Re-claim succeeds afterwards.

5. **`TestServerEndpoint_WrongMethod_NoPanic`** — `http.Get(srv.URL + "/v1/server")` (no upgrade headers, but with the three pyrycode headers set, to prove the upgrade gate fails *after* the header gate passes); expect a 4xx (the library will return 426/400); no panic; `reg.Counts() == (0, 0)`.

   And one more: `http.Post(srv.URL+"/v1/server", "application/octet-stream", nil)` with valid pyrycode headers — expect a 4xx, no panic, registry empty. Documents that POST is not a regression hole.

### What this test file deliberately does not do

- **Mock the registry.** Use the real one — `Registry` is already race-tested in #3.
- **Mock `nhooyr.io/websocket`.** Same reasoning as #15.
- **Test `randHex8` for distribution / collision.** It's `crypto/rand` + `hex.EncodeToString`; testing the stdlib is out of scope.
- **Assert exact logger output.** The slog handler is `io.Discard` in tests. If a future ticket needs to verify log fields, swap to a JSON handler writing to a `bytes.Buffer` and decode — out of scope here.

### Lint expectations

`make vet`, `make test` (under `-race`), `make build` all clean. `gosec ./...` and `govulncheck ./...` clean. No new dependencies — `nhooyr.io/websocket`, `crypto/rand`, `encoding/hex`, `net`, `log/slog`, `errors` are already in scope.

## Open questions

1. **Should the handler emit a `server_upgrade_rejected` log when `websocket.Accept` errors?** No, leave it silent. The library writes a 4xx and the failure is visible in the http access log. Adding our own log line per upgrade-failure invites log-flood from misconfigured clients. If observability later wants this, a single counter (Prometheus, future) is the right shape, not a log line.

2. **Why `OriginPatterns: []string{"*"}` and not omit it / use `InsecureSkipVerify`?** `nhooyr.io/websocket.Accept` rejects all cross-origin upgrades by default unless the request has no `Origin` header. Browsers always send `Origin`; programmatic clients don't have to. Setting `OriginPatterns: ["*"]` says explicitly "this endpoint is not browser-facing; same-origin policy does not apply." `InsecureSkipVerify` is for TLS dial verification — the wrong knob entirely. The choice is documented with a comment in the source.

3. **Should `binary_version` be sanitised before logging?** It's an attacker-controlled string. `slog.NewTextHandler` quotes string values, so a newline in the version cannot forge a fake log line. We rely on slog's escaping; no manual sanitisation. If the relay later switches to a JSON handler in production, that property is preserved (JSON encoders escape).

## Out of scope (re-stated, for the developer)

- Frame forwarding loop (`#6`) — `CloseRead` discards frames; replace with the real loop in #6.
- Heartbeat / ping-pong (`#7`).
- 30-second grace period on disconnect (`#8`) — this handler releases immediately; the grace wrapper goes around `ReleaseServer` in #8.
- `/v1/client` endpoint (`#5`) — separate handler, separate spec.
- Per-IP connection limit / global cap — deferred to the DoS-hardening ticket (`docs/threat-model.md` § DoS resistance).
- Token validation — the relay does not see tokens on `/v1/server`; the binary side is unauthenticated by design (the binary owns the trust relationship with phones).
- Updating `docs/PROJECT-MEMORY.md` and `docs/knowledge/` — owned by the documentation step in the pipeline, not this ticket's commit.

## Done means

- `internal/relay/server_endpoint.go` exists with `ServerHandler`, `randHex8`, `remoteHost`, and a top-of-file comment block listing close codes.
- `internal/relay/server_endpoint_test.go` covers the five tests above.
- `cmd/pyrycode-relay/main.go` registers the handler on `/v1/server`.
- `make vet`, `make test` (under `-race`), `make build`, `gosec ./...`, `govulncheck ./...` all clean.
- One commit on `feature/16`: `feat(relay): /v1/server WS upgrade with header gate and server-id claim (#16)`.

---

## Security review (security-sensitive label)

### Threat surface for THIS ticket

This is the binary-side ingress. The handler is one of the two places the relay accepts traffic from the public internet (the other being `/v1/client`, #5, and `/healthz`, which is read-only). Every byte of every frame the relay later forwards arrives through this WS upgrade. The endpoint is unauthenticated — the protocol has no token on `/v1/server`; trust is established post-claim by virtue of the binary holding the slot for `serverID`.

### Categories walked

- **Trust boundaries.** The handler's input is everything in `*http.Request`: method, path (already routed by mux), headers, RemoteAddr. (a) `serverID` from `X-Pyrycode-Server` is treated as an opaque map key by the registry — no parsing, no filesystem use, no command construction, no SQL. (b) `versionHeader` is logged but not parsed, compared, or used for routing. (c) `userAgent` is validated for presence and discarded. (d) `r.RemoteAddr` is parsed by `net.SplitHostPort` (stdlib, well-tested) for logging only. **Finding:** no trust boundary widened beyond what the registry already accepts; `serverID` is the same opaque-key shape #3 documents.

- **Input validation / pre-upgrade gate.** All three required headers are checked before `websocket.Accept`. A request missing any header exits with `400` and zero allocation beyond the response. The gate is intentionally minimal — no length cap, no charset check, no regex — because the only consumer (registry) treats `serverID` as opaque. Adding a length cap would be invented enforcement (no observed failure to motivate it). The protocol spec is the right place to define `serverID` shape; if it adds constraints, this gate inherits them. **Finding:** validation matches the registry's contract; no defense added beyond what the AC requires.

- **Resource exhaustion / DoS — connection floods.** This handler does not cap concurrent connections. `docs/threat-model.md` § "DoS resistance" already names this as a deferred v1 gap with the WS upgrade path called out by name. **Finding:** no new mitigation; the residual is the same residual the threat model already accepts. Per-IP / global caps belong to a future DoS ticket.

- **Resource exhaustion / DoS — slow-loris on upgrade.** `http.Server.ReadHeaderTimeout: 10s` (`cmd/pyrycode-relay/main.go:53-95`) bounds the time to receive headers. If a peer completes the WS handshake but never sends frames, `CloseRead` simply waits — at the cost of one goroutine per such peer. Connection-count caps are the right answer (deferred). The 10-second `Send` timeout in `WSConn` (#15) bounds the write side. **Finding:** no new exposure; existing mitigations apply.

- **Resource exhaustion / memory.** Per-connection memory is one `WSConn` (~120 B), one `CloseRead` goroutine (~8 KB initial stack), the registry's map entry. No per-connection buffer pool, no read queue. Logging fields are stack-allocated by slog's variadic API. **Finding:** baseline; identical to what #5/#6 will add per phone connection.

- **Race conditions / first-claim-wins atomicity.** `ClaimServer` holds the registry write lock across the read-then-write check, so two concurrent claims see one winner and one `ErrServerIDConflict`. The handler's caller-side ordering is irrelevant to atomicity; the lock owns it. The handler's defer (`ReleaseServer + wsconn.Close + log`) runs only when `ClaimServer` succeeded — the conflict path returns before the defer is registered. **Finding:** atomicity preserved; no TOCTOU window.

- **Race conditions / release-before-success.** If the handler returned without claim succeeding but with the defer already registered, `ReleaseServer` would run on a slot we don't hold (no-op on the registry, false return — safe). The chosen ordering puts `defer` AFTER the successful `ClaimServer`, so this race is structurally absent. **Finding:** safe by construction; spec calls out the ordering explicitly.

- **Information disclosure — log content.** Logged fields: `server_id`, `binary_version`, `remote`. Not logged: `User-Agent`, full headers, payloads, conn-id internals (the random suffix is a relay-internal value with no leakage value, but it's also not adversarial-relevant; not logging it is just thrift). The conflict path deliberately omits `binary_version` to avoid turning the conflict signal into a version-disclosure oracle for an attacker probing whether a `serverID` is held — the threat model's "do not log header values on the conflict path" is encoded in the AC and reproduced here. **Finding:** matches `docs/threat-model.md` § Log hygiene exactly.

- **Information disclosure — response bodies.** `400` body is empty. The `4409` close-reason is the literal `"server-id already claimed"` — a public, protocol-defined string with no internal-state leakage. The `websocket.Accept` failure path lets the library write its own 4xx (`nhooyr.io/websocket` returns short, generic strings). **Finding:** matches `docs/threat-model.md` § "Error response leakage."

- **Conn-id unpredictability.** 32 bits from `crypto/rand`. The conn-id is scoped per server-id and used only as an opaque map key inside `UnregisterPhone`; it is not a security token. An attacker who knew it could not impersonate the connection (the connection IS the trust). Using `crypto/rand` over `math/rand` is principled defence-in-depth: if a future ticket exposes the conn-id (e.g. echoing it in an error envelope), an unguessable id avoids creating an oracle by accident. **Finding:** no harm, modest forward-compat benefit.

- **Lock-order / deadlock.** Registry write lock is taken inside `ClaimServer` and `ReleaseServer`. Neither call invokes `wsconn.Send` or `wsconn.Close` while holding the lock — `Close` runs in the defer, after `ReleaseServer` has returned. The `CloseRead` goroutine never touches the registry. **Finding:** no deadlock path.

- **Use-after-close.** If the binary closes the connection mid-handler, `CloseRead`'s read errors and cancels `readCtx`; `<-readCtx.Done()` returns; defer runs. `ReleaseServer` then `wsconn.Close()`; the latter is `sync.Once`-guarded (`#15`) so a stray `Close` from a future broadcast loop is a no-op. **Finding:** safe.

- **Panics / nil-deref.** `randHex8` panics on RNG failure (intentional). All other paths are nil-safe: `r.Header.Get` returns `""` for missing headers; `net.SplitHostPort` returns an error rather than panicking. The http server's panic recovery would catch a panic if one fired and close the connection; defer would still run because Go's `recover` happens after registered defers — except the defer is registered AFTER `ClaimServer`, so a panic before claim leaves the registry untouched. **Finding:** safe.

### Adversarial framings considered

- *"Attacker fakes `X-Pyrycode-Server: <legit-id>` to claim the slot first."* This is the underlying threat the protocol model accepts: the binary side of `/v1/server` is unauthenticated; first-claim-wins is the trust primitive. Mitigation lives at the protocol layer (binary validates phone tokens; phones validate the binary out-of-band). Out of scope for this ticket; named here so a reader doesn't think it's an oversight.

- *"Attacker spam-claims a `serverID`, holds the slot, never serves frames."* Same as above. The legitimate binary sees `4409` until the attacker disconnects (or the grace period closes the squatter via another mechanism, post-#8). Mitigations: connection caps (deferred), and at the protocol level, phones treat unresponsive binaries as offline. The relay's job here is to enforce the claim atomically; it does.

- *"Attacker sends crafted headers with NULs / very long values to try to crash slog or net.SplitHostPort."* slog's text handler quotes string values (newlines, NULs become escapes). `net.SplitHostPort` operates on `r.RemoteAddr`, which is set by `net/http` (not the attacker's headers). `r.Header.Get` returns whatever the client sent without further parsing. **Finding:** no parse step the attacker controls is fragile.

- *"Attacker omits `User-Agent`, `X-Pyrycode-Version`, or sends them as space-only strings."* `r.Header.Get` returns `""` for unset headers; the gate explicitly checks for non-empty (`!= ""`). A space-only string passes the `!= ""` check — accepted, logged literally. Hardening to "trim then check non-empty" would be invented enforcement; per evidence-based defence, deferred until an observed failure motivates it. The fields are informational, not enforcement.

- *"Attacker sends a valid upgrade then floods the connection with frames."* `CloseRead` reads-and-discards them. Each goroutine handles one connection. Per-frame work is tiny (read header, drop body). A high-rate attacker hits OS kernel buffers, then `read` rate-limits naturally. No amplification (the relay does not respond). **Finding:** acceptable for v1; named in DoS deferral.

- *"Attacker uses `OriginPatterns: ["*"]` permissiveness to mount a CSWSH (cross-site WebSocket hijacking) from a browser."* Browsers cannot generate a `pyry` binary's headers (`X-Pyrycode-Server`, `X-Pyrycode-Version`) by simple cross-site request — `fetch` with custom headers triggers CORS pre-flight; raw WebSocket from a browser cannot set custom request headers at all. So a browser-driven attacker cannot pass the header gate. The threat is structurally blocked one layer up. **Finding:** `OriginPatterns: ["*"]` is safe given the header gate; the comment in source documents the reasoning.

- *"Attacker calls `POST /v1/server` to confuse the upgrade path."* `websocket.Accept` rejects non-GET upgrade requests with a library-written 4xx. Registry untouched. Test coverage explicitly exercises this.

- *"Attacker holds the slot, then triggers handler shutdown to leak the goroutine."* On shutdown the http server cancels `r.Context()`; the `CloseRead` goroutine's read returns with the cancelled context; `readCtx` cancels; the handler unblocks; defer runs. No goroutine leak.

- *"Random conn-id collision."* Birthday bound: collisions become non-negligible at ~2¹⁶ live conns under ONE server-id. Far beyond v1 scale. If it ever matters, widen the suffix; the public API does not change.

- *"`crypto/rand` fails (panic)."* On Linux/macOS, `getrandom`/`/dev/urandom` does not block once the kernel pool is initialised at boot. A failure here means the host is broken. Panic terminates the connection, http server recovers, other connections continue. A failing host at this depth is an operator-visible fault; refusing to serve is the correct response (loud failure over silent correction).

### Verdict: PASS

The handler enforces the AC's required header gate before allocating any WebSocket state, defers to `ClaimServer` for the atomic-claim primitive, and uses log fields that match the threat-model's hygiene rules exactly (with the conflict path's deliberate omission of `binary_version` called out as defence against version-disclosure probing). `OriginPatterns: ["*"]` is safe because the custom-header gate one layer above structurally excludes browser attackers. The `crypto/rand` conn-id suffix adds modest forward-compat hardening at zero cost. Connection caps and the slow-peer fleet are documented residual risks already named in `docs/threat-model.md`; this ticket inherits, not widens, those gaps. The `security-sensitive` label is justified — this is internet-facing, unauthenticated ingress on the binary side — and the review is correspondingly thorough.
