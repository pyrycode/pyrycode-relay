# Connection registry

The relay's routing core needs one canonical structure that owns "who is connected as what for which server-id." Every routing-layer ticket downstream — `/v1/server` upgrade (#16), `/v1/client` upgrade (#5), frame forwarding (#6), heartbeat (#7), grace period (#20/#21), health (#10) — reads or writes through this registry.

Two non-negotiable shapes flow from `docs/architecture.md`:

1. **server-id → binary is 1:1, first-claim-wins.** A second binary claiming a held server-id is a `4409` conflict — *unless* a grace timer is pending (see "Grace-period reclaim" below), in which case the second claim succeeds and replaces the binary. A caller that has independently established the incumbent is unresponsive (the `/v1/server` conflict-probe path, #112) can also replace it outside any grace window, via `TakeoverServer` — see "Probe-driven takeover" below.
2. **server-id → phones is 1:N, gated on a binary holding the slot.** A phone whose server-id has no binary is a `4404` no-server.

The registry is a **passive in-memory store**. It holds opaque `Conn` handles, never speaks WebSocket, never invokes `Send` on the conns it tracks. With the grace-period primitive added in #20 it now owns *one* timer per server-id and calls `Close()` on phones at expiry — but always outside its lock, snapshot-and-iterate.

## API

Package `internal/relay` (`registry.go`):

```go
type Conn interface {
    ConnID() string         // stable, non-blocking; called under the registry lock
    Send(msg []byte) error  // never called under the registry lock
    Close()                 // idempotent; never called under the registry lock
}

type Registry struct { /* sync.RWMutex + binaries map + phones map */ }

func NewRegistry() *Registry

func (r *Registry) ClaimServer(serverID string, conn Conn) error
func (r *Registry) TakeoverServer(serverID string, incumbent, conn Conn) error
func (r *Registry) ReleaseServer(serverID string) (released bool)
func (r *Registry) ScheduleReleaseServer(serverID string, d time.Duration)
func (r *Registry) ScheduleReleaseServerIfHeld(serverID string, conn Conn, d time.Duration) bool
func (r *Registry) RegisterPhone(serverID string, conn Conn) error
func (r *Registry) RegisterPhoneCapped(serverID string, conn Conn, max int) error
func (r *Registry) UnregisterPhone(serverID string, connID string)
func (r *Registry) BinaryFor(serverID string) (Conn, bool)
func (r *Registry) PhonesFor(serverID string) []Conn
func (r *Registry) Counts() (binaries, phones int)
```

Sentinel errors (branch with `errors.Is`):

| Error | Returned by | Maps to |
|---|---|---|
| `ErrServerIDConflict` | `ClaimServer` when slot already held | WS close `4409` |
| `ErrNoServer` | `RegisterPhone` / `RegisterPhoneCapped` when no binary holds the slot | WS close `4404` |
| `ErrPhonesAtCap` | `RegisterPhoneCapped` when `len(phones[serverID]) >= max` | WS close `4429` |

The mapping to close codes is informational; the registry doesn't know about WebSockets — the upgrade handlers translate.

`RegisterPhone(serverID, conn)` is equivalent to `RegisterPhoneCapped(serverID, conn, 0)` — the legacy no-cap entry point, retained for test fixtures and callers that explicitly do not want the cap-aware contract. Production traffic on `/v1/client` uses `RegisterPhoneCapped` with the wiring-site `maxPhones` value (16 today, see [`/v1/client`](client-endpoint.md)). `ErrNoServer` takes precedence over `ErrPhonesAtCap`; the cap check (`max > 0 && len(phones[serverID]) >= max`) and the slice append both run under the same write lock, so two concurrent callers at `max-1` cannot both succeed (race shape mirrors `ClaimServer`'s first-claim-wins).

## Grace-period reclaim (`ScheduleReleaseServer`, #20)

`ScheduleReleaseServer(serverID, d)` arms a deferred release after duration `d`. The grace window IS the reclaim path — see [ADR-0006](../decisions/0006-grace-period-as-reclaim-path.md) for the rationale.

While the timer is pending:

| Operation | Behaviour during grace window |
|---|---|
| `BinaryFor(serverID)` | Returns the (now-closed) binary `Conn`. `Send` through it errors per the underlying impl's contract; the registry adds no new error semantics. |
| `RegisterPhone(serverID, conn)` | Succeeds — the binary entry is still present, only the timer is pending. |
| `ClaimServer(serverID, conn)` | Returns `nil`, **not** `ErrServerIDConflict`. The pending timer is `Stop()`'d, the binary `Conn` is replaced atomically, and every phone registered for `serverID` (before or during the window) is removed under the lock and closed with `4404` outside it, one goroutine per phone (#127, ADR-0006 amendment). The phones re-dial and re-handshake against the new binary. |
| `ScheduleReleaseServer(serverID, d')` | Replaces the pending timer (last call wins). Old timer is `Stop()`'d. |
| `ReleaseServer(serverID)` | Unchanged — still removes the binary entry immediately. Does not interact with the timer. |

On expiry (no reclaim within `d`): the binary entry is removed, the phones slice for `serverID` is snapshotted and deleted under the lock, then `Close()` is called on every phone *outside* the lock. After expiry, `RegisterPhone` returns `ErrNoServer` and `ClaimServer` succeeds normally.

`ScheduleReleaseServer` on an unheld server-id is a defensive no-op: it arms a timer that on fire deletes whatever happens to be present (nothing). Same shape as `ReleaseServer("unknown")` returning `false`.

## Probe-driven takeover (`TakeoverServer`, `ScheduleReleaseServerIfHeld`, #112)

The grace window only starts once the relay has *detected* a disconnect — a clean close, a network error, or the heartbeat's own 30s+30s timeout. On a silent drop (power loss, NAT timeout) none of those fire, so a crash-restarted daemon's claim would get `4409` against its own dead predecessor for up to 60s. `TakeoverServer` gives a caller that has independently confirmed the incumbent is unresponsive — the `/v1/server` handler's liveness probe, see [`/v1/server`](server-endpoint.md) — a second, narrower route into the same reclaim semantics, without waiting for detection or a grace window at all:

```go
func (r *Registry) TakeoverServer(serverID string, incumbent, conn Conn) error
```

- **Identity-scoped.** Under `mu`, if `binaries[serverID]` is held by some conn other than `incumbent`, `TakeoverServer` returns `ErrServerIDConflict` and touches nothing — a concurrent reclaim, or a second prober's takeover, always wins over a stale probe result. `incumbent == nil` means "the slot was empty when the caller looked"; it is refused the same way if anyone has claimed it since.
- **Same reclaim semantics as `ClaimServer`'s grace branch.** Held by `incumbent` (or empty) → cancel and delete any pending grace timer, install `conn` as the binary, and snapshot-and-delete `phones[serverID]`, closed outside the lock with `reclaimCloseCode`/`reclaimCloseReason` (`4404`), one goroutine per phone — identical to a grace-window reclaim (ADR-0006's 2026-09-15 amendment). Both paths share one implementation, `replaceBinaryLocked`, so there is exactly one reclaim behaviour to reason about.
- **Does not close `incumbent`.** The registry never invokes `Send`/`Close` on a `Conn` while holding `mu`, and the close code (`1011 "heartbeat timeout"`) is a decision the handler owns, not the registry. The caller closes the incumbent itself, outside the lock, after `TakeoverServer` returns.

`ScheduleReleaseServerIfHeld(serverID, conn, d) bool` closes the hazard a takeover creates: the incumbent's own handler still runs its disconnect defer *after* being displaced, and an unscoped `ScheduleReleaseServer` call there would arm a grace timer whose expiry deletes the **new** binary 30s later. The `IfHeld` variant checks, under the same lock, that `conn` is still the current holder of `serverID` before arming anything; for a displaced conn it is a no-op and returns `false`. `ScheduleReleaseServer` itself is unchanged and still unscoped — it has 18 call sites across five test files, and nothing in production calls it after #112 (the `/v1/server` handler now calls the `IfHeld` variant exclusively). Both share an internal `armGraceLocked` helper, so there is one place the timer-arming logic lives.

Expiry needs no matching change: every place that replaces `binaries[serverID]` while a timer is pending — `ClaimServer`'s grace branch and `TakeoverServer` alike — deletes the timer entry under the same lock via `replaceBinaryLocked`. A timer is therefore only ever armed while its own conn holds the slot, and it is removed the instant the slot changes hands by any route. The existing pointer-identity guard below (unchanged) is what makes a stale fire of that timer a no-op if it races the deletion.

### Pointer-identity stale-fire defence

The one race the registry's `sync.RWMutex` cannot prevent: a `time.AfterFunc` whose body has already started executing cannot be `Stop()`'d. If `ClaimServer` (or a re-armed `ScheduleReleaseServer`) takes the lock before the expiry handler does, it `Stop()`s a timer whose body is already in flight, replaces the entry, and returns. The expiry-handler goroutine then acquires the lock and would naïvely tear down the new claimant's state.

The defence: each armed timer is wrapped in a `*graceEntry`, stored in `r.timers[serverID]`, and the `AfterFunc` closure captures the entry pointer. On fire, the handler asserts `r.timers[serverID] == self` under the lock. If `ClaimServer` replaced the entry, the pointer no longer matches and the handler returns without touching state. The wrapper indirection (rather than capturing `*time.Timer` directly) avoids a self-referential local var that trips the race detector at construction.

### Goroutine lifecycle

`time.AfterFunc` is the only goroutine spawn the registry introduces. Lifetime is bounded by `d`. Cancelled-before-fire timers (`Stop()` returns true) never spawn the goroutine. Cancelled-after-fire timers spawn the goroutine but it returns at the pointer-identity check. The `timers` map holds at most one entry per server-id; rapid disconnect/reclaim cycles do not grow it.

The duration `d` is fully trusted — degenerate values (`d <= 0` fires immediately, `d` near `MaxInt64` never fires before process exit) are safe and named in the doc-comment so callers don't add bounds at the registry layer. The `/v1/server` handler in #21 supplies `30*time.Second`; tests use ms-scale.

## Concurrency model

- **One `sync.RWMutex` covers all three maps** (`binaries`, `phones`, `timers`). Mutating methods take `Lock`; lookups take `RLock`. Sharding is a possible later optimisation but irrelevant at v1 (tens to hundreds of conns, sub-microsecond critical sections, no measured contention).
- **Conn callbacks are never invoked under the lock,** with one documented exception: `UnregisterPhone` calls `ConnID()` while holding the write lock to scan for the target. The `Conn` contract requires `ConnID` to be a non-blocking getter.
- **Broadcast pattern.** A caller wanting to fan out a frame to all phones for a server-id calls `PhonesFor` (returns a freshly allocated copy) and iterates the snapshot calling `Send` per conn — no registry lock held during I/O.
- **No callbacks, no channels.** The registry is a passive store. Notification of binary loss / new phone arrival happens elsewhere (the WS upgrade handlers and the grace timer the registry arms in `ScheduleReleaseServer`).
- **Liveness probing lives entirely outside the registry.** `TakeoverServer` (#112) never pings a `Conn` — the caller has already decided, off-lock, that the incumbent is unresponsive, before calling in. The registry's only new responsibility is the identity check and the atomic swap.

## Invariants the registry enforces

- `ClaimServer` is atomic check-then-set under the write lock; concurrent claims for the same server-id cannot both succeed.
- `RegisterPhone` is atomic check-binary-then-append under the write lock; a phone cannot land in the map for a server-id whose binary was released between check and append.
- `UnregisterPhone` removes by `ConnID` via swap-and-truncate, nils the trailing slot for GC hygiene, and `delete`s the map key when the slice empties — no orphaned empty slices.
- `PhonesFor` returns nil for unknown server-ids or empty slices — callers don't have to distinguish "unknown" from "known with zero phones."
- `PhonesFor` returns a copy: mutating the returned slice cannot affect registry state.
- `Counts` is internally consistent for one call (read under RLock); two concurrent calls may observe different values.

## What the registry deliberately does NOT do

- **Does not close `Conn` handles in the immediate-release path.** `ReleaseServer` and `UnregisterPhone` only remove map entries. Connection lifetime is owned by the WS upgrade handlers. The grace-period expiry handler is the one exception — it `Close()`s phones whose binary did not reclaim within the grace window. See [ADR-0006](../decisions/0006-grace-period-as-reclaim-path.md).
- **Does not deduplicate phones by `ConnID`.** Caller invariant: each `Conn` is registered once.
- **Does not own the per-server-id phone cap value.** `RegisterPhoneCapped` enforces a cap supplied by the caller (#30); the value itself lives at the wiring site in `cmd/pyrycode-relay/main.go`. `RegisterPhone` (no cap) remains available for fixtures that opt out.
- **Does not propagate binary loss to phones via `ReleaseServer`.** When `ReleaseServer` runs, the phones slice is left in place. The owner of phones must `UnregisterPhone` (and choose whether to close them) when their server's binary is gone. The deferred sibling `ScheduleReleaseServer` *does* close phones — but only if the timer expires without a reclaim. This split lets the disconnect path opt into the grace window while the synchronous path (e.g. cleanup after a failed claim) stays narrow.
- **Does not validate `serverID` content.** Length, charset, prefix — owned by `x-pyrycode-server` header validation in #4. The registry treats any string as an opaque map key.

## Adversarial framing

The registry has no networking and no direct exposure to adversarial input. Adversaries reach it only through future tickets (#4/#5) that pass strings and `Conn` handles into its API. Notes for the layers above:

- **`serverID` of pathological size or content** is harmless to the registry (just a map key) but the WS upgrade layer must cap header lengths.
- **`Conn.ConnID()` returning unstable values** would cause `UnregisterPhone` to remove the wrong phone (or none). Documented as a contract on the `Conn` interface; violation is a leak, not a cross-tenant data leak.
- **Repeated `RegisterPhone` for the same server-id** is unbounded memory growth. The cap-aware `RegisterPhoneCapped` (#30) is the entry point production wiring uses; the no-cap `RegisterPhone` remains for test fixtures. Across-server-id total caps are not the registry's job (deferred to per-IP rate-limit #34 and any future global cap).
- **Slow `ConnID` blocking the write lock** is the cost of the documented non-blocking contract; copying the slice and scanning outside the lock would create a TOCTOU window where a phone could be added between snapshot and removal.

## Testing

`internal/relay/registry_test.go`, `package relay`. Tests use a minimal `fakeConn` (id, sent buffer, mutex-guarded `closed` flag, optional `closeCh` to signal close synchronously). The mutex was added in #20 because the grace-expiry handler runs `Close()` on a different goroutine from the test, making `closed` cross-goroutine state.

Functional cases (one subtest per AC bullet):

- `ClaimServer` first-wins / second-conflicts; `BinaryFor` still resolves to the first conn.
- `ReleaseServer` reclaim — release returns `true`, second release of an unheld id returns `false`, claim again with a different conn succeeds.
- `RegisterPhone` requires a binary (`ErrNoServer` until `ClaimServer`).
- `RegisterPhoneCapped` cap semantics (#30): exact-cap registrations succeed, the next returns `ErrPhonesAtCap` without mutating the slice (`TestRegisterPhoneCapped_BoundaryAndRejection`); slot frees after `UnregisterPhone` and the next registration succeeds again (`TestRegisterPhoneCapped_RecoveryAfterUnregister`); cap is per-server-id, not global (`TestRegisterPhoneCapped_PerServerIDIndependent`); `RegisterPhone` delegates to `RegisterPhoneCapped(_, _, 0)` and the no-cap contract is preserved (`TestRegisterPhone_NoCapAfterDelegation`). `TestRegistry_RaceFreedom` was extended to drive `RegisterPhoneCapped` under the race detector.
- `UnregisterPhone` removes by `ConnID`, leaves siblings intact, no-op on unknown id.
- `PhonesFor` snapshot isolation: mutating the returned slice doesn't affect the registry; later registrations don't appear in earlier snapshots.
- `Counts` across the full lifecycle including the `ReleaseServer` orphan case (`(0, 1)` while phones survive a release).
- `UnregisterPhone` deletes the empty-slice map entry.

Grace-period cases (added in #20):

- Reclaim within grace → no release fires; new conn is the active binary; previously-registered phones remain.
- Grace expires without reclaim → binary entry gone, phones `Close()`-called and removed; subsequent `ClaimServer` succeeds, subsequent `RegisterPhone` returns `ErrNoServer`.
- Phone registered during grace window survives reclaim; same phone is closed on expiry.
- Re-arming `ScheduleReleaseServer` for the same id stops the prior timer (last call wins); only the latest timer's expiry fires.
- `ScheduleReleaseServer` on an unheld id is a no-op.
- Race-freedom under rapid disconnect/reclaim cycles (16 goroutines × 200 iterations, ms-scale durations).

Probe-driven takeover cases (added in #112):

- `TestTakeoverServer_IdleIncumbent_ReplacesAndEvicts` — takeover of an idle incumbent swaps the binary, evicts phones with `4404` before the call returns, and leaves the incumbent itself unclosed (the registry never closes it).
- `TestTakeoverServer_IncumbentReplaced_Conflicts` — a takeover whose named incumbent is no longer the holder returns `ErrServerIDConflict` and touches nothing (current binary and its phones untouched); also covers `incumbent == nil` on both a held slot (refused) and an empty one (granted).
- `TestTakeoverServer_DuringGrace_CancelsTimer` — a takeover racing a pending grace timer cancels it; the new binary still holds the slot well past `3×grace`.
- `TestScheduleReleaseServerIfHeld_DisplacedConn_NoOp` — the hazard #112 exists to close: after a takeover, the displaced incumbent's `ScheduleReleaseServerIfHeld` call returns `false` and arms nothing, so the new binary survives `3×grace`; the new holder's own `ScheduleReleaseServerIfHeld` call still returns `true` and its release still fires normally.

Race coverage: `TestRegistry_RaceFreedom` (#3) hammers `ClaimServer` / `BinaryFor` / `RegisterPhone` / `PhonesFor` / `UnregisterPhone` / `Counts` / `ReleaseServer` from 32 goroutines × 200 ops over four contended server-ids. `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles` (#20) covers the timer paths analogously. Both deliberately ignore returned errors — `ErrServerIDConflict` under contention is expected behaviour, not a failure. The point is the absence of `DATA RACE` reports.

`make test` runs `-race` by default. The `-count=20` invocation in the AC is a manual stress command, documented in the test's doc comment, not a knob in the test code:

```sh
go test -race -count=20 -run TestRegistry_RaceFreedom ./internal/relay
```

## Related

- [ADR-0003: Connection registry as a passive store](../decisions/0003-connection-registry-passive-store.md) — single RWMutex, snapshot returns, the orphan-phones invariant.
- [ADR-0006: Grace window IS the reclaim path](../decisions/0006-grace-period-as-reclaim-path.md) — why a `ClaimServer` during grace succeeds rather than conflicts; the pointer-identity stale-fire defence; amended for probe-driven takeover (#112).
- [`/v1/server`](server-endpoint.md) — the sole caller of `TakeoverServer` and `ScheduleReleaseServerIfHeld`; owns the liveness probe and the incumbent's close code.
- [Routing envelope](routing-envelope.md) — the wrapper used by frame forwarding (#6) once the registry is wired up.
- [Architecture overview](../../architecture.md) — where the registry fits in the data flow.
