# Spec: SetReadLimit on WSConn — per-frame size cap (#29)

## Files to read first

- `internal/relay/ws_conn.go:44-52` — `NewWSConn` constructor; this is the single application site for `SetReadLimit`.
- `internal/relay/ws_conn.go:81-84` — `WSConn.Read`; the cap surfaces here as a non-nil error from the library.
- `internal/relay/server_endpoint.go:35-59` — `ServerHandler` signature + `NewWSConn` call; one of the two production wiring threads.
- `internal/relay/client_endpoint.go:27-51` — `ClientHandler` signature + `NewWSConn` call; the other thread.
- `cmd/pyrycode-relay/main.go:23-51` — the wiring site. Mirrors the `30*time.Second` grace literal pattern (`ServerHandler(reg, logger, 30*time.Second)`).
- `internal/relay/ws_conn_test.go:20-54` — `startEcho` harness; the cap tests reuse it.
- `internal/relay/heartbeat_test.go:30-48` — `NewWSConn` call inside `startHeartbeatPair`; mechanical signature update.
- `internal/relay/server_endpoint_test.go:21-28` — `startServer` calls `ServerHandler`; signature update.
- `internal/relay/client_endpoint_test.go:21-28` — `startClient` calls `ClientHandler`; signature update.
- `internal/relay/forward.go` — confirms `phoneSource.Read` / `binarySource.Read` are the sole `WSConn.Read` callers, so cap enforcement at construction time covers both forwarders without per-call-site changes.
- `docs/knowledge/features/ws-conn-adapter.md` § *Out of scope* (the "No per-message size cap on `Read`" bullet — this ticket retires that follow-up) and § *What the adapter deliberately does NOT do* (the bullet must move to *Adversarial framing* once shipped).
- `pyrycode/pyrycode/docs/protocol-mobile.md` § *Message envelope* (l.177–201) and § *`message_chunk`* (l.456–467) + *Backfill semantics* (l.501–510): the upper-bound derivation below leans on the `≤ 50 messages per envelope` rule.

## Context

`*websocket.Conn` from `nhooyr.io/websocket` ships with a default 32 MiB read limit. That is two orders of magnitude larger than the largest expected pyrycode envelope and gives a misbehaving peer a generous pin on relay read memory. Per [`ws-conn-adapter.md`](../../knowledge/features/ws-conn-adapter.md) § *Out of scope*, a deliberate `SetReadLimit` policy was deferred until both forwarders existed; #25 (phone-side) and #26 (binary-side) have landed, so the policy now covers a single chokepoint.

`NewWSConn` is the only construction site through which either handler reaches the wire. Applying `SetReadLimit` there — rather than at each handler — is structurally simpler (one call, one literal, no forgetting one side) and is the seam the AC selects.

## Design

### Cap value: 256 KiB

Derived from `pyrycode/pyrycode/docs/protocol-mobile.md`:

- The relay frames inbound traffic on **both** sides as the inner envelope wrapped (by the receiving handler's *peer*) in either the routing envelope (binary → relay) or the bare inner envelope (phone → relay). The cap applies to the raw WS frame the relay reads, which is in all cases ≤ the largest envelope plus a small wrapper.
- The largest envelope type defined in the spec is `message_chunk`: § *Backfill semantics* fixes the default chunk size at **≤ 50 messages per envelope** (l.507). Each `message.payload` carries `conversation_id`, `message_id`, `role`, and `text`; v1 is text-only (no attachments, l.327). Treating a generous-but-not-absurd assistant message at ≤ 4 KiB UTF-8, 50 × 4 KiB ≈ 200 KiB.
- Adding JSON whitespace, envelope keys, and the routing wrapper (`conn_id` + `frame` keys, ~80 bytes) keeps the worst-case inbound frame comfortably under 256 KiB while leaving headroom for outliers (a single unusually long assistant message inside a smaller chunk).
- All other envelopes (`hello`, `hello_ack`, `error`, `send_message`, `ack`, `register_push_token`, `backfill_since`, `backfill_done`) are < 2 KiB by inspection of the spec examples.
- 256 KiB is **four orders of magnitude** below the default 32 MiB, and one order of magnitude above the protocol's worst-case legitimate envelope. The pad absorbs future text-bearing fields without a policy change.

This is a single policy value: phone-side and binary-side share it, because the binary's outbound envelopes (routing-wrapped `message_chunk`) and the phone's outbound envelopes are both bounded by the same `message_chunk` worst case. If binary-side framing later demonstrates a different bound in practice (e.g. a future streaming chunk type with a larger envelope), it gets a separate ticket per the ticket's "Out of scope" note.

### API changes

`internal/relay/ws_conn.go` — `NewWSConn` gains a `maxFrameBytes int64` parameter and applies the cap before returning:

```go
func NewWSConn(c *websocket.Conn, connID string, maxFrameBytes int64) *WSConn {
    c.SetReadLimit(maxFrameBytes)
    ctx, cancel := context.WithCancel(context.Background())
    return &WSConn{
        conn:     c,
        connID:   connID,
        closeCtx: ctx,
        cancel:   cancel,
    }
}
```

The call must precede the first `WSConn.Read` (which, by the single-reader contract, only fires from inside the forwarder once the handler launches it). Calling it at the top of `NewWSConn` — before the struct is even returned — discharges the AC's "before any `Read` is performed against it."

Type is `int64` because that is the exact signature of `nhooyr.io/websocket.Conn.SetReadLimit`. No conversion at the call site.

`internal/relay/server_endpoint.go` — `ServerHandler` gains a `maxFrameBytes int64` parameter and threads it into `NewWSConn`:

```go
func ServerHandler(reg *Registry, logger *slog.Logger, grace time.Duration, maxFrameBytes int64) http.Handler {
    return http.HandlerFunc(func(...) {
        ...
        wsconn := NewWSConn(c, connID, maxFrameBytes)
        ...
    })
}
```

`internal/relay/client_endpoint.go` — `ClientHandler` gains `maxFrameBytes int64` identically. Both handlers close over the captured value; the closure is created once per `mux.Handle` registration.

`cmd/pyrycode-relay/main.go` — single literal at the wiring site, mirroring the existing `30*time.Second` pattern:

```go
const maxFrameBytes = 256 * 1024 // 256 KiB; see docs/specs/architecture/29-wsconn-read-limit.md

mux.Handle("/v1/server", relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes))
mux.Handle("/v1/client", relay.ClientHandler(reg, logger, maxFrameBytes))
```

The literal lives in `main` as a `const` (not a `var`, not a flag) so it is compile-time and visible at the composition root. The inline comment points to this spec for the derivation. A separate package-level constant in `internal/relay` is explicitly NOT introduced (the AC forbids it and the established convention in [project-memory.md] § *Patterns established* — "Policy values live at the wiring site" — endorses keeping the literal here until a second wiring entry point needs the same value).

### Data flow

```
phone WS frame ──► nhooyr Conn.Read ──► (library checks SetReadLimit)
                                          │
                              over cap ───┤
                                          ├──► close handshake w/ 1009
                                          │     (library; no relay code)
                                          ▼
                                    WSConn.Read returns non-nil err
                                          │
                                          ▼
                        StartPhoneForwarder returns
                                          │
                                          ▼
                          handler defer { UnregisterPhone; Close; log }

binary WS frame ──► same path through StartBinaryForwarder
```

No new code paths are introduced. The library performs the close-with-1009 itself; the relay observes only the surfaced `Read` error. The handler's existing LIFO defer chain (registered after the successful claim/register) runs unchanged.

### Concurrency model

No change. `SetReadLimit` mutates state on the `*websocket.Conn` and is called once, on the construction goroutine, before any other goroutine has a reference to the `WSConn`. The library does not document `SetReadLimit` as needing concurrency protection, and the post-construction `WSConn` API never calls it again — there is no race.

### Error handling

- `WSConn.Read` returns whatever non-nil error the library surfaces for an oversize frame. The current spec for `nhooyr.io/websocket` v1.8.x close-fires with `StatusMessageTooBig` (1009) and the next `Read` returns a `*websocket.CloseError`, but the AC explicitly does NOT assert on the specific type — only on the non-nil contract. Concretely the test only asserts `err != nil`. This protects the test against library wrapping changes between minor versions.
- After the overlimit `Read`, the underlying conn is closed by the library. The next `Read` call returns the same close error. The forwarder loop sees the first error and returns; no further `Read` is issued. The test still asserts that a *subsequent* `Read` also returns a non-nil error, because that is the contract the AC enumerates ("subsequent reads fail") and because if a future library version starts auto-reopening after a 1009 we want a test signal.
- Handler-side: the AC requires no defer additions in `client_endpoint.go`. The existing defer (`UnregisterPhone`; `wsconn.Close()`; log) is sufficient: the library has already torn down the conn, and `wsconn.Close()` is idempotent under `closeOnce`. `UnregisterPhone` runs regardless.

### Testing strategy

Two new tests in `internal/relay/ws_conn_test.go`, plus mechanical signature updates at all `NewWSConn` and `ServerHandler` / `ClientHandler` call sites.

**`TestWSConn_Read_FrameExceedingCap_ReturnsError`**

- Uses `startEcho` with a small cap (e.g. 64 bytes — small enough that the test runs fast and the failure is unambiguous; the cap is a parameter, not the production 256 KiB).
- The test calls `wc.Send(<oversize payload, e.g. 256 bytes>)` so the echo handler receives it and echos it back; the **echo server** does not have the limit applied (only the client-side `WSConn` does), so the oversize frame round-trips and arrives at `wc.Read`. The receiving side surfaces the cap.
- Asserts: first `wc.Read` returns `err != nil`; second `wc.Read` (with a tight context, e.g. 500 ms) also returns `err != nil`.
- Discriminator: the test asserts *only* that errors are non-nil; the library's specific error type (close-error, library-wrapped-context-cancelled, etc.) is not asserted.

`startEcho` already returns the **client-side** `WSConn`. The cap is being applied on the client-side. The echo path (client → server → echo back → client receives) means the cap is enforced when the client reads the echoed-back oversize frame. This is the simplest way to exercise the read-side cap with the existing harness — the alternative (sending raw oversize frames from the server side) would require a custom server harness and add complexity for no test-value gain.

To make `startEcho` parametric on the cap, change its signature:

```go
func startEcho(t *testing.T, maxFrameBytes int64) (*WSConn, <-chan []byte, func())
```

Existing callers pass the production literal (`256 * 1024`); the over-cap test passes a tiny value (64). The change is a single-line edit at four existing call sites inside `ws_conn_test.go`.

**`TestWSConn_Read_FrameAtCap_DeliveredIntact`**

- `startEcho` with cap = 256 bytes; send a frame of exactly 256 bytes; assert it arrives on the channel intact (existing pattern) AND that a subsequent `wc.Read` against a freshly-sent in-cap frame returns the same bytes with `err == nil`.

**Existing tests:** `TestWSConn_ConnID_ReturnsConstructorValue` currently calls `NewWSConn(nil, "abc")`. With the new signature `NewWSConn(c, connID, maxFrameBytes)` and `SetReadLimit` called inside the constructor, passing `nil` for `c` would NPE. Options:

1. Keep the test by changing it to use `startEcho` and asserting `wc.ConnID() == "test-conn-id"`.
2. Delete the test — the assertion is then covered by `startEcho`'s constructor call and any test that uses `wc.ConnID()`.

Option (1) is the minimal-disruption path: rename to `TestWSConn_ConnID_ReturnsConstructorValue` (unchanged), swap the body to use the echo harness with a unique connID. The test still locks the constructor's connID round-trip.

**Forwarder tests:** the existing forwarder test pattern continues to work because `phoneSource` / `binarySource` are local interfaces (`internal/relay/forward.go`) and the existing forwarder tests substitute fakes that do not go through `NewWSConn` — those tests are unaffected by the signature change.

**`make test -race`, `make vet`, `make build`**: required clean per AC.

### Out of scope (reaffirming the ticket's notes)

- Per-IP / per-server-id rate limiting.
- Slow-loris hardening on the upgrade handshake.
- Differentiating phone vs. binary caps.
- Operator-configurable cap via flag.
- Per-IP / per-server-id connection count caps.

### Doc updates the developer must make

After implementation, `docs/knowledge/features/ws-conn-adapter.md` needs two edits:

1. Remove the "No per-message size cap on `Read`" bullet from § *What the adapter deliberately does NOT do*.
2. Add a "Per-frame read cap" entry to § *Adversarial framing* documenting the 256 KiB policy, the wiring-site literal, and the library's 1009 close behaviour.

(These doc edits are part of "make the spec true," not a separate ticket.)

### Open questions

None. The cap value is justified inline; the signature plumbing is the natural seam the AC names; the test surface fits the existing harness with one parametrisation.

---

## Security review (label: `security-sensitive`)

### Trust boundaries this spec touches

| Boundary | Trust posture | Where enforced |
|---|---|---|
| Phone WS frame → `WSConn.Read` on `/v1/client` | Adversarial. Phone is internet-facing, no authentication at the relay layer. | Library, via `SetReadLimit(256*1024)` set in `NewWSConn`. |
| Binary WS frame → `WSConn.Read` on `/v1/server` | Lower-trust-but-not-trusted. Binary completed the header gate; relay does not verify token. | Same chokepoint. |
| Handler defer chain after over-cap `Read` | Already trust-bounded by #5 / #16 / #21 / #25 / #26; this spec adds no new paths. | Existing `defer { Unregister/ScheduleRelease; Close; log }`. |

### Adversarial walk

**(1) DoS — read-side memory pinning.** Before: a peer can send up to ~32 MiB per frame. A handful of concurrent connections each sending a single 32 MiB frame ties up ~tens-of-MiB-per-conn of read buffer until the read completes or fails. With the 256 KiB cap, the equivalent attack pins ≤256 KiB per connection — 128× reduction. Connection-count caps remain out of scope (separate ticket), so the attack surface narrows but is not closed; this is acceptable because (a) the ticket scopes this defence narrowly and (b) the upstream HTTP server already has its own read timeouts (`ReadTimeout: 60s` in `main.go`).

**(2) DoS — slow drain.** A peer could try to send 256 KiB very slowly to keep one connection pinned. Mitigation: the library's read context (passed by the forwarder) bounds total wait; `nhooyr.io/websocket.Conn.Read` honours ctx cancellation. No new code needed — this property is unchanged from pre-#29.

**(3) Amplification across forwarders.** Pre-#29, an oversize phone frame translates (via `StartPhoneForwarder` → `Marshal` → `BinaryFor.Send`) into a Marshal allocation of similar size and a write of similar size to the binary. The 256 KiB read cap on the phone side therefore also caps the synthesised write to the binary, eliminating the cross-forwarder amplification path the ticket flags. The symmetric path (binary → phone) is identically capped via `StartBinaryForwarder`.

**(4) Differentiated cap bypass.** Single policy value means no per-side mismatch can be exploited. Adding a separate larger cap on one side is explicitly deferred.

**(5) `SetReadLimit` call placement.** The cap is applied inside `NewWSConn`, before the struct is returned, before any goroutine other than the constructor has a reference to the `WSConn`. There is no window in which a `Read` could fire against an uncapped conn — the construction site discharges the "before any `Read`" AC.

**(6) Trust of the library's enforcement.** `nhooyr.io/websocket` v1.8.x is the same direct dep called out in `docs/threat-model.md` § *Supply chain — Go dependencies*. The supply-chain risk is already named. `make lint` (`govulncheck`) covers known CVEs. No new dependency.

**(7) Error-string leakage.** The library's close error (or its message) is logged via the existing forwarder logging (`forward.go` logs the cause on return). No user-controlled bytes from the oversize payload appear in any log — the library surfaces a typed close-error, not the offending payload. No new log call is added.

**(8) Test cap of 64 / 256 bytes.** The test parametrises the cap so production code paths under test use small values. The production literal remains 256 KiB. No test-only flag or hook is added; the cap is a constructor parameter, not a global.

**(9) Idempotence of close.** Post-overlimit, the library closes the underlying conn. The handler's `wsconn.Close()` in the defer is idempotent under `closeOnce`. The active-conn close (the library's) and the handler's clean-close-on-exit collapse to a single `*websocket.Conn.Close` — no double-close anomaly.

**(10) Behavioural drift from library upgrade.** If a future minor version changes the wrapped error type or the specific close code, the tests still pass (they assert only `err != nil`). If the library starts auto-reopening the conn (extremely unlikely), the "subsequent reads fail" assertion surfaces the regression.

### Findings

None. Verdict: **PASS**.

The change is a one-line library call applied at the single existing chokepoint, with a literal threaded from one wiring site. No new code paths, no new logs, no new failure modes beyond the one the ticket exists to introduce.
