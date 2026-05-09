# Lessons

Gotchas worth carrying forward. Each entry: what bit us (or nearly did), and what we do about it.

## `time.Timer.Stop()` returns false if the func has already started — a mutex alone won't save you

`time.AfterFunc(d, fn)` fires `fn` on a runtime goroutine. Calling `t.Stop()` returns true if the timer was cancelled before firing, false if it already fired *or its `fn` is currently executing*. The "currently executing" case is a race the registry's `sync.RWMutex` cannot prevent: the AfterFunc body has started, blocked on the lock; another goroutine acquires the lock first, calls `Stop()` (gets false), replaces the map entry, releases the lock; the AfterFunc body then takes the lock and would naïvely tear down the new claimant's state. Defence: wrap each pending timer in a small struct (`*graceEntry`), store the wrapper in the map keyed by the same id, and have the AfterFunc closure capture the wrapper pointer. On fire, check `m[key] == self` under the lock — if the entry was replaced, the pointer no longer matches and the body returns. Source: `Registry.ScheduleReleaseServer` (#20).

## Capture a wrapper pointer, not `*time.Timer`, to avoid a self-referential local var

The natural shape — `var t *time.Timer; t = time.AfterFunc(d, func() { ... uses t ... })` — assigns `t` *after* `AfterFunc` returns, so the closure reads `t` from the outer frame. Under stress, the race detector flags this as a write/read race on the local var. Hoisting the timer pointer into a heap-allocated wrapper (`entry := &graceEntry{}; r.timers[id] = entry; entry.timer = time.AfterFunc(d, func() { ... uses entry ... })`) sidesteps the issue: the closure captures `entry`, the field assignment on `entry` is visibility-sealed by the lock the AfterFunc body must acquire. Source: `Registry.ScheduleReleaseServer` (#20).

## A long-lived WS handler that does not read frames will never observe peer close

A WebSocket connection only sees a peer-side close when *something* on this side reads from the conn — control frames (ping/pong/close) are processed inline with reads. A handler that just blocks forever (e.g. `select {}` or `<-r.Context().Done()`) keeps the goroutine alive but does not move the close machinery: the conn stays "open" until the kernel TCP timeout, and `r.Context()` does not cancel on peer close (it cancels on *server* shutdown / client TCP RST seen by the http server). Use `c.CloseRead(r.Context())` to spawn a drain-and-discard goroutine and block on the returned context — that goroutine processes control frames and cancels the context on close. When the real frame loop lands, **delete the `CloseRead` call entirely** along with `<-readCtx.Done()`; the loop is now the sole reader and a parallel `CloseRead` goroutine would race the sole-reader contract on the underlying `*websocket.Conn`. Source: `/v1/server` (#16); confirmed by the #25 swap in `/v1/client`.

## `*websocket.Conn.Close(code, reason)` emits an application close code; the `WSConn` adapter does not

The registry's `WSConn.Close()` is fixed to `StatusNormalClosure` by contract. To emit `4409` / `4401` / `4404`, call `Close` directly on the underlying `*websocket.Conn` from the upgrade handler — but only in the stillborn-WSConn window (after construction, before any `Send`, before the close `defer` is registered). Outside that window you'd race the adapter's `closeOnce` and `writeMu`. The handler comments name the exception so it doesn't read as a "use the underlying conn whenever you like" pattern. Captured as ADR-0005. Source: `/v1/server` conflict path (#16).

## `defer { release-or-schedule }` must be registered AFTER the successful `ClaimServer`

If you `defer reg.ReleaseServer(serverID)` (or `ScheduleReleaseServer`) before `ClaimServer` returns, the conflict path (`ErrServerIDConflict`) ends up acting on a slot it does not hold. Pre-#21 this was a benign no-op (`ReleaseServer` returned false); after #21 swapped the defer to `ScheduleReleaseServer`, violating the rule would *arm a 30-second grace timer for an id the handler never owned* — at expiry the timer fires against whatever entry happens to be present (typically nothing, but indistinguishable in shape from a deletion). Order is non-negotiable: claim, then log success, then `defer`. Source: `/v1/server` (#16, sharpened by #21).

## `crypto/rand.Read` failure is fatal-by-design on Linux/macOS

The OS RNG (`getrandom` / `/dev/urandom`) does not block once the kernel pool is initialised at boot. A failure at runtime means the host is broken in a way the relay cannot meaningfully recover from. Stdlib `crypto/rand` examples treat it as fatal; `panic("crypto/rand: " + err.Error())` is the idiom. The http server's panic recovery terminates the connection cleanly; other connections continue. Source: `randHex8` in `/v1/server` (#16).

## `json.Valid` rejects nil and empty bytes

`encoding/json.Valid(nil)` and `json.Valid([]byte{})` both return `false`. Useful: a single `if !json.Valid(frame)` check covers nil, empty, and malformed bytes — no need for separate length guards. Source: routing-envelope `Marshal` (#1).

## `json.RawMessage` of a JSON `null` is the 4-byte string `"null"`

When unmarshalling `{"frame": null}` into a struct with `Frame json.RawMessage`, the resulting `Frame` is `[]byte("null")` — length 4, *not* zero — so a length-only check treats it as present. If absence-vs-null matters semantically, also compare against `[]byte("null")` explicitly. Source: routing-envelope `Unmarshal` (#1).

## `json.RawMessage` round-trips are byte-stable *modulo whitespace*

`encoding/json` re-encodes `RawMessage` through the outer marshaller and may strip insignificant whitespace. Tests asserting opacity should canonicalise both sides with `json.Compact` before `bytes.Equal`, otherwise they flake on formatting. Source: routing-envelope tests (#1).

## Slice swap-and-truncate needs an explicit nil for GC hygiene

Removing an element from `[]Conn` (or any slice of pointer-bearing values) by `slice[i] = slice[len(slice)-1]; slice = slice[:len(slice)-1]` leaves the original last element reachable from the underlying array's spare capacity, pinning whatever it references in memory. Set `slice[len(slice)-1] = nil` *before* re-slicing. Source: `Registry.UnregisterPhone` (#3). Catches: connection handles can be heavy structs; a leaked `Conn` keeps a goroutine and a socket buffer alive.

## Race-test count is a CI-runner knob, not test code

The AC asked for `go test -race -count=20` coverage. `count` belongs on the command line, not as a loop inside the test. `make test` already runs `-race`; document the manual stress invocation in the test's doc comment so future contributors know how to reproduce locally. Source: `TestRegistry_RaceFreedom` (#3).

## A passive registry returns copies, callers do the slow work outside the lock

When a shared map holds connection handles (or anything you'll iterate to do I/O over), the read accessor should allocate and return a fresh copy. Callers can then `Send` / `Close` / fan out without holding the lock — and concurrent mutators don't trip the iteration. The cost is one allocation per call; the benefit is no foot-gun where a slow `Send` blocks every other registry caller. Source: `Registry.PhonesFor` (#3).

## To close cleanly through a serialised writer, cancel a context — don't take the write mutex

Wrapping a library whose `Write` is *not* concurrency-safe forces a per-conn write mutex. The naive `Close` then takes the same mutex to ensure no `Write` is in flight — and that deadlocks against a slow peer holding the mutex inside an in-flight `Write`. The fix: give the adapter a `closeCtx` cancelled by `Close`; `Send` derives `WithTimeout(closeCtx, …)` per call. `Close` cancels the context *without taking the write mutex*; the in-flight `Write` aborts on cancellation and releases the mutex on its own. Requires the underlying library to document `Close`-safe-with-in-flight-`Write` (the case for `nhooyr.io/websocket`). Source: `internal/relay/ws_conn.go` (#15).

## `nhooyr.io/websocket` is the v1.8.x line under a vanity import path

Vanity import path is `nhooyr.io/websocket`. Upstream development moved to `github.com/coder/websocket`, but the `nhooyr.io/websocket` line continues to be published. Use the vanity path; do not opportunistically switch to the coder fork without an ADR. `go get nhooyr.io/websocket@latest` resolves into v1.8.x. Source: ADR-0004 (#15).

## `autocert.Manager.HTTPHandler(nil)` redirects to HTTPS — it does not 404

The autocert docs and the `nil` argument are easy to misread as "no fallback → 404." Actual behaviour: GET/HEAD get a `302` to HTTPS; other methods get `400`. To get an explicit 404 (or any other handler), pass it explicitly: `mgr.HTTPHandler(http.NotFoundHandler())`. See ADR-0002. Source: autocert TLS wiring (#9).

## TLS handshake SNI does not bind the HTTP `Host` header

A client can complete the TLS handshake using SNI for `relay.example.com` and then send `Host: somethingelse.com` in the HTTP request on the same connection. The wire spec says they must match for the relay; enforce it in application code (`EnforceHost` returns `421 Misdirected Request`). Don't assume autocert's `HostWhitelist` covers this — that gates ACME *issuance*, not request-time host validation. Source: autocert TLS (#9).

## `r.Host` may carry a `:port` suffix

RFC 7230 §5.4 allows it. When comparing to a configured hostname, strip the port via `net.SplitHostPort` first (it errors out cleanly if there's no port — use the original string in that case). Compare with `strings.EqualFold` because hostnames are case-insensitive. Source: `EnforceHost` (#9).

## `autocert.Manager.TLSConfig()` doesn't set `MinVersion`

`gosec` G402 fires on `make lint` if you use it raw. Wrap it in a helper that pins `MinVersion = tls.VersionTLS12` (or 1.3) before handing it to `http.Server`. Centralising the override means a future bump is a one-line change. Source: `relay.TLSConfig` (#9).
