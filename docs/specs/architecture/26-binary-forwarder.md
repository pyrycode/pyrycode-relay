# Binary-side frame forwarder (#26)

Per-binary read pump that unwraps each inbound routing envelope, looks
up the addressed phone within the binary's own server-id, and writes
the inner frame to that phone. Replaces the
`c.CloseRead(r.Context())` + `<-readCtx.Done()` placeholder in
`internal/relay/server_endpoint.go`. Mirror-image of the phone-side
forwarder shipped in #25 — same overall shape, but with a divergent
error policy: a single misaddressed or malformed frame, or a single
failing phone `Send`, MUST NOT tear down the binary connection.

## Files to read first

- `internal/relay/forward.go` (whole file, 75 lines) — `phoneSource`
  interface and `StartPhoneForwarder` body. The new function mirrors
  this shape exactly except for the Send-error / unknown-id /
  malformed-envelope branches, which `continue` rather than `return`.
  Lines 27-30 capture the "forwarder owns reading; handler owns
  cleanup" rule that must apply equally here.
- `internal/relay/forward_test.go` (whole file, 270 lines) —
  `fakePhone`, `fakeBinary`, `runForwarder`, `waitForSent`,
  `compactJSON`, `discardLogger`, and the `// Manual stress: go test
  -race -count=20 …` doc-comment header. Reuse all of these. The new
  tests need a parallel "binary as source" fake (Read side) and a
  "phone as sink" fake (Send side). See § Testing strategy for the
  minimal additions.
- `internal/relay/server_endpoint.go:35-112` — current `/v1/server`
  handler. Lines 109-110 hold the `c.CloseRead(r.Context())` +
  `<-readCtx.Done()` block this spec replaces. Note the existing
  `defer { ScheduleReleaseServer; Close; log }` (lines 87-91), the
  heartbeat goroutine + `defer cancelHB` (lines 99-101), and the
  defer order — the forwarder must NOT touch any of those, and the
  LIFO unwind on forwarder return must remain `cancelHB → release →
  Close`.
- `internal/relay/client_endpoint.go:74-99` — the call shape we mirror:
  `_ = StartPhoneForwarder(r.Context(), reg, serverID, wsconn, logger)`
  parked on the handler goroutine, return value discarded, defers
  running on return.
- `internal/relay/envelope.go` (whole file, 80 lines) — `Unmarshal`
  signature, the `Envelope` shape (`ConnID string`, `Frame
  json.RawMessage`), and the four sentinel errors
  (`ErrMalformedEnvelope`, `ErrMissingConnID`, `ErrMissingFrame`,
  plus `ErrInvalidFrameJSON` from the marshal side). The forwarder
  treats every `Unmarshal` error the same way (warn + drop +
  continue), so `errors.Is` branching is not strictly required —
  but the spec uses `errors.Is(err, ErrMalformedEnvelope) || …` in
  the AC bullet to signal that all sentinels are handled, not just
  the type-assertion-style `_, isJSON := err.(*json.SyntaxError)`.
- `internal/relay/registry.go:32-47` — `Conn` interface (`ConnID`,
  `Send`, `Close`); `PhonesFor` at lines 246-256 — the per-frame
  lookup. The forwarder iterates the returned snapshot (not the
  registry's internal slice) and matches `ConnID()` against
  `env.ConnID`.
- `internal/relay/registry.go:125-184` —
  `ScheduleReleaseServer` / `handleGraceExpiry` semantics. Critical:
  during grace, `PhonesFor` continues to return phones registered
  under the (now disconnected) server; on expiry, each phone's
  `Close()` is invoked by the registry. The forwarder is dead
  before grace expiry (its `Read` returned), so it never sees
  expiry-time state.
- `internal/relay/heartbeat.go` (whole file, 59 lines) — the
  heartbeat goroutine wired alongside the binary's WS conn. The
  forwarder runs synchronously on the HTTP handler goroutine in
  parallel with the heartbeat goroutine; both terminate cleanly via
  the existing `defer cancelHB()` + handler defer.
- `internal/relay/ws_conn.go:74-84` — `Read` method on `*WSConn`
  (already shipped in #25). The binary's `*WSConn` will satisfy the
  new `binarySource` interface via this same method.
- `docs/specs/architecture/25-phone-forwarder.md` (whole file) —
  prior-art spec. Same loop shape, same fake-substitution test
  strategy. Read it once; this spec elides anything #25 already
  motivated and only spells out what diverges.
- `docs/lessons.md` § "json.RawMessage round-trips are byte-stable
  modulo whitespace" (lines 37-39); § "Race-test count is a
  CI-runner knob" (lines 45-47); § "A long-lived WS handler that
  does not read frames will never observe peer close" (lines 13-15)
  — explains why the existing `CloseRead` call must be deleted, not
  retained alongside the new reader.
- `pyrycode/pyrycode/docs/protocol-mobile.md` § Routing envelope —
  the wire shape `{conn_id, frame}` and the rule that the relay
  treats `frame` as opaque bytes. Out-of-scope for inspection.

## Context

After `/v1/server` (#16, #21) claims the server-id slot and (#7)
launches the heartbeat goroutine, the handler currently parks on
`c.CloseRead(r.Context())` + `<-readCtx.Done()` so the WS processes
peer-side control frames but discards data frames. This ticket lands
the data path in the binary→phone direction: read each inbound
envelope, look up the addressed phone within `serverID`, and write
the inner frame to that phone.

The relay continues to treat inner frames as opaque bytes
(`json.RawMessage`); only the envelope's `conn_id` and `frame`
fields are inspected, exactly as #25's outbound path inspects only
`conn_id` (it constructs the envelope) and the inbound phone-frame
is wrapped without inspection.

Stateless relay; per-frame `PhonesFor` lookup picks up phone
registration changes transparently (a phone that just connected
between envelope #N and #N+1 becomes addressable on #N+1).

## Design

### Edit: `internal/relay/forward.go`

Add one type and one function. The type is unexported (the
"interface at the consumer" pattern adopted in #25). The function is
exported for `server_endpoint.go` to call.

```go
// binarySource is the read-side contract the forwarder needs from a
// binary connection. Defined at the consumer, not on WSConn, so tests
// can substitute a fake. Production passes *WSConn; concurrent Read
// callers are NOT supported (matches WSConn.Read's contract).
//
// Structurally identical to phoneSource; the distinct named type
// documents the call site's intent and keeps the two Start*Forwarder
// signatures self-describing.
type binarySource interface {
    ConnID() string
    Read(ctx context.Context) ([]byte, error)
}
```

```go
// StartBinaryForwarder runs the per-binary read pump synchronously:
// it reads envelopes from binary, unwraps each, finds the phone
// registered under serverID whose ConnID equals env.ConnID, and
// writes env.Frame (the verbatim inner bytes) to that phone.
//
// Returns when binary.Read errors or ctx is cancelled. Does NOT
// return on per-frame errors: a malformed envelope, an unknown
// conn_id, or a phone Send failure all log + drop + continue. A
// single bad frame from the binary MUST NOT tear the binary
// connection down — phones come and go, and an envelope addressing
// a just-disconnected phone is a normal race, not a binary fault.
//
// The caller's defer (in /v1/server) handles ScheduleReleaseServer,
// wsconn.Close, and the heartbeat cancel; the forwarder must NOT
// touch any of those. The relay treats inner frames as opaque
// bytes: only Unmarshal's structural checks inspect the envelope,
// never env.Frame.
//
// Despite the Start verb, the call is synchronous; the verb matches
// the AC and mirrors StartPhoneForwarder.
func StartBinaryForwarder(
    ctx context.Context,
    reg *Registry,
    serverID string,
    binary binarySource,
    logger *slog.Logger,
) error
```

### Loop body

```
for {
    wrapped, err := binary.Read(ctx)
    if err != nil {
        // ctx cancellation, peer close, library error — all funnel
        // here. Log at info; the handler's server_released log
        // closes out the lifecycle.
        logger.Info("binary_forwarder_read_end",
            "server_id", serverID,
            "binary_conn_id", binary.ConnID(),
            "err", err)
        return err
    }

    env, err := Unmarshal(wrapped)
    if err != nil {
        // Adversarial / buggy binary: malformed envelope, missing
        // conn_id, or missing frame. Drop the frame, keep serving
        // the rest of the conn. The binary owns its own protocol
        // health; the relay does not punish it for one bad frame.
        logger.Warn("binary_forwarder_unmarshal_err",
            "server_id", serverID,
            "binary_conn_id", binary.ConnID(),
            "err", err)
        continue
    }

    // PhonesFor returns a fresh snapshot — safe to iterate without
    // holding any registry lock. O(N) in registered phones; current
    // scale (handful per server) makes this fine.
    var phone Conn
    for _, p := range reg.PhonesFor(serverID) {
        if p.ConnID() == env.ConnID {
            phone = p
            break
        }
    }
    if phone == nil {
        // Unknown conn_id: phone disconnected between the binary's
        // last observation and this envelope, or the binary
        // addressed a phone it shouldn't know about. Either way, a
        // normal race or a binary bug — drop, continue.
        logger.Warn("binary_forwarder_unknown_conn_id",
            "server_id", serverID,
            "conn_id", env.ConnID)
        continue
    }

    if err := phone.Send(env.Frame); err != nil {
        // Phone is wedged or already closed. The phone's own
        // /v1/client handler will see its read fail (or has
        // already) and run its UnregisterPhone+Close defer. Drop
        // this frame; keep serving other phones on this binary.
        // DIVERGES from StartPhoneForwarder, which returns on
        // Send error: there, the only sink is the binary, so a
        // failing Send means the conn is dead. Here, we have N
        // sinks and one bad sink does not end the loop.
        logger.Info("binary_forwarder_phone_send_failed",
            "server_id", serverID,
            "conn_id", env.ConnID,
            "err", err)
        continue
    }
}
```

`env.Frame` is `json.RawMessage`, i.e. `[]byte`. It passes verbatim
to `phone.Send` (which writes a single binary WS frame). The relay
neither parses nor canonicalises the inner bytes; tests assert
byte-stability modulo whitespace via `json.Compact` (see § Testing
strategy).

### Edit: `internal/relay/server_endpoint.go`

Replace the placeholder block (lines 109-110) with a
`StartBinaryForwarder` call:

```go
// Before:
readCtx := c.CloseRead(r.Context())
<-readCtx.Done()

// After:
_ = StartBinaryForwarder(r.Context(), reg, serverID, wsconn, logger)
```

Drop the `CloseRead` call entirely. The new read loop processes
control frames inline with data reads, so the drain-and-discard
goroutine is no longer needed and would race the
`*websocket.Conn`-level sole-reader contract if retained
(`docs/lessons.md` lines 13-15; `WSConn.Read` doc).

The existing `defer { ScheduleReleaseServer; Close; log }` (lines
87-91) and `defer cancelHB()` (line 100) run in LIFO order on
forwarder return: `cancelHB` first (heartbeat goroutine observes ctx
cancel and exits without touching the conn), then the release defer
(`ScheduleReleaseServer` + `wsconn.Close()`, idempotent). The
forwarder MUST NOT call `ScheduleReleaseServer`, `wsconn.Close()`,
or `cancelHB` itself.

The return value is discarded (`_ =`). The handler's
`server_released` log already terminates the lifecycle from the
HTTP side; the forwarder's own logs cover the data path.

The heartbeat-related comment at lines 103-110 is replaced by a
short comment that names the forwarder and references this spec, in
the same style as the equivalent comment in `client_endpoint.go`
lines 92-98.

## Concurrency model

- One forwarder goroutine per binary (the HTTP handler goroutine
  itself; no extra goroutine spawned). Runs in parallel with the
  heartbeat goroutine on a sibling goroutine; the two never share
  state.
- `binary.Read` is single-caller (forwarder).
- `phone.Send` is multi-caller across the system: every binary's
  forwarder writing to a given phone, plus any future server-side
  signal path. `WSConn.writeMu` (#15) serialises them. No new locks
  added by this ticket.
- `reg.PhonesFor` takes RLock and returns a fresh snapshot; cheap,
  contention-free, and the snapshot is iterated without holding any
  registry lock — `phone.Send` and `phone.ConnID` are called
  outside the registry lock. Matches the established passive-store
  pattern (`docs/lessons.md` line 49-51).
- Shutdown paths:
  1. **Binary closes WS:** `c.Read` returns close error → loop
     returns → handler defer runs (`cancelHB` → `ScheduleReleaseServer`
     → `wsconn.Close`).
  2. **Server shutdown / request cancel:** `ctx` cancels → `c.Read`
     returns ctx error → loop returns.
  3. **Heartbeat-driven close (1011):** heartbeat goroutine calls
     `wsconn.CloseWithCode(1011, "heartbeat timeout")` → underlying
     `*websocket.Conn` aborts in-flight `Read` → forwarder returns
     via Read error.
  4. **Per-frame errors:** malformed envelope, unknown conn_id,
     phone Send error — log + `continue`. Loop is not terminated;
     the binary continues serving other phones / other frames.
- No goroutine leaks: all binary-fault termination paths terminate
  the single forwarder goroutine, and the handler's defer cleans up
  registry state.

## Error handling

| Cause                                              | Action                            | Log level |
|----------------------------------------------------|-----------------------------------|-----------|
| `binary.Read` error (any)                          | log, return err                   | info      |
| `Unmarshal` error (any sentinel; checked structurally) | log, **continue**             | warn      |
| `PhonesFor` snapshot lacks `env.ConnID`            | log, **continue**                 | warn      |
| `phone.Send` error                                 | log, **continue**                 | info      |

The three "continue" branches are the structural divergence from
`StartPhoneForwarder`. Per the AC: "A single bad frame from the
binary MUST NOT tear the binary connection down."

The forwarder does not branch on specific `Unmarshal` sentinels —
all four (`ErrMalformedEnvelope`, `ErrMissingConnID`,
`ErrMissingFrame`, plus a defensive catch-all) get the same
warn-and-continue treatment. Branching adds no behaviour and risks
divergence if envelope errors are added later.

## Testing strategy

`internal/relay/forward_test.go` (extending the existing file). All
tests use mocks against a real `Registry`; no httptest server. The
existing `compactJSON`, `discardLogger`, and `waitForSent` helpers
are reused.

### Two new test fakes

1. **`fakeBinarySource`** — implements `binarySource`. Holds an
   `id` and a `frames chan []byte`. `Read` selects on `ctx.Done()`
   and on `frames`; on closed frames-chan, returns `io.EOF`.
   Mirrors `fakePhone`'s shape; ~15 LOC.

2. **A phone fake that captures `Send`**. Two reasonable approaches
   — developer picks one:

   - **(a)** Add `mu sync.Mutex` + `sent [][]byte` + `sendErr error`
     fields to the existing `fakePhone`, and give it a `Send`
     method that captures (mu-protected). This makes `fakePhone`
     directly satisfy `Conn`, removing the need for `registryConn`
     in the new tests. Existing #25 tests are unaffected (they use
     `&registryConn{phone}` and never inspect `fakePhone.sent`).

   - **(b)** Introduce a parallel `fakePhoneSink` type with the
     same shape as `fakeBinary` (id, mu, sent, sendErr, snapshot).

   Approach (a) is preferred — reduces type count and matches the
   "one fake per role" feel of the existing file. Approach (b) is
   acceptable if the developer wants strict separation between
   "source-only" and "sink-only" fakes for symmetry with #25.

   Either way, a `waitForSent`-shape helper is needed for the
   phone-side sink. The existing `waitForSent(t, *fakeBinary, ...)`
   takes `*fakeBinary` concretely; either generalise it or
   duplicate it for the new type. Either is fine — ~10 LOC.

### Test cases (7 total)

1. **`TestStartBinaryForwarder_RoutesToAddressedPhone`.** Claim a
   `fakeBinarySource` for server-id `"s1"` (via `ClaimServer` —
   wrap as `Conn` similarly to how #25 wraps `fakePhone`, since the
   binary needs to be in the registry for `BinaryFor` lookups by
   anyone, though this forwarder doesn't use `BinaryFor`; in
   practice the registry simply needs the slot present so
   `RegisterPhone` succeeds). Register two phones P1
   (`"client-s1-aaaa1111"`) and P2 (`"client-s1-bbbb2222"`) via
   `RegisterPhone`. Run `StartBinaryForwarder` in a goroutine.
   Build an envelope addressed to P1 with inner frame
   `{"type":"hello","x":[1,2,3]}` via `Marshal`. Push it onto
   `fakeBinarySource.frames`. Wait until P1.sent has 1 entry; assert
   P2.sent is empty; assert P1.sent[0] is byte-equal modulo
   whitespace to the inner frame via `compactJSON`.

2. **`TestStartBinaryForwarder_MultiplePhones`.** Same setup. Push
   one envelope addressed to P1 and one addressed to P2. Wait for
   each to receive exactly one frame; assert the inner bytes match.

3. **`TestStartBinaryForwarder_UnknownConnID_DropsAndContinues`.**
   Same setup. Push envelope #1 addressed to a bogus
   `"client-s1-deadbeef"`. Push envelope #2 addressed to P1.
   Assert P1 receives exactly one frame (envelope #2's inner) and
   P2 receives nothing. Assert the forwarder is still running by
   then closing the binary's frames chan and observing the
   forwarder return with `io.EOF` on its done chan.

4. **`TestStartBinaryForwarder_MalformedEnvelope_DropsAndContinues`.**
   Push raw bytes that fail `Unmarshal`: `[]byte("not-json")` (or
   `[]byte("{}")` to trigger `ErrMissingConnID`, or
   `[]byte(\`{"conn_id":"x"}\`)` for `ErrMissingFrame` — pick one;
   the AC says "drop, continue" for all). Push a valid envelope
   addressed to P1 immediately after. Assert P1 receives the
   second envelope's inner frame; the first never reaches any
   phone. Confirm continuation by closing frames and observing
   `io.EOF` return.

5. **`TestStartBinaryForwarder_PhoneSendError_DropsAndContinues`.**
   Configure P1 with a non-nil `sendErr`. Push envelope to P1
   (asserted to be dropped via `len(P1.sent)==0`), then push
   envelope to P2 (asserted to land). Forwarder is still running:
   close the binary frames chan; observe `io.EOF` return. Asserts
   the divergence from `StartPhoneForwarder`'s
   return-on-Send-error behaviour.

6. **`TestStartBinaryForwarder_BinaryDisconnect_Returns`.** Push
   one envelope to P1 (assert it lands). Close
   `fakeBinarySource.frames` → `Read` returns `io.EOF` → forwarder
   returns. From the test goroutine, mimic the handler defer by
   calling `reg.ScheduleReleaseServer("s1", 0)` (zero grace for
   instant expiry) and assert the server slot becomes free —
   verifies the AC bullet "the handler-level `defer` runs
   `ScheduleReleaseServer` as expected" structurally.

7. **`TestStartBinaryForwarder_ContextCancellation_Returns`.** Same
   setup as test 1 but DON'T push frames. Cancel the parent ctx.
   Forwarder's `Read` returns ctx.Err → loop returns. Assert
   return within 100 ms (generous bound under `-race`).

`make test` clean with `-race`. The package-level doc comment for
`forward_test.go` already documents the `go test -race -count=20
./internal/relay/` invocation per the race-count lesson; no edit
needed there.

## Open questions

None blocking. Two judgement calls the developer will hit:

1. **`binarySource` vs reusing `phoneSource`.** Spec defines a
   distinct named type — three lines of code, plus a descriptive
   doc comment. The two interfaces are structurally identical;
   reusing `phoneSource` would compile fine. The named-type choice
   is for documentation: the call sites read `phone phoneSource`
   vs `binary binarySource` rather than both saying `phoneSource`.
   If the developer prefers reuse, that's acceptable — argue the
   call.

2. **Test-fake reuse strategy.** § Testing strategy describes two
   approaches for a phone-with-Send-capture fake (extend
   `fakePhone` vs introduce a parallel `fakePhoneSink`). Approach
   (a) is preferred but (b) is fine. Either keeps the file at ~140
   LOC of new tests as the AC sized.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings — the binary is the source of
  envelopes here. The single explicit boundary is `Unmarshal` in
  `envelope.go:67`; all four sentinel returns funnel through the
  warn-and-continue branch. `env.Frame` is forwarded as opaque
  bytes — `json.RawMessage` makes it hard to accidentally inspect.
  The downstream phone's protocol layer already knows it receives
  untrusted-via-relay bytes (the binary is trusted relative to the
  phone, not relative to the relay).
- **[Cross-server addressing]** No findings — `PhonesFor(serverID)`
  scopes the lookup to the binary's own server-id slot. A binary
  cannot address phones registered under a different server-id
  even if it forges an `env.ConnID` collision: the iteration only
  considers phones it owns. This is a structural defence, not a
  runtime check, and follows from the registry's per-server-id map
  shape established in #3.
- **[Tokens, secrets, credentials]** No findings — the forwarder
  never reads or constructs `X-Pyrycode-Server`, headers, or any
  token. The binary handshake (#16) already happened. No log call
  site in this spec includes any header value; enumerated log
  fields are `event`, `server_id`, `binary_conn_id`, `conn_id`,
  `err` (a library or sentinel error, not a request payload).
- **[File operations]** N/A — no filesystem access.
- **[Subprocess execution]** N/A — no subprocess.
- **[Cryptographic primitives]** N/A — no cryptographic operations
  in this ticket.
- **[Network & I/O]** SHOULD FIX (deferred) — `c.Read(ctx)` has no
  per-message size cap. A malicious binary can send arbitrarily
  large envelopes to exhaust memory in `Unmarshal` and on the
  phone's write buffers. **Out of scope for #26** — same
  unbounded-read posture inherited from #15 / #25; the per-frame
  size cap belongs on `WSConn.SetReadLimit` (the right hook on
  `*websocket.Conn`) so it covers both `/v1/server` and
  `/v1/client`. Should be a follow-up ticket, not a regression.
  Default nhooyr read limit is 32 MiB which provides a soft floor.
  Same finding as the #25 spec; not addressed here.
- **[Network & I/O]** No finding on `PhonesFor` linear scan — O(N)
  per envelope where N is phones registered for this server-id.
  Current scale (handful of phones per server, low envelope rate
  per phone) makes this comfortably bounded; the alternative
  (per-server-id `map[connID]Conn`) is a registry-shape change
  that should be motivated by observed cost, not anticipated.
- **[Network & I/O]** No finding on backpressure — a slow phone
  blocks only its own `Send` call; concurrent phones for the same
  binary are not affected because the binary forwarder iterates
  one envelope at a time and each `Send` is independent. The
  `WSConn.Send` 10 s deadline (#15) bounds any single write, so a
  wedged phone causes the forwarder to log `binary_forwarder_phone_send_failed`
  within 10 s and continue. A high-rate stream of envelopes
  destined for a single wedged phone would still block the
  binary's loop for up to 10 s per envelope; if this becomes a
  real failure mode, a follow-up ticket can introduce per-phone
  send queues. Not observed yet, deferred.
- **[Error messages, logs, telemetry]** No findings — the four log
  call sites enumerate fields explicitly. No envelope bytes, no
  frame bytes, no headers, no tokens enter logs. `err` carries
  library errors and `Unmarshal` sentinels — none of which embed
  user payloads (the `Unmarshal` wrap in `envelope.go:70` includes
  the JSON decoder error, which can name a byte offset but not
  payload contents). No telemetry / metrics added.
- **[Concurrency]** No findings — single forwarder goroutine per
  binary; no new locks; per-frame `PhonesFor` snapshot is
  RLock-only and does not nest with any caller-held lock; the
  iteration-after-snapshot pattern matches #3's established
  passive-store contract. Goroutine lifecycle: the four
  termination paths in § Concurrency model cover ctx cancel,
  binary close, heartbeat-driven close, and server shutdown.
  Per-frame errors do not terminate. No leaks.
- **[Threat model alignment]** No findings — the relay's role per
  `protocol-mobile.md` § Routing envelope is "wrap, address,
  forward; never inspect". This spec preserves that:
  `Unmarshal` is structural (envelope shape only), not semantic
  (inner-frame contents); the binary owns inner-frame
  construction and the phone owns inner-frame interpretation.
  Out-of-scope items (per-message size cap, per-phone send
  queueing) are named above with deferral rationale.

**Reviewer:** architect (self-review per
`pyrycode-relay-agents/architect/security-review.md`)
**Date:** 2026-05-10
