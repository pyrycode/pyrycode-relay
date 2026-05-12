# Spec — Prometheus metrics registry scaffolding (#59)

## Files to read first

- `internal/relay/healthz.go:35-57` — `NewHealthzHandler` is the handler-factory
  pattern the new `NewMetricsHandler` mirrors: returns `http.Handler` (not
  `HandlerFunc`); no per-request state; safe for concurrent use; no exported
  fields. `docs/PROJECT-MEMORY.md` line 43 is the rule.
- `internal/relay/healthz_test.go:11-69` — assertion shape: `httptest.NewRecorder` +
  `httptest.NewRequest(http.MethodGet, …)`; `rec.Code` for status; `rec.Header().Get("Content-Type")` for content type; body parsed with a decoder (json there → text-format-decoder here). Reuse the same test layout.
- `docs/PROJECT-MEMORY.md` line 37 — "new deps need a justification (the ADR
  is the justification)". This is the rule ADR-0008 satisfies.
- `docs/PROJECT-MEMORY.md` line 43 — handler-factory rule. `NewMetricsHandler`
  follows it.
- `docs/knowledge/decisions/0008-prometheus-client-adoption.md` (this commit)
  — adoption ADR; *Scope of use* section binds what `metrics.go` may and may
  not do.
- `docs/knowledge/decisions/0004-ws-library-and-adapter-context-strategy.md`
  — shape reference for an ADR justifying a third-party dep. Read for tone /
  structure if you need to extend the ADR.
- `go.mod` — current dep surface (stdlib + `x/crypto` + `nhooyr.io/websocket`,
  no transitive Prometheus deps yet). `go mod tidy` after the new dep lands
  fills `go.sum` with the transitives ADR-0008 enumerates.
- `Makefile` — `make vet`, `make test` (runs with `-race`), and `make build`
  are the AC gates; lint targets are unrelated to this ticket.
- `cmd/pyrycode-relay/main.go` — read only enough to confirm there is **no**
  call to anything in `metrics.go` after this ticket. Wiring is #60's
  problem; this ticket is structurally a scaffold.

## Context

The relay currently has no metrics surface — `/healthz` (#10) is a snapshot
only. Sibling tickets #57, #58 (counters) and #60 (listener) all depend on
the registry shape this ticket lands.

This is the **scaffolding slice**: it adopts `github.com/prometheus/client_golang`,
records the adoption in ADR-0008, and ships a private registry + handler
factory. It ships **no** counters, **no** gauges, and **no** listener wiring.
Those land in their own tickets and consume the seam this ticket establishes.

The single load-bearing design choice for this ticket is the *seam shape*:
how do counters that sibling tickets add wire onto a registry that this
ticket constructs, without forcing this ticket's call sites (the test, and
later #60's `main.go` line) to mutate their import sets every time a sibling
lands? AC names that choice explicitly ("the architect picks the
package-vars-vs-struct shape").

## Design

Two artefacts in `internal/relay/metrics.go`, plus one test file, plus the
ADR (already written under `docs/knowledge/decisions/0008-…`), plus the
`go.mod` / `go.sum` updates from `go mod tidy`.

### 1. `internal/relay/metrics.go` (new, ~25 lines plus doc comments)

```go
package relay

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewMetricsRegistry constructs a fresh *prometheus.Registry private to the
// relay. Each call returns an independent instance — tests construct as
// many as they need without process-wide collisions. The relay's wiring
// site (cmd/pyrycode-relay/main.go, landing in #60) holds exactly one
// registry for the process lifetime.
//
// Counters and gauges added by sibling tickets MUST register against the
// registry returned here (or a prometheus.Registerer derived from it),
// NEVER against prometheus.DefaultRegisterer. ADR-0008 § Scope of use
// fixes that rule.
func NewMetricsRegistry() *prometheus.Registry {
	return prometheus.NewRegistry()
}

// NewMetricsHandler returns an http.Handler that serves reg in the standard
// Prometheus text exposition format ("text/plain; version=0.0.4;
// charset=utf-8"). Pattern follows NewHealthzHandler (#10):
// factory-shaped, no per-request state, safe for concurrent use.
//
// The handler does not negotiate the OpenMetrics content type
// (EnableOpenMetrics defaults to false in promhttp.HandlerOpts). Holding
// the text format stable keeps the on-the-wire response shape
// independent of the caller's Accept header, matching the relay's
// /healthz posture (no content negotiation).
//
// Errors during collection are propagated to the response with HTTP 500
// (promhttp's default ErrorHandling = HTTPErrorOnError); the relay does
// not currently inject a logger into the handler — if a future ticket
// wants collection errors in slog, it threads them via ErrorLog at the
// wiring site, not here.
func NewMetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
	})
}
```

**Note on `HandlerOpts.Registry`.** Setting `Registry: reg` enables
`promhttp` to register its own self-instrumentation counters
(`promhttp_metric_handler_*`) on the same private registry. This is
desirable: it keeps internal handler errors (e.g. concurrent-scrape
limit hits, if a future ticket sets one) visible on `/metrics` instead
of silently absent. Leaving the field nil would land those counters on
`DefaultRegisterer`, which ADR-0008 § Scope of use forbids — so setting
the field is structurally how this ticket holds the ADR's no-default-
registry boundary even for the library's own bookkeeping.

### 2. The architect's "package-vars-vs-struct" pick

**Pick: per-concern collector struct, constructed by a sibling-ticket
helper that takes `prometheus.Registerer`.** Each sibling ticket
defines a *separate* file under `internal/relay/` (e.g.
`metrics_upgrade.go` in #57, `metrics_register.go` in #58) with its
own type and constructor:

```go
// (sibling ticket — illustrative, not landed by #59)
type upgradeFloodMetrics struct {
	rejectsTotal *prometheus.CounterVec
}

func newUpgradeFloodMetrics(reg prometheus.Registerer) *upgradeFloodMetrics {
	m := &upgradeFloodMetrics{
		rejectsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "relay_upgrade_rejects_total", Help: "…"},
			[]string{"reason"},
		),
	}
	reg.MustRegister(m.rejectsTotal)
	return m
}
```

Why this shape, not the alternatives:

1. **Package-level vars (rejected).** A `var upgradeFloodTotal = …` at
   package scope cannot register against a registry that
   `NewMetricsRegistry()` constructs per call — package-level
   initialisers run at import time and can only attach to a
   package-level registry, which forces the registry into a singleton.
   The relay's existing seam shape (`NewRegistry()` returns a fresh
   `Registry`; tests construct as many as they need; see
   ADR-0003) treats per-call construction as load-bearing for
   testability. A metrics singleton would diverge for no benefit.
2. **One mega-struct on a public type (rejected).** A
   `type Metrics struct { UpgradeFloodTotal …; RegisterFailsTotal …; … }`
   that grows a field per sibling ticket forces every sibling PR to
   touch the same file in the same struct. That is a guaranteed merge
   conflict between concurrently-architected siblings (#57 and #58 in
   flight at the same time — the file-overlap check at architect time
   would block whichever lands second from running). It also bundles
   unrelated concerns under one type.
3. **Per-concern collector struct (adopted).** Each sibling owns a
   private `*xxxMetrics` type in its own file. The wiring site
   (`main.go`, #60) holds *N* of them as locals, threads each into
   the handler that increments it. Adding a new metric type is a
   greenfield file under `internal/relay/`; no edit to `metrics.go`
   or to other siblings' files. This is the natural Go-idiomatic
   shape and the one ADR-0008's *Scope of use* assumes.

This pick lives in this ticket's spec, not in `metrics.go` itself —
the seam this ticket exposes (`NewMetricsRegistry`, `NewMetricsHandler`)
admits the pattern without naming it. The spec is the place sibling
architects read first.

### 3. `internal/relay/metrics_test.go` (new, ~80 lines)

Three tests, no fixtures, pure stdlib + `prometheus/common/expfmt` (a
transitive of `client_golang` — already in the module graph, no new
dep).

```go
package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
)
```

**Test 1: `TestMetricsHandler_EmptyRegistry_ResponseShape`** — covers the
explicit AC ("an empty registry served via the handler returns HTTP 200
with `Content-Type: text/plain; version=0.0.4; charset=utf-8` … and a
parseable body").

```go
func TestMetricsHandler_EmptyRegistry_ResponseShape(t *testing.T) {
	t.Parallel()

	reg := NewMetricsRegistry()
	h := NewMetricsHandler(reg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Content-Type"),
		"text/plain; version=0.0.4; charset=utf-8"; got != want {
		t.Errorf("Content-Type: got %q, want %q", got, want)
	}

	// "Parseable body" — round-trip the response through expfmt's text
	// decoder. An empty registry produces zero MetricFamily entries;
	// the decoder must reach io.EOF without an error before that.
	dec := expfmt.NewDecoder(rec.Body, expfmt.FmtText)
	var families int
	for {
		var mf dto.MetricFamily // see import note below
		if err := dec.Decode(&mf); err != nil {
			if err.Error() == "EOF" || errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
		}
		families++
	}
	if families != 0 {
		t.Errorf("empty registry: got %d MetricFamilies, want 0", families)
	}
}
```

Imports the test needs: `io`, `errors`, plus
`dto "github.com/prometheus/client_model/go"` for `dto.MetricFamily`.
Both are transitives of `client_golang` already in the module graph
(see ADR-0008 § Supply-chain notes). The developer wires the import
block accordingly.

Implementation note on the content-type literal: at `client_golang` v1.x
the constant `expfmt.FmtText` resolves to that exact string. If a future
release renames or version-bumps the format constant, the test's literal
string is the canary — keep the literal in the assertion (not a
re-export of `expfmt.FmtText`) so a silent format change in a bump
surfaces as a test failure rather than a tautological pass.

**Test 2: `TestMetricsHandler_RegisterAndScrape_RoundTrip`** — a smoke
check that the seam actually plumbs through. Constructs a registry,
registers one trivial counter against it, increments it, scrapes,
asserts the counter appears in the body in text format.

```go
func TestMetricsHandler_RegisterAndScrape_RoundTrip(t *testing.T) {
	t.Parallel()

	reg := NewMetricsRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "relay_test_counter_total",
		Help: "Sole purpose: prove the registry → handler seam works.",
	})
	reg.MustRegister(c)
	c.Add(2)

	h := NewMetricsHandler(reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "relay_test_counter_total 2") {
		t.Errorf("body missing counter+value; got:\n%s", body)
	}
}
```

This test is not in the AC literally but is the cheapest possible
seam-verification — it would catch a regression where, e.g., a future
bump of `client_golang` changes the handler's `prometheus.Gatherer`
contract and the registered counter no longer surfaces. ~15 lines; it
pays its freight.

**Test 3: `TestMetricsRegistry_NoGlobalRegistrarLeak`** — structural
defence for ADR-0008's "never `prometheus.DefaultRegisterer`" rule.

```go
func TestMetricsRegistry_NoGlobalRegistrarLeak(t *testing.T) {
	// Snapshot the default registerer's collectors. After
	// NewMetricsRegistry()+NewMetricsHandler() are called, the default
	// registerer must contain the same set — nothing the relay does
	// in this ticket may register against the global.
	t.Parallel()
	before := defaultRegistererSize(t)
	_ = NewMetricsHandler(NewMetricsRegistry())
	after := defaultRegistererSize(t)
	if before != after {
		t.Fatalf("default registerer changed: before=%d, after=%d "+
			"(metrics.go must never register on prometheus.DefaultRegisterer; "+
			"see ADR-0008 § Scope of use)", before, after)
	}
}

func defaultRegistererSize(t *testing.T) int {
	t.Helper()
	// Use the default gatherer (which wraps DefaultRegisterer). Gather()
	// returns one MetricFamily per registered Collector's metrics.
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("DefaultGatherer.Gather: %v", err)
	}
	return len(mfs)
}
```

The "snapshot before/after" framing tolerates whatever the Go runtime
or `client_golang` itself has already registered on the default
gatherer at process start — the test asserts a delta of zero, not an
absolute count. This makes it stable across `client_golang` version
bumps that might pre-register their own internals.

This test enforces the ADR rule structurally rather than by review.
~20 lines; pays its freight by closing the most-likely future bypass
(a contributor reaches for `promauto.NewCounter(...)` which silently
registers on `DefaultRegisterer`).

## Concurrency model

None this ticket lands. `promhttp.HandlerFor`'s returned handler is
documented safe for concurrent calls; the registry's `Register` /
`MustRegister` / `Gather` methods take their own locks internally.
Sibling tickets that register counters will do so once at construction
(in the wiring site) — that is a single-goroutine call before
serving begins.

## Error handling

Three concrete failure modes the spec accounts for:

1. **`go mod tidy` cannot resolve the new dep.** Most likely cause is a
   sandboxed CI environment with no proxy access — `make build` would
   fail with a Go module error. Out of scope to mitigate in this
   ticket; the developer reports the env issue on the ticket and the
   pipeline owner sets `GOPROXY` appropriately. Recovery: re-run
   `make build` after the env is fixed.
2. **`promhttp.HandlerFor` returns a 500 on a collector error at scrape
   time.** Not reachable in this ticket (empty registry; no collectors).
   The handler's `ErrorHandling` defaults to `HTTPErrorOnError` — the
   500 is returned with a body containing the error message. ADR-0008's
   *Scope of use* notes that a future ticket can inject `ErrorLog`
   via `HandlerOpts` to route the error into `slog`; this ticket does
   not.
3. **A sibling ticket registers a colliding metric name.** `MustRegister`
   panics on collision. The panic surfaces at process startup (in
   `main.go`'s wiring), not at scrape time — desirable, because it
   refuses to serve a broken metrics surface. Spec relies on each
   sibling's tests to catch their own collision before merge.

## Testing strategy

- **`make test` runs the three tests above; they each exercise an
  independent contract** (empty-registry response shape, round-trip
  registration, no-global-leak structural invariant).
- **No race tests.** The seam this ticket lands is single-goroutine;
  the library handles its own locking. `go test -race ./...` will run
  the new tests under `-race` because `make test` is `-race`-wide, but
  there is no goroutine the tests need to spin up for race coverage.
- **No new e2e or integration coverage.** The listener wiring is #60;
  that ticket's tests will hit `/metrics` over HTTP against a running
  process. This ticket's gate is unit-test-only.

## Open questions

1. **`promhttp.HandlerOpts.MaxRequestsInFlight` and `Timeout`.** Both
   default to zero (unbounded / no timeout). The relay's listener (#60)
   sits behind the existing `http.Server` whose `ReadTimeout` /
   `WriteTimeout` bound scrape-side I/O — so the in-handler defaults
   are not a DoS exposure here. Recorded so the #60 architect sees the
   decision delegated to them: if the metrics listener is on a separate
   port with its own `http.Server`, the timeouts on *that* server are
   what matters; the in-handler options remain optional.
2. **Process / Go-runtime collectors.** Out of scope for this ticket
   (ADR-0008 § Scope of use names them as forbidden for #59). A future
   ticket may opt in by registering them on the relay's private
   registry, never on the default. No design decision needed here; the
   ADR is the contract.
3. **`docs/knowledge/codebase/59.md` and `docs/knowledge/INDEX.md`
   updates.** The AC lists both. Per the architect's own "Never Update"
   rule (`docs/knowledge/INDEX.md` is documentation-phase territory),
   the architect does **not** write either file in this ticket. The
   developer's commit ships the code; the documentation agent's
   post-merge run lands the codebase summary and the INDEX entry. The
   AC is satisfied at the ticket level by the documentation phase, not
   by the architect or developer phases. Recorded here so the developer
   does not chase an AC item that is not theirs.

## Out of scope

- Any gauge or counter (`/metrics` is empty after this ticket).
- The `/metrics` listener (#60's job).
- Bind-address validation, flag plumbing, TLS for the metrics port.
- Process / Go-runtime collectors.
- An `ErrorLog` wiring from `promhttp` into `slog`.
- Updating the `docs/PROJECT-MEMORY.md` line 37 dep list (frozen
  document; documentation phase owns evergreen knowledge).

## Security review (security-sensitive ticket)

### Mindset

Re-reading the spec as an adversary. Default verdict FAIL until each
applicable category below has either a concrete finding or an explicit
design decision that makes the category not applicable.

### 1. Trust boundaries

- **Inbound trust boundary:** none introduced by this ticket. The
  `/metrics` listener (which would terminate untrusted scrape traffic)
  is #60's responsibility. This ticket ships a handler factory and
  a registry constructor; both are pure constructors with no I/O.
- **Internal boundary:** the registry private to the relay package vs.
  `prometheus.DefaultRegisterer`. ADR-0008 names the rule; this spec
  enforces it structurally with Test 3
  (`TestMetricsRegistry_NoGlobalRegistrarLeak`).

### 2. Tokens, secrets, credentials

- The relay's only handled secret is the phone token (`X-Pyrycode-Token`),
  presence-checked and discarded at `/v1/client` (#5). This ticket adds
  no code path that touches a token. Metric labels added by sibling
  tickets MUST NOT carry token values; that rule lands in each sibling's
  spec, not here. Recorded so the next architect reading this file sees
  the boundary explicitly: **metric labels are an exfiltration channel;
  treat them with the same MUST-NOT-emit posture as log values
  (`docs/threat-model.md` § Log hygiene)**.

### 3. File operations

- None. `metrics.go` performs no file I/O.

### 4. Subprocess / external command execution

- None.

### 5. Cryptographic primitives

- None directly. `client_golang` v1.x has no `crypto/*` API surface
  the relay reaches into. (The transitive dep `xxhash/v2` is a
  non-cryptographic hash used internally for label-set keying; not
  attacker-influenced from outside the process.)

### 6. Network & I/O

- **Handler exposure surface.** `promhttp.HandlerFor` returns a handler
  whose response size is proportional to the number of registered
  collectors. For this ticket, the registry is empty — response is
  essentially zero bytes. Sibling tickets registering CounterVec /
  HistogramVec with high-cardinality labels could expand this; that
  is each sibling's threat-model concern, with the security-sensitive
  label on their tickets gating the check.
- **No `Accept` content negotiation in this ticket.** The handler
  always returns text format (OpenMetrics disabled by leaving
  `EnableOpenMetrics: false` at default). This avoids the negotiator
  becoming a covert channel for content-type-based parser
  differentials. Decision documented in `metrics.go`'s docstring.
- **No `http.Server` timeout policy in this ticket.** The handler is
  *constructed* here; serving is #60's job. Recorded in *Open
  questions* #1 for the #60 architect.

### 7. Error messages, logs, telemetry

- **No `slog` integration.** The handler does not log; collection
  errors return HTTP 500 with the error string in the body. The error
  string from `prometheus.Gatherer.Gather` is library-internal and does
  not carry relay state — it names a collector type or a label name,
  not a request header or a token. Acceptable for this ticket; the #60
  architect can revisit by injecting `ErrorLog` if a future regression
  flags a sensitive string.
- **Metric labels as a telemetry channel.** Empty for this ticket. The
  boundary applies to siblings (point in *Tokens, secrets, credentials*
  above).

### 8. Concurrency

- The handler is documented safe for concurrent calls; the registry
  uses internal locking. This ticket spawns no goroutines, holds no
  locks, owns no shared state.

### 9. Threat model alignment

- `docs/threat-model.md` § *Log hygiene*: extends to metric labels per
  the rule recorded above (sibling tickets enforce).
- `docs/threat-model.md` § *DoS*: the `/metrics` listener as a scrape
  surface is #60's design; this ticket does not introduce a new
  network-reachable endpoint.
- `docs/threat-model.md` § *Supply chain*: ADR-0008 enumerates the
  transitive dep set, names the maintenance status, and binds the dep
  to `go.sum` digests. `govulncheck` covers the expanded surface via
  `make lint`.
- Protocol spec (`pyrycode/pyrycode/docs/protocol-mobile.md`) is
  unaffected by this ticket — no wire-protocol surface change.

### Findings

- **[Trust boundaries]** No findings — single internal boundary
  (private registry vs. `DefaultRegisterer`) enforced structurally by
  Test 3.
- **[Tokens]** SHOULD FIX (deferred to sibling specs) — metric labels
  are a future exfiltration channel; rule recorded above so each
  sibling architect sees the constraint when their spec lands.
- **[File operations]** No findings — none performed.
- **[Subprocess]** No findings — none performed.
- **[Cryptographic primitives]** No findings — none used.
- **[Network & I/O]** OUT OF SCOPE — listener is #60. Handler-side
  content negotiation pinned to text format; recorded.
- **[Errors / logs / telemetry]** No findings for this ticket — handler
  does not log; collection-error strings are library-internal.
- **[Concurrency]** No findings — no new goroutines, locks, or shared
  state.
- **[Threat model]** No findings unique to this ticket — supply-chain
  expansion documented in ADR-0008; log/DoS/protocol surfaces
  unchanged or deferred.

### Verdict

**PASS.** No MUST FIX findings. The one SHOULD FIX (metric-label
exfiltration) is deferred to sibling specs by design — they each
carry the `security-sensitive` label and gate the check at the right
layer. Recording the rule here propagates the constraint without
inventing scaffolding work that has no caller in this ticket.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-11
