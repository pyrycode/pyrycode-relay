# Connection registry

The relay's routing core needs one canonical structure that owns "who is connected as what for which server-id." Every routing-layer ticket downstream — `/v1/server` upgrade (#4), `/v1/client` upgrade (#5), frame forwarding (#6), heartbeat (#7), grace period (#8), health (#10) — reads or writes through this registry.

Two non-negotiable shapes flow from `docs/architecture.md`:

1. **server-id → binary is 1:1, first-claim-wins.** A second binary claiming a held server-id is a `4409` conflict.
2. **server-id → phones is 1:N, gated on a binary holding the slot.** A phone whose server-id has no binary is a `4404` no-server.

The registry is a **passive in-memory store**. It holds opaque `Conn` handles, never speaks WebSocket, never invokes `Send` / `Close` on the conns it tracks, and never times anything out. The 30-second grace period on binary disconnect (#8) wraps the registry rather than living inside it.

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
func (r *Registry) ReleaseServer(serverID string) (released bool)
func (r *Registry) RegisterPhone(serverID string, conn Conn) error
func (r *Registry) UnregisterPhone(serverID string, connID string)
func (r *Registry) BinaryFor(serverID string) (Conn, bool)
func (r *Registry) PhonesFor(serverID string) []Conn
func (r *Registry) Counts() (binaries, phones int)
```

Sentinel errors (branch with `errors.Is`):

| Error | Returned by | Maps to |
|---|---|---|
| `ErrServerIDConflict` | `ClaimServer` when slot already held | WS close `4409` |
| `ErrNoServer` | `RegisterPhone` when no binary holds the slot | WS close `4404` |

The mapping to close codes is informational; the registry doesn't know about WebSockets — the upgrade handlers translate.

## Concurrency model

- **One `sync.RWMutex` covers both maps.** Mutating methods take `Lock`; lookups take `RLock`. Sharding is a possible later optimisation but irrelevant at v1 (tens to hundreds of conns, sub-microsecond critical sections, no measured contention).
- **Conn callbacks are never invoked under the lock,** with one documented exception: `UnregisterPhone` calls `ConnID()` while holding the write lock to scan for the target. The `Conn` contract requires `ConnID` to be a non-blocking getter.
- **Broadcast pattern.** A caller wanting to fan out a frame to all phones for a server-id calls `PhonesFor` (returns a freshly allocated copy) and iterates the snapshot calling `Send` per conn — no registry lock held during I/O.
- **No callbacks, no channels.** The registry is a passive store. Notification of binary loss / new phone arrival happens elsewhere (the WS upgrade handlers and #8's grace timer).

## Invariants the registry enforces

- `ClaimServer` is atomic check-then-set under the write lock; concurrent claims for the same server-id cannot both succeed.
- `RegisterPhone` is atomic check-binary-then-append under the write lock; a phone cannot land in the map for a server-id whose binary was released between check and append.
- `UnregisterPhone` removes by `ConnID` via swap-and-truncate, nils the trailing slot for GC hygiene, and `delete`s the map key when the slice empties — no orphaned empty slices.
- `PhonesFor` returns nil for unknown server-ids or empty slices — callers don't have to distinguish "unknown" from "known with zero phones."
- `PhonesFor` returns a copy: mutating the returned slice cannot affect registry state.
- `Counts` is internally consistent for one call (read under RLock); two concurrent calls may observe different values.

## What the registry deliberately does NOT do

- **Does not close `Conn` handles.** `ReleaseServer` and `UnregisterPhone` only remove map entries. Connection lifetime is owned by the WS upgrade handlers.
- **Does not deduplicate phones by `ConnID`.** Caller invariant: each `Conn` is registered once.
- **Does not bound the per-server-id phone slice.** Per-server caps belong to the WS upgrade layer (#5).
- **Does not propagate binary loss to phones.** When `ReleaseServer` runs, the phones slice for that server-id is left in place. The owner of phones must `UnregisterPhone` (and choose whether to close them) when their server's binary is gone. This is intentional: the grace ticket (#8) needs phones to remain reachable across the 30-second disconnect window.
- **Does not validate `serverID` content.** Length, charset, prefix — owned by `x-pyrycode-server` header validation in #4. The registry treats any string as an opaque map key.

## Adversarial framing

The registry has no networking and no direct exposure to adversarial input. Adversaries reach it only through future tickets (#4/#5) that pass strings and `Conn` handles into its API. Notes for the layers above:

- **`serverID` of pathological size or content** is harmless to the registry (just a map key) but the WS upgrade layer must cap header lengths.
- **`Conn.ConnID()` returning unstable values** would cause `UnregisterPhone` to remove the wrong phone (or none). Documented as a contract on the `Conn` interface; violation is a leak, not a cross-tenant data leak.
- **Repeated `RegisterPhone` for the same server-id** is unbounded memory growth, gated only by the requirement that a binary holds the slot. Per-server connection caps are #5's job.
- **Slow `ConnID` blocking the write lock** is the cost of the documented non-blocking contract; copying the slice and scanning outside the lock would create a TOCTOU window where a phone could be added between snapshot and removal.

## Testing

`internal/relay/registry_test.go`, `package relay`. Tests use a minimal `fakeConn` (id, sent buffer, closed flag — no synchronisation; the race test gives each goroutine its own fakes).

Functional cases (one subtest per AC bullet):

- `ClaimServer` first-wins / second-conflicts; `BinaryFor` still resolves to the first conn.
- `ReleaseServer` reclaim — release returns `true`, second release of an unheld id returns `false`, claim again with a different conn succeeds.
- `RegisterPhone` requires a binary (`ErrNoServer` until `ClaimServer`).
- `UnregisterPhone` removes by `ConnID`, leaves siblings intact, no-op on unknown id.
- `PhonesFor` snapshot isolation: mutating the returned slice doesn't affect the registry; later registrations don't appear in earlier snapshots.
- `Counts` across the full lifecycle including the `ReleaseServer` orphan case (`(0, 1)` while phones survive a release).
- `UnregisterPhone` deletes the empty-slice map entry.

Race coverage: one `TestRegistry_RaceFreedom` hammers `ClaimServer` / `BinaryFor` / `RegisterPhone` / `PhonesFor` / `UnregisterPhone` / `Counts` / `ReleaseServer` from 32 goroutines × 200 ops over four contended server-ids. The test deliberately ignores returned errors — `ErrServerIDConflict` under contention is expected behaviour, not a failure. The point is the absence of `DATA RACE` reports.

`make test` runs `-race` by default. The `-count=20` invocation in the AC is a manual stress command, documented in the test's doc comment, not a knob in the test code:

```sh
go test -race -count=20 -run TestRegistry_RaceFreedom ./internal/relay
```

## Related

- [ADR-0003: Connection registry as a passive store](../decisions/0003-connection-registry-passive-store.md) — single RWMutex, snapshot returns, no callbacks, why grace logic lives outside.
- [Routing envelope](routing-envelope.md) — the wrapper used by frame forwarding (#6) once the registry is wired up.
- [Architecture overview](../../architecture.md) — where the registry fits in the data flow.
