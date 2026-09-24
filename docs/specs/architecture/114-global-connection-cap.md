# #114 — Global live-connection cap before accept

## Files read

- `internal/relay/ratelimit_middleware.go` → `NewRateLimitMiddleware` — the middleware shape this ticket mirrors (deny before the wrapped handler, one Warn line, empty-body `http.Error`).
- `internal/relay/push_wake.go` → `pushWakeMaxInFlight`, `ErrPushWakeInFlightCap` — existing buffered-channel cap with non-blocking try-acquire; same primitive here.
- `internal/relay/client_endpoint.go` → `ClientHandler` — stays in `ServeHTTP` for the connection's life; its deferred cleanup waits for the outbox writer; the 4404 / 4429 reject-after-accept paths return from the handler.
- `internal/relay/server_endpoint.go` → `ServerHandler`, `remoteHost` — same whole-lifetime shape; `remoteHost` is the `RemoteAddr`-only host extractor the Warn line reuses.
- `internal/relay/registry.go` → `handleGraceExpiry` — closes every phone of an expired server-id, which makes the phone handler return; the third exit path the tests cover.
- `internal/relay/phone_outbox.go` → `phoneOutboxDepth` — the per-phone queue whose worst case dominates the per-connection memory cost.
- `internal/relay/metrics_upgrade.go` → `WrapServerRateLimitDeny`, `WrapClientRateLimitDeny` — count only HTTP 429; the cap wrapper must sit inside them and write 503 so they never miscount it.
- `internal/relay/log_allowlist.go` → `allowedLogKeys` — `remote` and `path` are already allowlisted; no edit.
- `internal/relay/client_endpoint_test.go` → `dialWithClient`, `validClientHeaders`; `internal/relay/server_endpoint_test.go` → `dialWith`, `validHeaders` — the dial helpers the integration test reuses.
- `cmd/pyrycode-relay/main.go` → `run`, the `maxFrameBytes` derivation comment, the rate-limit wiring — where the flag, default and wrapper land.
- `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_InsecureMutexWithAutocertListenFlags`, `dialServer`, `freePort`, `waitForDial` — harness for the flag-validation and wiring tests.
- `docs/knowledge/features/rate-limit-middleware.md` § "Why middleware, not constructor injection" — composition at the root; `/healthz` exemption falls out of not wrapping it.
- `docs/threat-model.md` § DoS resistance — residual-risk sentence this ticket retires (documentation handoff).

In-flight overlap: `feature/127` touches none of this ticket's files.

## Context

The per-IP rate limiter bounds the *rate* of upgrade attempts, not the number of *live* connections. An attacker staying under the refill rate adds connections without bound until the 256 MB Fly machine OOMs. This ticket adds one global cap shared by `/v1/server` and `/v1/client`. The per-IP concurrency cap stays deferred (`docs/security-followups.md` "Per-IP concurrent connection cap").

No ADR warranted: this is a policy value plus a middleware in an established shape.

## Design

New file `internal/relay/conncap.go`:

```go
var ErrInvalidConnCap = errors.New("relay: connection cap must be positive")

// ConnCap is a relay-wide cap on live upgrade-handler invocations.
type ConnCap struct { slots chan struct{}; logger *slog.Logger }

func NewConnCap(max int, logger *slog.Logger) (*ConnCap, error) // max <= 0 → wrapped ErrInvalidConnCap
func (c *ConnCap) Wrap(next http.Handler) http.Handler
func (c *ConnCap) inUse() int // len(slots); unexported, for tests
```

`Wrap` per request: non-blocking send on `slots`.
- Acquired → `defer` the receive (release), then `next.ServeHTTP`. The defer releases on every exit, including a handler panic recovered by `net/http`.
- Full → `http.Error(w, "", http.StatusServiceUnavailable)`, one `logger.Warn("conn_cap_reached", "path", r.URL.Path, "remote", remoteHost(r))`, return. `next` never runs, so `websocket.Accept` never runs. No header is read.

One `*ConnCap` instance wraps both endpoints, so binaries and phones draw from one pool. `Wrap` has the `func(http.Handler) http.Handler` shape, so it composes like `rateLimit`.

Wiring in `run` (`cmd/pyrycode-relay/main.go`):

```
/v1/server: upgradeMetrics.WrapServerRateLimitDeny(rateLimit(connCap.Wrap(relay.ServerHandler(...))))
/v1/client: upgradeMetrics.WrapClientRateLimitDeny(rateLimit(connCap.Wrap(relay.ClientHandler(...))))
```

Inside the rate limiter: a rate-limited request never takes a slot, and the 429 observers see only 429s. `/healthz` stays unwrapped; `/metrics` is on a separate server and mux and is structurally ungated.

Flag: `--max-connections` (int), default `defaultMaxConnections`. `NewConnCap` is called right after the insecure-listen mutual-exclusion guard (before any goroutine-starting setup), and an error logs `refusing to start: invalid --max-connections` with a `fix` hint and returns exit 2 — the loud-failure pattern.

### Default derivation (goes in the comment next to `defaultMaxConnections`)

Charge every connection at the phone worst case (a binary costs less — no outbox):
- handler, heartbeat and outbox-writer goroutines: 3 stacks, ~8 KiB each once grown ≈ 24 KiB;
- websocket/bufio/TLS record buffers ≈ 64 KiB;
- `phoneOutboxDepth` × `maxFrameBytes` = 16 × 256 KiB = 4 MiB of queued frames — reachable by an attacker who owns both a binary and a non-reading phone;
- one `maxFrameBytes` frame in flight on the read side = 256 KiB.

≈ 4.4 MiB per connection. Of the 256 MB, reserve ~56 MiB for the binary, Go runtime, autocert and kernel socket buffers → ~200 MiB. No `GOMEMLIMIT` is set, so at `GOGC=100` the heap may reach ~2× live before collection → ~100 MiB of live connection state → ~22 connections. Round down: **20**. An operator on a bigger machine raises it with the flag.

At that default, the production `fly.toml` command needs no change.

## Concurrency model

No new goroutines. The slot channel is the only shared state; buffered-channel send/receive is safe under concurrent handlers. Acquire happens on the request goroutine before `next` runs; release happens on the same goroutine when `next` returns. Both handlers return only after their own cleanup defers (outbox writer joined, heartbeat cancelled), so a slot outlives every per-connection goroutine. Graceful shutdown closes conns → handlers return → slots release; nothing to shut down in `ConnCap`.

## Error handling

- `max <= 0` at construction → `fmt.Errorf("max-connections %d: %w", max, ErrInvalidConnCap)`; `run` refuses to start (exit 2).
- Over cap at request time → 503, empty body, no retry hint, one Warn line. Not an error return.

## Testing strategy

`internal/relay/conncap_test.go` (package `relay`):
- `NewConnCap` with 0 and -1 → `errors.Is(err, ErrInvalidConnCap)`; 1 → ok.
- Over-cap unit test, cap 1: first request blocks inside a sentinel handler; a second request (carrying distinctive `X-Pyrycode-Server` / `X-Pyrycode-Token` values) gets 503, the sentinel ran once, the log buffer holds exactly one `conn_cap_reached` line containing neither header value; unblock, wait for `inUse()==0`, a third request gets 200.
- Integration, real `ServerHandler` + `ClientHandler` on one `httptest` mux sharing one `ConnCap` (dial via the existing `dialWith` / `dialWithClient` helpers), one subtest per exit path; each waits on `inUse()` then proves a fresh upgrade succeeds at the boundary:
  - normal close: cap 2, binary + phone fill it, a third dial gets HTTP 503 (`resp.StatusCode`); the phone closes normally; a new phone upgrades.
  - reject after accept: cap 1, no binary, phone upgrades then reads close 4404; `inUse()` returns to 0; another phone upgrades (again 4404, not 503).
  - grace expiry: cap 2, short grace; binary + phone fill it, third dial 503; binary closes → `inUse()==1`; grace expires, phone is closed → `inUse()==0`; binary + phone upgrade again, a third dial gets 503.

`cmd/pyrycode-relay/main_e2e_test.go`:
- `--max-connections 0` and `-1` → `run` returns 2.
- Wiring: `--max-connections 1`, one binary holds the slot; a `/v1/client` request gets 503; `/healthz` gets 200.

## Open questions

- None blocking. If the grace-expiry phone close races the test's `inUse()` poll, the poll's deadline absorbs it (poll, not sleep).

## Documentation handoff (pending — documentation stage)

- `docs/threat-model.md` § DoS resistance: rewrite the residual-risk sentence on concurrent connection count — live connections are now capped globally (`--max-connections`, default 20, 503 before accept); remaining gaps are the per-IP concurrency limit and connections still in the TLS/header phase (bounded only by `ReadHeaderTimeout` 5s and the rate limiter). Also update the "Future hardening" line and the #113 sentence that calls the 64 MiB-per-server-id case an instance of the deferred cap.
- `docs/security-followups.md`: "Slowloris connection-count cap" and "Per-IP concurrent connection cap" — global cap shipped (#114); per-IP cap still deferred.
- New feature doc `docs/knowledge/features/connection-cap.md` plus an INDEX line. `docs/knowledge/features/rate-limit-middleware.md` § Out of scope names the global cap as deferred — update to link the new doc.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — `ConnCap.Wrap` reads no header and no body; the only request field it touches is `RemoteAddr` (via `remoteHost`) for the Warn line, plus `URL.Path`. Payloads stay `json.RawMessage` in the unchanged handlers; the header gates in `ServerHandler` / `ClientHandler` run after the slot is taken, unchanged.
- [Tokens] No findings — `X-Pyrycode-Token` is never read on the cap path; the over-cap unit test asserts a distinctive token value never reaches the log.
- [File operations / Subprocess / Crypto] Not applicable — no file, no subprocess, no randomness or secret comparison.
- [Network & I/O] No findings on the gate itself — the try-acquire is non-blocking, so over-cap requests never queue or hold a goroutine past the 503; the slot is taken before `websocket.Accept` and released by `defer` on every handler exit (panic included). No new socket read, no new `http.Server`.
- [Network & I/O] OUT OF SCOPE — a single source IP can fill the global cap: the rate limiter's burst (20) equals the default cap (20), and heartbeats keep idle connections alive, so one IP can deny *new* upgrades to everyone while it holds its slots. Established connections are unaffected, and the relay no longer crash-loops on OOM, which was reachable before from one IP within minutes (owned binary + non-reading phones filling outboxes). The remedy is the per-IP concurrent cap, deferred in `docs/security-followups.md` "Per-IP concurrent connection cap"; the documentation handoff asks that entry to record that the global cap now makes its trigger reachable by a single IP.
- [Network & I/O] OUT OF SCOPE — connections in the TLS or HTTP-header phase are not counted; bounded by `ReadHeaderTimeout` (5s) and the rate limiter. Named as residual in the documentation handoff per the ticket.
- [Network & I/O] Kernel socket buffers are outside the Go heap and are covered only by the ~56 MiB reserve in the default derivation; a stalled phone's autotuned send buffer can exceed that share. Accepted: the operator can lower `--max-connections`; noted in the derivation comment.
- [Logs] No findings — keys `path` and `remote` are both in `allowedLogKeys`; `path` is one of the two fixed mux patterns (exact-match `ServeMux` routes), `remote` is the same host value every endpoint log line already carries. Log volume on the deny path is bounded by the rate limiter, which sits outside the cap. The 503 body is empty.
- [Concurrency] No findings — one buffered channel, no locks, no goroutines; acquire and release run on the request goroutine; release follows the handlers' own cleanup defers, so a slot outlives every per-connection goroutine.
- [Threat model alignment] No re-review trigger tripped: no new dependency, no new endpoint, no deploy-target change. The cap is per-process, consistent with the single-instance constraint.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
