# #113 — Per-phone delivery queue: one slow phone can't head-of-line-block the fan-out

## Files read

- `internal/relay/forward.go` → `StartBinaryForwarder`, `closePhone`, `phoneCloser` — the serial loop that calls `phone.Send` synchronously; the only production caller of `Send` on a phone.
- `internal/relay/ws_conn.go` → `WSConn.Send`, `WSConn.CloseWithCode`, `writeTimeout` — Send is bounded by 10s; `CloseWithCode` cancels `closeCtx`, which aborts an in-flight Send.
- `internal/relay/client_endpoint.go` → `ClientHandler` — registers the phone Conn, owns its teardown defer (unregister, close), starts the heartbeat and `StartPhoneForwarder`.
- `internal/relay/registry.go` → `Conn`, `RegisterPhoneCapped`, `PhonesFor`, `evictPhones`, `closeWithCode`, `handleGraceExpiry` — the registry holds whatever Conn the handler registers; eviction and grace expiry close it via `gracefulCloser` / `Close`.
- `internal/relay/shutdown.go` → `gracefulCloser`, `Shutdown` — shutdown closes every registered Conn with 1001 through the `CloseWithCode` capability, so a wrapper must keep that method.
- `internal/relay/server_endpoint.go` → `conflictProbeTimeout` — its doc comment's justification changes with this ticket (value stays).
- `internal/relay/envelope.go` → `hasFrame` — a close directive's frame is optional.
- `internal/relay/heartbeat.go` → `runHeartbeat` — already closes with `websocket.StatusInternalError` (1011).
- `internal/relay/log_allowlist.go` → `allowedLogKeys` — no new key needed (see Design § Logging).
- `internal/relay/forward_test.go` → `fakePhone`, `fakeBinarySource`, `claimAndRegister`, `closeEnvelopeJSON`, `runBinaryForwarder` — the harness the new tests extend.
- `internal/relay/client_endpoint_test.go` → `startClient`, `seedBinary`, `dialWithClient`, `waitForPhones` — handler-level harness.
- `github.com/coder/websocket@v1.8.14` → `Conn.setupWriteTimeout`, `Conn.Close` — a write whose context expires closes the net.Conn; `Close` on an already-closed conn returns fast.
- `docs/knowledge/features/binary-forwarder.md` § Concurrency model, § What this forwarder deliberately does NOT do — the design notes this ticket supersedes (doc edit handed off).
- `docs/threat-model.md` § DoS resistance — the #140 pong-starvation residual this ticket removes; notes an attacker needs a daemon-accepted phone session to get sizeable frames queued.
- protocol-mobile.md § Error codes — `1011` Server error, direction "either". No new code.

No in-flight feature branch touches these files (`origin/feature/127` touches `registry.go` only, which this design does not edit).

## Context

`StartBinaryForwarder` delivers every envelope with a synchronous `phone.Send`. One phone that stops reading holds the loop for up to `writeTimeout` (10s) per frame, during which no other phone on the server-id gets anything and the binary's conn is not read — so its pongs are not processed either (issue #140, the #112 conflict-probe risk). A close directive's `CloseWithCode` also runs on that loop and can block on the close handshake.

The fix: each phone gets a bounded FIFO and its own writer goroutine. The binary forwarder only enqueues, which never blocks. No ADR needed — this is an internal concurrency change behind existing interfaces; the feature doc update covers it.

## Design

### New type: `phoneOutbox` (`internal/relay/phone_outbox.go`)

A phone Conn wrapped with a bounded delivery queue. It is what `ClientHandler` registers in the registry in place of the bare `*WSConn`.

```go
const phoneOutboxDepth = 16

var (
    ErrPhoneBacklogFull  = errors.New("relay: phone delivery backlog full")
    ErrPhoneOutboxClosed = errors.New("relay: phone outbox closed")
)

type phoneOutbox struct { /* conn Conn; items chan outboxItem (cap depth); done chan struct{}; doneOnce; serverID; logger; onWritten func() */ }

func newPhoneOutbox(conn Conn, serverID string, depth int, onWritten func(), logger *slog.Logger) *phoneOutbox

func (o *phoneOutbox) ConnID() string                              // delegates
func (o *phoneOutbox) Send(msg []byte) error                       // = Enqueue(msg, 0)
func (o *phoneOutbox) Close()                                      // delegates to conn.Close (immediate)
func (o *phoneOutbox) CloseWithCode(code websocket.StatusCode, reason string) // delegates via closeWithCode (immediate)
func (o *phoneOutbox) Enqueue(frame []byte, closeCode uint16) error // never blocks
func (o *phoneOutbox) run()                                        // drain loop; caller runs it on its own goroutine
func (o *phoneOutbox) stop()                                       // idempotent; releases run and refuses later Enqueue
```

`outboxItem` is `{frame []byte; closeCode uint16}` — `closeCode != 0` marks a close directive, frame optional.

`Close` / `CloseWithCode` stay immediate (not queued): eviction (4404), grace expiry and shutdown (1001) must cut the connection now, and they abort an in-flight write by cancelling `WSConn.closeCtx`. Keeping `CloseWithCode` on the wrapper is what preserves those codes — the registry's `closeWithCode` and `Shutdown` type-assert `gracefulCloser`, and a wrapper without the method would silently degrade them to 1000.

**Enqueue** (forwarder goroutine):
1. `done` closed → `ErrPhoneOutboxClosed`.
2. Non-blocking send to `items`. Success → nil.
3. Full → the phone can't keep up: mark done (once) and close the conn **asynchronously** (`go closeWithCode(conn, code, "phone backlog full")`, same shape as `evictPhones`, because a close can block on the handshake). `code` is 1011, except for an overflowing close directive, which closes with the directive's own code (the daemon's intent — e.g. an auth reject — is more informative to the phone than 1011; the pending frames are dropped either way). Returns `ErrPhoneBacklogFull`.

**run** (per-phone goroutine), per item, FIFO:
- Stop if `done` is closed (checked before each write, so a stopped outbox writes nothing more).
- If the item has a frame (`closeCode == 0` or `hasFrame`): `conn.Send`. On error: close the conn with 1011 (or the directive's code if the item is a close directive), mark done, return — the remaining items are dropped. On success: `onWritten()` (the `onBinaryForwarded` hook — so the metric counts only frames actually written).
- If `closeCode != 0`: mark done, `closePhone(conn, closeCode)`, log, return. Because run is FIFO and single-writer, the close takes effect only after this frame and every earlier one was written or dropped (AC2).

A write timeout needs no special case: `WSConn.Send` returns an error when `writeTimeout` expires, and the library has already closed the socket. The 1011 close frame is best-effort — on a timed-out conn it can't reach the wire, but `CloseWithCode` still runs through `closeOnce`, and the phone's own `Read` in `StartPhoneForwarder` fails, so the handler's existing defer unregisters it (AC3).

### Forwarder change (`forward.go`)

A consumer-defined capability, mirroring `phoneCloser`:

```go
type phoneQueue interface {
    Enqueue(frame []byte, closeCode uint16) error
}
```

In `StartBinaryForwarder`, after the phone lookup: if the Conn implements `phoneQueue`, call `Enqueue(env.Frame, env.CloseCode)`, log a non-nil error, and `continue`. Otherwise the existing synchronous path runs unchanged. Production registers only `*phoneOutbox`, so the synchronous path serves Conns with no queue (the existing unit fakes); it is the same optional-capability-with-fallback shape as `closePhone`. The forwarder never writes to a phone socket itself on the queued path.

### Handler wiring (`client_endpoint.go`)

- Construct `phone := newPhoneOutbox(wsconn, serverID, phoneOutboxDepth, reg.onBinaryForwarded, logger)` and register `phone` (not `wsconn`) with `RegisterPhoneCapped`. The stillborn-reject paths are unchanged (they close `c` directly; run was never started).
- After registration succeeds, start `run` on a goroutine that closes an `outboxDone` channel on exit.
- The existing teardown defer becomes: `UnregisterPhone` → `wsconn.Close()` (aborts any in-flight write) → `phone.stop()` → `<-outboxDone` → log. The wait makes AC4 structural: the handler does not return while its writer goroutine is alive.
- `runHeartbeat` and `StartPhoneForwarder` keep taking `wsconn`.

### Queue depth: 16 frames

Bound by count (ticket). Worst case is every slot holding a max-size frame: the relay's read cap is 256 KiB (`maxFrameBytes`, `main.go`), so one phone pins at most 16 × 256 KiB = **4 MiB**, and a full server-id (16 phones, `main.go`'s `maxPhones`) pins **64 MiB** — a quarter of the 256 MB machine. Realistic traffic (streamed deltas of ~1 KiB) keeps a full queue in tens of KiB. The queue only fills once the kernel send buffer is already full, i.e. the phone is already far behind, so 16 is headroom for a burst on a briefly stalled link, not steady state. Deeper (32 → 128 MiB per server-id) gives one hostile daemon half the machine; shallower (8) closes healthy phones on short cellular stalls. The value is one constant, easy to retune on evidence.

### Logging

No new keys. The drain loop reuses the forwarder's existing event names so operator greps keep working: `binary_forwarder_phone_send_failed`, `binary_forwarder_close_frame_send_failed`, `binary_forwarder_phone_closed` (keys `server_id`, `conn_id`, `close_code`, `err`). The forwarder logs a refused enqueue as `binary_forwarder_phone_enqueue_failed` (`server_id`, `conn_id`, `err`); `err` is one of the two sentinels.

### `conflictProbeTimeout` comment

Rewrite the justification: phone writes no longer run on the forwarder (#113), so a live incumbent's pong waits at most for the forwarder's current binary read and enqueue; 10s stays as a conservative ceiling for a slow network. Value unchanged.

## Concurrency model

- Per phone: one extra goroutine (`run`), started by `ClientHandler` after registration and awaited by its teardown defer. It is the only writer to the phone socket for routed frames. (`WSConn.writeMu` still serialises it against nothing else; the heartbeat pings go through the library's control-frame path.)
- `items` is written only by `Enqueue` (the server-id's binary forwarder) and read only by `run`. It is never closed — `done` signals stop — so a late `Enqueue` can never panic on a closed channel; an item that races past the `done` check sits in the buffer and is garbage-collected with the outbox.
- `done` is closed exactly once (`doneOnce`) by whichever comes first: `stop` (handler teardown), overflow in `Enqueue`, a write failure or a close directive in `run`.
- Exit paths for `run`: `done` closed while idle; `Send` returns (success is followed by the next select; failure returns). An in-flight `Send` is always bounded — by `writeTimeout`, or earlier when anyone closes the `WSConn` (`closeCtx` cancel).
- The overflow close goroutine exits when `CloseWithCode` returns (bounded by the library's close handshake timeouts), exactly as `evictPhones`'s goroutines do.
- No new locks. The registry lock is never held while calling into an outbox.

## Error handling

| Failure | Where | Outcome |
|---|---|---|
| Backlog at bound | `Enqueue` | done; async close 1011 (or directive code); `ErrPhoneBacklogFull`; forwarder logs, continues |
| Enqueue after stop/failure | `Enqueue` | `ErrPhoneOutboxClosed`; forwarder logs, continues |
| Write error or timeout | `run` | close 1011 (or directive code); done; pending items dropped; `run` returns |
| Close directive | `run` | frame (if any) written, then `closePhone(code)`; done; later items dropped |
| Phone disconnects | handler defer | unregister, close, stop, wait for `run` |

In every case the phone leaves the registry through the existing `ClientHandler` defer, triggered by its `StartPhoneForwarder` read failing once the socket is closed.

## Testing strategy

New `internal/relay/phone_outbox_test.go` (package `relay`), with a `stallPhone` fake whose `Send` blocks until released or closed:

- **Stalled phone does not block others (AC1).** Two outboxes on one server-id, one stalled mid-`Send`; the binary sends a frame to the stalled phone, then several to the healthy one; all healthy frames arrive while the stalled `Send` is still blocked, and the binary source's frames are all consumed.
- **Ordering and close-after-drain (AC2).** Gated phone; enqueue f1, f2, then close directive with f3 and code 4401; while gated, no close has happened; release; sent order is f1, f2, f3, then close code 4401 recorded.
- **Overflow closes with 1011 (AC3).** Depth 2, stalled phone: after the first item is in flight, fill the queue; the next `Enqueue` returns `ErrPhoneBacklogFull`, the phone is closed with 1011, `run` returns, later `Enqueue` returns `ErrPhoneOutboxClosed`.
- **Write failure closes with 1011 (AC3) and counts nothing.** `Send` error → closed with 1011, `run` returns, `onWritten` never called; the success-path tests assert one `onWritten` per written frame.
- **Stop releases everything (AC4).** Idle `run` returns after `stop`; `Enqueue` after `stop` returns immediately with `ErrPhoneOutboxClosed`; a `run` blocked in `Send` returns once the conn is closed.
- **Handler wiring (AC4).** `client_endpoint_test.go`: a phone dialled through `ClientHandler` is registered as a `phoneQueue`; a frame enqueued on it reaches the dialled socket; after the client closes, the registered outbox refuses `Enqueue` with `ErrPhoneOutboxClosed` (stop ran).

Existing `forward_test.go` tests keep running against the synchronous path through `fakePhone`; existing endpoint and shutdown tests cover eviction/shutdown codes through the wrapper's `CloseWithCode`. All under `go test -race ./internal/relay/...`.

## Open questions

- None blocking. Whether 16 is the right depth is an evidence question for production; it is a single constant.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/binary-forwarder.md`: replace the "No buffering, no bounded channels" note and the Backpressure / Slow-phone bullets (§ Concurrency model, § Adversarial framing) with the per-phone outbox: depth 16, overflow and write-failure close with 1011, close directive ordered behind earlier frames, the new `binary_forwarder_phone_enqueue_failed` event.
- `docs/security-followups.md`: mark "Decouple pong handling from the forwarder's phone sends (issue #140)" resolved by #113.
- `docs/threat-model.md` § DoS resistance: the #140 pong-starvation residual no longer holds; record the per-phone queue's worst-case memory (4 MiB per phone, 64 MiB per server-id).

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the outbox carries `env.Frame` as opaque bytes from `StartBinaryForwarder` to `WSConn.Send`; nothing in `phoneOutbox` reads, parses or logs it. The only inspection is the existing `hasFrame` presence check on a close directive. No header handling changes.
- [Tokens / files / subprocess / crypto] No findings — the design touches none of these; no credential reaches the queue, no file or process is created, no randomness is added.
- [Network & I/O — resource exhaustion] No findings for the per-connection bound: each queue is capped at `phoneOutboxDepth` frames of at most `maxFrameBytes`, so 4 MiB per phone and 64 MiB per server-id worst case (derived in Design § Queue depth). Only frames the binary addresses to a phone are queued; a phone cannot fill its own queue, and per the threat model a hostile phone gets only a small reject frame from an honest daemon.
- [Network & I/O — resource exhaustion] OUT OF SCOPE — the sum across server-ids is not bounded: a hostile operator of several daemons, each with 16 non-reading phones, can pin 64 MiB per server-id. This is the existing "connection-count caps (per-IP and global) are deferred" residual in `docs/threat-model.md` § DoS resistance; the documentation handoff records the new per-server-id figure there.
- [Network & I/O — goroutines] SHOULD FIX — `Enqueue`'s overflow branch must spawn its close goroutine only on the call that actually closes `done` (inside the `doneOnce` guard), so repeated overflows cannot multiply goroutines. The forwarder is the only enqueuer today, but `Send` is on the `Conn` interface. Implement in Phase B; verifier checks.
- [Network & I/O — amplification] No findings — delivery stays 1:1; one routed envelope produces at most one write and one close.
- [Errors / logs] No findings — no new log keys; `err` values are the two new sentinels or library errors, none carrying payload bytes. Close reasons on the wire are fixed strings ("phone backlog full", "phone write failed"), never an error string.
- [Concurrency] No findings — no new locks; `items` is never closed, so a late `Enqueue` cannot panic; `done` closes once. The handler's wait on `run` is bounded: an in-flight `Send` is aborted by `wsconn.Close` cancelling `closeCtx`, and a `run` inside a directive's `CloseWithCode` holds `closeOnce` for at most the library's close handshake timeouts. The overflow close goroutine may outlive the handler by that same bounded time, the accepted `evictPhones` pattern.
- [Concurrency — close codes] No findings — the wrapper keeps `CloseWithCode`, so eviction (4404) and shutdown (1001) through `closeWithCode` / `Shutdown`'s `gracefulCloser` assertion keep their codes; the endpoint and shutdown tests exercise those paths through the wrapper.
- [Threat model alignment] No findings — removes the #140 pong-starvation residual; no new dependency, endpoint or deploy change, so no re-review trigger trips. Nothing assumes a single replica beyond what the registry already does.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
