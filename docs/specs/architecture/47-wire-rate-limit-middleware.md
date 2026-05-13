# Spec: wire per-IP rate-limit middleware on /v1/server + /v1/client (#47)

## Files to read first

- `cmd/pyrycode-relay/main.go:122-129` — the existing wiring shape: `const maxFrameBytes` policy value at the call site, `mux.Handle(...)` registration of `relay.ServerHandler`/`relay.ClientHandler`. The new middleware composes around these two handler results (NOT `/healthz`). The `defer limiter.Close()` lands before the listeners block.
- `internal/relay/ratelimit.go:50-115` — `NewIPRateLimiter`, `Allow(ip string) bool`, `Close()` contracts. Note: `Allow("")` is treated as any other map key (not a deny) — the empty-string guard MUST live in the middleware.
- `internal/relay/client_ip.go:1-48` — `ClientIP(r, trustForwardedFor)`; returns empty string when no usable source is available. Doc comment explicitly says callers logging the string must `strconv.Quote` it.
- `internal/relay/server_endpoint.go:38-75` — the handler the middleware wraps. Note the existing `logger.Info("server_id_conflict", "server_id", ..., "remote", remoteHost(r))` shape — same key set the new log line uses, same message-string-as-event-type convention.
- `internal/relay/client_endpoint.go:37-...` — the other handler the middleware wraps. Same shape.
- `internal/relay/log_allowlist.go:9-17` — the closed set of permitted log keys. `"remote"` is already present; **no allowlist edit needed** for this ticket.
- `internal/relay/log_keys_test.go:59-180` — the AST walker that gates `slog.Info|Warn|Error|Debug` keys. The middleware's `Warn` call must use string-literal keys only (no `slog.String(...)`, no `With`/`LogAttrs`/`Log`).
- `docs/specs/architecture/50-ip-rate-limiter.md` — the limiter primitive spec, especially § *Concurrency model* and § *Parameter contracts (not enforced at runtime)* — the wiring ticket picks the policy values; the limiter doesn't validate them.
- `docs/specs/architecture/51-client-ip-helper.md` (if present in repo; otherwise infer from `internal/relay/client_ip.go`) — the extraction primitive spec; the `trustForwardedFor` semantics the new CLI flag controls.
- `docs/threat-model.md` § *DoS resistance — connection floods, slow-loris, fork-bomb retry* and § *Log hygiene* and § *Error response leakage* — the three threat-model entries this ticket exercises. The DoS entry's "future hardening" line is what this ticket converts to live mitigation; the wiring ticket updates that entry (architect's edit in this commit's doc-update list).
- `docs/PROJECT-MEMORY.md` § *Project-level conventions* — "Loud failure over silent correction" (empty-IP denies rather than admitting un-throttled) and "Credentials the relay does not validate are presence-checked then discarded" (the mobile/binary token is NOT read by the middleware).
- `go.mod` — confirms stdlib + `internal/relay`. No new dep introduced.

## Context

`docs/threat-model.md` § *DoS resistance* lists "a token-bucket rate limit on /v1/server and /v1/client" as the future-hardening item. #50 landed the limiter primitive; #51 landed the IP-extraction helper. This ticket wires both into the upgrade pipeline as **middleware**, so excess attempts are rejected before `websocket.Accept` runs. Combined with the per-frame size cap (#29) and the per-server-id phone cap (#30), this closes the third leg the threat-model names.

The composition seam is middleware, not constructor arguments. The handler functions know nothing about rate limiting; the limiter knows nothing about HTTP. The wiring at the composition root is the only place both meet — matching the "Policy values live at the wiring site" rule (`docs/PROJECT-MEMORY.md`).

## Design

### File layout

- **edit** `cmd/pyrycode-relay/main.go` — add CLI flag, policy constants, limiter construction, `defer limiter.Close()`, middleware composition at the two `mux.Handle` call sites.
- **new** `internal/relay/ratelimit_middleware.go` — middleware factory + the empty-IP guard + the 429 + the deny-log line.
- **new** `internal/relay/ratelimit_middleware_test.go` — burst/refill/per-IP-isolation through the middleware, the `--trust-x-forwarded-for=true` behaviour, the empty-IP-deny path, the "registry never touched" and "inner handler never called on deny" assertions, the log-line shape.
- **edit** `docs/threat-model.md` § *DoS resistance* — v1 mitigation updated to reference this wiring; "Residual risk" + "Future hardening" trimmed of the items this ticket delivers. (This is the doc update the wiring ticket owes per #50's spec.)

Total production code: ~70 lines. No new exported types from `internal/relay` beyond the middleware factory. Below all six S red lines.

### Middleware factory (the only new exported symbol)

```go
// NewRateLimitMiddleware returns an http.Handler-wrapping middleware that
// rejects requests whose source IP has exhausted its token bucket. On a
// denied request, the middleware writes 429 Too Many Requests, emits one
// slog Warn line ("rate_limited", "remote", strconv.Quote(host)), and
// does NOT invoke the wrapped handler. Empty-IP requests are denied the
// same way (Loud failure: refusing to admit un-attributable traffic
// without throttling).
//
// trustForwardedFor is threaded into relay.ClientIP. The caller MUST NOT
// set this true unless a trusted reverse proxy fronts the relay — see the
// --trust-x-forwarded-for CLI flag's Usage string in cmd/pyrycode-relay.
func NewRateLimitMiddleware(limiter *IPRateLimiter, logger *slog.Logger, trustForwardedFor bool) func(http.Handler) http.Handler
```

Behaviour summary (one paragraph in lieu of a function body):

The returned middleware extracts `ip := ClientIP(r, trustForwardedFor)`. If `ip == ""` it writes 429, emits the deny log line, and returns. Otherwise it calls `limiter.Allow(ip)`; on `false` it writes 429, emits the deny log line, and returns. Only on `true` does it call `next.ServeHTTP(w, r)`. The handler invocation order is the test contract — verified by a sentinel handler that fails the test if invoked on a deny path.

### 429 response shape

Match the existing `http.Error(w, "", http.StatusBadRequest)` shape used at `internal/relay/server_endpoint.go:44` and `client_endpoint.go:42`:

```go
http.Error(w, "", http.StatusTooManyRequests)
```

Empty message → body is just `"\n"` with `Content-Type: text/plain; charset=utf-8` and `X-Content-Type-Options: nosniff`. No internal state leaked (`docs/threat-model.md` § *Error response leakage*). `websocket.Accept` is structurally unreachable because we never call `next.ServeHTTP`.

### Log line shape

Exactly one line per denied attempt, regardless of whether the cause is empty-IP or limiter-deny:

```go
logger.Warn("rate_limited", "remote", strconv.Quote(ip))
```

- **Level: `Warn`.** Operational signal for ops dashboards; not noisy enough to need `Info`, not severe enough for `Error`. Matches "this attempt was rejected but the relay is healthy."
- **Message string `"rate_limited"`** — the event type, per the codebase convention (e.g. `"server_claimed"`, `"server_id_conflict"`).
- **Single key `"remote"`** — already in `allowedLogKeys`; no edit to `log_allowlist.go` needed.
- **Value `strconv.Quote(ip)`** — per `internal/relay/client_ip.go`'s log-injection note. For empty-IP this renders `""` (an explicit empty quoted string, distinguishable from a missing field).
- **Mobile/binary token never read.** The middleware short-circuits before `next.ServeHTTP`, so `X-Pyrycode-Token` is not touched by this code path. Stated as a non-AC reminder, enforced structurally by not having a `r.Header.Get("X-Pyrycode-Token")` anywhere in `ratelimit_middleware.go`.

The `log_keys_test.go` AST walker enforces (a) literal-string keys and (b) no `With`/`LogAttrs`/`Log` shapes. Both satisfied.

### CLI flag

Add to the `flag.X(...)` block in `cmd/pyrycode-relay/main.go`:

```go
trustXFF = flag.Bool("trust-x-forwarded-for", false,
    "Trust the X-Forwarded-For header for per-IP rate limiting. "+
        "WARNING: enabling this without a trusted reverse proxy in front of "+
        "the relay allows clients to spoof their source IP and bypass "+
        "per-IP rate limits.")
```

Default `false`. Threaded as the third argument to `relay.NewRateLimitMiddleware`. The warning in Usage is the operator-facing trust contract per AC.

### Wiring at the composition root

In `main.go`, after the existing `const maxFrameBytes` and before `mux := http.NewServeMux()`:

```go
// Per-IP rate-limit policy: ~10 attempts/IP/minute steady-state, burst 20.
// Derivation: docs/threat-model.md § DoS resistance future-hardening line
// names ~10/min/IP and burst headroom for retry storms; eviction interval
// 5 min keeps the bucket map's resident size bounded under address-space
// scanning (see docs/specs/architecture/50-ip-rate-limiter.md § Adversarial
// walk #1). All three values are positive — NewIPRateLimiter panics on a
// zero/negative evictionInterval via time.NewTicker.
const (
    rateLimitRefillEvery      = 6 * time.Second
    rateLimitBurst            = 20
    rateLimitEvictionInterval = 5 * time.Minute
)

limiter := relay.NewIPRateLimiter(rateLimitRefillEvery, rateLimitBurst, rateLimitEvictionInterval)
defer limiter.Close()

rateLimit := relay.NewRateLimitMiddleware(limiter, logger, *trustXFF)

mux := http.NewServeMux()
mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))
mux.Handle("/v1/server", rateLimit(relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes)))
mux.Handle("/v1/client", rateLimit(relay.ClientHandler(reg, logger, maxFrameBytes, 16)))
```

`/healthz` is NOT wrapped — per AC, and because healthz must remain pollable from anywhere (load-balancers, monitoring) without throttling.

`defer limiter.Close()` placement is "before the listeners block" per AC. As the ticket body acknowledges, `main.go` currently `os.Exit`s on listener error, which does not run defers — this is best-effort only. A real graceful-shutdown path is out of scope.

### Why middleware, not constructor injection

Pushing the limiter into `ServerHandler`/`ClientHandler` constructors would:

- Couple two unrelated concerns (frame routing + admission control) at the type level.
- Force the limiter and its policy values to be known one layer deeper than necessary.
- Make `/healthz`'s exemption awkward (the constructor would need an `enabled bool` or a nil-limiter check).

Middleware composition keeps each layer single-purpose. The composition root in `main.go` is the only place that knows both. Matches the `EnforceHost(*domain, mux)` pattern already in `main.go:191` — host enforcement is also middleware-shape, not handler-internal.

### Concurrency model

The middleware is stateless aside from the shared `*IPRateLimiter`. Concurrency safety is fully delegated to the limiter (`internal/relay/ratelimit.go` § *Concurrency model*). No new locks, no new goroutines.

The `defer limiter.Close()` synchronises with the limiter's eviction goroutine via the limiter's internal WaitGroup; the middleware itself owns no goroutine to shut down.

### Error handling

The middleware has two terminal branches (empty-IP deny, limiter deny) and one pass-through. None return errors — `http.Error` writes the response directly. The pass-through is just `next.ServeHTTP(w, r)`; any error from the inner handler is the inner handler's responsibility.

`ClientIP` and `Allow` are both infallible by design. There is no third failure mode to plumb.

### Testing strategy

All tests live in `internal/relay/ratelimit_middleware_test.go`, `package relay` (matching the convention).

Test infrastructure (one helper, reusing the limiter's fake clock):

- A `sentinelHandler` that increments a counter when called. The middleware test asserts the counter for both pass and deny paths.
- A `bytes.Buffer`-backed `slog.NewTextHandler` for capturing log lines. The middleware factory accepts a `*slog.Logger` so the test wires its own.
- Use `httptest.NewRecorder()` + `http.NewRequest`; set `req.RemoteAddr` directly (e.g. `"1.2.3.4:5555"`) and `req.Header.Set("X-Forwarded-For", "9.9.9.9")` per test.

Test cases (bullet-pointed scenarios; developer writes the test bodies in the project's idiom):

1. **Burst, then deny.** With burst=2, `refillEvery=time.Hour` (effectively no refill during the test), three POSTs to a wrapped sentinel from `RemoteAddr="1.1.1.1:1234"`: first two reach the sentinel and return its status (200); third returns 429 and the sentinel is NOT called (counter still at 2). Asserts the AC "denied attempts return 429 *before* websocket.Accept runs."

2. **Per-IP isolation.** Same limiter, burst=1. Request from `"1.1.1.1:1"` → 200, sentinel called. Second request from `"1.1.1.1:1"` → 429, sentinel not re-called. Third request from `"2.2.2.2:1"` → 200, sentinel called. Asserts the AC's "request from a second IP is unaffected."

3. **Trust XFF on.** `trustForwardedFor=true`, burst=1. Two requests with `RemoteAddr="1.1.1.1:1"` but `X-Forwarded-For="9.9.9.9"`: first → 200, second → 429. A third request with same RemoteAddr but `X-Forwarded-For="8.8.8.8"` → 200. Asserts the AC's "with the flag, a forged XFF counts against the forged IP's bucket — documents the trust model."

4. **Trust XFF off (default).** `trustForwardedFor=false`. Two requests with `RemoteAddr="1.1.1.1:1"` and `X-Forwarded-For="9.9.9.9"` (set but ignored): first → 200, second → 429 (keyed to `1.1.1.1`, not `9.9.9.9`). Sanity-check that the default ignores the header.

5. **Empty IP denies.** Craft a request whose `RemoteAddr` is unparseable (e.g. `RemoteAddr=""` — `net.SplitHostPort` returns an error; `ClientIP` returns `""`). With `trustForwardedFor=false`, expect 429 and the sentinel NOT called, **without** calling `limiter.Allow`. Verification of "limiter.Allow not called" can be done by using a fresh limiter for this test and asserting the bucket map is still empty after the request via `len(limiter.buckets)` under the limiter's lock (package-internal access — same pattern as `ratelimit_test.go`). Asserts the AC's "when ClientIP returns empty, deny before calling Allow."

6. **Deny emits exactly one log line with the right shape.** A buffer-backed logger; one denied request; assert (a) exactly one line written, (b) line contains `level=WARN`, (c) line contains `msg=rate_limited`, (d) line contains `remote="\"1.2.3.4\""` (the strconv.Quote-wrapped value — slog text handler will escape the inner quotes). Repeat with empty-RemoteAddr request and assert `remote="\"\""` (empty quoted string). Asserts the AC's log-shape constraints. No other keys present.

7. **Allowed request emits no log line.** Pass-through case; assert the buffer is empty.

8. **Registry never touched on deny.** Wire a real `relay.NewRegistry()` into a real `ServerHandler` (the wrapped handler under test), make a denied request, assert `len(reg.AllPhones())` (or equivalent registry-state inspection) is unchanged. This pins the "registry is never touched" AC clause structurally. The simpler form is the sentinel-counter assertion (test #1); this test adds the registry-state assertion as a regression guard.

No new test infrastructure beyond `bytes.Buffer` + `httptest`. The limiter's own fake clock is not needed: `refillEvery=time.Hour` makes all tests effectively rate-time-independent within a sub-second test duration.

`make vet`, `make test -race`, and `make build` must all pass per project convention.

### Doc updates the developer must make

1. **`docs/threat-model.md` § *DoS resistance*** — update v1 mitigation to reference the wiring (file:line anchor at `cmd/pyrycode-relay/main.go`'s new const block + middleware composition). Remove the "A token-bucket rate limit on /v1/server and /v1/client" line from § *Future hardening* — it's now v1. The connection-count cap (per-IP and global, separate from rate limiting) stays in *Future hardening*.

2. **`docs/knowledge/codebase/47.md`** — new file per the 2026-05-10 convention. Implementation summary (files touched, public API of the middleware factory, the empty-IP guard rationale, the 429 shape, the deny-log shape, the policy-defaults derivation). Include a "Patterns established" bullet if any (likely none new — this ticket consumes existing patterns).

3. **`docs/knowledge/INDEX.md`** — documentation phase appends. Architect does NOT edit (per the agent role's "Never Update" list).

4. **No `docs/PROJECT-MEMORY.md` edit** — human-maintained.

### Open questions

None. The middleware shape is fully constrained by AC; the log-level choice (Warn over Info) is documented inline; the policy defaults are inside the AC-suggested ranges with derivation traced to the threat-model + the limiter spec.

---

## Security review

**Verdict:** PASS

### Trust boundaries this spec touches

| Boundary | Trust posture | Where enforced |
|---|---|---|
| `r.RemoteAddr` — TCP-layer source address | Trusted as far as TCP is honest. Can still be the empty/unparseable case (loopback unix-socket binds, or a misconfigured proxy). | `ClientIP` returns `""` on unparseable; middleware denies on empty before `Allow`. |
| `X-Forwarded-For` request header — attacker-controlled by default | Untrusted unless `--trust-x-forwarded-for` is set. CLI flag defaults `false`. Flag's Usage string carries the explicit "operator owns the trust" warning. | `relay.ClientIP(r, trustForwardedFor)`; flag default in `main.go`. |
| `IPRateLimiter` state — shared across `/v1/server` and `/v1/client` | Trusted (in-process). Sharing one limiter across both endpoints is deliberate: a single attacker IP retrying against either endpoint should share one bucket, not two. | One `*IPRateLimiter` constructed once in `main`, passed to one `rateLimit` middleware, applied to both `mux.Handle` registrations. |
| Mobile/binary `X-Pyrycode-Token` header | Untrusted, validated by the binary. The relay never reads it; the rate-limit middleware doubly never reads it (runs before any handler). | Structurally: no `r.Header.Get("X-Pyrycode-Token")` call in `ratelimit_middleware.go`. |

### Adversarial walk

**1. Trust boundaries.** The two boundaries (`RemoteAddr`, `XFF`) are crossed exactly once each, in `ClientIP`. The middleware never re-parses, never sees the raw header. The trust mode (XFF on/off) is set at process start and never changes — no per-request mode-flipping, no header self-asserting "trust me." The flag's default-false posture is the loud-failure choice (`docs/PROJECT-MEMORY.md` § *Loud failure over silent correction*): the operator must opt in to the spoofable mode. **No findings.**

**2. Tokens, secrets, credentials.** The mobile/binary token is not read by this middleware. The 429 response body is empty (no leak). The deny log line carries only `remote` (allowlisted) — not the token, not the User-Agent, not the server-id, not any header. The threat-model § Log hygiene rule is satisfied by the closed-set allowlist already in place (`log_allowlist.go:9-17`) + the AST walker (`log_keys_test.go`). **No findings.**

**3. File operations.** None. Middleware is pure-memory. **N/A.**

**4. Subprocess / external command execution.** None. **N/A.**

**5. Cryptographic primitives.** None. **N/A.**

**6. Network & I/O.**
- *Input size limits.* The middleware runs before `websocket.Accept`. Per-frame caps (`#29`) and the http.Server header timeouts (`ReadHeaderTimeout: 10s`, `ReadTimeout: 60s`) bound everything upstream of this code path. No new I/O the middleware itself performs.
- *Header validation.* `XFF` parsing is in `ClientIP`'s scope (#51). The middleware treats the result as an opaque string for map-keying.
- *Resource exhaustion.* The limiter's eviction goroutine + the bounded bucket map (proof in #50's spec) cap the limiter's memory. The middleware adds no other state.
- *Connection-count cap (separate from rate-limit).* Out of scope for this ticket — explicitly listed in the issue's *Out of Scope* and still in `docs/threat-model.md` § *Future hardening* after the doc update. The per-IP rate limit does not subsume the per-IP / global connection-count caps.
- **No findings.**

**7. Error messages, logs, telemetry.**
- *429 body.* Empty (`http.Error(w, "", 429)`). No internal state leaked (`docs/threat-model.md` § *Error response leakage*).
- *Deny log line.* Exactly one line per deny, with one allowlisted key + a `strconv.Quote`-wrapped value (defends against control-byte log injection per `client_ip.go`'s comment). Same shape regardless of empty-IP vs limiter-deny — operator can disambiguate by the `remote` value (`""` vs a real IP).
- *Allow path.* No log line. Allowed traffic flows silently; the wrapped handler may log normally.
- *No telemetry yet.* A future metrics ticket could add a "rate_limited_total" counter (cf. #58's metric pattern). Listed as **OUT OF SCOPE — separate metrics ticket**, not a finding.
- **No findings.**

**8. Concurrency.**
- *Limiter shared across both endpoints.* Single `*IPRateLimiter` is goroutine-safe per #50's race-tested concurrency model.
- *Middleware itself stateless.* No new locks, no shared mutable state added by this layer.
- *Shutdown safety.* `defer limiter.Close()` in `main` is best-effort (current `os.Exit`-on-error path skips defers). On clean shutdown via Ctrl-C in dev, the defer runs; the limiter's `Close()` is synchronous (`wg.Wait()`) and the eviction goroutine exits cleanly. The current `main.go` has no signal-handler wired (not introduced by this ticket); a real graceful-shutdown path is out of scope per AC. **SHOULD FIX → deferred — listed in *Out of Scope*; opening a follow-up issue for "graceful shutdown + signal handling" is appropriate when WS support lands.**
- **No findings (the shutdown gap is pre-existing and explicitly deferred).**

**9. Threat model alignment.**
- *`docs/threat-model.md` § DoS resistance.* This ticket converts the "future hardening: token-bucket rate limit on /v1/server and /v1/client" line to a v1 mitigation. The doc update is part of this ticket's deliverables.
- *§ Log hygiene.* Conformed via the existing allowlist + walker.
- *§ Error response leakage.* Empty 429 body honours the "public-facing handlers return generic messages" rule.
- *Protocol spec § Security model.* The protocol-level threats (server-id race, token leak, replay, MITM) are upstream of this middleware. The middleware short-circuits before any of those code paths can run — it does not weaken any existing mitigation.
- **No findings.**

### Decision summary

| Category | Verdict |
|---|---|
| Trust boundaries | No findings |
| Tokens / secrets | No findings |
| File operations | N/A |
| Subprocess | N/A |
| Cryptography | N/A |
| Network & I/O | No findings (connection-count cap explicitly OUT OF SCOPE) |
| Errors / logs | No findings (metrics counter OUT OF SCOPE, future ticket) |
| Concurrency | No findings (graceful-shutdown pre-existing gap, explicitly deferred) |
| Threat model alignment | No findings (doc update is part of this ticket) |

No **MUST FIX**. No **SHOULD FIX**. Two **OUT OF SCOPE** items, both already named in the issue body's *Out of Scope* section: connection-count caps (separate threat surface), graceful-shutdown signal handling (orthogonal). Both stay in `docs/threat-model.md` § *Future hardening* after this ticket's doc update.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
