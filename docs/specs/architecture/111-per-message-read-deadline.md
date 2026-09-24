# #111 — Per-message read deadline on `WSConn.Read`

## Files read

- `internal/relay/ws_conn.go` → `WSConn`, `NewWSConn`, `Read`, `writeTimeout` — the only file that changes; `Read` is the single read primitive both forwarders use.
- `internal/relay/forward.go` → `StartPhoneForwarder`, `StartBinaryForwarder` — call `Read(ctx)` in a loop and treat any non-nil error as exit (log `*_read_end`, return). Unchanged: a deadline error takes the same path as a peer close.
- `internal/relay/heartbeat.go` → `runHeartbeat` — proves liveness, not progress. Unchanged.
- `internal/relay/ws_conn_test.go` → `startEcho` — the harness style the new tests mirror (real `coder/websocket` over `httptest`).
- `github.com/coder/websocket@v1.8.14` `read.go` → `Conn.Reader`, `Conn.prepareRead`, `msgReader.read`; `conn.go` → `setupReadTimeout` — the ctx passed to `Reader` is stored on the message reader and bounds every later header/payload read of that message, including continuation frames; `setupReadTimeout` uses `context.AfterFunc(ctx, c.close)` armed only while a read is in progress and cleared when it finishes. So a cancel after the message is fully read is inert, and a cancel mid-message closes the connection.
- `docs/knowledge/features/ws-conn-adapter.md` § *Concurrency model*, § *Context strategy* — `Read` is single-caller and does not join `closeCtx`; that stays true.

No in-flight feature branch touches `ws_conn.go` or its test.

## Change

`Read(ctx)` becomes: derive `msgCtx, cancel := context.WithCancel(ctx)` (deferred cancel); call `w.conn.Reader(msgCtx)` — the wait for the first data-frame header is bounded only by the caller's `ctx`, exactly as today, so idle connections are untouched; once `Reader` returns, arm `time.AfterFunc(w.readTimeout, cancel)` (stopped on return) and `io.ReadAll` the message. If the peer has not delivered the whole message (all fragments, all payload bytes) before the timer fires, the library closes the underlying connection and the read returns a non-nil error; the forwarder's existing exit and cleanup path runs.

This is the pattern the library's own `Reader` doc comment prescribes. The message type is still discarded; the payload is still returned as opaque bytes.

New file-level constant next to `writeTimeout`:

- `readMessageTimeout = 30 * time.Second` — bounds receipt of one complete message once its first frame header has arrived. 30 s over the 256 KiB `maxFrameBytes` cap is a floor of ~8.7 KB/s for a max-size message; real frames are far smaller, so a slow mobile link is not at risk.

New unexported field `readTimeout time.Duration` on `WSConn`, set to `readMessageTimeout` in `NewWSConn`. It exists so same-package tests can shorten the deadline on one connection without mutating package state (which would race across parallel tests). Production never writes it after construction.

No signature changes: `phoneSource`/`binarySource` and every caller of `Read` are unaffected. No new exported types, no new log keys, no new dependency.

Error value on deadline: `context.Canceled` (possibly wrapped). The forwarders don't branch on the error kind, so no sentinel is needed.

## Testing strategy

New tests in `internal/relay/ws_conn_test.go`, each against a real `coder/websocket` connection with `wc.readTimeout` shortened to ~300 ms. A small helper stands up an `httptest` server that hands its accepted `*websocket.Conn` to the test (the scripted peer) and returns a client-side `WSConn`.

- **Stalled fragmented message, peer answers pings (AC 1).** Peer opens `Conn.Writer` and writes part of a message (one non-fin data frame: header + part of the payload), never closes the writer, and runs a read loop so it answers pings. The test pings from the `WSConn` side during the stall and asserts the pong comes back (the heartbeat would not catch this peer), then asserts `Read` returns a non-nil error within deadline + slack (~2 s), and that a following `Send` fails (connection closed).
- **Stalled mid-frame dribble (AC 1, single-frame form).** Peer hand-rolls the handshake over a hijacked conn, writes a text frame header declaring N bytes, then dribbles one payload byte at intervals shorter than the deadline. `Read` must error within deadline + slack even though bytes keep arriving.
- **Idle longer than the deadline, then a complete message (AC 2).** Peer sleeps ~3× the deadline, then writes a complete message; `Read` returns it intact.

RED check: before the production change, the two stall tests hang to their outer timeout (the Read has no deadline); the idle test passes both before and after, which is what it pins.

## Documentation handoff (pending — documentation stage)

- `docs/threat-model.md` § *DoS resistance — connection floods, slow-loris, fork-bomb retry*: replace "per-frame deadlines (#15)" with: a per-message read deadline armed when a message starts (#111, `readMessageTimeout` 30 s), the write timeout on `Send` (#15), and the heartbeat for idle liveness (#7).
- `docs/security-followups.md` *Slowloris connection-count cap* item and `docs/knowledge/features/autocert-tls.md`: correct "per-frame deadlines (#15)" the same way.
- `docs/knowledge/features/ws-conn-adapter.md`: document `readMessageTimeout` next to `writeTimeout`, and update the `Read` row of the concurrency table (context is now `WithCancel(caller ctx)` plus a timer armed after `Reader` returns).

Operator follow-up: reaches production only on the next manual `flyctl deploy`.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — `Read` still returns the payload as opaque bytes from `io.ReadAll`; nothing inspects it. The only new input is time, not content.
- [Tokens, secrets, credentials] No findings — no header or credential is touched.
- [File operations] No findings — no filesystem access.
- [Subprocess] No findings — none spawned.
- [Crypto] No findings — no randomness or crypto.
- [Network & I/O] No findings on the change itself — it closes the slowloris gap `docs/threat-model.md` claimed was closed: the deadline covers the whole message including continuation frames, because `msgReader` keeps the `Reader` ctx for the message's lifetime. The size cap (`SetReadLimit(maxFrameBytes)` in `NewWSConn`) is unchanged and still applies. Checked the idle case: the timer is armed only after `Reader` returns (first data-frame header received), so control-frame traffic (pings/pongs) and silence never start it; a peer cannot make an idle phone look slow.
- [Network & I/O] Checked the race where the timer fires just after `io.ReadAll` completes: the library clears its `AfterFunc` when each read finishes, so a late cancel of an already-finished message is inert; the next `Read` gets a fresh ctx. The timer is stopped on return in any case.
- [Network & I/O] OUT OF SCOPE — a slow peer can still hold a connection for 30 s per message, and many such peers are bounded only by the per-IP upgrade rate limit; a global connection-count cap is the *Slowloris connection-count cap* item in `docs/security-followups.md`.
- [Errors, logs] No findings — no new log calls or keys; the deadline error surfaces through the existing `phone_forwarder_read_end` / `binary_forwarder_read_end` `err` field, whose value is a library/context error string with no payload bytes.
- [Concurrency] No findings — one `time.AfterFunc` per message, stopped on return; its callback only cancels a context. `Read` stays single-caller; no new lock. Cancelling closes the conn via the library, the same path `CloseWithCode` uses; `closeOnce` is not involved, so a later `Close` from the forwarder's cleanup still runs once and is harmless on an already-closed conn.
- [Threat model alignment] No trigger for re-review tripped (no dependency, endpoint or deploy change). The threat-model wording fix is in the Documentation handoff.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
