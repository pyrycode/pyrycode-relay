# Per-phone delivery queue (`phoneOutbox`)

Each phone connection is wrapped in a bounded FIFO queue with its own writer
goroutine, so [`StartBinaryForwarder`](binary-forwarder.md) never waits on a
phone's socket. Before this, the forwarder called `phone.Send` synchronously;
a phone that stopped reading held the binary's read loop — and with it every
other phone on the same server-id, and the binary's own pong processing — for
up to `writeTimeout` (10s) per frame (issue #140). `ClientHandler` now
registers a `*phoneOutbox` in place of the bare `*WSConn`, and the forwarder
only enqueues.

## API

Package `internal/relay` (`phone_outbox.go`):

```go
const phoneOutboxBudget = 4 << 20 // 4 MiB
const outboxItemOverhead = 64

var (
    ErrPhoneBacklogFull  = errors.New("relay: phone delivery backlog full")
    ErrPhoneOutboxClosed = errors.New("relay: phone outbox closed")
)

type phoneOutbox struct { /* conn Conn; mu sync.Mutex; queue []outboxItem; queued int; budget int; ready chan struct{}; done chan struct{}; doneOnce sync.Once; closeRequested atomic.Bool; serverID string; logger *slog.Logger; onWritten func() */ }

func newPhoneOutbox(conn Conn, serverID string, budget int, onWritten func(), logger *slog.Logger) *phoneOutbox

func itemCost(frame []byte) int // len(frame) + outboxItemOverhead

func (o *phoneOutbox) ConnID() string
func (o *phoneOutbox) Send(msg []byte) error                                  // = Enqueue(msg, 0)
func (o *phoneOutbox) Close()                                                 // delegates immediately
func (o *phoneOutbox) CloseWithCode(code websocket.StatusCode, reason string) // delegates immediately
func (o *phoneOutbox) Enqueue(frame []byte, closeCode uint16) error           // never blocks
func (o *phoneOutbox) run()                                                   // drain loop; caller's own goroutine
func (o *phoneOutbox) stop()                                                  // idempotent
func (o *phoneOutbox) closedByBinary() bool                                   // #152, see below
```

`outboxItem{frame []byte; closeCode uint16}` is one pending delivery;
`closeCode != 0` marks a close directive (the frame is optional — same
`hasFrame` presence check the forwarder already used for a directly-sent
directive).

`phoneOutbox` satisfies `Conn` (via `ConnID`/`Send`/`Close`/`CloseWithCode`)
and the forwarder-only `phoneQueue` capability (`forward.go`):

```go
type phoneQueue interface {
    Enqueue(frame []byte, closeCode uint16) error
}
```

`StartBinaryForwarder` type-asserts the addressed phone against `phoneQueue`
after the `PhonesFor` lookup; every production phone matches (`ClientHandler`
registers a `*phoneOutbox` for all of them), so the old direct-`Send` and
direct-close-directive branches now serve only the package's `fakePhone` unit
tests. A refused `Enqueue` is logged as `binary_forwarder_phone_enqueue_failed`
and the loop continues — the same "one bad frame doesn't tear down the
binary" contract as every other per-frame drop.

## Why `Close`/`CloseWithCode` stay immediate, not queued

Eviction (`4404`), grace expiry, and `Shutdown` (`1001`) must cut the
connection now, not after a backlog drains — and closing the underlying conn
is what aborts a write `run` has in flight (`WSConn`'s `closeCtx` cancel).
The registry's `closeWithCode` helper and `Shutdown` type-assert the private
`gracefulCloser` interface (`CloseWithCode(websocket.StatusCode, string)`); a
wrapper that implemented only `Conn` would silently downgrade both closes to
a plain `1000`, and no existing test would catch it. Keeping `CloseWithCode`
on `phoneOutbox` — delegating straight to the wrapped conn — is what
preserves those codes.

## Enqueue and delivery order

**`Enqueue`** (called from the binary forwarder's goroutine), never blocks:

1. `done` already closed → `ErrPhoneOutboxClosed`.
2. `closeCode != 0` → `closeRequested.Store(true)` (see § Binary-requested close tracking below), then continue; this runs even if the item below turns out to overflow.
3. Compute `cost := itemCost(frame)` (`len(frame) + outboxItemOverhead`). Under `mu`, if `queued + cost <= budget`: append `{frame, closeCode}` to `queue`, add `cost` to `queued`, unlock, then a non-blocking send on `ready` (capacity 1 — `default` if `run` already has a wake-up pending) → success, `nil`. The lock is held only for the slice append and int add, never across I/O.
4. Otherwise the item would take the outbox past `phoneOutboxBudget` — the phone can't keep up: mark `done` (via `doneOnce`) and,
   only on the call that actually closes it, spawn `go closeWithCode(conn,
   code, "phone backlog full")` — off-goroutine because a close can block on
   the handshake, and guarded by `doneOnce` so repeated overflow calls can't
   spawn more than one closer. `code` is `1011`, except when the overflowing
   item is itself a close directive, in which case the directive's own code
   is used — the daemon's original intent (an auth reject, say) is more
   informative to the phone than a generic `1011`; the pending frames are
   dropped either way. Returns `ErrPhoneBacklogFull`.

**`run`** (the phone's own goroutine, started by `ClientHandler`), FIFO,
single writer:

- Waits on `done` or `ready`, then pops items one at a time via `next()`
  (pops `queue[0]` under `mu`, zeroing the vacated slot and resetting the
  slice to `nil` once it empties so the backing array doesn't grow without
  bound) and calls `deliver` on each, outside the lock.
- Exits immediately if `done` is closed before the next item is even popped.
- A frame item (`closeCode == 0`, or `hasFrame` true on a directive):
  `conn.Send`. On error, marks `done`, closes the conn with `1011` (or the
  directive's own code, logged as `binary_forwarder_close_frame_send_failed`
  if it's a directive), and returns — everything still queued is dropped. On
  success, calls `onWritten` (the `onBinaryForwarded` metrics hook, so the
  counter reflects frames actually written, not merely enqueued).
- A close directive (`closeCode != 0`): after any frame is written, marks
  `done`, calls `closePhone(conn, closeCode)`, logs
  `binary_forwarder_phone_closed`, and returns.
- Whether `deliver` returns or not, `run` subtracts the item's `itemCost`
  from `queued` (under `mu`) only after `deliver` returns — the item being
  written stays charged against the budget for the whole write, which is
  why a full-max-size-frame outbox holds 15 pending frames rather than 16
  (see § Queue budget below).

Because `run` is FIFO and the only writer, a close directive takes effect
only after its own frame (if any) and every frame queued before it have been
written or dropped — the ordering guarantee the forwarder's `env.CloseCode`
contract needs.

A write timeout needs no special-case handling: `WSConn.Send` already
returns an error once `writeTimeout` expires, and the library has already
closed the socket by then. The `1011` close is best-effort on an
already-timed-out conn (it can't reach the wire), but `CloseWithCode` still
runs through `closeOnce`, and the phone's own `Read` in `StartPhoneForwarder`
fails right after — so the handler's existing teardown defer runs regardless.

## Binary-requested close tracking (#152)

`closeRequested atomic.Bool`, set by `Enqueue` the moment it accepts (or
overflows on) a close directive (`closeCode != 0`), while the outbox is
still live — step 2 above. `closedByBinary()` reads it back.

This is the signal [`ClientHandler`](client-endpoint.md#close-notice-to-the-binary-152)
checks after the phone's forwarder loop ends, to decide whether to send the
binary a [close notice](routing-envelope.md#close-notice-relay-to-binary-152):
a phone the binary itself asked to close must not be reported back to that
same binary as if it had disconnected on its own. Every production close
directive reaches a phone through `Enqueue` — there is no other path — so
this one flag is a complete record of "did the binary ask for this."

`atomic.Bool` rather than a mutex-guarded field because the write
(`Enqueue`, on the binary forwarder's goroutine) and the read
(`ClientHandler`, on the phone handler's own goroutine, after `run` has
already exited) cross goroutines with no other synchronisation between
them — the same reasoning that already governs every other cross-goroutine
field on this type (see § Concurrency model).

## Queue budget: bytes, not frames (#154)

Originally bound by frame count (`phoneOutboxDepth = 16`), on the premise
that the queue only fills once the kernel send buffer for that phone is
already full — i.e. the phone is already well behind. That premise held for
a steadily-writing phone but not for a burst: the daemon's connect-time
reconciles (`reconcileModals`, `reconcileQuestions`, `reconcileQueues`,
`reconcileModelLists`, `reconcileSlashCommandLists`,
`reconcileBackgroundTaskRosters`) each send one small frame per session, so a
daemon holding a dozen or more sessions can produce well over 16 frames, read
off the binary socket in a few TCP reads and enqueued in microseconds — long
before `run` has written more than one or two. The 17th frame overflowed the
queue and closed a perfectly healthy phone with `1011` (issue #154, reported
against pyrycode-mobile's live e2e suite).

The bound is now **`phoneOutboxBudget` = 4 MiB**, charged in bytes via
`itemCost(frame) = len(frame) + outboxItemOverhead` (`outboxItemOverhead` =
64), the same 4 MiB per-phone figure #113 originally derived and #114's
`defaultMaxConnections` still assumes — only the mechanism changed, not the
number. `itemCost` charges the frame being written, not just what's still
pending, so the whole time a write is in flight it counts against the
budget too (see § Enqueue and delivery order).

- **Max-size frames** (`maxFrameBytes` = 256 KiB): 15 fit before the 16th
  would exceed 4 MiB — one fewer than the old design's 16-queued-plus-1-in-flight,
  so the worst case went down, not up.
- **Small or empty frames**: `outboxItemOverhead` bounds the count
  independently of size — at most `phoneOutboxBudget / outboxItemOverhead` =
  65536 items can ever be charged, so a flood of tiny frames to a
  non-reading phone can't turn a byte budget into an unbounded-count queue.
  (Go's slice-growth doubling can transiently add up to ~2 MiB above that on
  top of the queue's own accounting, at 65536 items — absorbed by, not
  engineered around by, the `defaultMaxConnections` derivation's rounding.)
- **A realistic burst** (hundreds of ~1 KiB reconcile frames) now fits
  comfortably inside 4 MiB and is delivered once the phone starts reading —
  it is no longer treated as an overflow.

A full server-id (`maxPhones` = 16, `main.go`) still pins **64 MiB** worst
case — a quarter of a 256 MB machine, unchanged from before this ticket.
`phoneOutboxBudget` is one named constant, easy to retune on evidence, same
as `phoneOutboxDepth` was.

The sum across server-ids is **not** bounded by this queue — a hostile
operator running several daemons, each with 16 non-reading phones, can still
pin 64 MiB per server-id they control. That is the pre-existing
"connection-count caps (per-IP and global) are deferred" residual in
[`docs/threat-model.md`](../../threat-model.md) § DoS resistance, not a new
gap this ticket introduces.

**Unconfirmed: what close code the binary sees for an overflow close.**
When `Enqueue` overflows the budget it closes the phone with `1011` (or the
directive's own code). [`phoneCloseCode`](client-endpoint.md) then maps
whatever error the phone's own `StartPhoneForwarder` read loop sees to the
close notice sent to the binary — using the phone's own WS close status when
one is present, otherwise `1001`. Whether a relay-initiated `1011` close
actually reaches that read loop as a `1011` status, or as a status-less
error that falls back to `1001`, was not confirmed while building #154 —
it's plausible the latter, since the relay closed the socket itself rather
than the phone sending its own close frame, but nobody has traced it end to
end. Left as an open question for whoever next touches this path.

## Concurrency model

- One extra goroutine per phone (`run`), started by `ClientHandler` right
  after registration and awaited by its teardown defer. It is the sole
  writer of routed frames and close directives to that phone's socket.
- `queue` and `queued` are guarded by `mu`, a leaf lock: nothing is called
  while holding it (no conn I/O, no `markDone`, no channel send). `Enqueue`
  (a server-id's binary forwarder, single caller at a time per phone since
  `StartBinaryForwarder` is single-reader) appends under `mu`; `run`'s
  `next()` pops under `mu`. `ready` (capacity 1) is the wake-up signal
  between them and is **never closed** — `done` signals stop — so a late
  `Enqueue` racing the outbox's shutdown can never panic on a closed
  channel; anything it manages to buffer is simply garbage-collected with
  the outbox. No lost wake-up: `run` only waits on `ready` after observing
  an empty `queue` under `mu`, and every `Enqueue` append is followed by a
  non-blocking send on `ready` — the capacity-1 buffer holds that signal
  until `run`'s next select even if `run` hasn't reached it yet.
- `done` closes exactly once (`doneOnce`), whichever of these happens first:
  `stop()` (handler teardown), an `Enqueue` overflow, a write failure in
  `run`, or a close directive delivered in `run`.
- `run` exits either because `done` is already closed when it next selects,
  or because a `deliver` call returns `false` (write failure or close
  directive). An in-flight `conn.Send` is always bounded — by `writeTimeout`,
  or sooner if anything closes the underlying `WSConn` (`closeCtx` cancel).
- The overflow-close goroutine spawned from `Enqueue` exits once
  `CloseWithCode` returns, bounded by the library's close-handshake
  timeouts — the same shape `evictPhones`'s per-conn goroutines already use.
- `closeRequested` (#152) is written by `Enqueue` (binary forwarder
  goroutine) and read by `closedByBinary()` (phone handler goroutine, after
  `run` has exited) — `atomic.Bool`, no lock, see § Binary-requested close
  tracking above.
- `mu` (#154) is the outbox's only lock, scoped to `queue`/`queued`; it is
  never held across conn I/O or another lock. The registry lock is never
  held while calling into an outbox; `WSConn.writeMu` still serialises
  `run`'s writes against nothing else (the heartbeat's pings go through the
  library's own control-frame path, not `Send`).

## Handler wiring (`client_endpoint.go`)

`ClientHandler` constructs `phone := newPhoneOutbox(wsconn, serverID,
phoneOutboxBudget, reg.onBinaryForwarded, logger)` and registers `phone` —
not `wsconn` — with `RegisterPhoneCapped`. The stillborn-reject paths
(`ErrNoServer` → `4404`, `ErrPhonesAtCap` → `4429`) are unchanged: they close
the underlying `*websocket.Conn` directly, before `run` is ever started.

Once registration succeeds, `run` starts on its own goroutine that closes an
`outboxDone` channel on exit. Teardown order matters and is now:

```go
defer func() {
    reg.UnregisterPhone(serverID, connID)
    wsconn.Close()   // aborts any in-flight Send
    phone.stop()     // idempotent; releases run, fails later Enqueue
    <-outboxDone     // wait for run to actually exit
    logger.Info("phone_unregistered", ...)
}()
```

Waiting on `outboxDone` makes "nothing outlives the connection" structural:
the handler cannot return while its writer goroutine is still alive.
`runHeartbeat` and `StartPhoneForwarder` are unchanged — they still take
`wsconn` directly, not the outbox.

## Error handling

| Failure | Where | Outcome |
|---|---|---|
| Item would exceed `phoneOutboxBudget` | `Enqueue` | `done` closed; async close `1011` (or the directive's own code); `ErrPhoneBacklogFull`; forwarder logs `binary_forwarder_phone_enqueue_failed` and continues |
| `Enqueue` after stop / after a prior failure | `Enqueue` | `ErrPhoneOutboxClosed`; forwarder logs and continues |
| Write error or timeout | `run` | close `1011` (or the directive's code); `done` closed; everything still queued dropped; `run` returns |
| Close directive delivered | `run` | frame (if any) written first, then `closePhone(code)`; `done` closed; later items dropped |
| Phone disconnects | handler defer | unregister → close → `stop` → wait for `run` |

In every case the phone still leaves the registry through the existing
`ClientHandler` teardown defer — triggered, as before, by `StartPhoneForwarder`'s
read failing once the socket is closed.

## Logging

No new log keys — the drain loop reuses the forwarder's existing event names
and fields so operator greps keep working:

| event | level | fields |
|---|---|---|
| `binary_forwarder_phone_enqueue_failed` | info | `server_id`, `conn_id`, `err` |
| `binary_forwarder_phone_send_failed` | info | `server_id`, `conn_id`, `err` |
| `binary_forwarder_close_frame_send_failed` | info | `server_id`, `conn_id`, `close_code`, `err` |
| `binary_forwarder_phone_closed` | info | `server_id`, `conn_id`, `close_code` |

`err` is always one of the two sentinels above or a library error; none
carry frame or payload bytes. Close reasons written to the wire are fixed
strings (`"phone backlog full"`, `"phone write failed"`), never an error's
`.Error()` text.

## Adversarial framing

- **Stalled phone.** Can no longer hold up the binary's read loop, its
  siblings, or the binary's pong processing — it only ever blocks its own
  `run` goroutine, bounded first by `phoneOutboxBudget` (backlog fills, then
  `1011`) and second by `WSConn.Send`'s `writeTimeout` (10s) once a write is
  actually in flight. Closes the residual issue #140 named against
  [`/v1/server`'s conflict probe](server-endpoint.md) (#112): a live
  incumbent's pong now waits at most for the forwarder's current binary read
  and enqueue, never for a phone write.
- **Connect-time burst.** A daemon with many sessions sending hundreds of
  small frames in a few TCP reads is no longer mistaken for a stalled phone
  (#154, see § Queue budget above) — the byte budget, not frame count, is
  what protects memory, and a realistic burst fits well inside it.
- **Memory per stalled phone / per server-id.** Bounded structurally at 4 MiB
  and 64 MiB respectively (see § Queue budget). Only frames the binary
  addresses to a phone are queued — a phone cannot inflate its own queue. A
  flood of tiny or empty frames is separately bounded in *count* by
  `outboxItemOverhead` (≤ 65536 items), so a byte-only budget can't be
  turned into an unbounded-count queue by shrinking the frame size (#154).
- **Sum across server-ids.** Not bounded by this queue; existing deferred
  residual, see § Queue budget above and
  [`docs/security-followups.md`](../../security-followups.md).
- **Repeated overflow.** `Enqueue`'s async closer is guarded by `doneOnce`
  (`markDone`'s return value), so a phone that keeps racing `Enqueue` calls
  against an already-overflowing queue cannot spawn more than one closing
  goroutine.
- **Crafted log content.** `err` values are sentinels or library errors; no
  envelope or frame bytes reach a log line.

Verdict from the architect's security review (embedded in
`docs/specs/architecture/113-per-phone-delivery-queue.md`) and the verifier's
independent pass: **PASS**. The review's one `SHOULD FIX` — guarding the
overflow closer against goroutine multiplication — is the `doneOnce`-gated
spawn described above. The byte-budget rework (#154, spec at
`docs/specs/architecture/154-phone-outbox-byte-budget.md`) carries its own
self-reviewed **PASS** and the verifier's independent pass, with no findings
against the `mu`/`ready` concurrency change or the per-item overhead charge.

## Testing

`internal/relay/phone_outbox_test.go` (package `relay`), built around a
`stallPhone` fake whose `Send` blocks until released or closed and records
sends/closes in delivery order:

- **Stalled phone doesn't block siblings (AC1).** Two phones on one
  server-id; one's `Send` is stalled mid-call while the binary routes several
  frames to the healthy one — all arrive while the stall holds, and the
  binary source is fully drained.
- **Ordering and close-after-drain (AC2).** A gated phone is enqueued two
  frames then a close directive with a third frame and a close code; no close
  happens while gated; on release, delivery order is frame, frame, frame,
  then the recorded close code.
- **Overflow closes with `1011` (AC3).** A stalled phone with a budget sized
  in `itemCost` units for one in-flight item plus two more: once the budget
  is exhausted, the next `Enqueue` returns `ErrPhoneBacklogFull`, the phone
  is closed with `1011`, `run` returns, and a later `Enqueue` returns
  `ErrPhoneOutboxClosed`.
- **Connect-time burst survives, delivered in order (#154 AC1,
  `TestPhoneOutbox_Burst_DeliveredInOrder`).** 300 distinct 1 KiB frames
  enqueued back to back against a stalled phone (all return `nil`, none
  overflow); on release, all 300 are delivered in order with no close. Fails
  on `main`'s frame-count bound around frame 18.
- **Stalled phone still bounded by bytes, not frame count (#154 AC2,
  `TestPhoneOutbox_StalledPhone_BoundedBytes`).** Table over max-size
  (256 KiB) and small (1 KiB) frames against a never-released stalled phone:
  in both cases `Enqueue` eventually returns `ErrPhoneBacklogFull`, the
  accepted byte total never exceeds `phoneOutboxBudget`, the phone is closed
  `1011` with pending frames dropped, and a later `Enqueue` returns
  `ErrPhoneOutboxClosed`. The small-frame case also asserts more than 300
  frames were accepted — a frame-count bound would fail this well before
  300.
- **Write failure closes with `1011` and counts nothing (AC3).** A `Send`
  error closes with `1011`, `run` returns, and `onWritten` is never called —
  contrasted against the success-path tests, which each assert one
  `onWritten` per frame actually written.
- **`stop` releases everything (AC4).** An idle `run` returns once `stop` is
  called; `Enqueue` after `stop` returns `ErrPhoneOutboxClosed` immediately; a
  `run` blocked inside `Send` returns once the underlying conn is closed.
- **Handler wiring (AC4).** `client_endpoint_test.go`: a phone dialled
  through `ClientHandler` is registered as a `phoneQueue`; a frame enqueued
  on it reaches the dialled socket; once the client disconnects, the
  registered outbox refuses further `Enqueue` calls with
  `ErrPhoneOutboxClosed` (proving `stop` ran and `run` exited).

Existing `forward_test.go` tests keep exercising the synchronous fallback
path through `fakePhone` (which does not implement `phoneQueue`). All under
`go test -race ./internal/relay/...`.

`stallPhone.Send`'s `entered` signal (used elsewhere to synchronise on "the
stall has begun") became a non-blocking send with a `default` fallback
(#154): it previously blocked on a 64-slot buffered channel, which a
300-write burst test would silently wedge on instead of failing, since
nothing after the first 64 sends could ever proceed.

## Related

- [Binary-side frame forwarder](binary-forwarder.md) — the sole production
  caller of `Enqueue`; owns the `phoneQueue` capability interface.
- [`/v1/server` conflict probe](server-endpoint.md) — the issue #140
  pong-starvation risk this queue closes.
- [Connection registry](connection-registry.md) — `evictPhones`,
  `handleGraceExpiry`, and `Shutdown` all reach a registered `*phoneOutbox`
  only through `Conn` / `gracefulCloser`, never through the outbox's own
  type.
- [WSConn adapter](ws-conn-adapter.md) — `writeTimeout` bounds every
  in-flight `Send` the queue's `run` goroutine makes.
- `docs/specs/architecture/113-per-phone-delivery-queue.md` — the architect's
  plan, including the security review this doc's verdict is drawn from.
- `docs/specs/architecture/154-phone-outbox-byte-budget.md` — the byte-budget
  rework's plan and security review.
- [Codebase: #154](../codebase/154.md) — implementation notes for the
  byte-budget rework.
- [`/v1/client` § Close notice to the binary](client-endpoint.md#close-notice-to-the-binary-152) —
  the sole reader of `closedByBinary()`.
- [Routing envelope § Close notice](routing-envelope.md#close-notice-relay-to-binary-152) —
  the wire shape `closedByBinary()` gates.
