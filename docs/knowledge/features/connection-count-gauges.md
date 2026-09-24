# Connection-count gauges

Two Prometheus gauges expose the relay's live connection counts, the same numbers `/healthz` returns as a one-shot snapshot, but as a time series an operator can graph and alert on:

- `pyrycode_relay_connected_binaries` — number of pyrycode binary connections currently held by the registry. Excludes a binary in its grace window (disconnected, pending reclaim) — see below (#115).
- `pyrycode_relay_connected_phones` — number of mobile-client connections currently held, summed across all server-ids. Includes phones whose binary is in its grace window; they stay connected through it.

Both are scalar gauges with no labels. Values are non-negative integers.

## Grace-window exclusion (#115)

Both gauges read `Registry.Counts()` verbatim (see [Connection registry § Invariants](connection-registry.md)), so `Counts()`'s definition of "connected" is the gauges' definition, with no code of the gauges' own. A binary that has disconnected keeps its registry entry — and would otherwise keep being counted — until its grace timer fires or it reclaims the slot; `Counts()` now excludes a server-id with a pending grace timer from `binaries`, so the gauge (and `/healthz`'s `connected_binaries`, which reads the same call) reflects live daemon connections rather than live-plus-gracing ones. A reclaim or takeover during the grace window counts the binary again immediately, since both delete the timer as part of the same locked swap that installs the new binary.

## API

Package `internal/relay` (`metrics_connections.go`):

```go
func NewConnectionsMetrics(reg prometheus.Registerer, src *Registry)
```

Registers a single private `connectionsCollector` against `reg` that reads `src.Counts()` on every scrape. Returns nothing — the registerer holds the collector alive.

The constructor takes `prometheus.Registerer` (the `MustRegister`-only sub-interface), not the concrete `*prometheus.Registry`, so it composes with anything implementing the interface (e.g. `prometheus.WrapRegistererWith`). Same convention as `prometheus/client_golang`'s own constructors.

## Design — pull-based collector

The collector emits the gauges via `prometheus.Collector` / `MustNewConstMetric` on each `Collect`, reading the registry's live state through the existing `Registry.Counts()` getter. It never mutates the registry, never holds the registry lock itself, and adds no Inc/Dec call sites to `registry.go`.

Pull-based was picked over a push-based shape (Inc/Dec at every registry mutation site) on five concrete axes — recorded in the spec, summarised here because the rationale is load-bearing for sibling tickets:

1. **Zero diff in `registry.go`.** The registry is internet-exposed and already raced against under `-race`; widening its audit surface is expensive.
2. **The grace-expiry stale-fire AC is satisfied structurally.** `handleGraceExpiry`'s pointer-identity guard already keeps the maps unchanged on a stale fire (ADR-0006). Because the gauge IS the map size via `Counts()`, the no-op path can't move the gauge by construction — no `if r.timers[serverID] == self` branch to wire Dec into, no test required to assert the branch was taken.
3. **`Counts()` is the single source of truth.** Push-based would maintain a second source (the gauge's internal `AtomicInt64`) that must agree with the first; divergence is a class of bug pull-based cannot have.
4. **No new lock-acquisition path.** `Counts()` already holds RLock and walks the maps. The collector calls `Counts()` once per scrape — no new locking surface in `registry.go`.
5. **The metrics seam admits it.** The per-concern collector pattern from #59 was illustrated with a push-shaped `CounterVec`. This ticket instantiates the same pattern with a pull-shaped `Collector` — both shapes coexist; gauges-of-a-state are naturally pull, counters-of-events are naturally push.

Pull-based's nominal cost is freshness: the gauge is only as current as the last scrape. For Prometheus's typical 15-60s scrape interval and the relay's alerting use case, the freshness gap is well below the operator's alert window. Acceptable.

## No labels — load-bearing for log hygiene

Both gauges are scalar; `prometheus.NewDesc(name, help, nil, nil)` passes empty `variableLabels` and `constLabels`. A `{server="<server-id>"}` label would carry the attacker-influenced `x-pyrycode-server` header value onto the metrics surface, which the threat model § Log hygiene rule (which extends to metric labels) forbids. The production file's comments and the spec both name this constraint so a future contributor cannot quietly re-add a server label without first deleting the spec's instruction.

The constant-cardinality property also keeps the `/metrics` response size delta small (~80 bytes) regardless of how many server-ids the relay holds.

## Wiring

The collector lives in the registry's state for the process lifetime once `NewConnectionsMetrics(mreg, registry)` is called at startup. The wiring call site is `cmd/pyrycode-relay/main.go` next to `relay.NewRegistry()` and `relay.NewMetricsRegistry()`, alongside the [localhost-only `/metrics` listener](metrics-listener.md) wired in #60.

## Concurrency

- The collector spawns no goroutines and holds no mutable state — two `*prometheus.Desc` fields and one `*Registry` pointer set at construction.
- `Collect` calls `Counts()` once per scrape. `Counts()` takes the registry's RLock; the collector itself is lock-free. Concurrent scrapes are serialised inside `prometheus.Registry.Gather()`.
- `TestConnectionsMetrics_RaceFreedom` runs 16 mutator goroutines (claim / register / schedule-release / reclaim / unregister) against a tight-loop scraper under `-race`; the race detector's verdict is the structural assertion.

## Related

- [Metrics registry (scaffolding)](metrics-registry.md) — the seam this collector plugs into (#59).
- [ADR-0008: Prometheus client adoption](../decisions/0008-prometheus-client-adoption.md) — scope-of-use rules (no `DefaultRegisterer`, no process/runtime collectors).
- [ADR-0006: Grace window IS the reclaim path](../decisions/0006-grace-period-as-reclaim-path.md) — pointer-identity guard the pull-based design rides for free.
- [Connection registry](connection-registry.md) — `Counts()` is the live count source the gauges read.
- [`/healthz` JSON endpoint](healthz.md) — one-shot snapshot of the same two numbers (the gauges are the time-series companion).
- [Threat model](../../threat-model.md) — log-hygiene rule that extends to metric labels.
