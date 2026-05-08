# Spec — /v1/server: defer binary release by 30s grace window (#21)

## Files to read first

- `internal/relay/server_endpoint.go` — full file. The single call-site swap and the new constructor parameter both live here. The disconnect `defer` block is lines 79-83.
- `internal/relay/server_endpoint.go:79-83` — exact `defer` to modify. Preserve `wsconn.Close()` and the `server_released` log line; only the registry call changes.
- `internal/relay/registry.go:125-160` — `ScheduleReleaseServer` contract (signature, doc, semantics). Read it in full so the spec choices match the registry's actual behaviour. Note: returns nothing; arms a timer; `ClaimServer` during the grace window cancels and reclaims.
- `internal/relay/server_endpoint_test.go` — full file. The existing helper `startServer` (lines 20-27) and the four test functions all construct `ServerHandler(reg, logger)`; they all need the new third argument. The new grace test goes here too.
- `internal/relay/server_endpoint_test.go:197-248` — `TestServerEndpoint_PeerClose_ReleasesSlot`. Today it asserts release within 2 s; with the swap the release becomes scheduled, so this test must pass a *short* grace through the helper to keep the 2-second wait meaningful.
- `cmd/pyrycode-relay/main.go:50` — single production wiring site. Becomes `ServerHandler(reg, logger, 30*time.Second)`.
- `docs/lessons.md` lines 21-23 — *"`defer ReleaseServer(...)` must be registered AFTER the successful `ClaimServer`."* The new call inherits this constraint verbatim; the defer position does not move.
- `docs/specs/architecture/20-grace-period-deferred-release.md` — sibling registry spec. Especially the "What this ticket deliberately does not do" section: phone-side close code is the registry's deferral, not this ticket's. The handler does *not* pick it up here either.
- `pyrycode/pyrycode/docs/protocol-mobile.md` § Authentication → Binary → relay — wire-protocol source for the 30-second grace ("the server-id is released after a 30-second grace period"). The duration constant lives at the handler's wiring site (production) so it stays near the policy definition.

## Context

#20 shipped `Registry.ScheduleReleaseServer(serverID, d)` — a deferred sibling of `ReleaseServer` that arms a timer, keeps the binary entry visible during the grace window so `ClaimServer` can reclaim it, and on expiry closes orphan phones. No call sites migrated in #20 by design; the `/v1/server` handler still calls the immediate `ReleaseServer`.

This ticket flips the handler. After this lands:

- A binary disconnect (any reason: clean close, ping timeout, network error) leaves the server-id slot reserved for `30*time.Second`.
- A reconnect within the grace window from the same logical binary lands back in the same slot atomically (registry-side behaviour from #20).
- If no reconnect arrives, the timer fires, the binary entry is removed, and any phones registered against that server-id are closed (registry-side behaviour from #20).

Scope is narrow: one call swap in `ServerHandler`, one grace duration threaded through the constructor, one new test, one wiring update in `main.go`.

## Design

### Files

Modified: `internal/relay/server_endpoint.go`, `internal/relay/server_endpoint_test.go`, `cmd/pyrycode-relay/main.go`. No new files. No new package.

### `ServerHandler` signature change

```go
// ServerHandler returns the http.Handler for /v1/server. On binary
// disconnect (clean close, network error, ping timeout) the server-id slot
// is scheduled for release after `grace`; a reconnect within that window
// (#20's reclaim path) inherits the slot atomically. Production passes
// 30*time.Second per protocol spec § Authentication → Binary → relay.
// Tests pass a short duration to exercise the scheduled-release path
// without slowing the suite.
func ServerHandler(reg *Registry, logger *slog.Logger, grace time.Duration) http.Handler
```

Rationale for "constructor parameter" over the two alternatives PO offered:

- **Constructor parameter (chosen).** Honest about the dependency; consistent with how `ServerHandler` already takes `reg` and `logger`; the duration is policy data and policy lives at the wiring site (`cmd/pyrycode-relay/main.go`), not buried in a package-level var.
- *Unexported package-level override.* Action-at-a-distance; tests would need a `setGracePeriod(t, d)` helper or a mutex to keep parallel tests honest. Avoid.
- *Options struct.* Premature abstraction for a one-field config. Reach for it when there's a second knob.

The grace duration is fully trusted from the wiring site. The handler does not validate it (`d <= 0` degrades to "release now," `d` very large degrades to "never release before process exit" — both safe per #20's registry contract; see the security-review section below).

### Disconnect-cleanup `defer`

The current block (`internal/relay/server_endpoint.go:79-83`):

```go
defer func() {
    reg.ReleaseServer(serverID)
    wsconn.Close()
    logger.Info("server_released", "server_id", serverID)
}()
```

Becomes:

```go
defer func() {
    reg.ScheduleReleaseServer(serverID, grace)
    wsconn.Close()
    logger.Info("server_released", "server_id", serverID)
}()
```

Three observations the developer should preserve:

1. **`defer` registration order is unchanged.** The lesson at `docs/lessons.md:21-23` still applies: register the cleanup *after* `ClaimServer` succeeds, so the conflict path doesn't arm a stray timer for a slot it never owned. The `defer` already lives below the `ClaimServer` success branch (line 79, after line 74's success log) — leave it there.
2. **`wsconn.Close()` stays.** The grace window concerns *registry state*; the underlying WebSocket is still gone (the peer closed it; we close our side too for hygiene). Leaving the WS open during the grace would leak file descriptors. The closed `Conn` remains in the registry's `binaries` map — that's #20's contract; phones that try to `Send` through it observe whatever error the WS adapter returns, and frame forwarding's reaction is #6's territory (deliberately out of scope).
3. **The `server_released` log line stays unchanged.** Despite the slight inaccuracy ("released" now means "scheduled for release"), the existing string is what operators search for and what #20's spec calls out. A rename would ripple into log queries with no observable benefit. If we ever distinguish "scheduled" from "fired," that's a separate field on the *expiry* path, not here.

The `readCtx := c.CloseRead(r.Context()); <-readCtx.Done()` block at the end of the handler is unchanged. The `defer` runs on return from the handler — same as before.

No other handler changes: header gate, conflict path (`ErrServerIDConflict` → 4409 close on the underlying `*websocket.Conn`), `connID` generation, `ClaimServer` invocation, success log — all untouched.

### `cmd/pyrycode-relay/main.go` wiring

One line change at line 50:

```go
mux.Handle("/v1/server", relay.ServerHandler(reg, logger, 30*time.Second))
```

The `time` package is already imported (line 14: `"time"`). The `30*time.Second` literal stays inline at the wiring site rather than a package-level constant: it appears exactly once, the value is policy not implementation detail, and inlining it keeps the protocol-spec linkage visible in the file that wires the relay together.

### Concurrency model

No new goroutines in this ticket. The handler's lifecycle is unchanged: one goroutine per upgrade (Go's `http.Server` default), `CloseRead` adds one drain goroutine on the underlying conn (existing behaviour), and the disconnect `defer` runs on the handler's goroutine.

The grace timer's goroutine is owned by `time.AfterFunc` inside the registry — that machinery is #20's responsibility and is already exercised by the registry's test suite. The handler does not block on, observe, or reason about the timer's goroutine.

### Error handling

`ScheduleReleaseServer` returns nothing. The `defer` is fire-and-forget; there is no failure mode for the handler to surface. Any operational anomaly (timer didn't fire, phones not closed) is a registry-internal concern owned by #20's tests.

### Testing strategy

`internal/relay/server_endpoint_test.go`. Three pieces of work, in order.

#### 1. Helper signature update

`startServer(t)` becomes `startServer(t, grace)`:

```go
// startServer spins up an httptest.NewServer running ServerHandler against a
// fresh registry. The grace duration is the third arg to ServerHandler; tests
// that don't exercise the disconnect path can pass any reasonable value
// (a small duration is fine — none of the non-disconnect tests wait on it).
func startServer(t *testing.T, grace time.Duration) (*Registry, string, func()) {
    t.Helper()
    reg := NewRegistry()
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    srv := httptest.NewServer(ServerHandler(reg, logger, grace))
    wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
    return reg, wsURL, srv.Close
}
```

Updating call sites (3 of them in the existing file):

- `TestServerEndpoint_ValidUpgrade_RegistersBinary` (line 45): pass `100*time.Millisecond` — test does not wait on disconnect.
- `TestServerEndpoint_DuplicateClaim_4409` (line 148): pass `100*time.Millisecond` — conflict path, no disconnect wait.
- `TestServerEndpoint_PeerClose_ReleasesSlot` (line 198): pass `100*time.Millisecond`. The existing 2-second wait for release still works (100 ms ≪ 2 s) and the test continues to verify the end-to-end "disconnect → eventual release → re-claim succeeds" property.

The two inline `httptest.NewServer(ServerHandler(reg, logger))` sites that don't go through `startServer`:

- `TestServerEndpoint_HeaderGate_400` (line 105): pass `100*time.Millisecond`.
- `TestServerEndpoint_WrongMethod_NoPanic` (line 289): pass `100*time.Millisecond`.

These are mechanical edits. Total updates: 5 call sites (1 helper + 2 inline + the 2 tests that go through the helper get the change for free).

#### 2. New test: scheduled-not-immediate release

```go
func TestServerEndpoint_PeerClose_SchedulesGraceRelease(t *testing.T) {
    grace := 200 * time.Millisecond
    reg, wsURL, cleanup := startServer(t, grace)
    defer cleanup()

    c, _, err := dialWith(t, wsURL, validHeaders("s1"))
    if err != nil {
        t.Fatalf("dial: %v", err)
    }

    // Wait for the claim to land (handler runs in its own goroutine).
    deadline := time.Now().Add(time.Second)
    for time.Now().Before(deadline) {
        if _, ok := reg.BinaryFor("s1"); ok {
            break
        }
        time.Sleep(5 * time.Millisecond)
    }
    if _, ok := reg.BinaryFor("s1"); !ok {
        t.Fatalf("BinaryFor(s1) not registered before close")
    }

    closeAt := time.Now()
    if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
        t.Fatalf("client Close: %v", err)
    }

    // Phase 1 (scheduled, NOT immediate): for ~grace/4 after closeAt, the
    // binary entry must remain in the registry. If the handler were calling
    // ReleaseServer (the pre-#21 path) instead of ScheduleReleaseServer,
    // BinaryFor would flip to false within milliseconds of the handler
    // observing the close.
    earlyDeadline := closeAt.Add(grace / 4)
    for time.Now().Before(earlyDeadline) {
        if _, ok := reg.BinaryFor("s1"); !ok {
            t.Fatalf("BinaryFor(s1) released at T+%v < grace=%v (handler used immediate ReleaseServer, not scheduled)", time.Since(closeAt), grace)
        }
        time.Sleep(5 * time.Millisecond)
    }

    // Phase 2 (timer fires): within grace + slack, the binary entry must be
    // gone. Registry's #20 expiry handler does the deletion.
    lateDeadline := closeAt.Add(2*grace + 500*time.Millisecond)
    for time.Now().Before(lateDeadline) {
        if _, ok := reg.BinaryFor("s1"); !ok {
            return // pass
        }
        time.Sleep(5 * time.Millisecond)
    }
    t.Fatalf("BinaryFor(s1) still registered at T+%v; expected scheduled release to have fired (grace=%v)", time.Since(closeAt), grace)
}
```

Why these knobs:

- **`grace = 200 ms`.** Long enough that the early-deadline window (50 ms) is comfortably outside scheduler jitter on slow CI runners; short enough that the late-deadline window (~900 ms) keeps the suite fast. The existing `TestServerEndpoint_PeerClose_ReleasesSlot` uses 100 ms successfully on the same harness; doubling it for the assertion-rich variant is a safe margin.
- **Early window is `grace/4` (50 ms), not `grace - ε`.** With ε too small the test races the timer; `grace/4` proves "scheduled, not immediate" with adequate margin. The handler-observes-close lag is at most a few ms in `httptest`, so the binary entry's earliest possible removal (under a buggy immediate-release path) is closeAt + handler-lag (~few ms), well inside the 50 ms early window.
- **Direct registry inspection** rather than waiting on a `Conn.Close()` channel: the AC explicitly suggests this approach ("by inspecting `Registry` state directly, since `ScheduleReleaseServer` exposes no return value"). It also avoids dragging the not-yet-built `/v1/client` handler into this test.

#### 3. AC: "Existing /v1/server tests still pass unchanged"

The four existing tests are unchanged in *behaviour* — header gate, conflict path, and the claim/release happy path all continue to assert what they always asserted. The mechanical helper-signature update is the only edit. Verify with `go test ./internal/relay -run TestServerEndpoint`.

### Open questions

1. **Should the grace duration be a package-level constant `defaultGrace = 30 * time.Second` exported from the relay package?** Picked **no** — the constant lives at the policy site (`main.go`). If a second binary ever needs the same value (e.g. an integration test harness or a second wiring entry point), promote it to `package relay` then. Premature constants make grep harder.
2. **Should `ServerHandler` validate `grace > 0`?** No. The registry tolerates `d <= 0` (degenerates to immediate release on the AfterFunc goroutine — same end state, marginally different timing). Adding a panic or a clamp at the handler's API surface would be defensive code with no observed failure mode (per CLAUDE.md "evidence-based fix selection"). The wiring site is the policy, and the wiring site passes `30*time.Second`.
3. **Should the `server_released` log line gain a `grace` field?** Tempting, but: the log fires at the *moment the defer runs*, which is "scheduled," and adding `grace=30s` to every disconnect log is structured-log noise. If operators ever want to distinguish "this slot is in grace" from "this slot fully released," the right place is a new event the registry's expiry handler emits, not this log line.

## Done means

- `internal/relay/server_endpoint.go`'s `ServerHandler` takes `grace time.Duration` as the third parameter; the disconnect `defer` calls `reg.ScheduleReleaseServer(serverID, grace)` instead of `reg.ReleaseServer(serverID)`. `wsconn.Close()` and the `server_released` log line are unchanged. No other edits to the file.
- `internal/relay/server_endpoint_test.go`: `startServer` signature updated; all 5 `ServerHandler(...)` call sites updated; new `TestServerEndpoint_PeerClose_SchedulesGraceRelease` added. No other test changes.
- `cmd/pyrycode-relay/main.go:50` passes `30*time.Second` as the third argument to `relay.ServerHandler`.
- `make vet`, `make test`, `make build` all clean.
- One commit on `feature/21`: `feat(relay): defer binary release with 30s grace window (#21)`.

---

## Security review

### Threat surface for THIS ticket

This ticket swaps one call site and threads one duration through the constructor. The new attack surface is whatever the swap and the constructor parameter expose:

- A binary that was previously evicted immediately on disconnect now lingers in the registry for `grace` seconds.
- The `grace` value is passed at process startup; not adversarial input.
- The handler does not gain new code paths handling untrusted input — header gate, claim, conflict path, hold-open loop are all unchanged.

### Categories walked

- **Trust boundaries.** No new boundary. The `serverID` flows in from the request header (`X-Pyrycode-Server`), is gated by the existing presence/non-empty check, and is passed verbatim to `ScheduleReleaseServer`. The registry already accepts arbitrary string keys (#3) and doesn't interpret them. No finding.
- **Tokens, secrets, credentials.** N/A — this ticket touches no auth state. Token-based binary auth is a future ticket; no surface here.
- **File operations.** N/A.
- **Subprocess / external command execution.** N/A.
- **Cryptographic primitives.** N/A — `randHex8` for `connID` is unchanged and already audited in #16.
- **Network & I/O.**
  - **Per-server slot retention.** A binary that disconnects holds its server-id slot for `grace` (30 s) instead of being evicted immediately. Operationally this is the protocol-spec-mandated reclaim window; adversarially it means an attacker who connects with a chosen server-id and then disconnects keeps that slot reserved against legitimate competing claims for 30 s. **Finding:** *not exploitable* — the reclaim-during-grace path lets a *competing legitimate connection succeed* (registry-side: `ClaimServer` during the grace window returns `nil` and replaces the binary). So a competing legit binary can take the slot inside the grace window by completing the WS handshake. The attacker's "squat" only delays a clean transition; it does not block legitimate access. (Compare: pre-#21 the attacker could also squat by *not disconnecting*; that's the same threat, neither better nor worse here.)
  - **Server-id pool exhaustion via grace.** Could an attacker open N WS connections with N distinct server-ids, drop them all, and force the relay to retain N entries for 30 s? Yes — the `binaries` map and the `timers` map both grow proportional to "distinct server-ids in flight + in grace." The total memory cost is bounded by (a) the registry's per-entry footprint (one map entry per id; one timer; phone slices are empty for never-claimed-by-phones ids) and (b) the connection-acceptance rate. **Finding:** *Mentioned, not addressed here.* Per-IP and total-connection caps are #5/#16's territory and the AC explicitly defers them. The `ScheduleReleaseServer` shape doesn't introduce *unbounded* growth — the timer eventually fires and reclaims the entry — but it does multiply the steady-state cap by `grace × accept-rate`. CLAUDE.md "evidence-based" applies: no observed exhaustion, deferral is correct.
  - **Timeouts on the WS upgrade.** Unchanged from #16/#9 (`http.Server` has the four explicit timeouts). The grace duration is unrelated to read/write deadlines.
- **Error messages, logs, telemetry.** No new error branches. The `server_released` log line is unchanged in shape and content, even though its meaning shifts slightly (now "scheduled for release"). Operators searching for the existing string keep working. **Finding:** none.
- **Concurrency.**
  - **Defer ordering.** The lesson at `docs/lessons.md:21-23` still holds — the `defer` is registered after the successful `ClaimServer`, so the conflict path doesn't arm a stray grace timer. Verified the spec preserves the line position. **Finding:** none.
  - **`wsconn.Close()` after `ScheduleReleaseServer`.** The order in the new `defer` body is `Schedule → Close → Log`, same as old `Release → Close → Log`. Closing the WS happens after the registry has the timer armed, so a phone trying to `Send` during the grace window observes whatever the closed `WSConn` returns (existing #15 contract). No race the registry's lock doesn't already cover.
  - **Goroutine lifecycle.** The handler does not spawn anything new. The grace timer's goroutine is owned by `time.AfterFunc` and audited in #20.
  - **Shutdown safety.** Process exit drops pending timers (registry has no on-disk state). A binary mid-grace at shutdown loses its slot the moment the new process starts — same as today's "binary mid-claim at shutdown" behaviour. **Finding:** none.
- **Threat model alignment.** Protocol spec § Authentication → Binary → relay specifies the 30-second grace window — this ticket implements that mechanism on the handler side. Phone-side `1011` close code is *deliberately deferred* per #20's spec (the registry's `Close()` invocation goes through the `Conn.Close()` interface which emits `1000`; emitting `1011` requires a parallel close primitive on `Conn` and is tracked as a follow-up). Out-of-scope deferral named in the spec. No further misalignment.

### Adversarial framings considered

- *"Attacker connects as server-id `X`, disconnects, reconnects within 30s — does the relay treat this as a fresh claim or a reclaim?"* — Reclaim, by design (#20). The new binary inherits the slot. This is the AC; not exploitable, it's the feature.
- *"Attacker connects as server-id `X`, disconnects, immediately tries to connect as a *different* server-id `Y` — does the grace on `X` interfere?"* — No. `X` and `Y` are independent map keys. The grace timer on `X` is unrelated to claims on `Y`.
- *"Attacker rapidly connect-disconnects with random server-ids to flood the `timers` map."* — Bounded by `grace × accept-rate`. The accept-rate cap (per-IP / total) is not in this ticket; it's #16's continuation territory and #5's per-server cap. CLAUDE.md's "evidence-based" framing applies: no observed flood, no defensive code added here. Mentioned in the spec so the developer doesn't ad-hoc a cap.
- *"Attacker passes a malicious value through the `grace` parameter."* — They can't. `grace` is set at process startup from a hard-coded literal in `main.go`; it never crosses the network boundary. A test that passes `0` or a negative value would degenerate the registry to "fire immediately"; safe per #20's contract.
- *"Race between client close and a competing `ClaimServer`."* — The competing claim either: (a) arrives before the disconnect `defer` runs, in which case it observes the slot still claimed by the first binary and gets `ErrServerIDConflict` (correct, that's the conflict path); (b) arrives after the `defer` runs, in which case it observes the timer-pending state and reclaims (correct, that's #20's reclaim path). The handler does no extra coordination here — the registry's lock orders the operations.
- *"What if `grace = 0` slips into production via a config typo?"* — `30*time.Second` is a literal in `main.go`, not a config field. There is no path to inject `0` short of editing the source. If/when a CLI flag for the duration is added, *that* ticket gets the validation; not this one.

### Verdict: PASS

The call swap inherits #20's well-tested registry-side semantics. No new untrusted-input boundary is introduced. The handler does not validate the grace duration because the duration is fully trusted (compile-time literal at the wiring site) and the registry tolerates degenerate values safely. The one concrete operational property worth flagging — `binaries` and `timers` map growth scales as `grace × accept-rate` — is named explicitly and tracked to the per-IP/total-cap tickets that own it.

**Findings:**

- [Trust boundaries] No findings — the only data crossing a boundary is `serverID`, gated by the existing header check and accepted by the registry as an opaque string key.
- [Network & I/O] OUT OF SCOPE — slot retention multiplies steady-state map size by `grace × accept-rate`. Per-IP and total-connection caps are #5/#16 territory and explicitly deferred by this ticket's "Out of scope" section.
- [Concurrency] No findings — defer position preserves the `docs/lessons.md:21-23` invariant; `Schedule → Close → Log` order is consistent with the old `Release → Close → Log` order; no new goroutines.
- [Threat model alignment] OUT OF SCOPE — phone-side `1011` close code per protocol spec is tracked as a follow-up to #20, not picked up here.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-08
