# #115 — Stop counting a gracing binary in `Counts()`

Short plan (one guard in one getter; no new type, state or failure mode).

## Files read

- `internal/relay/registry.go` → `Registry.Counts` — the one function that changes.
- `internal/relay/registry.go` → `armGraceLocked`, `replaceBinaryLocked`, `handleGraceExpiry` — `r.timers[serverID]` is present exactly while a grace window is pending; reclaim/takeover and expiry delete it under `r.mu`.
- `internal/relay/registry.go` → `ScheduleReleaseServer` — may arm a timer for a server-id no binary holds, so the count cannot be `len(binaries) - len(timers)`.
- `internal/relay/healthz.go`, `internal/relay/metrics_connections.go` → the two `Counts()` consumers; neither changes.
- `internal/relay/registry_test.go` → `TestCounts_AcrossLifecycle`, `TestClaimServer_ReclaimDuringGrace_EvictsPhonesWith4404` — existing `Counts` expectations.
- `docs/knowledge/features/connection-count-gauges.md` — gauges are pull-based over `Counts()`, so fixing `Counts` fixes both surfaces.

In-flight overlap: `origin/feature/127` touches `registry.go`, but #127 is closed and its change is already on main; no dependency.

## Change

`Counts()` currently returns `len(r.binaries)`. It will instead subtract every server-id that is both in `r.binaries` and has a pending entry in `r.timers` (iterate `r.timers`, decrement when the id is also in `r.binaries`), still under the existing `RLock`. A timer armed for an unheld id is therefore not subtracted. Phones are counted as before. Reclaim/takeover delete the timer in `replaceBinaryLocked`, so the new binary counts again; expiry deletes both entries, so nothing changes there. The doc comment on `Counts` is updated to say a gracing binary is excluded. `BinaryFor`, `Snapshot` and routing are untouched: the gracing entry still exists for them.

## Testing strategy

New registry unit test `TestCounts_ExcludesGracingBinary`: claim `s1` and `s2`, register a phone on `s1`; `ScheduleReleaseServer("s1", time.Hour)` → `Counts() == (1, 1)`; `ClaimServer("s1", …)` reclaim → binary counts again (phones evicted per #127, so `(2, 0)`); separately `TakeoverServer` during grace → counted again; and `ScheduleReleaseServer` on an unheld id does not drive the count below the claimed binaries. The existing `(2,1)` expectation in `TestClaimServer_ReclaimDuringGrace_EvictsPhonesWith4404` must still hold unchanged.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/connection-registry.md` § API / invariants on `Counts`: state that a binary in its grace window is excluded from `binaries`.
- `docs/knowledge/features/connection-count-gauges.md`: `pyrycode_relay_connected_binaries` excludes gracing binaries.
