# Spec — `ws_upgrade_attempts_total` + `register_failures_total` counters (#57)

## Files to read first

- `internal/relay/server_endpoint.go:38-115` — the four terminal branches the
  spec hooks: header-gate reject (line 43-46), `ErrServerIDConflict` (line
  65-77), success path (line 85-94), and an unreachable defensive branch
  (line 78-83) the spec must NOT increment.
- `internal/relay/client_endpoint.go:37-117` — the five terminal branches:
  header-gate reject (line 42-45), `ErrNoServer` (line 64-73),
  `ErrPhonesAtCap` (line 74-80), defensive unreachable branch (line 81-83),
  success path (line 85-97).
- `internal/relay/ratelimit_middleware.go` (whole file, ~40 lines) — the
  deny path this spec observes via response status. Empty-IP guard (line
  27-31) and bucket-deny (line 32-36) both write `http.StatusTooManyRequests`
  and are the ONLY HTTP-status-429 sources in either pipe.
- `internal/relay/metrics.go` (whole file, ~50 lines) — `NewMetricsRegistry`
  and the ADR-0008 rule: "register against the registry returned here,
  NEVER against `prometheus.DefaultRegisterer`". The
  `TestMetricsRegistry_NoGlobalRegistrarLeak` invariant in `metrics_test.go`
  is the structural enforcement.
- `internal/relay/metrics_forward.go` (whole file, ~45 lines) — pattern
  reference for a counter-vector concern: pre-bound `prometheus.Counter`
  fields on a small struct, single constructor that registers the vector
  and binds the labels. This ticket follows that shape with two vectors.
- `internal/relay/metrics_connections.go:32-51` — the `direction`-label
  pattern note: label values are hard-coded constants, never
  attacker-influenced. Same constraint applies to `endpoint`, `outcome`,
  and `kind` here.
- `internal/relay/metrics_counters_test.go:14-35` — `assertCounter` helper.
  Same package, no duplication. New tests scrape via `NewMetricsHandler` and
  substring-match `<metric>{<labels>} <want>`.
- `internal/relay/server_endpoint_test.go:18-43` — `startServer` / `dialWith`
  / `validHeaders` helpers. The new counter test reuses them.
- `internal/relay/client_endpoint_test.go:18-60` — `startClient` /
  `startClientWithCap` / `dialWithClient` / `validClientHeaders` /
  `seedBinary` helpers. The new counter test reuses them.
- `internal/relay/ratelimit_middleware_test.go:240-280` — shape reference
  for testing rate-limit deny against a real handler. Adapt to assert
  counter increments instead of registry pristineness.
- `cmd/pyrycode-relay/main.go:103-154` — current metrics-wiring + handler-
  mounting block. New `NewUpgradeMetrics` call lands alongside
  `NewConnectionsMetrics`/`NewForwardMetrics`/`NewGraceMetrics`; the two
  endpoint mounts gain the new metrics parameter and a per-endpoint
  rate-limit deny observer.
- `docs/specs/architecture/58-frame-and-grace-counters.md` § *Why
  hooks-on-Registry, not parameter threading* — negatively relevant: that
  spec rejected parameter threading because the increment sites lived in
  code that doesn't (and per ADR-0008 § Scope of use must not) import
  `prometheus`. This spec accepts a bounded parameter threading because
  three of the five terminal sites (header-reject, success, rate-limit
  deny) have NO Registry call to hook into.
- `docs/specs/architecture/59-metrics-scaffolding.md` § *Scope of use* —
  the ADR-0008 rule reproduced. Load-bearing for the test invariant.
- `docs/PROJECT-MEMORY.md` line 24 — *"Validate at the envelope boundary,
  not deeper."* Negatively relevant: the `endpoint`, `outcome`, and
  `kind` label values are hard-coded constants, set at the call site, not
  derived from request state.

## Context

Slice of the metrics rollout (split from #37). The metrics scaffolding
(#59) shipped `NewMetricsRegistry()` returning a private
`*prometheus.Registry`; #58 and #61 added the forward/grace/connected
metrics. This ticket adds the **upgrade-attempt counter** (labels
`endpoint`, `outcome`) and the **register-failure counter** (label `kind`)
at the `/v1/server` and `/v1/client` call sites.

Per the ticket body, the label cardinality is fixed and small: 2 endpoints
× 6 outcomes + 4 register-failure kinds = 16 series total. **All 12
endpoint×outcome combinations are pre-bound**, including the three
structurally unreachable ones (`server,reject_404`; `server,reject_429`;
`client,reject_409`), so the metric surface is stable and discoverable —
ops dashboards see zero-valued series rather than the series silently
missing. The AC pins this: "exposes **all 16 series** in Prometheus text
format" after the per-path exercise.

### Two load-bearing design choices

1. **Where does the rate-limit-deny increment live?** The rate-limit
   middleware short-circuits before the handler runs; the handler cannot
   observe the deny. Adding a hook parameter to `NewRateLimitMiddleware`
   would cascade across 11 call sites (1 prod + 10 tests) — at the
   edit-fan-out red line. This spec picks a **status-code observer
   wrapper** stacked OUTSIDE the rate-limit middleware: HTTP 429 from the
   wrapped pipe is the rate-limit-deny signal, full stop. The rate-limit
   middleware is structurally the only HTTP-429 source in either pipe
   (the handler writes 400 for header-gate; WS-close codes 4409/4404/4429
   travel inside the 101-upgraded WebSocket, not as HTTP status). One
   wrapper per endpoint carries the `endpoint` label as a hard-coded
   constant.

2. **How are the counters reached from the handlers?** Three of the five
   per-endpoint terminal sites (header-reject, success, rate-limit-deny)
   have no Registry call to hook through. Hooks-on-Registry (the #58
   pattern) does not work here. This spec adds an exported
   `*UpgradeMetrics` parameter to `ServerHandler` and `ClientHandler`.
   The parameter is **nil-safe**: callers that don't care about metrics
   (every existing test) pass `nil`. The 5 server-handler test sites and
   4 client-handler test sites get a mechanical `, nil` append — under
   the 10-call-site red line for edit fan-out.

## Design

### Package layout

Two new files, one modified middleware test, one modified main, two
modified endpoint files, two modified endpoint test files.

```
internal/relay/
  metrics_upgrade.go              (new) — UpgradeMetrics struct + constructor + observer middleware
  metrics_upgrade_test.go         (new) — table-driven per-terminal-path tests + 16-series scrape test
  server_endpoint.go              (modify) — add metrics parameter, 4 increment points
  client_endpoint.go              (modify) — add metrics parameter, 5 increment points
  server_endpoint_test.go         (modify) — 3 call sites: append `, nil`
  client_endpoint_test.go         (modify) — 3 call sites: append `, nil`
  ratelimit_middleware_test.go    (modify) — 1 call site: append `, nil`
cmd/pyrycode-relay/main.go        (modify) — construct UpgradeMetrics, mount per-endpoint deny observer
```

### `UpgradeMetrics` — type and constructor (`metrics_upgrade.go`)

Shape mirrors `forwardMetrics` (`metrics_forward.go:17-44`):

- Exported type `UpgradeMetrics` with **unexported** pre-bound
  `prometheus.Counter` fields — one per (endpoint, outcome) combination
  in the full 12-cell cross product, one per `kind` in the 4-cell
  register-failure space. The fields themselves are unexported because
  callers reach them only through the event-named methods below; this
  matches `forwardMetrics`.
- Constructor `NewUpgradeMetrics(reg prometheus.Registerer) *UpgradeMetrics`
  registers two `*prometheus.CounterVec`s against `reg` (NOT
  `prometheus.DefaultRegisterer` — the existing
  `TestMetricsRegistry_NoGlobalRegistrarLeak` enforces this structurally)
  and binds all 16 label combinations via `WithLabelValues`.
- Metric names and help strings:
  - `pyrycode_relay_ws_upgrade_attempts_total` — "WebSocket upgrade
    attempts by endpoint and outcome. One increment per terminal
    decision; a single request never increments more than once."
  - `pyrycode_relay_register_failures_total` — "Registry-side
    registration failures by kind. Co-incremented with the matching
    `ws_upgrade_attempts_total{outcome=reject_*}` series; their sum on
    `kind` equals the sum on the matching `outcome` value."

- Event-named methods (nil-receiver safe — `if m == nil { return }`
  at the top of each):
  - `(*UpgradeMetrics).ServerAccept()`
  - `(*UpgradeMetrics).ServerHeaderReject()`
  - `(*UpgradeMetrics).ServerIDConflict()`     — bumps `upgrade{server,reject_409}` AND `failures{kind=server_in_use}`
  - `(*UpgradeMetrics).ClientAccept()`
  - `(*UpgradeMetrics).ClientHeaderReject()`
  - `(*UpgradeMetrics).ClientNoServer()`        — bumps `upgrade{client,reject_404}` AND `failures{kind=no_server}`
  - `(*UpgradeMetrics).ClientPhonesAtCap()`     — bumps `upgrade{client,reject_429}` AND `failures{kind=phones_at_cap}`
  - `(*UpgradeMetrics).ServerRateLimitDeny()`   — bumps `upgrade{server,reject_rate_limit}` AND `failures{kind=ratelimit}`
  - `(*UpgradeMetrics).ClientRateLimitDeny()`   — bumps `upgrade{client,reject_rate_limit}` AND `failures{kind=ratelimit}`

Method names encode the **event**, not the labels — same convention as
`forwardMetrics.phoneToBinary` / `binaryToPhone`. Composite methods are
the increment contract: register-failure and upgrade-outcome co-increment
in lockstep, so a single method bumps both. This makes the "their sum on
`kind` equals the sum on the matching `outcome`" invariant a property of
the method, not of the caller — tests can't accidentally bump one without
the other.

### Rate-limit deny observer — `WrapRateLimitDeny` (`metrics_upgrade.go`)

Small middleware factory; one call site per endpoint in `main.go`. Shape:

```
func (m *UpgradeMetrics) WrapServerRateLimitDeny(next http.Handler) http.Handler
func (m *UpgradeMetrics) WrapClientRateLimitDeny(next http.Handler) http.Handler
```

Both return an `http.Handler` that wraps `next` with a status-code
observer (an inner `http.ResponseWriter` that records `WriteHeader`
calls). If the wrapped pipe wrote `http.StatusTooManyRequests`, the
wrapper calls `m.ServerRateLimitDeny()` / `m.ClientRateLimitDeny()`
respectively. Nil-receiver safe.

Wiring order in `main.go`:

```
mux.Handle("/v1/server",
    upgradeMetrics.WrapServerRateLimitDeny(
        rateLimit(
            relay.ServerHandler(reg, logger, 30*time.Second, maxFrameBytes, upgradeMetrics))))
mux.Handle("/v1/client",
    upgradeMetrics.WrapClientRateLimitDeny(
        rateLimit(
            relay.ClientHandler(reg, logger, maxFrameBytes, 16, upgradeMetrics))))
```

The observer sits OUTSIDE the rate-limit middleware so it sees the
middleware's 429. It sits OUTSIDE the upgrade handler too, but the
handler never writes HTTP 429 — it writes HTTP 400 (header gate),
HTTP 101 (success path, regardless of subsequent WS close code), or no
HTTP status (4xx written by `websocket.Accept` itself on a malformed
upgrade). So the observer's "429 → rate-limit deny" interpretation is
unambiguous.

The two `WrapXxxRateLimitDeny` methods are named-per-endpoint rather than
a single `WrapRateLimitDeny(endpoint string, next)` because the label
value MUST be a compile-time constant, not a string parameter — same
constraint as `metrics_forward.go`'s hard-coded `direction` values.

### Handler increment placement

Each increment fires **immediately after** the deciding branch is taken
and **before** any error return / WS close. This keeps "did this code
path execute?" structurally tied to "did the counter increment?" — the
linkage is one statement, no intermediate control flow.

`server_endpoint.go`:

- After the header-gate check (line 43-46): on the reject branch, call
  `metrics.ServerHeaderReject()` between `http.Error` and `return`.
- After the `ClaimServer` `errors.Is(err, ErrServerIDConflict)` check
  (line 65-77): call `metrics.ServerIDConflict()` between the close-code
  write and the `logger.Info` line. (Compound method bumps both upgrade
  outcome and register-failure kind.)
- The defensive `wsconn.Close()` branch at line 78-83 is unreachable
  from the current registry — the spec MUST NOT increment there. A
  future error variant added to `ClaimServer` would be a new outcome and
  belongs in a follow-up ticket.
- After `reg.ClaimServer` returns nil and `logger.Info("server_claimed",
  ...)` fires (line 85-88): call `metrics.ServerAccept()` before the
  `defer func() { ... }()` block.

`client_endpoint.go`:

- After the header-gate check (line 42-45): `metrics.ClientHeaderReject()`
  before the `return`.
- After `ErrNoServer` branch (line 64-73): `metrics.ClientNoServer()`
  before the `return`.
- After `ErrPhonesAtCap` branch (line 74-80): `metrics.ClientPhonesAtCap()`
  before the `return`.
- The defensive `wsconn.Close()` branch at line 81-83 is unreachable —
  MUST NOT increment.
- After `reg.RegisterPhoneCapped` returns nil and the success log fires
  (line 85-89): `metrics.ClientAccept()` before the `defer func() {
  ... }()`.

### Concurrency model

`prometheus.Counter.Inc()` is documented thread-safe. No additional
synchronization needed. The observer middleware's `WriteHeader`
recording uses a per-request `*statusObserverResponseWriter` — request-
scoped, no shared state across goroutines.

### Error handling

The counter increments cannot fail (panic-only on a malformed metric
registration, which the constructor catches at boot via `MustRegister`).
Per-request paths have no error surface to add — they are post-condition
markers, not actions that can themselves fail.

### Testing strategy — `metrics_upgrade_test.go`

Five tests, all `t.Parallel()`-safe:

1. **`TestUpgradeMetrics_ServerEndpoint_TerminalPaths`** — table-driven
   over the four server terminal cases (`reject_headers`, `reject_409`,
   `accept`, **and one synthetic 429 case driven via the deny observer**).
   For each row: construct `NewRegistry()` + `NewMetricsRegistry()` +
   `NewUpgradeMetrics(mreg)`; stand up an `httptest.NewServer` running
   `ServerHandler(reg, logger, grace, maxFrameBytes, upgradeMetrics)`
   wrapped in `WrapServerRateLimitDeny`; exercise the terminal path;
   `assertCounter` on the exact label set with value 1, and on every
   OTHER `(server, *)` label set with value 0. The "and 0 elsewhere"
   half is what proves no double-increment on a single request.
2. **`TestUpgradeMetrics_ClientEndpoint_TerminalPaths`** — same shape
   over the five client terminal cases. The `reject_404` row dials
   without seeding a binary; the `reject_429` row seeds a binary,
   registers `maxPhones` phones first via `startClientWithCap`, then
   dials once more.
3. **`TestUpgradeMetrics_RegisterFailures_CoIncrement`** — pin the
   AC invariant that
   `register_failures_total{kind=server_in_use}` increments **exactly
   when** `ws_upgrade_attempts_total{endpoint=server,outcome=reject_409}`
   does, and the same for `no_server`, `phones_at_cap`, `ratelimit`.
   Drive one terminal of each kind, scrape, assert the matched pairs
   step in lockstep.
4. **`TestUpgradeMetrics_AllSixteenSeries_Exposed`** — after a synthetic
   exercise of all 9 reachable terminal paths (the unreachable 3 are
   pre-bound to zero by the constructor), scrape via
   `NewMetricsHandler(mreg)` and assert the response body contains a
   line for **each of the 16 expected series**. Use the existing
   `assertCounter` helper; the unreachable ones are asserted at value 0.
5. **`TestUpgradeMetrics_NoGlobalRegistrarLeak`** — invoke
   `NewUpgradeMetrics(NewMetricsRegistry())`; before/after snapshot of
   `prometheus.DefaultGatherer.Gather()` length is unchanged. Same shape
   as `TestMetricsRegistry_NoGlobalRegistrarLeak`. (This is also covered
   transitively by that existing test if it picks up our new
   constructor, but pinning it locally is cheap and survives future
   refactors of `metrics_test.go`.)

### Test fan-out (existing tests)

Mechanical `, nil` appends, no behavior change:

- `server_endpoint_test.go` lines 25, 106, 339 — three sites.
- `client_endpoint_test.go` lines 25, 36, 125 — three sites.
- `ratelimit_middleware_test.go` line 263 — one site.

Total 7 mechanical edits. Per the architect playbook's "no mechanical
edits escape" rule, raw count of edited call sites for the
`ServerHandler` + `ClientHandler` signature change is 9 (the 7 above +
2 in `cmd/pyrycode-relay/main.go`), strictly under the 10-call-site red
line. The `NewRateLimitMiddleware` signature is intentionally NOT changed
— that's the load-bearing reason the deny observer is a wrapper rather
than a middleware-internal hook (avoids 11-call-site fan-out).

## Open questions

- **Should the observer also fire on header-gate-induced 400s?** No —
  the AC defines `reject_headers` as the header-gate outcome, and the
  handler itself increments that branch. The observer's 429-only filter
  is intentional.
- **Should `WrapServerRateLimitDeny` exist on `*UpgradeMetrics` or as a
  free function?** Method on `*UpgradeMetrics` — keeps the per-endpoint
  label-binding co-located with the constructor that bound it, and means
  the wiring site reads `upgradeMetrics.WrapServerRateLimitDeny(...)`
  (clear: "wrap with the deny counter from these metrics") rather than
  `relay.WrapServerRateLimitDeny(upgradeMetrics, ...)` (one indirection
  more, and ambiguous about whose counters).
- **The `register_failures_total` `kind` label space** is defined as
  closed (4 values). If a future ticket introduces a fifth registry-side
  failure, the closed-label contract requires (a) updating the
  constructor's `WithLabelValues` calls, (b) updating
  `TestUpgradeMetrics_AllSixteenSeries_Exposed`'s expected-series list,
  and (c) updating the AC. The `ws_upgrade_attempts_total` `outcome`
  label is similarly closed. Both lists belong in the godoc on
  `NewUpgradeMetrics`.

## Security review

**Verdict:** PASS

**Findings:**

- **[1. Trust boundaries]** No findings. The handlers' trust boundary is
  the header-gate check at the top of `ServerHandler` / `ClientHandler`
  (unchanged by this spec). All counter increments fire AFTER the
  deciding branch is taken, so no attacker-influenced data reaches the
  metric system as a value (counters increment; they don't carry
  payloads).
- **[2. Tokens, secrets, credentials]** No findings — this spec touches
  no tokens. `X-Pyrycode-Token` remains unread/unlogged/uncompared per
  `client_endpoint.go:46-47`'s existing contract.
- **[3. File operations]** N/A — no filesystem code paths.
- **[4. Subprocess / external command execution]** N/A.
- **[5. Cryptographic primitives]** N/A.
- **[6. Network & I/O]** No findings. Input-size caps (`maxFrameBytes`),
  header validation, and timeout discipline are unchanged. The deny
  observer's `statusObserverResponseWriter` does not buffer the response
  body — it only records the status code passed to `WriteHeader` (one
  `int` per request). No new memory pressure or DoS vector.
- **[7. Error messages, logs, telemetry]** **MUST FIX prevented at the
  design level**: all three new labels (`endpoint`, `outcome`, `kind`)
  are **hard-coded constants** at the call site, set by the
  event-named methods. None are derived from request headers, request
  paths, or any other attacker-influenced input. Same load-bearing
  constraint as `metrics_forward.go`'s `direction` label, with the same
  rationale (`docs/specs/architecture/58-frame-and-grace-counters.md`
  § Security review § Tokens): a label whose value comes from a
  user-controlled header explodes cardinality (cost) and exposes the
  header value to anyone scraping `/metrics` (information leak). The
  fixed 16-series cardinality is recorded in this spec's Context as
  "label cardinality budget", auditable by reading the constructor.
- **[8. Concurrency]** No findings. `prometheus.Counter.Inc()` is
  thread-safe by contract. The observer middleware's
  `statusObserverResponseWriter` is request-scoped — one per
  `ServeHTTP` call, never shared.
- **[9. Threat model alignment]** Covers `docs/threat-model.md`'s
  observability requirement: the operator can distinguish the four
  "flood" patterns called out in the user story (malformed headers, 4409
  conflicts, 4404/4429 phone failures, rate-limited clients) without
  log-grepping. No new exposure to the threat-model adversary —
  `/metrics` continues to be served on a separate loopback listener
  (`metricsListen` in `main.go`), so the new counters are not reachable
  from the internet-exposed listener.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
