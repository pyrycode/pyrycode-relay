# Listener-cap fixture endpoint reservations (#206)

## Files read

- `cmd/pyrycode-relay/listener_cap_test.go` → `startCappedRelay`, `capAddr`, `TestRun_ListenerCapPublicModes`: current sequential address selection and the cap, metrics and HTTP-01 assertions to preserve.
- `cmd/pyrycode-relay/main_e2e_test.go` → `freePort`: closes each ephemeral listener before returning its address, allowing subsequent selections to alias.
- `docs/PROJECT-MEMORY.md` → project conventions: tests remain local to the fixture; production routing, logging and admission contracts are unaffected.
- `docs/knowledge/INDEX.md` and `docs/knowledge/features/connection-cap.md` → Testing: completed responses prove admission, and HTTP-01 and metrics must remain independent.
- `Makefile` → `check`, `vet`, `test`: required pre-push check and vet targets.
- Commit `5987beb02b12bc97fbbe56385557aaa127bebeff` → `reserveCapAddrs`: reuse its fixture-only reservation repair, with regression coverage extended from three to both two and three endpoints.

## Context

Separate `capAddr` calls can return the same released loopback endpoint, causing a false listener-admission failure. Fix the shared fixture used by insecure, HTTPS and PROXY HTTPS tests. This is one deliverable, with approximately 150–180 written lines including this plan, zero production files or exported types, one existing fixture call site updated, two acceptance criteria and one allocation-error branch. The #136 analogue wrote 68 test-code lines; this adds a reservation helper and lifecycle proof as well.

Overlap: #167 already carries the referenced fixture repair in `listener_cap_test.go`. Reuse that local change so integration converges; #167 depends on this repair and retains its fresh dispatcher gates and verifier judgment after integration. No new API from #167 is needed. No decision record is needed.

## Design

Add test-only `reserveCapAddrs(t *testing.T, count int) ([]string, func())`. It binds all requested addresses on `127.0.0.1:0`, retains every listener, and returns their addresses plus a release function registered with `t.Cleanup` before allocation starts.

`startCappedRelay` requests two endpoints in insecure mode and three in either secure mode. Assign public and metrics from the first two, and HTTP-01 from the third when secure. Keep reservations through certificate, transport and argument setup; release immediately before starting the existing `run` goroutine. Do not retry startup or alter the existing readiness, cap, metrics, HTTP-01 or shutdown assertions. Keep `capAddr` for unrelated single-address callers.

## Concurrency model

Reservation selection and release run synchronously on the test goroutine. No new goroutines or shared globals. The existing relay goroutine and its cancel/join cleanup remain as they are. All reservations are closed before the relay attempts to bind them; fallback cleanup closes only the original listeners.

## State transitions and identity reuse

| Event | Contract | Race-detector test |
| --- | --- | --- |
| Selecting two or three endpoints | All are distinct loopback addresses and unavailable to another bind while held. | `TestReserveCapAddrs/2`, `TestReserveCapAddrs/3` |
| Releasing reservations | Every selected endpoint can be rebound, holding all rebound listeners together. | `TestReserveCapAddrs/2`, `TestReserveCapAddrs/3` |
| Repeated release after endpoint reuse | Closing old reservations again does not release rebound listeners. | `TestReserveCapAddrs/2`, `TestReserveCapAddrs/3` |
| Starting each serving mode | Cap enforcement, metrics isolation and independent HTTP-01 remain valid. | `TestRun_ListenerCapPublicModes` |

## Error handling

A reservation bind failure fails the test; registered cleanup closes reservations already obtained. Selection adds no retry or silent fallback. Release follows the existing fixture's listener-close convention. Production errors and admission behaviour stay unchanged.

## Testing strategy

Write the two-count reservation/rebind regression first. Exercise it against the old sequential `capAddr` selection through a temporary helper, proving failure because the addresses are already bindable rather than waiting for accidental OS reuse. Then implement the held reservations and wire the fixture. Temporarily release reservations before returning to verify the held-address assertion detects early release; restore the intended helper before gates.

Run focused uncached race checks for the new regression and existing listener-cap tests, preserving all three modes. Record two complete `go test -race -count=1 ./...` runs and `make vet`, including every failure, in the PR. Also run `make check` before push as required by the checkout instructions and build `./cmd/pyrycode-relay`. Keep raw logs under `/tmp/builder-relay-206/`.

## Open questions

None. Socket reservations prevent aliasing within one selection set; the brief release-to-startup window remains because `run` owns its listeners. This ticket does not change production listener ownership or remove interference from other processes during that window.

## Documentation handoff

Pending for the documentation stage: `docs/knowledge/features/connection-cap.md` → Testing: document holding two/three loopback reservations through fixture setup and the deterministic reservation/rebind regression. No protocol or deployment changes.
