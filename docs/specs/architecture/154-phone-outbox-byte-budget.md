# #154 — Bound the phone outbox by bytes, not frames

## Files read

- `internal/relay/phone_outbox.go` → `phoneOutboxDepth`, `phoneOutbox`, `newPhoneOutbox`, `Enqueue`, `run`, `deliver`, `markDone` — the queue whose 16-frame bound closes a phone on a connect-time burst.
- `internal/relay/phone_outbox_test.go` → `stallPhone`, `startOutbox`, `TestPhoneOutbox_BacklogFull_Closes1011` — the harness the new tests extend; `stallPhone.Send` blocks on its 64-slot `entered` channel, so a 300-frame test needs that signal made non-blocking.
- `internal/relay/client_endpoint.go` → `ClientHandler` — the one production caller of `newPhoneOutbox`; `phoneCloseCode` — maps a status-less read error to 1001.
- `internal/relay/forward.go` → `StartBinaryForwarder`, `phoneQueue` — the sole production caller of `Enqueue`; the frame is `env.Frame`, a `json.RawMessage` that `json.Unmarshal` copies into its own allocation, so a queued frame pins `len(frame)` bytes and not the whole read buffer.
- `cmd/pyrycode-relay/main.go` → `defaultMaxConnections` — its derivation names `phoneOutboxDepth × maxFrameBytes`; the 4 MiB figure and the cap of 20 stay.
- `internal/relay/server_endpoint_test.go` → the stalled-phone conflict-probe test (#140) — its comment names `phoneOutboxDepth`.
- `docs/knowledge/features/phone-outbox.md` § Queue depth, § Concurrency model — the premise ("only fills once the kernel send buffer is full") this ticket falsifies; § Concurrency model's "items is never closed" rule, which the new signal channel keeps.
- `docs/knowledge/features/connection-cap.md` — restates the 4 MiB per-phone figure the cap derives from.

No in-flight branch touches these files (`feature/127` touches the registry only).

## Context

`phoneOutboxDepth = 16` counts frames. The daemon's connect-time reconciles send one frame per session per reconcile kind; with 20 sessions that is far more than 16 frames, read off the binary socket in a few TCP reads and enqueued in microseconds, before `run` has written more than one or two. The 17th overflows and the phone is closed 1011 although its socket is healthy. What #113 actually protects is memory — 16 × 256 KiB = 4 MiB per phone — so the bound becomes that byte figure directly. 300 small reconcile frames fit; 16 max-size frames still (nearly) exhaust it.

## Design

Replace the frame-count channel with a byte-budgeted FIFO.

```go
// phoneOutboxBudget bounds the bytes one phone's outbox may pin: 4 MiB,
// 16 max-size (maxFrameBytes = 256 KiB) frames.
const phoneOutboxBudget = 4 << 20

// outboxItemOverhead is charged per item on top of its frame bytes, so a
// flood of tiny or empty frames is bounded in count too (≤ 65536 items).
const outboxItemOverhead = 64

func newPhoneOutbox(conn Conn, serverID string, budget int, onWritten func(), logger *slog.Logger) *phoneOutbox
```

`phoneOutbox` fields change from `items chan outboxItem` to:

- `mu sync.Mutex` guarding `queue []outboxItem` (pending, FIFO) and `queued int` (bytes charged: pending items plus the one `run` is delivering);
- `budget int`;
- `ready chan struct{}` with capacity 1 — a wake-up token for `run`, never closed (same "late Enqueue cannot panic" reasoning the old `items` had).

`itemCost(frame) = len(frame) + outboxItemOverhead`.

**Enqueue** — steps unchanged except step 3: under `mu`, if `queued + cost > budget` → overflow (unchanged path: `markDone`, async close 1011 or the directive's code, `ErrPhoneBacklogFull`); otherwise append, add `cost`, unlock, then a non-blocking send on `ready`. Never blocks: the lock is held only for an append, never across I/O.

**run** — wait on `done` or `ready`; then pop items one at a time under `mu` (zeroing the vacated slot, resetting the slice to nil when it empties so the backing array does not grow without bound) and `deliver` each outside the lock. After `deliver` returns, subtract the item's cost. Charging until delivery rather than until dequeue means the in-flight frame counts, so the bound is on everything the outbox pins. Stops as today when `deliver` returns false or `done` closes.

No lost wake-up: `run` only waits after observing an empty queue under `mu`; any append after that observation is followed by a `ready` send, which the capacity-1 channel keeps until `run` selects.

`deliver`, `closedByBinary`, `stop`, `markDone`, `Close`, `CloseWithCode` are unchanged. `ClientHandler` passes `phoneOutboxBudget`.

**Bound arithmetic.** Pinned bytes per phone ≤ 4 MiB of charged bytes. Max-size frames: 15 fit (16 × (256 KiB + 64) just exceeds 4 MiB) — the old design pinned 16 queued plus one in flight, so the worst case is lower, not higher. Tiny frames: ≤ 65536 items, whose slot (32 bytes) plus frame allocation stays within the 64-byte overhead-plus-frame charge; slice growth can double the slot array transiently (≤ 2 MiB extra at 65536 items), which the main.go derivation's rounding already absorbs — noted, not engineered around. `defaultMaxConnections` stays 20; its comment is restated in `phoneOutboxBudget` terms.

## Concurrency model

Goroutines unchanged: the binary forwarder calls `Enqueue`; the phone's `run` is the single writer. New lock `mu` is a leaf: nothing is called while holding it (no conn I/O, no `markDone`, no channel send). The registry lock is never held while calling into an outbox (unchanged). Shutdown unchanged: `stop` closes `done`, `run` observes it in its wait or at the top of `deliver`.

## Error handling

Unchanged table from #113, with "queue at its bound" meaning "charged bytes would exceed `phoneOutboxBudget`". Items still pending at overflow, write failure or close directive are dropped with the outbox (not written).

## Testing strategy

In `phone_outbox_test.go` (package `relay`):

- **Burst survives (AC1).** `stallPhone` holds writes; enqueue 300 distinct 1 KiB frames back to back — all return nil; release; all 300 recorded in order, no close event. On `main` this returns `ErrPhoneBacklogFull` around frame 18 and the phone is closed 1011.
- **Stalled phone still closes, bytes bounded (AC2).** Table over max-size (256 KiB) and small (1 KiB) frames: a never-released `stallPhone`, enqueue until `ErrPhoneBacklogFull` (loop guard fails loudly); accepted × size ≤ 4 MiB; phone closed 1011, pending frames not written; later `Enqueue` → `ErrPhoneOutboxClosed`. The small case also asserts accepted > 300 so a frame-count bound would fail it.
- Existing `TestPhoneOutbox_BacklogFull_Closes1011` is re-expressed with a budget sized in `itemCost` units (the in-flight item now counts against it).
- `stallPhone`'s `entered` signal becomes non-blocking so a 300-write test does not wedge on the 64-slot buffer.
- Remaining tests swap `phoneOutboxDepth` for `phoneOutboxBudget`.

## Open questions

- Does an overflow close reach the binary as a 1001 notice? `phoneCloseCode` maps a status-less read error to 1001; whether `StartPhoneForwarder`'s read after the relay's own close carries the status is not confirmed here. Out of scope per the ticket; left for the documentation handoff as unconfirmed.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/phone-outbox.md` § Queue depth: replace the "only fills once the kernel send buffer is already full" premise with the connect-time burst case (#154); describe the byte budget (`phoneOutboxBudget` = 4 MiB, `outboxItemOverhead` = 64 per item, in-flight item charged); update § API, § Enqueue and delivery order, § Concurrency model (the `mu` leaf lock and `ready` token replace `items`), § Testing and § Error handling to match.
- `docs/knowledge/features/connection-cap.md` and `docs/threat-model.md`: wherever the per-phone bound is stated as `phoneOutboxDepth × maxFrameBytes`, restate it as the 4 MiB `phoneOutboxBudget`; the figure stays.
- The probable-but-unconfirmed 1001 close-notice for an overflow close (Open questions) may be recorded as such.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — `Enqueue` reads only `len(frame)`; the frame stays an opaque `[]byte` from `StartBinaryForwarder` to `deliver`. Nothing parses or inspects it.
- [Tokens, secrets, credentials] No findings — no header or credential flows through the outbox.
- [File operations] Not applicable — no filesystem access.
- [Subprocess] Not applicable — none.
- [Cryptographic primitives] Not applicable — none.
- [Network & I/O — resource exhaustion] No findings — the per-phone bound stays 4 MiB and now includes the in-flight frame (previously 16 queued + 1 in flight). A hostile daemon sending empty or tiny frames to a non-reading phone is bounded in count by `outboxItemOverhead` (≤ 65536 items, slot array ≤ 2 MiB, transiently doubled by slice growth) — the one new vector a byte-only budget would open, closed by the per-item charge. Only frames the binary addresses to a phone are queued; a phone still cannot fill its own queue.
- [Network & I/O — amplification] No findings — still 1:1 forwarding.
- [Network & I/O] OUT OF SCOPE — the sum across server-ids is not bounded by this queue; unchanged residual under `docs/security-followups.md` / threat model § DoS resistance, and the global `defaultMaxConnections` (#114) keeps its derivation.
- [Logs] No findings — no new log calls or keys; the overflow still logs `binary_forwarder_phone_enqueue_failed` with a sentinel `err`.
- [Concurrency] No findings — `mu` is a leaf lock held only for slice/int updates; `ready` is never closed; `done`/`doneOnce` semantics and the single-closer guard on overflow are unchanged. Lost wake-up analysed in Design.
- [Threat model alignment] No re-review trigger tripped (no dependency, endpoint or deploy change); the per-phone bound's wording changes in the threat model — carried in the documentation handoff.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-25
