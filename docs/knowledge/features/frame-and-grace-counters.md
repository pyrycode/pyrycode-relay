# Frame-forwarded and grace-expiry counters

Three Prometheus counter time series that let an operator see relay throughput and distinguish "binary disconnected and reconnected within the grace window" from "binary disconnected and the slot was actually reclaimed":

- `pyrycode_relay_frames_forwarded_total{direction="phone_to_binary"}` — frames the relay successfully forwarded from a phone to the binary holding the server-id.
- `pyrycode_relay_frames_forwarded_total{direction="binary_to_phone"}` — frames the relay successfully forwarded from a binary to a registered phone.
- `pyrycode_relay_grace_expiries_total` — number of grace timers that actually evicted a server-id slot (real evictions only; stale `time.AfterFunc` fires never increment).

All three are monotonic non-negative integer counters. The `direction` label has exactly two hard-coded values; no other labels exist on either metric.

## API

Package `internal/relay`:

```go
// metrics_forward.go
func NewForwardMetrics(reg prometheus.Registerer, src *Registry)

// metrics_grace.go
func NewGraceMetrics(reg prometheus.Registerer, src *Registry)
```

Each constructor (a) registers its counter against `reg` and (b) wires the per-event increment hook on `src` via `Registry.SetForwarderHooks` / `Registry.SetGraceExpiryHook`. Boot-time only — set the hooks before either listener starts serving.

## Design — push-shaped counters via nil-safe hooks on `*Registry`

Counters of events stay push-shaped (one Inc per event), the natural shape for `Counter`s. The wrinkle: the increment sites live in code (`registry.go`, `forward.go`) that — per ADR-0008's *Scope of use* boundary — must NOT import `prometheus`. Three private `func()` fields on `*Registry` provide the seam:

- `onPhoneForwarded` — invoked by `StartPhoneForwarder` after a successful `binary.Send`.
- `onBinaryForwarded` — invoked by `StartBinaryForwarder` after a successful `phone.Send`.
- `onGraceExpiry` — invoked by `handleGraceExpiry` after the pointer-identity guard's success branch.

All three fields are nil-safe (call sites are `if h := …; h != nil { h() }`); tests that don't wire metrics see no-op behaviour. The fields are exported only through `SetForwarderHooks` / `SetGraceExpiryHook` setters; hook bodies in `metrics_forward.go` / `metrics_grace.go` are pure `prometheus.Counter.Inc` calls that acquire no relay-side lock.

The two natural alternatives were rejected:

1. **Parameter threading** (passing `*forwardMetrics` through `ClientHandler` / `ServerHandler` / `Start*Forwarder` signatures) — measured cost was ≥10 call-site updates across ≥7 files, right at the architect's edit-fan-out red line. Rejected.
2. **Package-level vars** — forces one registry per process, breaks per-test isolation under `-race`. Already rejected by the #59 scaffolding spec.

The hooks-on-Registry shape pays a one-shot cost (Registry's surface gains three fields it does not "own") in exchange for zero signature cascade and zero test churn. Any future per-Registry-event counter (e.g. `phones_registered_total`) lands the same way: one private hook + one setter + one nil-safe call site.

## Increment placement — load-bearing for correctness

**`StartPhoneForwarder`** increments `direction="phone_to_binary"` exactly once per successful `binary.Send` (after the existing `if err := binary.Send(wrapped); err != nil` check passes). The single-sink forwarder returns on Send error, so the increment is naturally below all error paths.

**`StartBinaryForwarder`** increments `direction="binary_to_phone"` exactly once per successful `phone.Send`. The three `continue` paths — malformed envelope (`Unmarshal` err), unknown `conn_id` (no matching phone), per-phone `Send` err — branch out before the increment, structurally guaranteeing no over-count.

**`handleGraceExpiry`** increments `grace_expiries_total` as the LAST step of the success branch, after `r.mu.Unlock()` and the phone-close loop. The pointer-identity guard's stale-fire `return` exits before the hook call, so stale fires never increment by construction — the same structural defence the connection-count gauges (#61) ride for free.

## No attacker-influenced labels

The `direction` label's two values (`phone_to_binary`, `binary_to_phone`) are string literals in `metrics_forward.go`; neither comes from a request header, query parameter, frame payload, or registry state. Cardinality is exactly 2 regardless of traffic. `grace_expiries_total` has no labels.

The production files' doc comments restate the constraint so a future contributor cannot quietly re-add a `{server="<id>"}` label that would carry the attacker-influenced `x-pyrycode-server` header value onto the metrics surface (forbidden by threat model § Log hygiene, which extends to metric labels).

## Concurrency

- **No new goroutines.** Hook bodies run on whichever goroutine the caller is on: the forwarder goroutine (per connected phone/binary) for the frame hooks, the `time.AfterFunc` goroutine for the grace hook.
- **No new locks.** `Counter.Inc` is internally atomic per `client_golang`'s contract.
- **Hook field reads are unlocked.** The fields are written exactly once at boot (via the setters) before any forwarder or timer goroutine launches; per the Go memory model's "goroutine creation happens-before goroutine body" rule, the reads are race-free.
- **Hooks MUST NOT acquire `r.mu`.** The doc comment on each field states the constraint — `handleGraceExpiry` already holds the lock at parts of its body; a hook re-entering registry mutator paths would deadlock. The concrete hook bodies are pure `Counter.Inc` calls and acquire no relay-side lock.
- **CounterVec children are pre-bound.** `NewForwardMetrics` calls `WithLabelValues("phone_to_binary")` and `WithLabelValues("binary_to_phone")` once in the constructor and captures the resulting `prometheus.Counter` values, so each frame's hot path is a single atomic Inc on a pre-resolved child — no map lookup per frame.

`TestGraceMetrics_RaceFreedom` runs 16 goroutines × 200 ops of claim / schedule-release / cancel-replace cycles under `-race`; the race detector's verdict is the structural assertion that the hook field's concurrent reads against boot-time writes are clean.

## Wiring

Both constructors are called from `cmd/pyrycode-relay/main.go`'s metrics block, alongside `NewConnectionsMetrics`:

```go
relay.NewConnectionsMetrics(metricsReg, reg)
relay.NewForwardMetrics(metricsReg, reg)
relay.NewGraceMetrics(metricsReg, reg)
```

All three register against the private `metricsReg` (no `DefaultRegisterer`) and the resulting `/metrics` response is served by the localhost-only listener wired in #60.

## Out of scope

- **`pyrycode_relay_send_duration_seconds` histogram** for per-frame send latency was deferred. The hot-path cost (`time.Now()` × 2 per frame + bucket bookkeeping) warrants its own architect pass; the value/cost trade-off is unsettled. Its own ticket if it lands.
- **Upgrade/register counters** for the WS-upgrade reject paths (#57) — sibling ticket, same registry-hook pattern.

## Related

- [Metrics registry (scaffolding)](metrics-registry.md) — the seam these counters plug into (#59).
- [Metrics listener (localhost-only)](metrics-listener.md) — the `/metrics` surface that serves them (#60).
- [Connection-count gauges](connection-count-gauges.md) — pull-based sibling collector; the seam admits both shapes (#61).
- [Phone-side frame forwarder](phone-forwarder.md) — the `StartPhoneForwarder` call site for the `phone_to_binary` increment.
- [Binary-side frame forwarder](binary-forwarder.md) — the `StartBinaryForwarder` call site for the `binary_to_phone` increment.
- [Connection registry](connection-registry.md) — the `handleGraceExpiry` call site and pointer-identity guard (ADR-0006).
- [ADR-0008: Prometheus client adoption](../decisions/0008-prometheus-client-adoption.md) — scope-of-use rules that motivated the hook indirection.
- [ADR-0006: Grace window IS the reclaim path](../decisions/0006-grace-period-as-reclaim-path.md) — pointer-identity guard the grace counter rides for free.
- [Threat model](../../threat-model.md) — log-hygiene rule extended to metric labels.
