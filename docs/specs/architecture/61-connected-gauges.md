# Spec — `pyrycode_relay_connected_{binaries,phones}` gauges (#61)

## Files to read first

- `internal/relay/registry.go:56-79` — `Registry` struct, lock discipline,
  constructor. The collector holds a `*Registry` (same package, so no
  interface needed). `mu sync.RWMutex` is the lock `Counts()` already takes.
- `internal/relay/registry.go:148-184` — `ScheduleReleaseServer` +
  `handleGraceExpiry`. Read the pointer-identity guard at line 171
  (`if r.timers[serverID] != self { … return }`). This is the stale-fire
  no-op the AC names; the pull-based design rides it for free (see § Design
  *Why pull-based*).
- `internal/relay/registry.go:258-271` — `Counts()`. The collector's
  `Collect` calls this; the gauges ARE its return values. `Counts()` already
  holds `RLock` and walks the maps in a snapshot-consistent way; one call
  is internally consistent (per its doc comment).
- `internal/relay/registry_test.go:19-45` — `fakeConn`. The new race test
  reuses this type (same package, no duplication needed).
- `internal/relay/registry_test.go:413-451` — `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles`.
  Shape reference for the new race test: 16 goroutines × 200 ops, hammering
  the cancel/replace + stale-fire paths. The new test races the same
  registry mutations against periodic `Collect()` calls.
- `internal/relay/metrics.go:1-52` — `NewMetricsRegistry` and
  `NewMetricsHandler` from #59. `MustRegister` happens against
  `prometheus.Registerer`; the constructor in this ticket takes a
  `prometheus.Registerer` argument, not the concrete `*prometheus.Registry`
  — same convention as `promhttp.HandlerOpts.Registry`.
- `internal/relay/metrics_test.go:64-86` — `TestMetricsHandler_RegisterAndScrape_RoundTrip`.
  The basic scrape-and-assert pattern the new tests mirror: create registry,
  wire collector, scrape via the existing `NewMetricsHandler`, substring-match
  the expected text output.
- `docs/specs/architecture/59-metrics-scaffolding.md` § *The architect's
  "package-vars-vs-struct" pick* — the per-concern collector struct pattern
  this ticket follows. The seam rejects the package-level-var and
  mega-struct alternatives; the gauge here is a *third* shape (pull-based
  Collector wrapping external state) but still hosted by a private struct
  in its own file, so it stays inside the established seam.
- `docs/knowledge/decisions/0008-prometheus-client-adoption.md` § *Scope of
  use* — the "no `DefaultRegisterer`" boundary. The new constructor
  enforces it by taking the registerer as an explicit argument; the test
  for this ticket re-runs the snapshot-delta check inherited from #59 by
  way of the existing `TestMetricsRegistry_NoGlobalRegistrarLeak` (which
  still passes after this ticket: the gauges register on a fresh private
  registry, never on the default).
- `docs/PROJECT-MEMORY.md` line 41 — "Interface methods called under the
  lock are documented as non-blocking getters." Relevant *negatively*:
  pull-based design means the collector never holds the registry lock
  itself; `Counts()` already takes its own RLock. No new lock-acquisition
  paths.

## Context

`/healthz` reports a one-shot connection-count snapshot (#10). Time-series
visibility wants the same numbers exposed as Prometheus gauges so the
operator can graph utilisation and alert on drops. #59 landed the registry
+ handler scaffolding (`NewMetricsRegistry`, `NewMetricsHandler`); this
ticket adds the first pair of metrics on top of that seam.

The single load-bearing design choice the AC delegates to the architect:

> Gauges are incremented and decremented at the existing register,
> unregister, and grace-expiry-eviction sites in `internal/relay/registry.go`.
> The architect picks the exact shape (counters maintained inside the
> registry's lock vs. a pull-based `prometheus.Collector` over the existing
> `Counts()`).

This spec picks **pull-based**. The justification is the bulk of § Design.

#60 (listener) and #61 (this ticket) are independent siblings of #59 — once
this ticket lands the collector, the gauges are live in the registry's
view of the world regardless of whether #60 has wired the listener yet.
A test scrape via `NewMetricsHandler` exercises the seam without the
listener.

## Design

### Why pull-based, not push-based

The AC admits two shapes. Pull-based wins on five concrete axes:

1. **Zero diff in `registry.go`.** The registry is internet-exposed,
   security-sensitive, and already raced against under `-race`. Adding
   Inc/Dec calls at five mutation sites (ClaimServer success path,
   ClaimServer-during-grace replace path, ReleaseServer success path,
   handleGraceExpiry post-identity-check path, RegisterPhone success
   path, UnregisterPhone match path) widens the audit surface of a
   sensitive file. Pull-based leaves the file untouched.
2. **The stale-fire AC is satisfied structurally, not by review.** The
   AC requires that `handleGraceExpiry`'s pointer-identity guard
   no-op (when `r.timers[serverID] != self`) must NOT move the gauge.
   With push-based, the developer has to put the Dec calls *inside* the
   `if r.timers[serverID] != self` branch, AFTER the check, and the
   tests have to assert that pointer-identity-miss paths leave the
   gauge unchanged. With pull-based, the gauge IS `Counts()`; the guard
   already keeps the map unchanged on a stale fire, so the gauge is
   structurally unaffected. One fewer thing to get wrong.
3. **`Counts()` is the authoritative live count.** The AC requires
   "gauge values match the registry's live count at all observation
   points." Pull-based makes this a tautology: the gauge is the live
   count, by construction. Push-based introduces a second source of
   truth (the AtomicInt64 inside each gauge) that must agree with the
   first (the map sizes); divergence between the two is a class of bug
   pull-based cannot have.
4. **No new lock-acquisition path.** `Counts()` already holds RLock
   and walks the maps. The collector calls `Counts()`; that's the only
   read. Push-based adds Inc/Dec calls inside the registry's Lock
   regions — `prometheus.Gauge.Inc/Dec` is documented atomic and
   non-blocking, so this is *safe*, but it's a new pattern the lock
   discipline must continue to honour as the registry evolves.
5. **The seam pattern admits it.** #59's per-concern collector struct
   pattern was illustrated with a push-shaped CounterVec. This ticket
   instantiates the same pattern with a pull-shaped Collector — a
   private struct in its own file, constructed via a function taking
   `prometheus.Registerer`, registered against the relay's private
   registry. The pattern accommodates both shapes; this ticket
   documents the precedent that gauges-of-a-state are naturally
   pull-shaped, while sibling tickets #57/#58 (counters-of-events)
   stay push-shaped. Different semantics, different shapes; both
   live in the same seam.

Push-based has one nominal advantage worth naming: the gauge value
updates atomically with the state change, while pull-based is only as
fresh as the last scrape. For Prometheus's typical 15-60s scrape
interval and the relay's use case (operator alerts on connection
drops), that freshness gap is acceptable — the operator's alert
window dwarfs the scrape interval. The advantage does not outweigh
points 1-5.

### Artefacts

**Two new files, no edits to existing files.**

#### 1. `internal/relay/metrics_connections.go` (new, ~40 lines)

```go
package relay

import "github.com/prometheus/client_golang/prometheus"

// connectionsCollector emits pyrycode_relay_connected_binaries and
// pyrycode_relay_connected_phones as Prometheus gauges. The values come
// from Registry.Counts() on every scrape (pull-based); the collector
// never mutates the registry and never holds the registry lock itself.
//
// Pull-based design — rationale recorded in
// docs/specs/architecture/61-connected-gauges.md § Why pull-based:
//   - registry.go is untouched (audit surface unchanged),
//   - the grace-expiry stale-fire guard (registry.go handleGraceExpiry)
//     keeps the maps unchanged on a stale fire, so the gauge is
//     structurally unaffected — no second source of truth to keep in sync,
//   - Counts() already holds the right lock and returns a
//     snapshot-consistent (binaries, phones) pair.
type connectionsCollector struct {
	binaries *prometheus.Desc
	phones   *prometheus.Desc
	src      *Registry
}

// NewConnectionsMetrics registers the pair of connection-count gauges
// against reg, reading their values from src on each scrape. Returns
// nothing — the collector is held alive by the registerer.
//
// Wiring site: cmd/pyrycode-relay/main.go (the call lands with #60's
// listener wiring; until then the constructor is exercised only by
// metrics_connections_test.go).
func NewConnectionsMetrics(reg prometheus.Registerer, src *Registry) {
	reg.MustRegister(&connectionsCollector{
		binaries: prometheus.NewDesc(
			"pyrycode_relay_connected_binaries",
			"Number of pyrycode binary connections currently held by the relay registry.",
			nil, nil,
		),
		phones: prometheus.NewDesc(
			"pyrycode_relay_connected_phones",
			"Number of mobile client connections currently held by the relay registry, summed across all server-ids.",
			nil, nil,
		),
		src: src,
	})
}

func (c *connectionsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.binaries
	ch <- c.phones
}

func (c *connectionsCollector) Collect(ch chan<- prometheus.Metric) {
	b, p := c.src.Counts()
	ch <- prometheus.MustNewConstMetric(c.binaries, prometheus.GaugeValue, float64(b))
	ch <- prometheus.MustNewConstMetric(c.phones, prometheus.GaugeValue, float64(p))
}
```

Notes the developer must preserve verbatim:

- **No labels.** Both metrics are scalar gauges, not GaugeVecs.
  `prometheus.NewDesc(..., nil, nil)` — empty `variableLabels` and empty
  `constLabels`. The AC names two metrics with no `{server="..."}`
  cardinality. This is also load-bearing for security review § Tokens:
  a `server` label would carry server-ids (attacker-influenced via the
  `x-pyrycode-server` header on `/v1/server`) into the metrics surface,
  which the threat model § Log hygiene forbids. Keep labels empty.
- **`MustNewConstMetric`, not `NewConstMetric`.** The Desc is built
  once at construction; the value comes from `Counts()`, which can
  never produce values that violate the Desc contract (no labels to
  mismatch, count is a non-negative int). A nil return from
  `NewConstMetric` is impossible under these inputs; the `Must` form
  matches the contract and saves the per-scrape `if err != nil` branch.
- **`prometheus.Registerer` (interface), not `*prometheus.Registry`
  (concrete).** `Registerer` is the `MustRegister`-only sub-interface;
  passing it (rather than the concrete type) keeps the constructor
  composable with anything implementing the interface (e.g.
  `prometheus.WrapRegistererWith` for label-decoration), even though
  no caller currently uses that. Same convention as
  `prometheus/client_golang`'s own constructors.
- **No package-level `var`s.** Pattern reference: #59's spec §
  *package-vars-vs-struct pick* rejects them as singleton-forcing. This
  ticket keeps the precedent.

#### 2. `internal/relay/metrics_connections_test.go` (new, ~120 lines)

Three tests, in the same `package relay` (so the `fakeConn` from
`registry_test.go` is reusable without copy-paste — same convention as
the rest of the test suite, per PROJECT-MEMORY line 28).

```go
package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)
```

**Test 1: `TestConnectionsMetrics_ReflectLiveCounts`** — AC gate.
Constructs a registry, wires the collector, mutates the registry
through the public API (ClaimServer / RegisterPhone), scrapes via the
existing `NewMetricsHandler`, asserts the body contains the expected
gauge lines.

```go
func TestConnectionsMetrics_ReflectLiveCounts(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewConnectionsMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	// Empty registry: both gauges should be 0.
	assertGauge(t, h, "pyrycode_relay_connected_binaries", 0)
	assertGauge(t, h, "pyrycode_relay_connected_phones", 0)

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-2"}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	assertGauge(t, h, "pyrycode_relay_connected_binaries", 1)
	assertGauge(t, h, "pyrycode_relay_connected_phones", 2)

	// Unregister one phone and release the server; gauges follow.
	r.UnregisterPhone("s1", "p-1")
	if !r.ReleaseServer("s1") {
		t.Fatal("ReleaseServer: got false, want true")
	}
	assertGauge(t, h, "pyrycode_relay_connected_binaries", 0)
	// Phone p-2 outlives the release per ReleaseServer's contract (orphan
	// phones survive until explicit unregister or grace expiry).
	assertGauge(t, h, "pyrycode_relay_connected_phones", 1)
}
```

```go
// assertGauge scrapes h and asserts that body contains the literal line
// `<name> <want>` in the Prometheus text format. Substring-match (not
// equality on the full body) because the body also contains the
// promhttp_metric_handler_* self-instrumentation counters from
// HandlerOpts.Registry=reg.
func assertGauge(t *testing.T, h http.Handler, name string, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status: got %d, want 200", rec.Code)
	}
	wantLine := fmt.Sprintf("%s %d", name, want)
	if !strings.Contains(rec.Body.String(), wantLine) {
		t.Errorf("scrape body missing %q; got:\n%s", wantLine, rec.Body.String())
	}
}
```

The text-format literal `pyrycode_relay_connected_binaries 0` is the
canary the spec pins: a future bump of `client_golang` that changes
the formatting (extra whitespace, scientific notation for zero, etc.)
would surface as a test failure rather than a tautological pass — same
posture as `metrics_test.go`'s pinned `Content-Type` literal.

**Test 2: `TestConnectionsMetrics_GraceStaleFireDoesNotMoveGauge`** —
the stale-fire AC. Reproduces the cancel/replace pointer-identity
race window:

1. ClaimServer("s1", b1), RegisterPhone("s1", p1).
2. ScheduleReleaseServer("s1", short d) → arms entry A.
3. ClaimServer("s1", b2) → ClaimServer's grace path cancels entry A's
   timer, deletes the map entry, replaces the binary.
4. Sleep past d. If timer A had managed to start its body before
   Stop() observed it, the pointer-identity guard in
   `handleGraceExpiry` no-ops because `r.timers["s1"]` is now nil
   (entry A was deleted).
5. Scrape — gauges must reflect the live state (1 binary b2,
   1 phone p1), NOT the post-eviction state (0,0).

```go
func TestConnectionsMetrics_GraceStaleFireDoesNotMoveGauge(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewConnectionsMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer b-1: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone p-1: %v", err)
	}

	// Arm grace; immediately cancel-and-replace via reclaim.
	r.ScheduleReleaseServer("s1", 5*time.Millisecond)
	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("ClaimServer b-2 (reclaim): %v", err)
	}

	// Wait past the original grace window. If the stale fire were to
	// move the gauge, this is when it would.
	time.Sleep(30 * time.Millisecond)

	assertGauge(t, h, "pyrycode_relay_connected_binaries", 1)
	assertGauge(t, h, "pyrycode_relay_connected_phones", 1)
}
```

This test is the structural defence the AC demands for the
stale-fire-no-op rule. With pull-based design, the test reads as
"the registry's Counts() reflects b-2 + p-1, therefore the gauge
does too." With a hypothetical push-based design, the test would
have to additionally inspect a Dec call site for branch coverage.
Pull-based earns the simpler test.

**Test 3: `TestConnectionsMetrics_RaceFreedom`** — the explicit AC
"race register/unregister/grace-expiry against periodic gauge reads".

```go
func TestConnectionsMetrics_RaceFreedom(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewConnectionsMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	const goroutines = 16
	const opsPer = 200

	stopScrape := make(chan struct{})
	var scrapeCount atomic.Int64

	// Scraper: periodic gauge reads via the public handler — the same
	// path Prometheus would hit.
	go func() {
		for {
			select {
			case <-stopScrape:
				return
			default:
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if rec.Code != http.StatusOK {
					t.Errorf("scrape status: got %d, want 200", rec.Code)
					return
				}
				scrapeCount.Add(1)
				// Tight loop — no sleep — to maximise interleaving under
				// -race. The race detector reports on memory accesses, not
				// on iteration count.
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", g%4)
			for i := 0; i < opsPer; i++ {
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b-%d-%d", g, i)})
				_ = r.RegisterPhone(sid, &fakeConn{id: fmt.Sprintf("p-%d-%d", g, i)})
				r.ScheduleReleaseServer(sid, time.Millisecond)
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b2-%d-%d", g, i)})
				r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
			}
		}()
	}
	wg.Wait()
	close(stopScrape)

	// Sleep past max grace duration so any in-flight timers fire and exit.
	time.Sleep(50 * time.Millisecond)

	// Final state need not be empty (some claims won the cancellation
	// race); we only assert the scraper ran AND a final scrape returns 200
	// with parseable gauge lines. The race detector's verdict is the real
	// assertion — this test exists to feed -race interleavings, not to
	// check exact values.
	if scrapeCount.Load() == 0 {
		t.Error("scraper never ran")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("final scrape status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pyrycode_relay_connected_binaries ") {
		t.Errorf("final scrape body missing binaries gauge; got:\n%s", body)
	}
	if !strings.Contains(body, "pyrycode_relay_connected_phones ") {
		t.Errorf("final scrape body missing phones gauge; got:\n%s", body)
	}
}
```

Shape pinned to `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles`
(registry_test.go:419) — same goroutine count, same op shape, same
"final state need not be empty" pattern. The new wrinkle is the
concurrent scraper goroutine: it reads `Counts()` indirectly through
the collector, racing against every mutation site.

The scraper uses a tight loop without `time.Sleep` between iterations.
The race detector reports on memory access pairs, not on iteration
count; sleeping only reduces the interleaving density. The test should
finish in well under a second (16 × 200 ops with one-millisecond grace
timers).

**No fourth test.** A test asserting "`Counts()` and the gauge agree"
would be tautological under pull-based design (the gauge IS the
return value of `Counts()`). Skip it.

## Concurrency model

- The collector spawns no goroutines.
- `Collect` is called by `prometheus.Registry.Gather()`, which the
  handler invokes per scrape. Concurrent scrapes are serialised inside
  `prometheus.Registry.Gather()` (the library's lock); the collector
  itself is stateless beyond the two Desc pointers and the `*Registry`
  reference.
- `Counts()` takes the registry's RLock. The collector calls `Counts()`
  exactly once per `Collect`. No new lock-acquisition path; the
  registry's existing lock discipline carries through.
- The race test (Test 3) is the structural check: 16 mutator goroutines
  + 1 scraper goroutine, no DATA RACE reports under `-race`. The
  registry's existing race coverage
  (`TestRegistry_RaceFreedom`, `TestScheduleReleaseServer_RaceFreedomUnderRapidCycles`)
  already proves `Counts()` is race-free against the mutation API; this
  test extends the proof to "`Counts()` racing against scrape calls via
  the collector seam."

## Error handling

Three concrete failure modes the spec accounts for:

1. **Metric-name collision.** `MustRegister` panics if a collector with
   either gauge name is already registered against `reg`. The panic
   surfaces at process startup (in `main.go` once #60 wires the call),
   not at scrape time. Desirable: the relay refuses to serve a broken
   metrics surface. The metric names this ticket introduces
   (`pyrycode_relay_connected_binaries`, `pyrycode_relay_connected_phones`)
   are unique within the relay's metric space; sibling tickets #57/#58
   register under different names per their bodies. No conflict at the
   time this ticket lands.
2. **`Collect` invoked before the registry is fully constructed.** Not
   reachable: `NewConnectionsMetrics` is called once at startup, after
   `NewRegistry()` and `NewMetricsRegistry()`; the registry's zero-value
   maps are populated by `NewRegistry()` before the collector is
   registered. No nil-deref window.
3. **`Counts()` returns negative.** Not reachable: `Counts()` returns
   `len(map)` and a sum of `len(slice)` values; both are
   non-negative. `float64(int)` conversion is exact for the range Go
   maps occupy on the relay (tens of thousands at most). No clamp
   needed; if a future ticket changes `Counts()` to return a signed
   delta, that ticket revisits this design.

## Testing strategy

- `make vet`, `make test` (which is `-race`-wide on this repo per the
  Makefile), and `make build` are the AC gates. All three must be
  clean before merge.
- The three tests above. No fixtures, no new test helpers beyond
  `assertGauge` defined inline. `fakeConn` is reused from
  `registry_test.go` (same package; no import or duplication).
- No new e2e or integration coverage. #60's listener wiring will
  exercise the gauges via a live `/metrics` scrape over HTTP — that
  ticket's tests, not this one's.
- The `-race` test is the structural defence for the AC's "values
  match the registry's live count at all observation points": pull-based
  makes the equivalence trivially true; the race test proves no
  goroutine schedule can break it.

## Open questions

1. **`docs/knowledge/codebase/61.md` and `docs/knowledge/INDEX.md`.**
   The AC lists both. Per the architect's *Never Update* rule and #59's
   precedent (its spec § Open questions #3), the documentation phase
   writes both files post-merge. The architect and the developer leave
   them alone. Recorded so the developer does not chase an AC item
   that is not theirs.
2. **Wiring call site.** `NewConnectionsMetrics(mreg, registry)` must
   be invoked from somewhere; the natural site is
   `cmd/pyrycode-relay/main.go` next to `relay.NewRegistry()` and
   `relay.NewMetricsRegistry()`. #60's spec owns that wiring step.
   This ticket lands the constructor; #60 lands the call. Until #60
   merges, the gauges are exercised only by this ticket's tests —
   that's fine: tests are the AC gate, and the production wiring is
   a one-line follow-up that #60 handles inside its own spec.
3. **Should the same collector emit additional `connected_*` metrics
   in the future** (e.g. `pyrycode_relay_grace_period_active` gauge
   counting active timers)? Out of scope. If wanted, the natural
   extension is to add a third Desc field + a third `Counts()`-style
   getter on the registry. Recorded so the next sibling architect
   sees the seam admits it.

## Out of scope

- The `/metrics` listener (`http.Server`, bind address, TLS) — #60.
- Counters and histograms for the upgrade-attempt and frame-forwarded
  paths — #57, #58.
- Edits to `registry.go`. Pull-based explicitly avoids them.
- `docs/knowledge/codebase/61.md` and `docs/knowledge/INDEX.md` —
  documentation phase territory.
- Process / Go-runtime collectors (`collectors.NewGoCollector` etc.) —
  out of bounds per ADR-0008 § Scope of use.

## Security review (security-sensitive ticket)

### Mindset

Re-reading the spec as an adversary. Default verdict FAIL until each
applicable category below has either a concrete finding or an explicit
design decision that makes the category not applicable.

The label is the gate — not the architect's judgement of "this is
just a counter." Metrics-as-exfiltration is exactly the category
this ticket sits in.

### 1. Trust boundaries

- **Inbound trust boundary:** none introduced. The `/metrics` listener
  (which terminates untrusted scrape traffic) is #60. This ticket
  ships a Collector that reads `*Registry`'s public method `Counts()`
  on each scrape — no `net.Conn`, no `http.Request`, no
  user-controlled input reaches this code.
- **Internal boundary:** the relay's private registry vs.
  `prometheus.DefaultRegisterer`. Enforced by the constructor taking
  `prometheus.Registerer` explicitly (no default-registry shortcut)
  and reinforced structurally by the existing
  `TestMetricsRegistry_NoGlobalRegistrarLeak` (the snapshot-delta is
  zero before and after constructing the relay's wiring; this ticket
  adds collectors to a fresh private registry, not to the default).

### 2. Tokens, secrets, credentials

- The two metrics carry **no labels** (`prometheus.NewDesc(name,
  help, nil, nil)`). This is load-bearing for the threat model § Log
  hygiene rule that extends to metric labels (recorded in
  `docs/knowledge/features/metrics-registry.md` § *Scope of use*).
  A naive shape — `connected_phones{server="<server-id>"}` — would
  emit the `x-pyrycode-server` header value (attacker-influenced)
  onto the metrics surface; the spec rejects that shape explicitly.
- No metric value carries a token, IP, or any other secret. Values
  are non-negative integers from `len(map)` / sum of `len(slice)`.
- The collector holds a `*Registry` reference. The registry does not
  store tokens (per PROJECT-MEMORY: tokens are presence-checked and
  discarded at `/v1/client`). No transitive secret reach.

### 3. File operations

None. The collector performs no file I/O.

### 4. Subprocess / external command execution

None.

### 5. Cryptographic primitives

None directly. `prometheus.MustNewConstMetric` uses no `crypto/*` APIs.
The transitive `xxhash/v2` dependency (non-cryptographic label-set
keying) is irrelevant here — labels are empty.

### 6. Network & I/O

- **Handler exposure surface change.** Before this ticket: the
  `/metrics` body contains only `promhttp_metric_handler_*`
  self-instrumentation counters (~7 lines, ~200 bytes). After this
  ticket: + two scalar gauge lines (~80 bytes). Total response size
  stays well under any reasonable scrape buffer. No DoS expansion.
- **Cardinality bound is zero.** Both gauges are label-less; the metric
  count this ticket adds is exactly two, regardless of how many
  server-ids or phones the relay holds. A future ticket adding a
  per-server-id GaugeVec would re-open this category; this ticket
  closes it.
- **No new `Accept` content negotiation.** The collector emits via
  the existing `NewMetricsHandler`, which pins text format
  (OpenMetrics disabled). No content-type-based parser differential
  exposure.

### 7. Error messages, logs, telemetry

- **Metric labels are empty** — see § 2. The label-exfiltration
  channel the threat model § Log hygiene names is closed here by
  design.
- **No log lines added.** The collector does not log; collection
  errors from `Counts()` are impossible (the method returns two
  ints, no error).
- **Metric values are integers.** No format-string or scientific-notation
  shenanigans that could leak adjacent process memory through value
  serialisation.

### 8. Concurrency

- The collector holds no shared mutable state of its own. The two
  `*prometheus.Desc` fields and the `*Registry` reference are set
  at construction and never mutated.
- `Collect` reads via `Counts()`, which takes the registry's
  RLock. No new lock-acquisition path; no new ordering hazard.
- The race test (Test 3) is the structural check. 16 mutator
  goroutines + 1 scraper goroutine. The Go race detector's verdict
  under `-race` is the AC gate. If the test passes under `make
  test`, the seam is race-free; if it ever flakes, the failure mode
  is structural (Counts()'s RLock vs. mutator Lock interaction) and
  shared with the existing `TestRegistry_RaceFreedom`.

### 9. Threat model alignment

- `docs/threat-model.md` § *Log hygiene* — extends to metric labels.
  Closed by design (no labels). § 2 above.
- `docs/threat-model.md` § *DoS* — the response size delta is ~80
  bytes; the `/metrics` listener's bind-address policy and `http.Server`
  timeouts are #60's territory. This ticket does not introduce a new
  network-reachable endpoint and does not unbound the existing one's
  response size.
- `docs/threat-model.md` § *Supply chain* — no new dependency. The
  collector uses `prometheus/client_golang` symbols already pulled in
  by #59 (`prometheus.Registerer`, `prometheus.Collector`,
  `prometheus.NewDesc`, `prometheus.MustNewConstMetric`,
  `prometheus.GaugeValue`). `go mod tidy` after this ticket should be
  a no-op against `go.sum`; if it is not, the developer flags that as
  a regression — likely a stale `go.sum` from #59's merge, not new
  surface from this ticket.
- Protocol spec (`pyrycode/pyrycode/docs/protocol-mobile.md`)
  unaffected — no wire-protocol surface change.

### Findings

- **[Trust boundaries]** No findings — the collector reads a
  package-internal method via a `*Registry` reference; no user input
  reaches this code.
- **[Tokens]** PASS — gauges are label-less by design;
  spec calls out the rejected `{server="..."}` shape explicitly so
  the developer cannot quietly re-add it.
- **[File operations]** No findings.
- **[Subprocess]** No findings.
- **[Cryptographic primitives]** No findings.
- **[Network & I/O]** PASS — response size delta is ~80 bytes; no new
  endpoint; cardinality is constant.
- **[Errors / logs / telemetry]** No findings — no labels, no logs,
  integer values only.
- **[Concurrency]** PASS — no new lock-acquisition path; race test
  exercises the seam against the existing mutation API under `-race`.
- **[Threat model]** No findings — log-hygiene extension to metric
  labels is closed structurally; DoS surface unchanged; no
  supply-chain expansion.

### Verdict

**PASS.** No MUST FIX findings. The single load-bearing security
design choice (label-less gauges, no `server` label) is documented
both in the production file's comments and in this spec's § Design
"No labels" note, so the developer cannot regress it without first
deleting the spec's instruction.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-12
