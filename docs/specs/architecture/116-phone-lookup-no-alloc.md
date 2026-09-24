# #116 — Zero-alloc phone lookup on the binary→phone forward path

## Files read

- `internal/relay/registry.go` → `Registry.PhonesFor`, `Registry.BinaryFor`, `Registry.UnregisterPhone`: `PhonesFor` allocates and copies a snapshot. `BinaryFor` is the one-method accessor to mirror. `UnregisterPhone` already scans the live slice by `ConnID()` under the lock.
- `internal/relay/forward.go` → `StartBinaryForwarder`: its conn_id resolution loop calls `PhonesFor` once per frame. This is the only production caller of `PhonesFor`. Tests remain its other callers, so it stays.
- `internal/relay/registry_test.go` → `fakeConn`, `TestPhonesFor_SnapshotIsolation`: the fixture and the pattern for the new tests.
- `internal/relay/forward_test.go`: the existing forwarder tests, including the unknown-conn_id log-and-continue path, which must pass unchanged.

Overlap: `origin/feature/127` touches `registry.go` in `ClaimServer` (its work is already on main). That is a different function, so it is not a dependency.

## Change

Add `func (r *Registry) PhoneFor(serverID, connID string) (Conn, bool)`, placed next to `BinaryFor`. It takes `r.mu.RLock`, ranges over the live `r.phones[serverID]` slice and returns the first `Conn` whose `ConnID()` equals `connID`. It returns `(nil, false)` for an unknown server-id or conn_id and allocates nothing. `StartBinaryForwarder` replaces its `PhonesFor` scan loop with `phone, ok := reg.PhoneFor(serverID, env.ConnID)`, and `!ok` takes the existing `binary_forwarder_unknown_conn_id` branch unchanged. The scan is bounded by the per-server-id phone cap (`maxPhones`), so no conn_id index is added: an index would have to be kept in sync on the register, unregister, reclaim-eviction and grace-expiry paths. `PhonesFor` and its doc stay as they are.

Returning the `Conn` after the read lock is released has the same semantics as the old snapshot. The phone may be unregistered concurrently, and the forwarder already tolerates that through `Enqueue`/`Send` errors.

## Testing strategy

New tests in `registry_test.go`:

- `TestPhoneFor_FindsRegisteredPhone`: with two phones registered, each conn_id returns its own `Conn`, an unknown conn_id returns `(nil, false)`, and an unknown server-id returns `(nil, false)`.
- `TestPhoneFor_ZeroAllocs`: `testing.AllocsPerRun` over a hit and a miss returns 0.

The forwarder change is covered by the existing `forward_test.go` suite without edits.

## Documentation handoff

The ticket names none. Pending for the documentation stage: optionally mention `PhoneFor` next to `PhonesFor` in the connection-registry feature doc.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings. `env.ConnID` comes from the binary's envelope, which is parsed at the envelope boundary before `StartBinaryForwarder` sees it. It is used only as a string comparison key against `ConnID()`, exactly as before. The payload stays `json.RawMessage` (`env.Frame`), and the lookup never touches it.
- [Tokens, secrets] No findings. No credential flows through the lookup.
- [File operations] No findings. No file I/O.
- [Subprocess] No findings. None added.
- [Crypto] No findings. The conn_id comparison is a routing lookup against a relay-assigned id, not a secret compare, so constant-time comparison is not needed. This is the same comparison `UnregisterPhone` and the old loop do.
- [Network & I/O] No findings. No new socket read. The scan is bounded by the per-server-id phone cap, and the ticket removes one allocation per frame, which reduces binary-driven GC pressure. There is no new fan-out.
- [Logs] No findings. The existing log call is unchanged, and no key is added.
- [Concurrency] No findings. The lookup takes only `r.mu.RLock`, and no other lock is held. The forwarder hooks, which must not take `r.mu`, are not involved. After the unlock, the returned `Conn` has the same lifetime semantics as the element of the old snapshot.
- [Threat model] No findings. It changes no endpoint, dependency or deploy target, and does not affect single-instance behaviour.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
