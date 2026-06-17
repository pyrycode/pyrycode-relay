# Spec: `WSConn.Send` writes WebSocket text frames, not binary (#108)

Ticket: [#108](https://github.com/pyrycode/pyrycode-relay/issues/108). Size **XS**
(`size:xs`, `security-sensitive`). Single-literal production change at the one
outbound frame-write in the relay, plus one focused regression test. No new
types, no new files, no consumer cascade.

## Files to read first

- `internal/relay/ws_conn.go:71-82` — `Send`: the doc comment (71-75) and the
  single `w.conn.Write(ctx, websocket.MessageBinary, msg)` (81). **This is the
  entire production change.** `MessageText` lives in the same already-imported
  `github.com/coder/websocket` package; no import edit.
- `internal/relay/ws_conn.go:84-94` — `Read`: confirm the message type is
  already discarded (`_, data, err := w.conn.Read(ctx)`). Inbound is unaffected;
  the comment "binary vs text … is discarded" stays accurate. **Do not touch.**
- `internal/relay/ws_conn_test.go:17-63` — `startEcho` helper. Reuse it as the
  structural template for the new test, but **do not modify it**: it is the
  helper for 6 existing tests and its server handler intentionally discards the
  message type (`_, data, err := c.Read(...)`, line 37). The new test needs to
  *capture* the type, so it stands up its own peer rather than threading a type
  channel through `startEcho` (which would cascade into all 6 call sites).
- `internal/relay/ws_conn_test.go:65-117` — existing test shapes
  (`httptest.NewServer` → `websocket.Accept` → dial → `NewWSConn` → assert).
  Mirror this idiom; same package (`package relay`), same imports.
- `internal/relay/forward.go:66` (binary leg) and `internal/relay/forward.go:154`
  (phone leg) — the two and only callers of `Send`. Both peers expect text;
  read them only to confirm the change is direction-consistent. No edits here.
- `docs/knowledge/features/ws-conn-adapter.md:80-90` — the adapter's existing
  "Security model" section and the prior `security-sensitive` PASS verdict for
  `Send`. Context for the security review below; **read-only** (documentation
  phase owns this file; see § Hand-off).
- `pyrycode/pyrycode/docs/protocol-mobile.md:302` (external, in the `pyrycode`
  repo) — *"Encoding: line-delimited JSON over WS text frames … UTF-8."* The
  authoritative requirement the new doc comment must cite. Already referenced in
  sibling specs (`docs/specs/architecture/25-phone-forwarder.md:51`,
  `26-binary-forwarder.md:79`).

## Context

`WSConn.Send` writes every routed frame with the WebSocket **binary** opcode
(`internal/relay/ws_conn.go:81`). Every other component on the wire uses
**text**: the mobile client (`OkHttpRelayTransport`) closes with
`StatusUnsupportedData` on any binary frame; the daemon transport
(`internal/transport/wssclient.go`) writes text; the Go test doubles
(`fakerelay`, `fakephone`) use text. Because the fakes also use text, the
fakerelay-based wire tests stay green while the real relay→real-phone path is
broken — no test ever exercised the relay's actual outbound opcode.

Observed live (emulator e2e, rung 3): `phone_registered` then immediately
`phone_forwarder_read_end err="unexpected binary frame"`, repeated on retry.
Switching the literal to `websocket.MessageText` made the session stable.

The wire is **always text**: the relay forwards the routing envelope as UTF-8
JSON with ciphertext base64-encoded inside the `data` field
(`protocol-mobile.md:195`); the relay never sees raw ciphertext on the wire. The
original report's "Decision needed" resolves to text with no genuine choice and
no spec change required. `Send` is shared by both legs (phone, binary) and both
peers expect text, so the single literal change is consistent across both.

## Design

Two edits to one production file; one new test function.

### Production — `internal/relay/ws_conn.go`

1. **Line 81 — the opcode.** Change the message-type argument:

   - before: `return w.conn.Write(ctx, websocket.MessageBinary, msg)`
   - after:  `return w.conn.Write(ctx, websocket.MessageText, msg)`

   Nothing else in `Write`'s call changes. `MessageText` is exported from the
   same `github.com/coder/websocket` import already present (line 8).

2. **Doc comment (lines 71-75) — the contract.** Rewrite the first sentence so it
   no longer says "binary." It must state that `Send` writes a single **text**
   WebSocket frame, and cite the wire-spec text-frame requirement
   (`pyrycode/pyrycode/docs/protocol-mobile.md` § Encoding — line-delimited JSON
   over WS text frames, UTF-8). Keep the existing serialization /
   write-deadline / "non-nil return ⇒ drop the connection" sentences unchanged —
   only the framing word and the added spec reference change.

No other production file changes. Grep confirmed `ws_conn.go:81` is the **only**
outbound WS frame write in production (`healthz.go:55` is an HTTP body write, not
a WS frame).

### Test — `internal/relay/ws_conn_test.go`

Add one self-contained test, `TestWSConn_Send_UsesTextOpcode`, in `package
relay`. It is the regression guard the text-using fakes structurally cannot
provide (they never observe a real opcode). It must stand up its own peer that
captures the observed `websocket.MessageType`, because `startEcho` deliberately
discards it and is shared by 6 other tests.

Shape (mirror `startEcho`, lines 26-62, but capture the type):

- `httptest.NewServer` with a handler that: `websocket.Accept(w, r, nil)`; reads
  one frame as `typ, _, err := c.Read(r.Context())`; sends `typ` onto a
  buffered `chan websocket.MessageType` (cap 1); then closes. (Discard the
  payload — the opcode is the whole point.)
- Dial the server, wrap with `NewWSConn(client, "test-conn-id", 256*1024)`
  (same cap constant the sibling tests use), `defer` cleanup.
- Call `wc.Send([]byte("envelope"))` once; assert non-nil error is *not*
  returned.
- Receive the captured type with a timeout (`time.After`, ~2s, matching the
  existing tests' timeout idiom); on timeout, `t.Fatal`.

Assertions (the regression contract):

- The captured type **equals** `websocket.MessageText`.
- The captured type **is not** `websocket.MessageBinary` — assert this
  explicitly so the failure message names the regression directly if the literal
  ever flips back.

**Direction note for the developer:** in production the relay is the server side
and `Send` goes relay→peer; in this test the `WSConn` wraps the *dialed client*
and `Send` goes client→server. The opcode is identical regardless of side —
`Conn.Write(ctx, opcode, msg)` stamps the same frame opcode either way — so the
client-side direction faithfully exercises the same `Send` code path and the
same opcode. This matches how every existing `ws_conn_test.go` test already
drives `Send` from the client side.

**Leave the existing tests untouched.** `startEcho`'s handler echoes with
`MessageBinary` (line 42); since `Read` discards the inbound type, that echo
opcode is irrelevant to production behaviour and the cap/round-trip tests stay
correct. Do not "fix" it — that would be unrelated churn.

## Concurrency model

Unchanged. `Send` keeps its existing `writeMu` serialization, per-call
`context.WithTimeout(closeCtx, writeTimeout)`, and `closeOnce` cancellation. The
opcode argument has no bearing on locking, context, or goroutine lifecycle. No
new goroutines; the new test spawns only the standard `httptest` server handler
goroutine, which exits when the connection closes (cleanup `defer`).

## Error handling

Unchanged. `Send`'s error contract ("any non-nil return ⇒ drop the connection")
is identical for text and binary opcodes — the same library `Write` path, the
same wrapped cancellation/timeout errors. `SendAfterClose` and the write-deadline
behaviour are unaffected; the existing `TestWSConn_SendAfterClose_ReturnsError`
and `TestWSConn_ConcurrentSend_ProducesIntactFrames` continue to assert them.

## Testing strategy

- New `TestWSConn_Send_UsesTextOpcode` (above) — pins the outbound opcode at the
  `Send` boundary. This is the structural gap the fakes leave open.
- Existing `ws_conn_test.go` suite — must stay green unchanged; in particular the
  cap round-trip tests prove `Read` is opcode-agnostic.
- `make vet`, `make test` (`-race`), `make build` clean (AC 4).
- End-to-end "real phone stays connected" verification lives in the mobile repo's
  emulator e2e, not here — out of scope for this repo's tests by design.

## Acceptance criteria mapping

1. `WSConn.Send` writes the **text** opcode (`websocket.MessageText`) in
   `internal/relay/ws_conn.go` → Design § Production, edit 1.
2. A relay-package test connects a WS peer, calls `Send`, asserts the peer
   observes a **text** message type → `TestWSConn_Send_UsesTextOpcode`.
3. The `Send` doc comment no longer says "binary"; states text framing and
   references the `protocol-mobile.md` text-frame requirement → Design §
   Production, edit 2.
4. `make vet`, `make test` (`-race`), `make build` clean → Testing strategy.

## Open questions

None. The direction is settled by the authoritative wire spec; the change is a
single literal plus its doc comment and one regression test.

## Hand-off (not developer ACs)

- `docs/knowledge/features/ws-conn-adapter.md` still describes the adapter's
  framing in binary terms in places and records the prior `security-sensitive`
  PASS. Reconciling that wording is the **documentation phase's** job, post-merge
  — not a developer deliverable. Do not edit it in the feature branch.
- The relay-side fakes (`fakerelay`/`fakephone`) already use text, so no fixture
  realignment is needed; the new opcode test is the deterministic guard that the
  fakes cannot be.

## Security review

**Verdict:** PASS

The change is a single outbound-opcode literal (`MessageBinary` → `MessageText`)
plus a doc-comment edit and one regression test. The adversarial question for
each category: *what can a hostile peer (or buggy caller) trigger that this
opcode change introduces or fails to contain?*

**Findings:**

- **[Trust boundaries]** No findings. The opcode change does not alter what
  data crosses any boundary — `Send(msg []byte)`'s payload is byte-identical;
  only the WS frame's opcode byte differs. The untrusted *inbound* boundary
  (`Read` + the `SetReadLimit` per-frame cap in `NewWSConn`) is untouched, and
  `Read` still discards the inbound message type. No downstream caller's trust
  posture changes.
- **[Network & I/O — UTF-8 / text-frame semantics]** The one opcode-specific
  risk: RFC 6455 text frames are expected to carry valid UTF-8, whereas binary
  frames carry arbitrary bytes. Could a peer make the relay forward
  non-UTF-8 bytes as a text frame? **No — not reachable.** Both `Send` inputs are
  relay-validated UTF-8 JSON: the phone leg forwards `env.Frame`
  (`forward.go:154`), a `json.RawMessage` that passed the envelope boundary's
  JSON well-formedness check (JSON is UTF-8 by spec); the binary leg forwards a
  relay-`json.Marshal`-constructed envelope (`forward.go:66`), guaranteed valid
  UTF-8. No path forwards arbitrary attacker bytes through `Send`. Even in the
  hypothetical where the library rejected a malformed text Write, `Send`'s
  contract is "non-nil ⇒ drop this one connection" — a single, contained
  connection drop on a stateless relay, not a relay-wide effect. No MUST/SHOULD
  fix; documented here as the explicitly-considered opcode-specific case.
- **[Network & I/O — DoS posture]** No findings. The slow-loris guard
  (`writeTimeout = 10s` bounding one `Send`) and the inbound frame-size cap are
  both unchanged. No new read path, header, or timeout surface is introduced.
- **[Tokens / Crypto / File ops / Subprocess]** Not applicable — this change
  touches none of these. `Send` carries opaque forwarded bytes; the relay does no
  crypto (it forwards base64-in-JSON ciphertext blind), no file I/O, and no
  subprocess execution. `X-Pyrycode-Token` handling lives elsewhere and is
  unchanged.
- **[Error messages / logs / telemetry]** No findings. The change adds no log
  call and no error string; `Send` still returns the library's wrapped error and
  the forwarders' existing per-frame logging is untouched. No payload or
  credential reaches a log.
- **[Concurrency]** No findings. `writeMu` serialization, the per-call
  `WithTimeout(closeCtx, writeTimeout)`, and the `closeOnce` cancellation are
  unchanged — the opcode is a pure argument value. The new test spawns only the
  standard `httptest` handler goroutine, which exits on connection close.
- **[Threat-model alignment]** No findings — the change *reduces* divergence from
  `pyrycode/pyrycode/docs/protocol-mobile.md` § Encoding (line 302: text frames),
  bringing the sole non-conformant component into spec. The supply-chain risk the
  threat model already names for the WS library (a malicious release seeing
  cleartext frames) is identical — same library, same `Write` call, different
  opcode constant. The prior `security-sensitive` PASS for `Send` (feature doc
  `ws-conn-adapter.md:90`) still holds; this adds nothing to the threat surface.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-06-17
