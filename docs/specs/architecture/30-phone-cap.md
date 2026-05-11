# Spec — relay: cap phones per server-id (#30)

## Files to read first

- `internal/relay/registry.go` — full file (≈270 lines, dense). The sentinel block (lines 13-21) is the pattern this ticket extends; `RegisterPhone` (lines 186-198) is the method this ticket adds a sibling to; `Conn` interface (lines 32-47) is unchanged but worth re-reading because the new method takes the same `Conn` shape.
- `internal/relay/registry.go:186-198` — `RegisterPhone`'s current body. The cap-check must happen on the same lock-held path, BEFORE the `append`. Pattern to mirror: lock, check pre-conditions (binary present, cap), then mutate, then unlock.
- `internal/relay/client_endpoint.go:43-66` — the stillborn-WSConn 4404 branch. The new at-cap branch lives next to it, structurally identical: `errors.Is`, close the underlying `*websocket.Conn` with a 44xx code, log, return. WSConn.Close was not called — there's no goroutine holding writeMu — so the underlying-conn direct close is correct (ADR-0005).
- `internal/relay/server_endpoint.go:61-80` — sibling 4409 stillborn-WSConn branch (same shape; reference when matching the new 4429 branch's structure).
- `internal/relay/client_endpoint_test.go:156-185` — `TestClientEndpoint_NoBinary_4404` — the integration-test template for the new at-cap close-code test (dial, read, assert `websocket.CloseError` with the expected `Code` and `Reason`).
- `internal/relay/client_endpoint_test.go:18-50` — helpers (`startClient`, `dialWithClient`, `validClientHeaders`, `seedBinary`, `waitForPhones`). The new test reuses every one of these.
- `internal/relay/registry_test.go:94-109` — `TestRegisterPhone_RequiresBinary` — the unit-test template for the new cap tests (claim a binary, then exercise the registration path).
- `cmd/pyrycode-relay/main.go:45-52` — wiring site. The literal `16` lands at line 51 as the third arg to `ClientHandler`. The pattern is already in place for ServerHandler's `30*time.Second` on line 50.
- `docs/PROJECT-MEMORY.md` lines 41-43 — three patterns this ticket inherits: (a) "Application WS close codes are emitted on the underlying `*websocket.Conn`" (stillborn-conn variant), (b) "Policy values live at the wiring site", (c) "Sentinel errors, branched via `errors.Is`".
- `docs/PROJECT-MEMORY.md` line 40 — "Validate adversarial input *before* allocating WS state." Not directly applicable (the cap check is per-server-id, post-upgrade), but worth noting that the cap-rejection happens AFTER `websocket.Accept` because the cap is per-server-id state, not header-shape state.
- `docs/specs/architecture/21-server-endpoint-grace-release.md` § Design → "Rationale for constructor parameter" — the precedent this ticket mirrors for threading policy through the handler factory.
- `docs/threat-model.md` lines 32-40 — DoS resistance, the threat-model entry this ticket partially addresses (the per-server-id leg of the three-ticket family).
- `pyrycode/pyrycode/docs/protocol-mobile.md:544-552` — the protocol spec's WS close-code table. **4429 is not currently listed.** See Open Questions §1 — this ticket's close-code choice depends on a coordinated spec-side addition.

## Context

The relay's `phones[serverID]` slice grows without bound. `Registry.RegisterPhone` (`internal/relay/registry.go:190-198`) appends without checking a cap, so a misbehaving binary — or a leaked binary token shared across many devices — can grow per-server memory linearly with attacker effort. The cost compounds at the data path: `StartBinaryForwarder` (#26) walks the full slice for every binary→phone frame, so unbounded phone count means unbounded per-frame fanout cost on the same server-id.

`docs/threat-model.md` § DoS resistance flags this as one of three deferred-from-v1 hardenings. The other two are explicitly out of scope here and tracked separately:

- `SetReadLimit` on inbound WS frames — caps **per-frame** byte cost.
- Per-IP rate limit on `/v1/server` + `/v1/client` upgrades (#34) — caps **upgrade-attempt** cost.

This ticket caps the **per-server-id phone-count** cost only.

The constraint shape: the cap check and the slice append must be atomic under one lock — anything else admits a TOCTOU race where two concurrent registrations both observe `len < max` and both append, ending at `len > max`. The race-shape mirrors `ClaimServer`'s first-claim-wins.

## Design

### Files touched

Modified:

- `internal/relay/registry.go` — add `ErrPhonesAtCap`; add `RegisterPhoneCapped`; refactor `RegisterPhone` body to delegate to it.
- `internal/relay/registry_test.go` — three new unit tests for the cap behaviour.
- `internal/relay/client_endpoint.go` — add `maxPhones int` parameter to `ClientHandler`; switch the registration call from `RegisterPhone` → `RegisterPhoneCapped`; add the at-cap branch (close 4429, log `phone_register_at_cap`).
- `internal/relay/client_endpoint_test.go` — add `maxPhones int` to the 3 `ClientHandler(...)` call sites (`startClient` helper at line 25, inline at line 114, plus any new helper for the cap-integration test). Add one integration test for the 4429 close.
- `cmd/pyrycode-relay/main.go` — pass `16` as the third arg to `relay.ClientHandler`.

No new files. No new packages. No new exported types.

### Why dual-method `RegisterPhone` + `RegisterPhoneCapped` (and not a signature change)

The naive design is `RegisterPhone(serverID, conn, max int) error`. That changes one method signature with 23 existing call sites across `registry_test.go`, `forward_test.go`, `healthz_test.go`, and `client_endpoint.go`. Even with mechanical `, 0)` appends, the architect's >10-call-site red line (`agents/architect/CLAUDE.md` § "Edit fan-out check") trips — and the precedent (Pyrycode #75: 26 mechanical `, nil` appends → 61 turns / `max_turns`) is exactly this shape.

The Strangler Fig alternative — three children (introduce new method, migrate prod, migrate tests + delete old) — is also overkill for production code that's ~30 lines.

The chosen design adds a sibling method:

```go
// RegisterPhoneCapped registers conn against serverID, returning
// ErrPhonesAtCap when len(phones[serverID]) already equals max. The cap
// check and the slice append run under one write lock, so two concurrent
// callers at max-1 cannot both succeed (race shape mirrors ClaimServer's
// first-claim-wins; see Registry.ClaimServer).
//
// max <= 0 disables the cap and the method behaves as RegisterPhone. This
// is what RegisterPhone uses internally to preserve its no-cap contract
// without duplicating logic.
//
// Returns ErrNoServer when no binary holds serverID (same precondition as
// RegisterPhone). ErrNoServer takes precedence over ErrPhonesAtCap when
// both apply — the no-server check runs first because the cap check
// against a zero-length slice would otherwise mask the missing binary
// for max == 0 (it doesn't, but for callers reading the code the
// no-server-first order is the contract).
//
// The mapping ErrPhonesAtCap → WS close code 4429 is informational; the
// registry does not interpret close codes. /v1/client (#30) emits 4429
// in the stillborn-WSConn window.
func (r *Registry) RegisterPhoneCapped(serverID string, conn Conn, max int) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    if _, ok := r.binaries[serverID]; !ok {
        return ErrNoServer
    }
    if max > 0 && len(r.phones[serverID]) >= max {
        return ErrPhonesAtCap
    }
    r.phones[serverID] = append(r.phones[serverID], conn)
    return nil
}

func (r *Registry) RegisterPhone(serverID string, conn Conn) error {
    return r.RegisterPhoneCapped(serverID, conn, 0)
}
```

Tradeoff: a second method on the registry's small API surface. Justification: cap-awareness is a permanent semantic distinction (the test fixture path genuinely doesn't want a cap; the prod path always wants one). The two are sibling APIs, not legacy + new — they are intended to coexist. `RegisterPhone` is the "no cap" convenience; `RegisterPhoneCapped` is the cap-aware variant.

The alternative considered and rejected: a `Registry.SetMaxPhones(n int)` setter with a `Registry.maxPhones` field. Cleaner external API (one method, no `max` parameter on `RegisterPhone`), but a setter on a struct that is otherwise immutable post-construction violates the "passive in-memory store" framing in `docs/PROJECT-MEMORY.md` and creates a two-step-init bug surface ("forgot the setter"). Rejected.

### Sentinel error

Added alongside the existing two in `internal/relay/registry.go`:

```go
// ErrPhonesAtCap is returned by RegisterPhoneCapped when len(phones[serverID])
// already equals the max-phones policy value passed by the caller. The error
// is returned BEFORE the slice is mutated. Maps to WS close code 4429.
ErrPhonesAtCap = errors.New("relay: phones at cap for server-id")
```

The error string follows the existing `relay: ...` prefix convention (`ErrServerIDConflict`: `"relay: server-id already claimed"`; `ErrNoServer`: `"relay: no binary for server-id"`).

### `ClientHandler` signature change

```go
// ClientHandler returns the http.Handler for /v1/client. The maxPhones
// parameter caps the number of phones that may register against a single
// server-id at one time; an over-cap registration is rejected with WS
// close code 4429. Production passes 16 per docs/threat-model.md § DoS
// resistance — sized for phone + tablet + several desktops + headroom.
// Tests pass 0 to disable the cap (or a small value to exercise the
// boundary).
func ClientHandler(reg *Registry, logger *slog.Logger, maxPhones int) http.Handler
```

The `maxPhones` parameter is fully trusted (compile-time literal at the wiring site, never crosses the network). `maxPhones <= 0` degrades to "no cap" — same contract as `RegisterPhoneCapped`'s `max <= 0`. The handler does not validate it (see Open Questions §2).

### Cap-rejection branch in `/v1/client`

The current 4404 branch (`internal/relay/client_endpoint.go:53-66`):

```go
if err := reg.RegisterPhone(serverID, wsconn); err != nil {
    if errors.Is(err, ErrNoServer) {
        _ = c.Close(websocket.StatusCode(4404), "no server with that id")
        logger.Info("phone_register_no_server",
            "server_id", serverID,
            "remote", remoteHost(r))
        return
    }
    wsconn.Close()
    return
}
```

Becomes:

```go
if err := reg.RegisterPhoneCapped(serverID, wsconn, maxPhones); err != nil {
    if errors.Is(err, ErrNoServer) {
        _ = c.Close(websocket.StatusCode(4404), "no server with that id")
        logger.Info("phone_register_no_server",
            "server_id", serverID,
            "remote", remoteHost(r))
        return
    }
    if errors.Is(err, ErrPhonesAtCap) {
        _ = c.Close(websocket.StatusCode(4429), "too many phones for server-id")
        logger.Info("phone_register_at_cap",
            "server_id", serverID,
            "remote", remoteHost(r))
        return
    }
    wsconn.Close()
    return
}
```

Three structural points the developer should preserve:

1. **Order of checks is `ErrNoServer` first, then `ErrPhonesAtCap`.** This matches the registry's check order (no-server before cap) and means a no-binary state yields 4404, not 4429, even when the cap is 0. Symmetric with the registry; consistent error semantics for the caller.
2. **The at-cap log line does NOT include `device_name` or `conn_id`.** Registration didn't succeed, so the conn_id is local-only (the random hex was generated but the conn is going to be closed); device_name is uninteresting for a rejected registration. The field set matches the no-server line: `server_id` + `remote`.
3. **Token is still never logged.** The handler reads the token into a local string for the presence check and lets it go out of scope (`internal/relay/client_endpoint.go:30-35`). The at-cap path runs after that, so the local is already out of scope. Per `docs/PROJECT-MEMORY.md` line 45 — "Credentials the relay does not validate are presence-checked then discarded — never logged".

The unregister-defer block (`internal/relay/client_endpoint.go:74-80`) is unchanged — like the 4404 path, the at-cap path returns before the defer is registered, so `UnregisterPhone` is never called for a registration that never landed.

### Wiring (`cmd/pyrycode-relay/main.go`)

One line change at line 51:

```go
mux.Handle("/v1/client", relay.ClientHandler(reg, logger, 16))
```

The literal `16` lives at the wiring site, mirroring `30*time.Second` on the line above. The value comes from PO's technical-note suggestion ("phone + tablet + several desktops + headroom"); see "Cap value rationale" below.

### Cap value rationale

`16` is the upper bound of "plausible legitimate device fleet for a single binary": one primary phone, one tablet, two or three desktops, plus headroom for short-lived debug clients and reconnect races where an old phone hasn't been swept yet. The threat-model concern starts at the *unbounded* end of the spectrum — there's no precise number that distinguishes "legitimate fleet" from "attacker squat", just a heuristic. `16` is large enough that no legitimate user trips it, small enough that an attacker who maximally exploits it incurs a 16× memory amplification per server-id, not a 1000× or 10000× one.

If a legitimate user trips `16`, they see `phone_register_at_cap` in the relay logs and we revisit. No production deploys exist at the time of writing (#28 deploys the binary; the relay is pre-prod) so this is the no-cost moment to pick a value.

### Concurrency model

No new goroutines. The cap-check-and-append happens under the registry's existing `r.mu` write lock — same lock that `RegisterPhone`, `ClaimServer`, `UnregisterPhone`, `ScheduleReleaseServer`, and `handleGraceExpiry` already serialise on. No new lock ordering. The race shape ("first call wins; second observes `>= max` and rejects") is structurally identical to `ClaimServer`'s "first claim wins; second observes already-set and gets `ErrServerIDConflict`" path, which is already race-tested via `TestClaimServer_FirstWinsSecondConflicts` and the broad `TestRegistry_RaceFreedom`.

### Error handling

Errors from `RegisterPhoneCapped` are surfaced through the existing `errors.Is` branch pattern. The `wsconn.Close()` fallthrough at `internal/relay/client_endpoint.go:65` continues to exist — it is the catch-all for any future sentinel the registry might add. The fallthrough does not log; per the same convention as the server endpoint's defensive close (`internal/relay/server_endpoint.go:78`), the diff that introduces a new sentinel adds its own log line.

### Testing strategy

Three new unit tests in `internal/relay/registry_test.go` for `RegisterPhoneCapped`. One new integration test in `internal/relay/client_endpoint_test.go` for the 4429 close path. Two trivial mechanical edits in `client_endpoint_test.go` to thread `0` through existing `ClientHandler(...)` call sites (these don't exercise the cap).

#### 1. `TestRegisterPhoneCapped_BoundaryAndRejection`

Boundary: registering exactly `max` phones succeeds; the next attempt returns `ErrPhonesAtCap`. Template: `TestRegisterPhone_RequiresBinary` shape.

```go
func TestRegisterPhoneCapped_BoundaryAndRejection(t *testing.T) {
    t.Parallel()
    r := NewRegistry()
    if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
        t.Fatalf("ClaimServer: %v", err)
    }
    const max = 3
    for i := 0; i < max; i++ {
        p := &fakeConn{id: fmt.Sprintf("p-%d", i)}
        if err := r.RegisterPhoneCapped("s1", p, max); err != nil {
            t.Fatalf("RegisterPhoneCapped #%d at <=cap: got %v, want nil", i, err)
        }
    }
    over := &fakeConn{id: "p-over"}
    if err := r.RegisterPhoneCapped("s1", over, max); !errors.Is(err, ErrPhonesAtCap) {
        t.Fatalf("RegisterPhoneCapped at cap: got %v, want errors.Is(_, ErrPhonesAtCap)", err)
    }
    // The rejected registration must NOT have appended.
    if got := r.PhonesFor("s1"); len(got) != max {
        t.Errorf("PhonesFor after rejection: got len=%d, want %d", len(got), max)
    }
}
```

#### 2. `TestRegisterPhoneCapped_RecoveryAfterUnregister`

After `UnregisterPhone` brings the count below `max`, the next `RegisterPhoneCapped` succeeds again.

```go
func TestRegisterPhoneCapped_RecoveryAfterUnregister(t *testing.T) {
    t.Parallel()
    r := NewRegistry()
    if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
        t.Fatalf("ClaimServer: %v", err)
    }
    const max = 2
    p1 := &fakeConn{id: "p-1"}
    p2 := &fakeConn{id: "p-2"}
    if err := r.RegisterPhoneCapped("s1", p1, max); err != nil {
        t.Fatalf("RegisterPhoneCapped p1: %v", err)
    }
    if err := r.RegisterPhoneCapped("s1", p2, max); err != nil {
        t.Fatalf("RegisterPhoneCapped p2: %v", err)
    }
    if err := r.RegisterPhoneCapped("s1", &fakeConn{id: "p-3"}, max); !errors.Is(err, ErrPhonesAtCap) {
        t.Fatalf("RegisterPhoneCapped at cap: got %v, want errors.Is(_, ErrPhonesAtCap)", err)
    }
    r.UnregisterPhone("s1", "p-1")
    p3 := &fakeConn{id: "p-3"}
    if err := r.RegisterPhoneCapped("s1", p3, max); err != nil {
        t.Fatalf("RegisterPhoneCapped after unregister: got %v, want nil", err)
    }
}
```

#### 3. `TestRegisterPhoneCapped_PerServerIDIndependent`

The cap is per-server-id, not global — a second server-id can register up to its own cap independently.

```go
func TestRegisterPhoneCapped_PerServerIDIndependent(t *testing.T) {
    t.Parallel()
    r := NewRegistry()
    if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
        t.Fatalf("ClaimServer s1: %v", err)
    }
    if err := r.ClaimServer("s2", &fakeConn{id: "b-2"}); err != nil {
        t.Fatalf("ClaimServer s2: %v", err)
    }
    const max = 2
    // Fill s1 to cap.
    for i := 0; i < max; i++ {
        if err := r.RegisterPhoneCapped("s1", &fakeConn{id: fmt.Sprintf("p-s1-%d", i)}, max); err != nil {
            t.Fatalf("RegisterPhoneCapped s1 #%d: %v", i, err)
        }
    }
    if err := r.RegisterPhoneCapped("s1", &fakeConn{id: "p-s1-over"}, max); !errors.Is(err, ErrPhonesAtCap) {
        t.Fatalf("RegisterPhoneCapped s1 over: got %v, want ErrPhonesAtCap", err)
    }
    // s2 is still empty; up-to-cap registrations on s2 must succeed.
    for i := 0; i < max; i++ {
        if err := r.RegisterPhoneCapped("s2", &fakeConn{id: fmt.Sprintf("p-s2-%d", i)}, max); err != nil {
            t.Fatalf("RegisterPhoneCapped s2 #%d (s1 at cap): %v", i, err)
        }
    }
}
```

#### 4. Optional: `TestRegisterPhone_DelegatesToCappedWithZero`

The wrapper invariant — `RegisterPhone(sid, conn) == RegisterPhoneCapped(sid, conn, 0)`. Not strictly required by the AC, but a one-liner that documents the invariant in code and catches future regressions in the wrapper.

```go
func TestRegisterPhone_NoCapAfterDelegation(t *testing.T) {
    t.Parallel()
    r := NewRegistry()
    if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
        t.Fatalf("ClaimServer: %v", err)
    }
    // 64 registrations through RegisterPhone must all succeed — the
    // wrapper passes max=0 which disables the cap.
    for i := 0; i < 64; i++ {
        if err := r.RegisterPhone("s1", &fakeConn{id: fmt.Sprintf("p-%d", i)}); err != nil {
            t.Fatalf("RegisterPhone #%d: %v", i, err)
        }
    }
}
```

Include this test — it is cheap and pins the wrapper-delegation contract.

#### 5. `TestClientEndpoint_AtCap_4429` (integration)

Template: `TestClientEndpoint_NoBinary_4404` (`internal/relay/client_endpoint_test.go:156-185`). Spin up a `ClientHandler` with a small cap (e.g. `maxPhones = 2`), pre-register a binary, dial the cap-many phones successfully, then dial one more and assert the WS read returns a `*websocket.CloseError` with `Code == 4429` and `Reason == "too many phones for server-id"`. Add a dedicated helper to spin up the server with the cap, parallel to the existing `startClient`:

```go
func startClientWithCap(t *testing.T, maxPhones int) (*Registry, string, func()) {
    t.Helper()
    reg := NewRegistry()
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    srv := httptest.NewServer(ClientHandler(reg, logger, maxPhones))
    wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
    return reg, wsURL, srv.Close
}
```

The test:

```go
func TestClientEndpoint_AtCap_4429(t *testing.T) {
    const cap = 2
    reg, wsURL, cleanup := startClientWithCap(t, cap)
    defer cleanup()
    seedBinary(t, reg, "s1")

    // Dial the cap-many phones successfully.
    inCap := make([]*websocket.Conn, 0, cap)
    for i := 0; i < cap; i++ {
        c, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
        if err != nil {
            t.Fatalf("dial #%d: %v", i, err)
        }
        inCap = append(inCap, c)
    }
    waitForPhones(t, reg, "s1", cap, time.Second)

    // The (cap+1)-th must be closed with 4429.
    over, _, err := dialWithClient(t, wsURL, validClientHeaders("s1"))
    if err != nil {
        t.Fatalf("dial over-cap: %v", err)
    }
    defer over.Close(websocket.StatusNormalClosure, "")

    readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancelRead()
    _, _, readErr := over.Read(readCtx)
    var ce websocket.CloseError
    if !errors.As(readErr, &ce) {
        t.Fatalf("over-cap Read err = %v (%T), want *websocket.CloseError", readErr, readErr)
    }
    if ce.Code != websocket.StatusCode(4429) {
        t.Fatalf("over-cap close code = %d, want 4429", ce.Code)
    }
    if ce.Reason != "too many phones for server-id" {
        t.Fatalf("over-cap close reason = %q, want %q", ce.Reason, "too many phones for server-id")
    }

    // The cap-many in-cap phones are unaffected and remain registered.
    if got := reg.PhonesFor("s1"); len(got) != cap {
        t.Errorf("PhonesFor after over-cap rejection: got len=%d, want %d", len(got), cap)
    }

    // Clean up in-cap phones.
    for _, c := range inCap {
        _ = c.Close(websocket.StatusNormalClosure, "")
    }
}
```

#### 6. Existing `ClientHandler(...)` call sites — mechanical edits

Two sites in `internal/relay/client_endpoint_test.go` need `maxPhones` threaded:

- `startClient` helper at line 25: `httptest.NewServer(ClientHandler(reg, logger))` → `httptest.NewServer(ClientHandler(reg, logger, 0))`. The existing tests (`TestClientEndpoint_ValidUpgrade_RegistersPhone`, `TestClientEndpoint_NoBinary_4404`, `TestClientEndpoint_PeerClose_UnregistersPhone`, `TestClientEndpoint_MultiplePhones_IndependentLifecycle`, `TestClientEndpoint_DeviceNameOptional_HandlerAccepts`) do not exercise the cap; passing `0` (no cap) preserves their semantics.
- Inline at line 114 inside `TestClientEndpoint_HeaderGate_400`: same `, 0)` append.

The cap-integration test uses `startClientWithCap` (above), so it does not perturb existing tests.

`cmd/pyrycode-relay/main.go:51` becomes `relay.ClientHandler(reg, logger, 16)` — one Edit.

Total simultaneous-update call-site count: **3 `ClientHandler(...)` sites** (1 production + 2 test). Well below the 10 red line.

### What this ticket deliberately does NOT do

- **No global (across-server-ids) cap.** A different data structure and a different policy conversation. Out of scope.
- **No change to the data path.** `StartPhoneForwarder` / `StartBinaryForwarder` are untouched. The 1:N fanout cost is still proportional to slice length; this ticket bounds the slice but doesn't optimise the walk.
- **No change to existing 4404 / 4409 close-code semantics.** The new 4429 is additive.
- **No `SetReadLimit` on inbound WS frames.** Tracked separately (per-frame cost cap).
- **No per-IP rate limit on `/v1/server` + `/v1/client` upgrades.** Tracked in #34 (per-attempt cost cap).
- **No protocol-spec edit.** See Open Questions §1 — 4429 needs to be added to `protocol-mobile.md`'s WS close-code table in a coordinated spec-side change. Not done here because this repo doesn't own the canonical spec.

## Open questions

1. **WS close code 4429 is not currently listed in `pyrycode/pyrycode/docs/protocol-mobile.md:544-552`.** The existing table enumerates `1000`, `1011`, `4401`, `4404`, `4409`. The relay-side architect skill's instruction is explicit: "Do not invent message shapes; if the spec doesn't cover a case, surface that as a ticket against the spec, not as ad-hoc relay code." (`agents/architect/CLAUDE.md` § Repo Context).

   **Decision:** the spec adds 4429 with the rationale below; this ticket ships the relay-side enforcement; a paired protocol-spec PR adds 4429 to the close-code table before the relay change reaches production. The two are coordinated, not interlocked — a relay deployed with 4429 enforcement before the spec lands would just emit a 44xx code the phone client treats as a generic abort (existing close codes are already application-defined; the phone client must already be tolerant of unknown 44xx codes per RFC 6455's "no specific application-level meaning" framing). The developer of this ticket does NOT block on the spec PR; the developer of the spec PR cites this ticket.

   **Rationale for 4429:** The existing relay-emitted codes follow the convention "WS app code = HTTP status code value" (`4404` → HTTP 404 Not Found, `4409` → HTTP 409 Conflict). HTTP 429 is "Too Many Requests" — the standard rate-limit / cap-rejection status. Continuing the convention, `4429` is the unambiguous choice. PO endorsed "4429-range, treating it as the WS analogue of HTTP 429"; this spec confirms the exact value `4429`.

   **What the developer should do:** use `4429` as-is. If the spec PR proposes a different value (extremely unlikely; the convention is established), the relay change rebases trivially — one literal in the close-code line, one literal in the integration test's assertion.

2. **Should `ClientHandler` validate `maxPhones > 0`?** No. Same reasoning as `ServerHandler`'s `grace` (`docs/specs/architecture/21-server-endpoint-grace-release.md` § Open Questions §2): the wiring site is the policy, and the wiring site passes `16`. `maxPhones <= 0` degrades to "no cap" — the same contract `RegisterPhoneCapped` documents. A test that wants no cap passes `0`; a production deploy that wants no cap can't get there short of a source edit, and that source edit is exactly the protocol-level signal we'd want. No panic, no clamp.

3. **Should the at-cap log line include `device_name`?** No. Device-name is advertised by the phone via `X-Pyrycode-Device-Name`, an *optional* header (`docs/PROJECT-MEMORY.md` line 21 — "optional `x-pyrycode-device-name`"). The success log includes it (`phone_registered` log line at `internal/relay/client_endpoint.go:68-72`) because the registration succeeded and operators correlate by device. A rejected registration has nothing to correlate against — `server_id` and `remote` (the IP) are the operator-actionable fields. Symmetric with the 4404 line.

4. **Should the at-cap log line distinguish from the 4404 path with a `close_code` field?** No new field. The log event name (`phone_register_at_cap` vs `phone_register_no_server`) is the distinguisher. Adding a `close_code` field to both would duplicate the event-name information without adding signal. Match the existing 4404 line's field set verbatim.

## Done means

- `internal/relay/registry.go` exports `ErrPhonesAtCap` and `RegisterPhoneCapped(serverID string, conn Conn, max int) error`. `RegisterPhone(serverID, conn) error` is unchanged in signature; its body delegates to `RegisterPhoneCapped(serverID, conn, 0)`.
- `internal/relay/registry_test.go` contains four new tests: `TestRegisterPhoneCapped_BoundaryAndRejection`, `TestRegisterPhoneCapped_RecoveryAfterUnregister`, `TestRegisterPhoneCapped_PerServerIDIndependent`, `TestRegisterPhone_NoCapAfterDelegation`. No existing tests modified.
- `internal/relay/client_endpoint.go`'s `ClientHandler` takes `maxPhones int` as the third parameter. The registration call is `reg.RegisterPhoneCapped(serverID, wsconn, maxPhones)`. A new `errors.Is(err, ErrPhonesAtCap)` branch closes the underlying `*websocket.Conn` with `websocket.StatusCode(4429)` and reason `"too many phones for server-id"`; it logs `phone_register_at_cap` with `server_id` and `remote`. The 4404 branch and the unregister-defer are unchanged.
- `internal/relay/client_endpoint_test.go` has the two existing `ClientHandler(...)` call sites updated to pass `0`, plus a new `startClientWithCap` helper and `TestClientEndpoint_AtCap_4429` test.
- `cmd/pyrycode-relay/main.go:51` passes `16` as the third arg.
- `make vet`, `make test` (`-race`), and `make build` all clean.
- One commit on `feature/30`: `feat(relay): cap phones per server-id (#30)`.

---

## Security review

**Verdict:** PASS

### Threat surface for THIS ticket

This ticket adds a per-server-id cap on phone registrations, a new sentinel, and a new 4429 close-code path on `/v1/client`. The new attack surface is whatever the cap-enforcement adds, plus whatever the new 4429 path exposes. The `maxPhones` value is passed at process startup; not adversarial input.

### Categories walked

- **Trust boundaries.** No new boundary. `serverID` continues to flow in via the existing `X-Pyrycode-Server` presence-check (`internal/relay/client_endpoint.go:29-35`); it is passed verbatim to `RegisterPhoneCapped`, which treats it as an opaque map key. The `maxPhones int` parameter is a compile-time literal at the wiring site (`cmd/pyrycode-relay/main.go:51`); no path injects it from the network. **Finding:** No findings.
- **Tokens, secrets, credentials.** The at-cap path runs AFTER the token presence check (lines 30-35) but BEFORE any token-bearing field could enter a log line. The token local is already out of scope by the time `RegisterPhoneCapped` returns. The new `phone_register_at_cap` log line includes only `server_id` and `remote` (the IP) — symmetric with the 4404 line. The reason string `"too many phones for server-id"` on the close frame contains no token-derived data. **Finding:** No findings — token-handling invariants preserved.
- **File operations.** N/A — no file I/O.
- **Subprocess / external command execution.** N/A.
- **Cryptographic primitives.** N/A — no crypto. `randHex8` for `connID` (`internal/relay/server_endpoint.go:118`) is unchanged.
- **Network & I/O.**
  - **Resource exhaustion (the threat this ticket addresses).** The cap bounds `len(phones[serverID]) <= maxPhones = 16` per server-id, so a single binary's phone slice can't grow without bound, and the data-path fanout (`StartBinaryForwarder` walks the slice per frame) is bounded above. The threat from a leaked-token-fan-out attack — N devices all sharing a binary's token — is reduced from "linear in attacker effort" to "bounded by 16 per server-id". **Finding:** addresses the ticket's stated DoS-resistance goal. Per `docs/threat-model.md` § DoS resistance, this is one leg of a three-leg hardening (per-frame cap and per-IP upgrade rate-limit are the other two, deferred).
  - **Cross-server-id resource exhaustion.** This ticket does NOT bound total phone count across all server-ids. An attacker can still register many distinct server-ids (each within its own cap) and grow the registry. **Finding:** **OUT OF SCOPE.** Per-IP rate limit on `/v1/client` upgrades (#34) is the per-attacker cap; total-connection cap is its own future ticket. Named explicitly in "What this ticket deliberately does NOT do".
  - **Cap value choice.** `16` is the rationale-backed pick in § "Cap value rationale". It's a heuristic, not an attacker-derived bound. **Finding:** No findings — the value's selection is in scope for review; chosen value documented.
  - **Memory amplification per close-frame.** A 4429 close emits an 8-byte close frame (`websocket.StatusCode` + reason). Cost per rejection is bounded. **Finding:** None.
  - **Timeouts on the WS upgrade.** Unchanged. `http.Server` has the four explicit timeouts in `cmd/pyrycode-relay/main.go`. **Finding:** None.
- **Error messages, logs, telemetry.**
  - New log event `phone_register_at_cap`. Fields: `server_id` (request-header value; already-logged elsewhere), `remote` (client IP via `remoteHost(r)`; already-logged elsewhere). No token. No device-name. No payload. **Finding:** None — log field set is symmetric with the existing 4404 line and complies with `docs/threat-model.md` § Log hygiene.
  - New error string `"relay: phones at cap for server-id"` and close reason `"too many phones for server-id"`. Neither names the cap value, neither leaks state. The cap value is policy that lives at the wiring site; emitting `16` over the wire would tell an attacker the exact cap, which is mildly useful for a probe; not emitting it costs nothing. **Finding:** None — cap value is not in the close reason.
- **Concurrency.**
  - **Cap check and append are atomic.** Both run under `r.mu.Lock()` in `RegisterPhoneCapped`. The TOCTOU race shape (two callers at `max-1` both passing the check, both appending, ending at `max+1`) is structurally impossible. **Finding:** None — atomic by construction.
  - **Lock ordering.** No new locks. `RegisterPhoneCapped` takes `r.mu` exactly as `RegisterPhone` does; no change to the lock graph. **Finding:** None.
  - **Race tests already cover the new method.** `TestRegistry_RaceFreedom` (`internal/relay/registry_test.go:457-484`) hammers `RegisterPhone` from many goroutines. The wrapper delegation means `RegisterPhoneCapped` is exercised under the same race conditions. Adding a `RegisterPhoneCapped` direct call into that race loop is cheap and worth doing for posterity — the developer should add `_ = r.RegisterPhoneCapped(sid, &fakeConn{id: ...}, 16)` to one of the existing race-test goroutines. **Finding:** SHOULD FIX — non-blocking; ask the developer to add one call into the race test. Not a gate.
  - **Goroutine lifecycle.** No new goroutines. **Finding:** None.
  - **Shutdown safety.** No on-disk state. Process exit drops cap-rejected close frames mid-flight — same as today's 4404 close on process exit. **Finding:** None.
- **Threat model alignment.** `pyrycode/pyrycode/docs/protocol-mobile.md` § Security model lists the resource-exhaustion / DoS surface as a known v1-deferred area; `docs/threat-model.md` § DoS resistance is the in-relay counterpart. This ticket lands one of the three named hardenings. The protocol spec's WS close-code table (line 544-552) needs a coordinated update to add 4429 — flagged as Open Questions §1, not a security gate but a coordination requirement. **Finding:** No security findings; protocol-spec alignment tracked outside this ticket.

### Adversarial framings considered

- *"Attacker connects 17 phones to the same server-id."* — 16 succeed; the 17th is closed 4429. The 16 in-cap phones are unaffected. The data path's per-frame fanout is bounded at 16. This is the feature.
- *"Attacker connects to server-id A up to cap, then to B, C, D, ..."* — Per-server-id cap doesn't stop this. Total memory grows as (number of server-ids in flight) × 16. Out of scope; addressed by per-IP rate-limit (#34) and any future total-connection cap.
- *"Attacker disconnects an in-cap phone to free a slot, then immediately registers."* — The slot frees as soon as the disconnect handler runs (`reg.UnregisterPhone` in the cleanup defer). The cap is "live count", not "high-water mark", so an attacker can churn the slot. This is correct behaviour — a legitimate user re-pairing a device after a network blip should not be permanently locked out. No finding.
- *"Race: two registrations arrive at exactly the cap-1 point."* — Atomic under `r.mu.Lock()`. The first wins (count → max, ok), the second observes count == max, returns `ErrPhonesAtCap`. Same race shape as `ClaimServer` and covered by `TestRegistry_RaceFreedom` via the `RegisterPhone` delegation.
- *"Negative or zero `maxPhones` slips into production."* — `cmd/pyrycode-relay/main.go:51` is a literal `16`. No CLI flag; no config file; no path to inject. If `0` somehow got through, the cap would be disabled and the relay would behave as it does today (unbounded slice growth). Safe degradation, not unsafe.
- *"Attacker passes a malicious value for `serverID` to exploit the cap check."* — The cap check is `len(r.phones[serverID]) >= max`. `serverID` is a map key; Go's map indexing returns the zero value (nil slice, len 0) for an unknown key. No injection; no panic; no allocation of attacker-controlled size.
- *"Attacker triggers many 4429 closes hoping to amplify cost."* — Per-rejection cost is bounded by one map read + one close-frame write. Same shape as the existing 4404 path. No amplification.
- *"4429 not in the protocol spec — does the phone client misbehave?"* — Per RFC 6455, an application-level close code in [4000, 4999] is "no specific application-level meaning" without prior agreement. A phone client unaware of 4429 sees the close, reads the reason string, and aborts. The relay does not depend on the phone client's specific reaction. The phone client team's interpretation of 4429 is tracked in the coordinated spec PR (Open Questions §1).

### Findings summary

- [Trust boundaries] No findings — boundary unchanged; `maxPhones` is a compile-time literal at the wiring site.
- [Tokens] No findings — at-cap log line includes neither token nor device-name; symmetric with the 4404 line.
- [Network & I/O] No findings on the per-server-id cap itself; cross-server-id total cap is OUT OF SCOPE (deferred to #34 and a future total-cap ticket, named in "What this ticket deliberately does NOT do").
- [Logs] No findings — `phone_register_at_cap` field set is `server_id` + `remote`, identical to the 4404 line.
- [Concurrency] SHOULD FIX (non-gating) — the developer should add one `RegisterPhoneCapped` call into `TestRegistry_RaceFreedom`'s inner loop for posterity, even though the wrapper-delegation already exercises the same path.
- [Threat model alignment] OUT OF SCOPE for the protocol-spec edit — coordinated outside this ticket per Open Questions §1.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-11
