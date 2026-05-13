# Spec: WS upgrade handshake timeout — bound slow-loris exposure (#35)

## Files to read first

- `cmd/pyrycode-relay/main.go:185-255` — the three `http.Server` literals (`insecure-listen`, `httpsSrv`, `httpSrv`). Each carries `ReadHeaderTimeout: 10 * time.Second` (lines 188, 239, 251). These are the wiring sites the AC references.
- `internal/relay/metrics_listen.go:60-95` — fourth `http.Server` literal (line 90). The function's own comment ("Timeouts match the public listener (cmd/pyrycode-relay/main.go) so the metrics surface has the same DoS-resistance shape … today they match because that is the safest default") makes a coordinated change across all four sites mandatory; leaving this one at 10s would silently break the invariant the comment encodes.
- `cmd/pyrycode-relay/main.go:25-31` — the `drainDeadline` const, shown here as the established pattern for a policy literal documented at the wiring site (small comment + constant in `main`).
- `docs/threat-model.md:32-36` — § "DoS resistance" cites the current `ReadHeaderTimeout: 10s` value verbatim. The documentation phase will update this; the developer does not touch it.
- `internal/relay/server_endpoint_test.go:18-30` and `internal/relay/heartbeat_test.go:41-55` — `httptest.NewServer` patterns. The new slow-handshake test does **not** use `httptest.NewServer` (it cannot set `ReadHeaderTimeout`); it constructs a bare `http.Server` on a `net.Listen("tcp", "127.0.0.1:0")` listener. Read these so the test stylistically matches the package: t.Helper, `t.Cleanup` for teardown, `defer srv.Close()` patterns.
- `docs/threat-model.md` (whole file, scan only) — confirms the threat model places slow-loris under "DoS resistance" and ties to `http.Server` timeout fields; the spec inherits that framing.

## Context

The relay is internet-exposed. Per the threat model (`docs/threat-model.md` § DoS resistance), slow-loris mitigation rests on the `http.Server` timeout fields. Today every `http.Server` instance the relay constructs carries `ReadHeaderTimeout: 10 * time.Second`. That window is long enough that a single peer dribbling upgrade-request bytes can pin a socket + serving goroutine for ten seconds with negligible bandwidth cost; multiplied across attempts that survive the per-IP rate limit (~10/min/IP from #50), it remains a meaningful resource amplifier.

After header parse completes, `nhooyr.io/websocket.Accept` writes the 101 response and hijacks the connection. From that point on, `ReadTimeout`/`WriteTimeout` no longer apply — post-upgrade slow peers are covered by per-frame deadlines (#15) and heartbeat ping/pong (#7). This ticket bounds *only* the pre-upgrade window: the gap between TCP accept and full HTTP header receipt.

The fix is to tighten `ReadHeaderTimeout` from 10s to 5s at every site. No new code path, no factory, no exported helper.

## Design

### Edit set (production code)

Four literal-value changes, no structural changes:

1. `cmd/pyrycode-relay/main.go:188` (insecure listener `srv`) — `ReadHeaderTimeout: 10 * time.Second` → `5 * time.Second`.
2. `cmd/pyrycode-relay/main.go:239` (autocert TLS `httpsSrv`) — same change.
3. `cmd/pyrycode-relay/main.go:251` (autocert HTTP-01 `httpSrv`) — same change.
4. `internal/relay/metrics_listen.go:90` (`NewMetricsServer` return literal) — same change.

The metrics-listener change is required because `metrics_listen.go`'s function comment (lines 70-75) explicitly states the timeouts mirror the public listener as a "safest default"; tightening the public listeners without tightening this one silently violates the invariant the comment encodes. Loopback-only metrics traffic has no slow-loris threat, but the test surface and "duplicated, identical" rationale stay intact only if all four sites move together.

### Documenting the bound

Add a short comment above the first occurrence in `main.go` (the insecure-listen branch's `srv`) — single paragraph, in the same shape as the existing `drainDeadline` comment block at lines 25-31. Contents:

- States the value (5s) and what it bounds (pre-upgrade WebSocket handshake window).
- Cites the threat model and the post-upgrade complements (#15, #7).
- Names the wiring-site convention (per `docs/PROJECT-MEMORY.md` "Project-level conventions").

Do **not** factor a shared `const` or helper. Each of the four sites continues to carry its own literal, exactly as today; the inline pattern is load-bearing for the wiring-site convention the AC names. The metrics-listen function already has its own comment explaining why the value is duplicated rather than imported — leave that comment as-is.

The autocert-mode `httpsSrv` and `httpSrv` don't need a new comment; they are visibly identical to the insecure-mode `srv` they sit next to. The metrics file's existing comment block already covers its site.

### Choice of 5s (not 3s, not 10s)

The AC says "~5 seconds". Inside that band, 5s is chosen because:

- It is identical to nhooyr's per-conn close-handshake timeout already documented in `docs/lessons.md` and used as the basis for `drainDeadline` headroom — keeping the relay's slow-peer bounds at one consistent number reduces operator surprise.
- It comfortably exceeds round-trip + header-write time on the realistic worst-case path (mobile peer over LTE, ~500ms RTT, a few hundred bytes of headers), so legitimate clients are not at risk of intermittent boot failures.
- 3s would be tighter but starts to feel like a tuning value; 5s leaves observable margin without leaving the resource window meaningfully open.

The value is not a tuning parameter for this ticket and should not be revisited unless an operational signal demands it.

## Concurrency model

No change. The `http.Server` runs the same accept loop; `ReadHeaderTimeout` is enforced by `net/http`'s connection-state machine before the registered `http.Handler` is invoked. No new goroutines, no new channels, no new shutdown sequence interaction. The `relay.Shutdown` path (#31) is untouched.

## Error handling

When `ReadHeaderTimeout` fires, `net/http` closes the underlying `net.Conn`. No handler runs, no log line is emitted by relay code, no metric is incremented — this matches today's behaviour at 10s and is the correct floor: a peer that never sends a full request is indistinguishable from a peer that disconnected mid-write, and we already chose not to log either case (the existing 10s timeout produces no log line today and there is no signal that this is a gap worth filling). If a future ticket wants visibility into pre-upgrade timeouts as a slow-loris indicator, that is a metrics-pipeline ticket against `internal/relay/metrics_upgrade.go`, not this one.

## Testing strategy

Add one test file, `internal/relay/slow_upgrade_test.go` (new), one test function. Stylistically mirrors `internal/relay/heartbeat_test.go`.

Test shape (bullet-pointed scenario, not a function body):

- **Setup.** Construct a bare `http.Server` (not `httptest.NewServer`) with:
  - `Handler`: any minimal `http.HandlerFunc` (e.g. one that writes 200 OK). The handler **should never run** in this test; if it does, the test fails because that means `ReadHeaderTimeout` did not fire.
  - `ReadHeaderTimeout`: a short value (~150ms) to keep the test fast. The test verifies the `http.Server` *contract* — that `ReadHeaderTimeout` actually closes a peer that stalls header writes — at any value. The production literal (5s) is verified by code review, consistent with how every other inline policy literal in `main.go` is verified.
  - Other timeouts (`ReadTimeout`, `WriteTimeout`, `IdleTimeout`): set generously (seconds) so the test cannot accidentally pass via the wrong timeout.
- **Listener.** `net.Listen("tcp", "127.0.0.1:0")`, then `go srv.Serve(ln)`. `t.Cleanup` closes the server.
- **Slow client.** `net.Dial("tcp", ln.Addr().String())`. Write `"GET /v1/server HTTP/1.1\r\nHost: x\r\n"` — note the deliberate missing `\r\n\r\n` terminator. Do not write more.
- **Assertion.** Within `ReadHeaderTimeout + slack` (e.g. 500ms total), a `conn.Read` on the client side returns either EOF or a connection-reset/closed error. Use `conn.SetReadDeadline(time.Now().Add(slack))` to bound the test's own wait.
- **Negative-shape check.** If the handler ever runs (use an atomic counter), fail with a clear message — that's the signal that the timeout did not fire and something is wrong with the test setup.

The test sits in `package relay` (same-package test convention, per PROJECT-MEMORY.md). It does not exercise `nhooyr.io/websocket` at all — the bound being tested is purely at the `http.Server` layer, before the upgrade handshake even starts. Pulling websocket in would add complexity without adding coverage.

### What this test does and does not prove

- **Does** prove: the standard library's `ReadHeaderTimeout` contract — the thing the relay depends on — actually closes the connection at the configured deadline. If a future Go version regressed this, or if a future refactor accidentally zeroed the field, the test catches it.
- **Does not** prove: that `main.go` sets the value to 5s. That is verified by reading the diff (or `grep ReadHeaderTimeout cmd/pyrycode-relay/main.go internal/relay/metrics_listen.go`) at code review time, same as every other wiring-site literal.

Combining "behavioural test of the contract" + "code-review verification of the literal" is the established way this codebase validates wiring-site policy values (see how `drainDeadline`, `maxFrameBytes`, the rate-limit constants in `main.go`, and the timeouts in `metrics_listen.go` are all guarded).

## CI invariants

`make vet`, `make test -race`, `make build` all clean — same as every other ticket. No new dependencies. No new exported symbols.

## Documentation impact (not the developer's job)

`docs/threat-model.md:32-36` currently cites `ReadHeaderTimeout: 10s`. The documentation phase will update this to `5s` and tweak the surrounding sentence. The developer does not modify `docs/threat-model.md`; the architect does not modify it either (it is in the read-only set for both phases).

## Out of scope

- Per-IP connection rate limiting — owned by #50.
- Post-upgrade slow peers (idle frames, dribbled payloads) — owned by #15 (per-frame deadlines) and #7 (heartbeat).
- Adding a metric for `ReadHeaderTimeout` firings — no operational signal currently demands it; see Error handling above.
- Tightening `ReadTimeout`, `WriteTimeout`, or `IdleTimeout` — those govern post-header request lifecycle and are not the slow-loris surface this ticket addresses.

## Open questions

None.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No finding. The relevant boundary is TCP → `http.Server` header parser; `ReadHeaderTimeout` is enforced by `net/http` *before* any handler runs, so a slow-loris attempt never crosses into application code. Tightening the deadline narrows the time-budget for crossing the boundary without changing which code holds trusted vs untrusted data.
- [Tokens, secrets, credentials] No finding. The pre-upgrade window this ticket bounds carries no application credentials; `X-Pyrycode-Token` is parsed by the post-upgrade handlers (`/v1/server`, `/v1/client`) after `websocket.Accept`, outside the window. Shortening `ReadHeaderTimeout` from 10s → 5s does not change any token-handling path.
- [File operations] N/A — no filesystem code added or modified. Autocert cert-cache paths untouched.
- [Subprocess / external command execution] N/A — no subprocesses involved.
- [Cryptographic primitives] N/A — no crypto added or modified. `relay.TLSConfig(mgr)` is wired identically to today.
- [Network & I/O] No finding — this is the category the ticket is *for*. All four `http.Server` instances retain their full timeout set (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`) and `MaxHeaderBytes` (Go default, 1 MiB) is unchanged. Legitimate-client risk: worst-case mobile RTT (~1-2s on saturated LTE) plus a few hundred bytes of upgrade headers fits well inside the new 5s bound; the residual margin (3-4s) is large relative to realistic header-write time, so the change does not introduce a flakiness vector for normal peers. Loopback metrics path is even further from the edge (microsecond-scale RTT). Combined coverage: this ticket bounds per-attempt time at the pre-upgrade layer; #50 bounds attempts/IP/minute; #15 and #7 bound post-upgrade slow peers. The three layers are independent and additive.
- [Error messages, logs, telemetry] No finding. `ReadHeaderTimeout` firing produces no log line, no metric — matches today's 10s behaviour. The visibility gap (no signal when a slow-loris probe is dropped) is explicitly OUT OF SCOPE: the spec's "Out of scope" section names it and points future work at `internal/relay/metrics_upgrade.go`. No information is leaked in the no-log case because nothing is logged.
- [Concurrency] No finding. No new goroutines, no new locks, no change to the `relay.Shutdown` sequence (#31). The connection-close path on timeout is standard-library-owned.
- [Threat model alignment] No finding for design. The spec correctly names `docs/threat-model.md` § DoS resistance as the cited reference; that doc currently quotes the 10s value verbatim and will be updated by the documentation phase. Post-upgrade slow-peer threats (#15, #7) and per-IP attempt-rate cap (#50) are explicitly named as out of scope with ticket refs.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13

