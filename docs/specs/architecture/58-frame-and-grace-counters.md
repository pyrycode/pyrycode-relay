# Spec — `frames_forwarded_total` + `grace_expiries_total` counters (#58)

## Files to read first

- `internal/relay/registry.go:154-189` — `ScheduleReleaseServer` arms the
  timer; `handleGraceExpiry` is the body that runs on `time.AfterFunc`'s
  goroutine. Line 177 is the pointer-identity guard whose success branch
  this spec hooks; the stale-fire `return` at line 179 must NOT touch the
  hook.
- `internal/relay/registry.go:56-79` — `Registry` struct + constructor.
  The new hook fields live here; constructor doesn't change.
- `internal/relay/forward.go:32-74` — `StartPhoneForwarder` loop. The
  successful-path is line 66 (`binary.Send(wrapped)` returns nil); the
  increment hook fires immediately after.
- `internal/relay/forward.go:110-158` — `StartBinaryForwarder` loop. The
  successful-path is line 150 (`phone.Send(env.Frame)` returns nil); the
  increment hook fires immediately after. The three `continue` paths
  (unmarshal err, unknown conn_id, phone send err) MUST NOT increment —
  re-read each branch to confirm the placement.
- `internal/relay/forward_test.go:118-160` — `fakeBinary` + `fakePhone`
  shapes the new metrics tests reuse via `package relay` access. Same
  package, no import or duplication needed.
- `internal/relay/forward_test.go:195-243` — `TestStartPhoneForwarder_ForwardsFramesBytewise`
  is the shape reference for the phone→binary success-path test: stage
  a binary in the registry via `ClaimServer`, drive frames into the
  fake phone, assert the binary received them. Adapt: also construct
  `*forwardMetrics`, set the hook on the registry, scrape and assert
  the counter.
- `internal/relay/forward_test.go:419-491` — three "binary forwarder
  drops + continues" tests (multiple-phones, unknown conn_id, malformed
  envelope, phone send err). The new test mirrors their shape but asserts
  the counter increment count, not the dropped-frame count.
- `internal/relay/registry_test.go:413-451` — `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles`.
  Shape reference for the new grace-stale-fire counter test: cancel-and-
  replace race window, assert the counter is unchanged on the stale fire,
  exactly +1 on the real eviction. The same `fakeConn` (same package) is
  reused without duplication.
- `internal/relay/metrics_connections.go` (whole file, ~50 lines) — the
  per-concern collector pattern this ticket follows. Constructor takes
  `prometheus.Registerer` + `*Registry`; registers against the registerer;
  holds no state beyond the metric descriptors. The forward + grace
  counter constructors mirror this shape.
- `internal/relay/metrics_connections_test.go:241-303` — `assertGauge`
  helper. The new test file's `assertCounter` helper has the same shape
  (scrape, substring-match a literal text-format line, e.g.
  `pyrycode_relay_grace_expiries_total 1`).
- `cmd/pyrycode-relay/main.go:97-110` — current metrics wiring block.
  Two new constructor calls (`NewForwardMetrics`, `NewGraceMetrics`) land
  between `NewConnectionsMetrics` (line 102) and the public-listener mux
  construction. Each call BOTH registers the counter against `metricsReg`
  AND wires the hook on `reg`.
- `docs/specs/architecture/61-connected-gauges.md` § *Why pull-based, not
  push-based* + § *Artefacts → metrics_connections.go* — pattern reference.
  This ticket's counters are push-shaped (counters of events, not
  gauges-of-state); the seam admits both shapes per #59's spec.
- `docs/specs/architecture/59-metrics-scaffolding.md` § *The architect's
  "package-vars-vs-struct" pick* — the per-concern collector struct pattern.
  Two files (one per concern) is the precedent; this ticket follows it
  with two prod files for two metrics.
- `docs/PROJECT-MEMORY.md` line 24 — *"Validate at the envelope boundary,
  not deeper."* Negatively relevant: the `direction` label is hard-coded
  to two strings (`phone_to_binary`, `binary_to_phone`); neither value is
  attacker-influenced — load-bearing for security review § Tokens.
- `docs/threat-model.md` § *Log hygiene* (extends to metric labels per
  #59's spec) — the `direction` label is the only label this ticket
  introduces, and its values are constants.

## Context

Third slice of the metrics rollout (split from #37). #59 landed the
registry scaffolding; #57 (concurrent) adds upgrade/register counters;
#61 added the connected-* gauges; #60 wired the loopback listener. This
ticket lands the **forwarder frame counter** (label `direction`) and the
**grace-expiry counter** (no labels).

The single load-bearing design choice the AC hands the architect:

> The frame counter increments on the hot path inside
> `StartPhoneForwarder` / `StartBinaryForwarder` — one increment per
> forwarded frame. … the increment goes per successful `Send`, never on
> the loop iteration.
>
> The grace-expiry counter fires inside the `time.AfterFunc` callback in
> `Registry.ScheduleReleaseServer` — increment after the stale-fire
> pointer-identity guard returns true (i.e. only on a real eviction,
> never on stale fires).

Both increment sites live in code that does NOT (and per ADR-0008 *Scope
of use* MUST NOT) import `prometheus`. So the design problem is: how to
plumb a `prometheus.Counter` to a call site inside `registry.go` and to
the forwarder body in `forward.go` without (a) importing prometheus into
those files and (b) starting a 10-call-site signature cascade through
`ClientHandler`, `ServerHandler`, `StartPhoneForwarder`, and every test
that constructs the handlers.

This spec picks **nil-safe hooks on `*Registry`**. The justification is
the bulk of § Design.

## Design

### Why hooks-on-Registry, not parameter threading

The two natural alternatives, with the cascade math each implies:

1. **Parameter threading** (`StartPhoneForwarder` takes a
   `*forwardMetrics`; `ClientHandler` takes one; `main.go` constructs
   and threads it through). Concrete cascade:
   - `ClientHandler` signature → 1 file + 4 call sites (3 tests + main.go)
   - `ServerHandler` signature → 1 file + 4 call sites (3 tests + main.go)
   - `StartPhoneForwarder` / `StartBinaryForwarder` signatures → 2 sites
     in forward_test.go
   - Total ≥ 10 call sites needing simultaneous updates, ≥ 7 files
     modified.
   - This is right at the edit-fan-out red line in
     `architect/CLAUDE.md` § 1. The rationalization "they're mechanical
     `, nil` appends" is exactly the smell the red line covers. Split or
     redesign.

2. **Package-level vars** (e.g. `var forwardCounter *prometheus.CounterVec`
   set by `NewForwardMetrics`, read by the forwarder). #59's spec rejects
   this category — a singleton-shaped seam forces one registry per
   process, which forbids per-test registries and per-test isolation
   under `-race`.

The adopted shape — **nil-safe `func()` hook fields on `*Registry`** —
avoids both problems:

- `registry.go` and `forward.go` import nothing new (the hooks are
  `func()`, not `prometheus.Counter`). ADR-0008's *Scope of use*
  boundary holds.
- No signature changes anywhere. Zero cascade through handlers, zero
  cascade through tests.
- `*Registry` is already the dependency every forwarder, handler, and
  test threads around the codebase. Adding three private function-pointer
  fields adds zero new surface to the call graph.
- Tests construct their own registry, their own counter, set the hook,
  and scrape. Existing tests are unaffected because all hooks default to
  nil (no-op on call).
- The grace counter site is structurally identical: the hook fires from
  inside `handleGraceExpiry`'s success branch, after the pointer-identity
  guard, ensuring stale fires do not increment.

The asymmetry-cost (Registry's surface gains hook fields it does not
"own") is a one-shot cost paid once for the whole metrics rollout. Any
future per-Registry-event counter (e.g. `phones_registered_total`) lands
the same way — one private hook + one setter + one nil-safe call site.

### Artefacts

Three modified files, three new files. Hook fields on `*Registry` are
private; the setter methods are exported because main.go (different
package) needs to call them. Counter values' `direction` label is a
hard-coded constant set { `phone_to_binary`, `binary_to_phone` } — no
external input reaches it.

#### 1. `internal/relay/registry.go` (modified)

- **Add three private fields** to the `Registry` struct (near the existing
  `mu`, `binaries`, `phones`, `timers` block):

  - `onPhoneForwarded func()` — invoked by `StartPhoneForwarder` after
    a successful `binary.Send`. Nil = no-op.
  - `onBinaryForwarded func()` — invoked by `StartBinaryForwarder` after
    a successful `phone.Send`. Nil = no-op.
  - `onGraceExpiry func()` — invoked by `handleGraceExpiry` after the
    pointer-identity guard's success branch. Nil = no-op.

  Doc comments on each: *"Set once at boot via `SetForwarderHooks` /
  `SetGraceExpiryHook` before either listener starts serving. Nil-safe
  by design. Called outside any lock so the hook body can do its own
  locking; the hook MUST NOT acquire `r.mu` (deadlock risk). Concrete
  hook bodies in `metrics_forward.go` and `metrics_grace.go` are pure
  `prometheus.Counter.Inc` / `.WithLabelValues(...).Inc` calls — the
  library handles its own atomicity, no relay-side lock involved."*

- **Add two setter methods** to `Registry`:

  ```go
  // SetForwarderHooks installs the two per-frame increment hooks. Either
  // (or both) may be nil for no-op. Call once at boot before any forwarder
  // goroutine runs; concurrent calls during serving are undefined.
  func (r *Registry) SetForwarderHooks(phone, binary func())

  // SetGraceExpiryHook installs the eviction-increment hook. May be nil
  // for no-op. Call once at boot before any ScheduleReleaseServer call.
  func (r *Registry) SetGraceExpiryHook(grace func())
  ```

  Bodies are a single assignment each. No lock needed (single-goroutine
  boot-time call); doc comment names the constraint.

- **Hook invocation in `handleGraceExpiry`**: at the end of the success
  branch, after `r.mu.Unlock()` and the phone-close `for` loop. The
  hook fires for every real eviction, never for stale fires (the guard
  returns before reaching this line). Shape:

  ```
  func (r *Registry) handleGraceExpiry(serverID string, self *graceEntry) {
      r.mu.Lock()
      if r.timers[serverID] != self { r.mu.Unlock(); return }
      // ... existing eviction body (delete + snapshot) ...
      r.mu.Unlock()

      for _, p := range snapshot { p.Close() }

      if h := r.onGraceExpiry; h != nil { h() }    // <- new line
  }
  ```

  The hook is the *last* thing the function does. Reading `r.onGraceExpiry`
  is a single-word load: safe to read without the lock because the field
  is set once at boot. A future ticket adding mid-run hook mutation would
  need to revisit; the doc comment says boot-time only.

#### 2. `internal/relay/forward.go` (modified)

- **`StartPhoneForwarder` body**: immediately after `binary.Send(wrapped)`
  returns nil (i.e. the existing `if err := binary.Send(wrapped); err != nil`
  check passes), add a nil-safe hook call:

  ```
  if h := reg.onPhoneForwarded; h != nil { h() }
  ```

  Placement is critical: AFTER `Send` succeeds, BEFORE the loop's next
  iteration. Not before `Send` (would over-count on Send error), not
  inside the `err != nil` branch (would invert the AC).

- **`StartBinaryForwarder` body**: immediately after `phone.Send(env.Frame)`
  returns nil (i.e. the existing `if err := phone.Send(env.Frame); err !=
  nil` check passes), add the same nil-safe shape:

  ```
  if h := reg.onBinaryForwarded; h != nil { h() }
  ```

  Placement: AFTER the Send-success path. The three `continue` paths
  (unmarshal err, unknown conn_id, phone Send err) do not reach this
  line — they `continue` before it. That is the structural defence for
  the AC's "no increment on … per-sink `Send` error" rule.

- Ticket-body wording note (record only — does not change the spec): the
  body says *"the loop fans out to multiple phones per envelope"*. The
  current `StartBinaryForwarder` implementation routes one envelope to
  at most one phone (the one matching `env.ConnID`). The AC text
  ("exactly once per successful `phone.Send`") is correct; the rationale
  paragraph's "multiple phones" framing is a leftover from a multi-recipient
  design that did not land. Increment-per-`phone.Send` is what the spec
  encodes — same answer regardless of which framing is read.

#### 3. `cmd/pyrycode-relay/main.go` (modified)

Two new constructor calls in the metrics-wiring block, immediately after
`relay.NewConnectionsMetrics(metricsReg, reg)` at line 102 and before
the mux construction:

```
relay.NewForwardMetrics(metricsReg, reg)
relay.NewGraceMetrics(metricsReg, reg)
```

Each constructor (a) registers its counter against `metricsReg` and
(b) calls the appropriate setter on `reg`. Boot-time ordering guarantee:
both hooks are wired before `srv.ListenAndServe()` is called and before
any forwarder goroutine launches (those start on the first connection).

No other edits to main.go.

#### 4. `internal/relay/metrics_forward.go` (new, ~35 lines)

Per-concern collector struct. Constructor takes `prometheus.Registerer`
and `*Registry`. Mirrors `NewConnectionsMetrics`'s shape.

- `forwardMetrics` struct holds a single `*prometheus.CounterVec` field.
- `NewForwardMetrics(reg prometheus.Registerer, src *Registry)` constructs
  the CounterVec with `prometheus.CounterOpts{ Name:
  "pyrycode_relay_frames_forwarded_total", Help: "…" }` and label set
  `[]string{"direction"}`. Registers via `reg.MustRegister(m.counter)`.
  Then calls `src.SetForwarderHooks(...)` passing two closures, each
  pre-bound to the right label value:
  - `func() { m.counter.WithLabelValues("phone_to_binary").Inc() }`
  - `func() { m.counter.WithLabelValues("binary_to_phone").Inc() }`
  The closures call `WithLabelValues` on every increment; CounterVec
  caches by label set, so the cost is one map lookup per frame. (For
  the relay's frame rate this is negligible; pre-binding to a `Counter`
  in the constructor would be marginally faster but adds two more
  fields. Spec lets the developer pre-bind if they prefer; either
  satisfies the AC.)
- Doc comment on `forwardMetrics`: *"Push-shaped counter — the
  forwarder loop is the source of truth, not a snapshot of registry
  state (contrast `metrics_connections.go`'s pull-based gauges). The
  hook indirection through `*Registry` keeps the prometheus dep out of
  `forward.go`."*
- **Label values are hard-coded constants.** Both `phone_to_binary` and
  `binary_to_phone` are string literals in this file; neither is
  attacker-influenced. Load-bearing for security review § Tokens —
  cardinality is exactly 2, regardless of traffic. Documented in the
  constructor's doc comment so the next reader does not "fix" the
  hard-coding by reading the direction from request state.

#### 5. `internal/relay/metrics_grace.go` (new, ~25 lines)

Per-concern collector struct, same shape as `metrics_forward.go` but
without labels.

- `graceMetrics` struct holds a single `prometheus.Counter` field.
- `NewGraceMetrics(reg prometheus.Registerer, src *Registry)` constructs
  the Counter with `prometheus.CounterOpts{ Name:
  "pyrycode_relay_grace_expiries_total", Help: "…" }`, registers via
  `reg.MustRegister(m.counter)`, then calls
  `src.SetGraceExpiryHook(func() { m.counter.Inc() })`.
- Doc comment on `graceMetrics`: *"Push-shaped counter. The hook fires
  from `Registry.handleGraceExpiry`'s success branch, structurally
  ensuring stale-fire no-ops never increment. The pointer-identity guard
  in `handleGraceExpiry` is the load-bearing defence for the
  no-double-count invariant; the hook only fires after the guard
  passes."*
- **No labels.** Spec calls out the rejected `{server="<id>"}` shape so
  the developer cannot quietly re-add a server-id label that would
  carry attacker-influenced header values onto the metrics surface.

#### 6. `internal/relay/metrics_counters_test.go` (new, ~140 lines)

Four tests, all in `package relay` so `fakeConn`/`fakePhone`/`fakeBinary`
are reused without duplication. One shared `assertCounter` helper mirrors
`assertGauge` from `metrics_connections_test.go`.

- **`TestForwardMetrics_PhoneToBinary_OnlyOnSuccess`** — AC for the
  phone→binary increment. Scenario:
  - `r := NewRegistry()`, `mreg := NewMetricsRegistry()`,
    `NewForwardMetrics(mreg, r)`, `h := NewMetricsHandler(mreg)`.
  - `bin := &fakeBinary{id: "bin"}`; `r.ClaimServer("s1", bin)`.
  - Run `StartPhoneForwarder` in a goroutine driving 3 frames through a
    `fakePhone`. Verify all 3 received by `bin`. Cancel ctx.
  - Assert `frames_forwarded_total{direction="phone_to_binary"}` is 3.
    Assert `{direction="binary_to_phone"}` is 0.
  - Then a negative case: with `bin.sendErr = errors.New("nope")`, drive
    another frame; the forwarder returns. Counter unchanged at 3 (Send
    failed → no increment). The AC clause "No increment on … `Send`
    error" is checked.
  - The "No binary" / "marshal error" negative paths are covered by
    the existing forwarder tests' coverage of return values; this
    metrics test verifies them by inspection: drive a frame with
    `BinaryFor` returning nil (no `ClaimServer` first), forwarder
    returns nil, counter unchanged.

- **`TestForwardMetrics_BinaryToPhone_OnlyOnSuccess`** — AC for the
  binary→phone increment. Mirrors the shape of the phone→binary test
  but uses `StartBinaryForwarder`. Scenarios (drive successive
  envelopes through one `fakeBinarySource`, each with a different
  outcome):
  - 2 well-formed envelopes addressing a registered phone → counter
    `{direction="binary_to_phone"}` increments by 2.
  - 1 malformed envelope (not parseable by `Unmarshal`) → forwarder
    `continue`s; counter unchanged.
  - 1 well-formed envelope addressing an unknown conn_id → forwarder
    `continue`s; counter unchanged.
  - 1 well-formed envelope to a phone whose `Send` returns an error
    (set `fakePhone.sendErr`) → forwarder `continue`s; counter
    unchanged.
  - Final assertion: total = 2, no other direction incremented.

- **`TestGraceMetrics_OnlyOnRealEviction`** — AC for the grace counter.
  Scenario:
  - `r := NewRegistry()`, wire `NewGraceMetrics(mreg, r)`, handler `h`.
  - `r.ClaimServer("s1", &fakeBinary{id: "b1"})`.
  - `r.ScheduleReleaseServer("s1", 5*time.Millisecond)`.
  - `time.Sleep(30 * time.Millisecond)` to let the timer fire and the
    eviction complete.
  - Assert `grace_expiries_total` is 1.
  - Then a stale-fire scenario: `r.ClaimServer("s2", b)`,
    `r.ScheduleReleaseServer("s2", 5*ms)`, immediately
    `r.ClaimServer("s2", &fakeBinary{id: "b3"})` (cancels and replaces
    the timer). Sleep 30ms past the original window. Real eviction did
    NOT happen for `s2`; counter must still be 1, not 2.

- **`TestGraceMetrics_RaceFreedom`** — race coverage that the hook plays
  nicely with the stale-fire path under concurrent cycles. Shape pinned
  to `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles` (registry_test.go:413):
  - 16 goroutines, 200 ops each. Each op: `ClaimServer`, optionally
    `ScheduleReleaseServer` with 1ms grace, sometimes immediately
    `ClaimServer` again (cancel-replace), sometimes let the timer fire.
  - After `wg.Wait()` + a 50ms drain sleep, scrape and assert the
    counter is a non-negative integer; do NOT assert an exact value
    (the cancel-replace race window means the eviction count is
    schedule-dependent, between 0 and goroutines × ops). The
    structural assertion is `go test -race`: no DATA RACE report
    against the hook field. The pointer-identity guard's existing
    coverage carries through; the new wrinkle is the hook field's
    read from the timer goroutine concurrent with no writes (writes
    happen only at boot).
  - The counter value is bounded: invariant `0 ≤ value ≤ total schedule
    calls that were NOT cancelled`. Tests assert only the
    no-race-detector-finding condition. The single-eviction +
    single-stale-fire correctness is the previous test's job.

`assertCounter(t, h, "metric_name", labelStr, want)` substring-matches
`metric_name{labels} <want>` or `metric_name <want>` (no-label case)
in the scraped body. Same substring-match posture as
`metrics_connections_test.go`'s `assertGauge` (because the body also
contains `promhttp_metric_handler_*` self-instrumentation lines).
Pinned literals (`pyrycode_relay_frames_forwarded_total{direction="phone_to_binary"} 3`)
are the version-bump canaries for `client_golang` text-format changes —
same posture as the gauge tests in #61.

### Concurrency model

- **No new goroutines.** The hook bodies run on whatever goroutine the
  caller is on:
  - Phone hook → the phone forwarder goroutine (one per connected phone).
  - Binary hook → the binary forwarder goroutine (one per connected
    binary).
  - Grace hook → the `time.AfterFunc` goroutine (one per timer firing).
- **No new locks.** The Counter / CounterVec increments are atomic per
  `client_golang`'s contract.
- **Hook field reads under no lock.** Each forwarder reads `reg.onPhoneForwarded`
  / `reg.onBinaryForwarded` as a function-pointer load on every iteration.
  `handleGraceExpiry` reads `reg.onGraceExpiry` once after `Unlock`.
  Per the doc comment, the fields are written exactly once at boot
  before either listener starts, so the reads are race-free in the
  happens-before sense (boot writes happen-before forwarder
  goroutine starts via the standard "goroutine creation
  happens-before goroutine body" rule from the Go memory model).
- **Race-detector coverage.** `TestGraceMetrics_RaceFreedom` exercises
  the hook field's read under high concurrency. Existing
  `TestRegistry_RaceFreedom` / `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles`
  / `TestConnectionsMetrics_RaceFreedom` (#61) carry through unchanged.

### Error handling

| Failure mode | Surface | Recovery |
| --- | --- | --- |
| `MustRegister` collision (same metric name registered twice on `metricsReg`) | panic at boot inside `NewForwardMetrics` / `NewGraceMetrics` | impossible by construction — both names are unique across the relay's metric space; if a future sibling ticket re-uses a name the panic surfaces at boot, which is the right time |
| `Counter.Inc` on a nil pointer | impossible — the hook is registered with a closure that captures a non-nil counter constructed in the same function | n/a |
| Hook field still nil when forwarder runs | hook call is `if h := …; h != nil { h() }` no-op | n/a; tests that don't wire metrics see no-op behaviour |
| Forwarder Send returns nil but with zero bytes written | not reachable — `Send` returns nil iff the frame was written; nil is the success contract per the existing forwarder tests | n/a |
| Pointer-identity guard returns false (stale fire) | hook is never reached — guard's `return` exits before the hook call | n/a; the structural defence the AC specifies |

### Testing strategy

- `make vet`, `make test` (which is `-race`-wide per the Makefile), and
  `make build` are the AC gates. All three must be clean before merge.
- Four new tests, listed above. All live in `internal/relay/metrics_counters_test.go`.
- Existing tests (registry, forward, connections) are NOT modified —
  the hook fields default to nil and the existing tests never wire a
  metric, so the new code paths are invisible to them.
- `TestGraceMetrics_RaceFreedom`'s race coverage extends the existing
  `TestRegistry_RaceFreedom` proof to the new field-read path. The Go
  race detector's verdict under `make test` is the structural AC gate.
- No new e2e or integration tests. `/metrics` listener wiring (#60)
  is unchanged by this ticket; e2e coverage that scrapes `/metrics`
  in autocert mode is out of scope.

### Open questions

1. **`docs/knowledge/codebase/58.md` and `docs/knowledge/INDEX.md`
   updates.** AC names both. Per the architect's *Never Update* rule
   and the established precedent (#59/#60/#61 each deferred these to
   the documentation phase), the architect does NOT write either file
   in this ticket. The developer's commit ships the code; the
   documentation phase's post-merge run lands the codebase summary and
   the INDEX entry. The AC is satisfied at the ticket level by the
   documentation phase, not by the architect or developer phases.
   Recorded so the developer does not chase an AC item that is not
   theirs.
2. **Pre-bind labelled counters in the closure?** The constructor's
   `WithLabelValues("phone_to_binary")` could be called once and the
   resulting `prometheus.Counter` captured by the closure instead of
   re-resolving on every frame. CounterVec caches by label set so the
   cost difference is one map lookup per frame at relay traffic rates;
   not measurable. Either implementation satisfies the AC. Developer
   picks; if pre-binding, document the choice in a one-line comment.
3. **`time.AfterFunc` goroutine vs hook reentrancy.** The grace hook
   could in principle call back into the registry; it does not (the
   only caller is `metrics_grace.go`'s `m.counter.Inc()`). Doc comment
   on the field warns against acquiring `r.mu` in a hook (deadlock
   risk) so a future contributor adding a registry-state-reading hook
   sees the constraint.
4. **Should `SetForwarderHooks` / `SetGraceExpiryHook` be private?**
   They are exported because main.go is in a different package and
   must call them. Tests that exercise the metrics directly (in
   `package relay`) could use either an exported or unexported setter;
   exported avoids the duplicate-shape problem. The "Set once at boot"
   constraint is doc-comment-enforced, not type-enforced — consistent
   with `Registry`'s other thread-safety contracts (e.g. ClaimServer
   is documented as safe under concurrent calls, but the doc is the
   contract, not a type-level guarantee).

### Out of scope

- Histogram for send-duration (`pyrycode_relay_send_duration_seconds`):
  explicitly out of scope per ticket body. Its own ticket if it lands.
- Upgrade/register counters (#57's territory). #57 will land its own
  per-concern collector file (`metrics_upgrade.go` or similar) using the
  same registry-hook pattern this ticket establishes for the grace
  counter, since the upgrade-flood reject site is similarly inside
  the relay package's existing handler code.
- Touching the `/metrics` listener config (#60's territory).
- Process / Go-runtime collectors (forbidden by ADR-0008 § Scope of
  use).
- Documentation phase artefacts (`docs/knowledge/codebase/58.md`,
  `docs/knowledge/INDEX.md` append) — handled by the documentation
  phase post-merge.

## Security review (security-sensitive ticket)

### Mindset

Re-reading the spec as an adversary against the relay process. Default
verdict FAIL until each applicable category below has either a concrete
finding or an explicit design decision that makes the category not
applicable. The label is the gate; "this is just two counters" is
exactly the bias the pass exists to bypass — metric labels and metric
values are exfiltration channels regardless of how the data plane is
shaped.

### 1. Trust boundaries

- **Inbound trust boundary:** unchanged. The `/metrics` listener (which
  terminates scrape traffic) is #60's loopback-only surface; this ticket
  adds counters consumed by that listener but does not change its
  network surface.
- **Internal boundary:** the relay's private registry vs.
  `prometheus.DefaultRegisterer`. Enforced by both constructors taking
  `prometheus.Registerer` explicitly (no default-registry shortcut);
  reinforced structurally by the existing
  `TestMetricsRegistry_NoGlobalRegistrarLeak` (snapshot delta is zero
  after this ticket's wiring — the new counters register on a fresh
  private registry, never on the default).
- **New internal boundary** introduced: the hook fields on `*Registry`.
  Hook writes happen only at boot (single-goroutine, single-call); hook
  reads happen from forwarder + timer goroutines. The boundary defence
  is the doc-comment-enforced "set once at boot" rule. No
  network-supplied input ever writes to these fields.

### 2. Tokens, secrets, credentials

- **`frames_forwarded_total` labels:** the `direction` label has exactly
  two values, both hard-coded string literals in `metrics_forward.go`
  (`phone_to_binary`, `binary_to_phone`). Neither comes from any
  request header, query parameter, frame payload, or registry state.
  Cardinality is exactly 2. No attacker-influenced label values.
  Structurally — the spec calls out the constants in the constructor's
  doc comment so a future contributor cannot quietly rewrite them to
  read the direction from request state.
- **`grace_expiries_total` labels:** none. Scalar counter.
- **Metric values:** both counters are monotonic non-negative integers
  derived from the count of successful per-frame Send operations
  (forward counter) or actual evictions (grace counter). Neither
  value carries a token, IP, server-id, or frame content. The
  forwarded byte-count is NOT exposed; only the integer-valued frame
  count.
- **Token-handling code paths:** unchanged. The relay's only handled
  secret is `X-Pyrycode-Token` at `/v1/client` (#5). This ticket adds
  no code path that touches a token; forwarder code already in scope
  has no token handling.

### 3. File operations

- None. No new file I/O.

### 4. Subprocess / external command execution

- None.

### 5. Cryptographic primitives

- None directly. `prometheus.Counter.Inc` uses no `crypto/*` API.
- The hooks-on-Registry mechanism uses ordinary function pointers; no
  reflection, no unsafe.

### 6. Network & I/O

- **Handler exposure surface change.** Before this ticket: `/metrics`
  body contains `promhttp_metric_handler_*` self-instrumentation lines
  + the two `pyrycode_relay_connected_*` gauges. After this ticket: +
  two counter-vec lines (`frames_forwarded_total{direction="phone_to_binary"}`,
  `frames_forwarded_total{direction="binary_to_phone"}`) + one scalar
  counter line (`grace_expiries_total`). Total response size delta:
  ~250 bytes (well under any scrape buffer).
- **Cardinality bound.** `frames_forwarded_total` has cardinality 2
  (hard-coded). `grace_expiries_total` is scalar. Both are constant
  across all traffic patterns and load profiles. A future ticket
  adding a per-server-id or per-conn-id label would re-open this
  category; this ticket closes it for itself.
- **No new `Accept` content negotiation.** The collectors emit via the
  existing `NewMetricsHandler` (#59), which pins text format.
- **No new listener.** The `/metrics` surface is #60's territory; this
  ticket adds collectors to the registry that listener already serves.

### 7. Error messages, logs, telemetry

- **No new log lines.** The hook bodies are pure `Counter.Inc()` calls;
  they do not log. The forwarders already log at the existing levels
  (Info on success-path end, Warn on per-frame errors); this ticket
  does not change that.
- **Counter values are integers.** No format-string or scientific-notation
  paths through the exposition layer that could leak adjacent memory.
- **Hook write/read race:** doc-comment-bound to boot-only writes. The
  race detector in `TestGraceMetrics_RaceFreedom` exercises the read
  path under high concurrency.

### 8. Concurrency

- **No new goroutines** — the hook calls run on the forwarder /
  AfterFunc goroutines that already exist.
- **No new locks.** Counter `Inc` is internally atomic; hook field
  reads are single-word loads of function pointers established at
  boot (happens-before per the Go memory model: goroutine creation
  happens-before the goroutine body).
- **`r.onGraceExpiry` read is unlocked.** Same justification as
  above: the field is set once at boot, before any timer is armed.
  The read happens AFTER the eviction body completes (after
  `r.mu.Unlock()` and the phone-close loop), so even a hypothetical
  reordering cannot leak the lock-protected map state through the
  hook.
- **Hook MUST NOT acquire `r.mu`.** Documented on the field. Violation
  would deadlock (the grace hook runs after `r.mu.Unlock` but the
  forwarder hooks could in principle re-enter mutator paths if a
  future contributor adds one — the doc comment closes that). The
  concrete `metrics_forward.go` / `metrics_grace.go` hook bodies are
  pure `prometheus.Counter.Inc()` and acquire no relay-side lock.

### 9. Threat model alignment

- `docs/threat-model.md` § *Log hygiene* (extends to metric labels):
  closed by design — `direction` is a hard-coded constant set;
  `grace_expiries_total` is label-less. Recorded in the constructors'
  doc comments so the boundary is visible at the call site, not just in
  the spec.
- `docs/threat-model.md` § *DoS*: response size delta ~250 bytes;
  cardinality fixed at 3 series total. No DoS expansion. The `/metrics`
  listener's loopback gate (#60) caps the scrape population to same-host
  processes.
- `docs/threat-model.md` § *Supply chain*: no new dependency. The new
  prod files import `prometheus/client_golang` symbols already pulled in
  by #59. `go mod tidy` after this ticket is a no-op against `go.sum`.
- Protocol spec (`pyrycode/pyrycode/docs/protocol-mobile.md`): unaffected.
  No wire-protocol surface change. The forwarder's per-frame contract is
  unchanged — counter increments are purely observational.

### Findings

- **[Trust boundaries]** No findings. Hook field writes are boot-only;
  reads are race-free per the Go memory model; no network input reaches
  any hook field.
- **[Tokens]** PASS — `direction` label values are hard-coded constants;
  `grace_expiries_total` is label-less; values are integers carrying no
  identifier. Spec calls out the rejected `{server="..."}` shape so the
  developer cannot regress.
- **[File operations]** No findings.
- **[Subprocess]** No findings.
- **[Cryptographic primitives]** No findings.
- **[Network & I/O]** PASS — response size delta ~250 bytes; cardinality
  fixed at 3 series; no new endpoint, no new content negotiation.
- **[Errors / logs / telemetry]** No findings — hooks are silent; values
  are integers.
- **[Concurrency]** PASS — no new goroutines, no new locks; the hook
  field read/write discipline is doc-comment-enforced ("set once at
  boot") and the race test covers the concurrent-read path under
  `-race`.
- **[Threat model]** No findings — log hygiene, DoS, supply chain all
  unchanged or closed by design.

### Verdict

**PASS.** No MUST FIX findings. The single load-bearing security design
choice (hard-coded `direction` label values, no `server` label) is
documented both in the spec's § Design and in the production file's
doc comments, so the developer cannot regress it without first deleting
the spec's instruction. The hook-on-Registry pattern's "set once at
boot" rule is doc-comment-enforced rather than type-enforced — consistent
with the registry's other thread-safety contracts (e.g. `ClaimServer`'s
documented concurrent-safety) and the existing connections-collector
test's race coverage carries the proof.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
