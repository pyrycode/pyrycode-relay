# Spec — Connection registry (#3)

## Files to read first

- `internal/relay/envelope.go` — sentinel-error idiom, doc-comment style, `package relay` framing. The new file mirrors this shape.
- `internal/relay/envelope_test.go` — `package relay` (not `_test`) so tests can `errors.Is` against unexported helpers; table-driven rejections via `errors.Is`.
- `internal/relay/doc.go` — package doc explicitly names "per-server connection registry" as a routing-core concern. The new file is its second pillar.
- `docs/architecture.md:18-23` — what the relay does: 1:1 server-id → binary, 1:N server-id → phones, frame forwarding. The registry's invariants flow from these bullets.
- `docs/PROJECT-MEMORY.md:22-26` — established patterns: sentinel errors via `errors.Is`, stdlib only, package-internal tests.
- `Makefile:13-14` — `make test` runs `go test -race ./...`. Race coverage isn't a CI nicety here; it's the default mode every PR is judged against.
- `go.mod` — Go 1.26.2, stdlib only. `sync` and `errors` are the only imports the production file needs.

## Context

The relay's routing core needs a single canonical structure that owns "who is connected as what for which server-id." Every routing-layer ticket downstream — `/v1/server` upgrade (#4), `/v1/client` upgrade (#5), frame forwarding (#6), heartbeat (#7), grace period (#8), health (#10) — reads or writes through it. Without this type each downstream ticket would invent its own ad-hoc map and lock; with it they share one contract and one race-tested implementation.

Two non-negotiable shapes (per the protocol spec and `docs/architecture.md`):

1. **Server-id → binary is 1:1, first-claim-wins.** A second binary claiming a held server-id is a `4409` conflict. The 30-second grace period on disconnect is a separate ticket (#8) and **wraps** the registry rather than living inside it; the registry itself implements immediate semantics (claim/release with no timer).
2. **Server-id → phones is 1:N, gated on a binary holding the slot.** A phone whose server-id has no binary is a `4404` no-server. Phones can only be registered when the slot is held.

The registry holds **opaque connection handles** (a `Conn` interface) so the WS upgrade tickets can pick the WebSocket library independently and tests can use mocks. Concrete `Conn` implementations land in #4 and #5; this ticket ships only the interface.

## Design

### Package & files

- New file: `internal/relay/registry.go`
- New test file: `internal/relay/registry_test.go`
- Both `package relay` — same convention as `envelope.go` / `envelope_test.go`. Tests can branch on unexported state via `errors.Is` and reach package-private helpers if needed.

No edits to `doc.go`, `envelope.go`, `cmd/pyrycode-relay/main.go`, or `docs/`. The registry has no consumers yet; wiring lands in #4/#5.

### The `Conn` interface

```go
// Conn is the registry's view of a WebSocket connection. Real implementations
// land in the /v1/server (#4) and /v1/client (#5) upgrade tickets; tests use
// mocks.
//
// All three methods MUST be safe to call from any goroutine. ConnID and Send
// MUST NOT block on locks the registry itself takes — the registry never
// invokes Conn methods while holding its own lock, but callers iterating a
// PhonesFor snapshot may.
type Conn interface {
    // ConnID returns the relay-assigned connection identifier. Stable for the
    // lifetime of the connection. The format is decided in a later ticket;
    // the registry treats it as an opaque key for slice lookups.
    ConnID() string

    // Send delivers a fully-formed wire message. Implementations are
    // responsible for serialising writes to the underlying socket. Errors
    // are returned to the caller; the registry does not interpret them.
    Send(msg []byte) error

    // Close terminates the connection. Idempotent. Safe to call concurrently
    // with Send (the implementation must handle the race).
    Close()
}
```

Three methods, named precisely after the AC. The interface is defined in the registry's package because consumers (frame forwarding, etc.) all live in `internal/relay`; defining at the consumer is moot when there is one consumer package. (Keeping the interface in `relay` also avoids an import cycle once `cmd/pyrycode-relay/main.go` wires up real implementations from a `relay/ws` subpackage in #4/#5.)

### The `Registry` type

```go
// Registry maps server-ids to a single binary connection (1:1) and to a list
// of phone connections (1:N). All methods are safe to call concurrently.
//
// The registry holds Conn handles by reference and never inspects their
// internal state. It does not call any Conn method while holding its own
// lock — close/send-fan-out is the caller's responsibility, performed
// against a PhonesFor snapshot.
type Registry struct {
    mu       sync.RWMutex
    binaries map[string]Conn
    phones   map[string][]Conn
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry
```

One `sync.RWMutex` covers both maps. Sharding by server-id is a possible later optimisation but irrelevant at v1 — the relay handles tens to hundreds of concurrent connections, not millions, and the lock is held for sub-microsecond critical sections (map lookup, slice append/copy). Two separate locks would invite ordering bugs (always-A-then-B) for no measurable gain.

### Methods

```go
// ClaimServer registers conn as the binary for serverID. First-claim-wins:
// the second concurrent caller for the same serverID receives
// ErrServerIDConflict. The conflicting caller's conn is left untouched
// (registry does not Close it).
//
// Use ReleaseServer to free the slot. ClaimServer does NOT inspect or
// modify the phones slice for serverID; phones registered before a release
// remain in the map until they are explicitly UnregisterPhone'd by their
// owner, or removed by a higher-layer cleanup (#8 grace period).
func (r *Registry) ClaimServer(serverID string, conn Conn) error

// ReleaseServer removes the binary entry for serverID. Returns true if a
// binary held the slot, false otherwise. Does NOT close the connection;
// caller owns conn lifecycle. Does NOT touch the phones slice — see the
// "Open questions" section for why this is intentional and how #8 will
// layer cleanup on top.
func (r *Registry) ReleaseServer(serverID string) (released bool)

// RegisterPhone appends conn to the phones slice for serverID. Returns
// ErrNoServer if no binary currently holds serverID. Does not deduplicate
// by ConnID — the caller is responsible for not registering the same
// phone twice.
func (r *Registry) RegisterPhone(serverID string, conn Conn) error

// UnregisterPhone removes the phone whose ConnID equals connID from the
// slice for serverID. No-op if no matching phone is present (including
// when no slice exists for serverID). Does NOT close the connection;
// caller owns conn lifecycle.
//
// When the slice becomes empty as a result, the entry is deleted from the
// phones map so Counts and PhonesFor see no orphaned empty slices.
func (r *Registry) UnregisterPhone(serverID string, connID string)

// BinaryFor returns the binary holding serverID, if any.
func (r *Registry) BinaryFor(serverID string) (Conn, bool)

// PhonesFor returns a snapshot of the phones registered for serverID.
// The returned slice is freshly allocated; the caller may iterate, append,
// or otherwise mutate it without affecting the registry's internal state
// or holding any registry lock. Returns nil for an unknown serverID.
//
// The Conn handles inside the slice are the same references the registry
// holds; calling Send/Close on them affects the live connection.
func (r *Registry) PhonesFor(serverID string) []Conn

// Counts returns the number of binaries currently claimed and the total
// number of phone connections summed across all server-ids. For #10's
// health endpoint.
func (r *Registry) Counts() (binaries, phones int)
```

### Sentinel errors

```go
var (
    // ErrServerIDConflict is returned by ClaimServer when serverID is
    // already held by another binary. Maps to WS close code 4409.
    ErrServerIDConflict = errors.New("relay: server-id already claimed")

    // ErrNoServer is returned by RegisterPhone when no binary holds the
    // requested serverID. Maps to WS close code 4404.
    ErrNoServer = errors.New("relay: no binary for server-id")
)
```

Same idiom as `envelope.go`: callers use `errors.Is`. The mapping to close codes is informational; the registry doesn't know about WebSockets and won't return close codes itself — the WS upgrade handlers do that translation.

### Algorithms

**ClaimServer** — write lock, single-check map, set or error.

```
mu.Lock(); defer mu.Unlock()
if _, ok := binaries[serverID]; ok { return ErrServerIDConflict }
binaries[serverID] = conn
return nil
```

**ReleaseServer** — write lock, delete-and-report.

```
mu.Lock(); defer mu.Unlock()
if _, ok := binaries[serverID]; !ok { return false }
delete(binaries, serverID)
return true
```

**RegisterPhone** — write lock, gate on binary presence, append.

```
mu.Lock(); defer mu.Unlock()
if _, ok := binaries[serverID]; !ok { return ErrNoServer }
phones[serverID] = append(phones[serverID], conn)
return nil
```

**UnregisterPhone** — write lock, linear scan by ConnID, remove preserving order is unnecessary; use swap-and-truncate.

```
mu.Lock(); defer mu.Unlock()
slice, ok := phones[serverID]
if !ok { return }
for i, c := range slice {
    if c.ConnID() == connID {
        slice[i] = slice[len(slice)-1]
        slice[len(slice)-1] = nil  // GC hygiene; Conn may be a heavy struct.
        slice = slice[:len(slice)-1]
        if len(slice) == 0 {
            delete(phones, serverID)
        } else {
            phones[serverID] = slice
        }
        return
    }
}
```

ConnID is called *while holding the lock* — that's allowed because the contract on `Conn` says `ConnID` must not block (it's a getter). `Send` and `Close`, which can block on the network, are never called from inside any registry method.

Linear scan is correct for v1: a single server-id holds a small number of phones (typically 1–3 per user across devices). If profiling later shows hot spots, switch to `map[connID]Conn` per server-id; the API doesn't change.

**BinaryFor** — read lock, map lookup.

```
mu.RLock(); defer mu.RUnlock()
c, ok := binaries[serverID]
return c, ok
```

**PhonesFor** — read lock, copy.

```
mu.RLock(); defer mu.RUnlock()
src := phones[serverID]
if len(src) == 0 { return nil }
out := make([]Conn, len(src))
copy(out, src)
return out
```

The `nil` return on empty is deliberate: callers that range over the result iterate zero times either way; the caller never has to distinguish "unknown server-id" from "known with zero phones" because `RegisterPhone` makes those the same state (the registry only stores non-empty slices, courtesy of `UnregisterPhone`'s `delete` step).

**Counts** — read lock, len + summed iteration.

```
mu.RLock(); defer mu.RUnlock()
b := len(binaries)
p := 0
for _, s := range phones { p += len(s) }
return b, p
```

Iteration is acceptable: `Counts` is called by the health endpoint, not on the hot path. Tracking running totals is a premature optimisation.

### Concurrency model

- **One `sync.RWMutex` for the whole registry.** All write methods take `Lock`; all read methods take `RLock`.
- **No fairness assumptions.** Go's `sync.RWMutex` does not guarantee starvation-freedom for writers; the registry's API doesn't expose that. The five-method surface area is small enough that lock contention is unobserved in practice.
- **No callbacks, no channels.** The registry is a passive store. Notification of binary loss / new phone arrival happens elsewhere (the WS upgrade handlers and #8's grace timer).
- **Conn methods are never invoked under the lock.** Callers wanting to broadcast a frame to all phones for a server-id must:
  1. Call `PhonesFor` to get a snapshot.
  2. Iterate the snapshot and call `Send` on each — no registry lock held.
  This pattern is the entire reason `PhonesFor` returns a copy.
- **`ConnID()` IS called under the lock** by `UnregisterPhone`. Documented in `Conn`'s doc comment as a non-blocking, lock-free operation.

### Error handling

Two sentinels (`ErrServerIDConflict`, `ErrNoServer`). No wrapping with `fmt.Errorf` is needed — there is no underlying error to chain. No panics. No logging. Library code; the caller chooses the response.

### Lifecycle expectations the registry does NOT enforce

These are listed so the developer doesn't add defensive code for them:

- The registry does not close `Conn` handles on its own. `ReleaseServer` and `UnregisterPhone` remove map entries but never call `Close`. Callers (WS upgrade handlers) own the connection lifetime.
- The registry does not deduplicate phones by `ConnID` on `RegisterPhone`. Caller invariant: each `Conn` is registered once.
- The registry does not bound the per-server-id phone slice. Per-server connection limits belong to the WS upgrade layer (rate limiting / cap headers, not this ticket's concern).
- The registry does not propagate binary loss to phones. When `ReleaseServer` runs, phones for that server-id remain in the map — the layer that owns phones must `UnregisterPhone` them (and choose whether to close them) when their server's binary is gone. This is intentional: the grace ticket (#8) needs that decoupling to keep phones alive across a 30-second window.

## Testing strategy

`internal/relay/registry_test.go`, `package relay`, `testing` only.

### Mock `Conn`

A minimal in-package test helper:

```go
type fakeConn struct {
    id     string
    sent   [][]byte  // optional, for forwarding tests in #6; harmless here.
    closed bool
}

func (c *fakeConn) ConnID() string         { return c.id }
func (c *fakeConn) Send(msg []byte) error  { c.sent = append(c.sent, msg); return nil }
func (c *fakeConn) Close()                 { c.closed = true }
```

`fakeConn` has no synchronisation. The tests do not concurrently mutate the same `fakeConn` — the registry's race tests register/unregister distinct fakes per goroutine (or read-only fakes via `BinaryFor`/`PhonesFor`). This is a deliberate scope cut: the registry's race coverage proves the registry is race-free with well-behaved Conns, not that fakeConn is race-free.

### Functional tests (subtest names map 1:1 to AC bullets)

Each is a focused unit test using `errors.Is` for error assertions; no string compare.

1. **`TestClaimServer_FirstWinsSecondConflicts`** — claim succeeds, second claim with a different `Conn` returns `ErrServerIDConflict`, original `BinaryFor` lookup still resolves to the first conn.
2. **`TestReleaseServer_ReclaimSucceeds`** — claim, release (returns `true`), claim again with a different conn (returns `nil`), `BinaryFor` resolves to the new conn. Then release of an unheld id returns `false`.
3. **`TestRegisterPhone_RequiresBinary`** — `RegisterPhone("s1", phone)` with no binary returns `ErrNoServer`; after `ClaimServer("s1", bin)`, the same call succeeds.
4. **`TestUnregisterPhone_RemovesByConnID`** — register three phones, `UnregisterPhone` the middle one, `PhonesFor` returns exactly the other two and the removed phone is absent. `UnregisterPhone` with an unknown connID is a no-op.
5. **`TestPhonesFor_SnapshotIsolation`** — register two phones, take the snapshot, mutate the snapshot (overwrite an element with a different `*fakeConn`), assert `PhonesFor` again still returns the original two. Also: register a third phone after the snapshot was taken, assert the original snapshot still has length 2.
6. **`TestCounts_AcrossLifecycle`** — assert `(0,0)` initial, after `ClaimServer` `(1,0)`, after `RegisterPhone × 2` `(1,2)`, after `UnregisterPhone × 1` `(1,1)`, after `ReleaseServer` `(0,1)` (orphan phone retained — see open questions), after the orphan's `UnregisterPhone` `(0,0)`.
7. **`TestUnregisterPhone_RemovesEmptySlice`** — register one phone, unregister it, assert `PhonesFor` returns `nil` AND the underlying `phones` map has no entry for that server-id (verifiable via `Counts` returning 0 phones; this AC is structural so a length check is sufficient).

### Race coverage

One test, focused on hammering the public API under `-race`:

```go
func TestRegistry_RaceFreedom(t *testing.T) {
    t.Parallel()
    r := NewRegistry()

    const goroutines = 32
    const opsPer = 200

    var wg sync.WaitGroup
    wg.Add(goroutines)
    for g := 0; g < goroutines; g++ {
        g := g
        go func() {
            defer wg.Done()
            sid := fmt.Sprintf("s-%d", g%4)  // four contended server-ids
            bin := &fakeConn{id: fmt.Sprintf("b-%d", g)}
            for i := 0; i < opsPer; i++ {
                _ = r.ClaimServer(sid, bin)
                _, _ = r.BinaryFor(sid)
                _ = r.RegisterPhone(sid, &fakeConn{id: fmt.Sprintf("p-%d-%d", g, i)})
                _ = r.PhonesFor(sid)
                r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
                _, _ = r.Counts()
                _ = r.ReleaseServer(sid)
            }
        }()
    }
    wg.Wait()
}
```

The ticket asks for `-race -count=20`. The Makefile already runs `-race`; `count=20` is a CI-level invocation, not a test code knob. **The developer should NOT add `-count` flags to the test file.** Document the manual command in the test's doc comment so future contributors know how to stress it locally:

```go
// Run with: go test -race -count=20 -run TestRegistry_RaceFreedom ./internal/relay
```

The race test deliberately ignores returned errors: a `ClaimServer` that returns `ErrServerIDConflict` is expected behaviour under contention, not a test failure. The point is the absence of `DATA RACE` reports from the runtime.

### What we deliberately do not test

- **`Conn.Send` / `Conn.Close` invocation under lock** — the registry never calls them; there is no behaviour to assert. A "lock-not-held" assertion would require introspecting runtime state, which Go does not portably expose.
- **Per-server-id phone bound** — out of scope; later ticket.
- **`Conn.ConnID()` panic safety** — registry assumes it doesn't panic; matches the rest of the codebase's "trust your interfaces" stance.

## Open questions

1. **Should `ReleaseServer` clear the phones slice for that server-id?** The AC says it removes the binary entry; nothing about phones. The chosen semantic is **no, leave phones in place** — orphan phones survive a release until their owner unregisters them. Rationale: the grace period (#8) needs phones to remain reachable across a 30-second binary disconnect window, and the simplest way to allow that without coupling #8 into the registry is to keep release immediate-and-narrow. If a future ticket decides orphan phones should be auto-closed on release, it adds a `ReleaseServerAndClosePhones` method or wraps `ReleaseServer` in higher-layer logic — the public API in this ticket stays minimal.
2. **`Counts` race-vs-snapshot consistency.** `Counts` is read under `RLock` and is internally consistent for one call. Two concurrent calls may observe different values — that's fine for a health endpoint; no caller needs cross-call consistency.
3. **Should we keep an `Empty()` or `Has(serverID)` helper?** No. `BinaryFor` already returns `(Conn, bool)`; that's the existence test. Adding more helpers is anticipatory.

## Out of scope (re-stated, for the developer)

- No edits to `cmd/pyrycode-relay/main.go`. The registry has no consumers in this ticket.
- No WS upgrade, no header validation, no `conn_id` generation — see #4, #5, and the dedicated conn-id ticket.
- No grace period, no heartbeat, no metrics beyond `Counts`. See #7, #8, #10.
- No external dependencies. `sync`, `errors`, plus `fmt`/`testing`/`sync` for tests.

## Done means

- `internal/relay/registry.go` exists with `Conn`, `Registry`, `NewRegistry`, the seven methods named in the AC, and the two sentinel errors. Every exported symbol carries a doc comment that names its concurrency contract (whether the lock is held, what the caller may/may not do with returned values).
- `internal/relay/registry_test.go` covers the seven functional cases plus the race-freedom test. The race test runs clean under `go test -race -count=20 -run TestRegistry_RaceFreedom ./internal/relay`.
- `make vet`, `make test`, `make build` all clean from the repo root.
- One commit on `feature/3`: `feat(relay): connection registry (#3)`.

---

## Security review (security-sensitive label)

### Threat surface for THIS ticket

The registry has no networking and no direct exposure to adversarial input. Adversaries only reach it through future tickets (#4/#5 WS upgrades) that pass strings (`serverID`, `connID`) and `Conn` handles into its API. The review below confirms the registry's API does not turn well-behaved adversarial input from the WS layer into a security issue — it cannot vouch for the WS layer itself.

### Categories walked

- **Trust boundaries.** The registry trusts (a) `serverID` is whatever the WS upgrade handler decided to accept (header validation is a separate ticket; until it lands, the registry does not pretend to validate IDs); (b) `connID` returned by `Conn.ConnID()` is unique among connections registered for the same server-id (caller invariant — the conn-id generation ticket guarantees this); (c) `Conn` implementations honour the methods' contracts (non-blocking `ConnID`, idempotent `Close`). Each trust is documented in the doc comment of the API surface that depends on it. **Finding:** registry does not validate `serverID` content (length, charset). This is correct for this ticket — validation belongs at the WS upgrade layer (`x-pyrycode-server` header check). The registry's behaviour is well-defined for any string, including empty strings or strings with embedded NULs: they're just map keys. Enforced at the boundary (`internal/relay/registry.go`), not deeper.
- **Resource exhaustion.** Per-server-id phone slice is unbounded. An adversary who could repeatedly call `RegisterPhone` for the same server-id could grow the slice without bound and consume memory. **Finding:** acceptable in this ticket because (a) `RegisterPhone` is gated on `ErrNoServer`, so an adversary first needs a binary holding the slot — which means the adversary has compromised the binary or its owner connected the binary themselves; (b) the WS upgrade layer (#5) is the natural place to enforce per-server connection caps. Mentioned in "Lifecycle expectations" so #5's developer doesn't assume the registry caps phones.
- **Race conditions / TOCTOU.** ClaimServer's "check then set" is atomic under the write lock — a second claimant cannot slip between `binaries[serverID]` lookup and assignment. RegisterPhone's "check binary then append" is also atomic under the write lock. Reads (`BinaryFor`, `PhonesFor`, `Counts`) are atomic under the read lock. There is no API that returns a value the caller is supposed to act on before re-entering the registry — which is what would create a TOCTOU window. **Finding:** the API shape eliminates TOCTOU at this layer.
- **Lock-order / deadlock.** Registry takes only its own `mu`. Never invokes `Send` or `Close` while holding the lock. `ConnID` is invoked under the lock by `UnregisterPhone` — documented contract on `Conn` says `ConnID` must not block. **Finding:** lock graph is a single node; cannot deadlock with itself, cannot deadlock with caller locks because no callbacks are made under the lock except the `ConnID` getter.
- **Snapshot isolation / data leaks via aliasing.** `PhonesFor` returns a copy via `make` + `copy`. Callers cannot observe in-progress mutation by dereferencing slice header bytes. The Conn handles inside the copy are still the same references the registry holds — that's by design (broadcast pattern), and the Conn's own state is not registry state. **Finding:** matches the AC's stated invariant; tested explicitly in `TestPhonesFor_SnapshotIsolation`.
- **Memory leaks under adversarial workload.** `UnregisterPhone` deletes the phones map entry when the slice empties, preventing growth of orphaned `[]Conn{}` entries. Released binaries are deleted from the map. The only documented leak path is "phones survive `ReleaseServer`" — addressed in open question (1) and intentionally left for the layer that owns phone lifetime.
- **Information disclosure.** Counts returns aggregate ints; no per-server-id info. No method returns a Conn that wasn't already known to the caller (you can only retrieve Conns for server-ids you've already registered/claimed). **Finding:** API is not a sidechannel for enumerating other tenants.
- **Panics / nil-deref.** Maps are initialised in `NewRegistry`. No `*Conn` dereferences (interface, not pointer). No slice index past length. **Finding:** none observed.

### Adversarial framings considered

- *"What if an attacker sends a server-id of 100 KB?"* — Registry treats it as a map key. Memory cost is one allocation per unique key. Capping the header length is the WS upgrade layer's job; the registry does not need to second-guess.
- *"What if a malicious `Conn` impl returns different `ConnID()` values on each call?"* — `UnregisterPhone` linear-scans by ConnID; if `ConnID()` is unstable, the wrong phone (or no phone) may be removed. The `Conn` doc comment specifies "stable for the lifetime of the connection" — this is a contract the WS handler must honour. If a future Conn impl violates it, the bug manifests as a phone leak in the registry, not a security boundary failure (no other Conn's data is exposed).
- *"What if a binary disconnects without releasing?"* — Registry holds a stale `Conn` reference until something calls `ReleaseServer`. The grace ticket (#8) is responsible for this; the registry's contract makes no promises about liveness.
- *"What if two goroutines `UnregisterPhone` the same phone simultaneously?"* — Both take the write lock serially. The first finds and removes; the second finds nothing and is a no-op. No double-free, no panic.
- *"Slow `Conn.ConnID()` blocks `UnregisterPhone` while holding the lock and starves other callers."* — Yes, that's the cost of the documented contract. The `Conn` doc comment names `ConnID` as non-blocking; if a future impl violates that, fix the impl. The registry could copy the slice and scan outside the lock to defend against this, but that creates a new TOCTOU window (a phone could be added after the snapshot and erroneously not be removed). The cleaner contract — and the one this spec adopts — is "`ConnID` is a getter."

### Verdict: PASS

The registry's API and lock discipline do not introduce vulnerabilities under any adversarial framing examined. The findings flagged above are out-of-scope by design (per-server caps, header validation, conn-id generation) and are documented in the spec so downstream tickets pick them up. The `security-sensitive` label is justified by the registry's role on every routing path; the review is correspondingly thorough even though this ticket itself does no I/O.
