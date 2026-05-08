# `/healthz` — JSON health endpoint

`GET /healthz` returns a small JSON object with the relay's status, build version, current connection counts, and uptime in seconds. Unauthenticated by design: off-host probes (Uptime Kuma, Healthchecks.io, future Prometheus collectors) need a structured signal beyond "200 OK" without secret distribution.

The endpoint replaces the prior `"ok\n"` plain-text response (#10).

## Wire shape

```http
GET /healthz HTTP/1.1
```

```http
HTTP/1.1 200 OK
Content-Type: application/json; charset=utf-8
Cache-Control: no-store

{"status":"ok","version":"0.1.0","connected_binaries":3,"connected_phones":12,"uptime_seconds":4512}
```

| Field | Type | Meaning |
|---|---|---|
| `status` | string | Always `"ok"` in v1. No `degraded` / `unhealthy` states yet. |
| `version` | string | Build-time `Version` (matches `--version`). Defaults to `"dev"` when not overridden via `-ldflags`. |
| `connected_binaries` | int | Currently-claimed binary slots, from `Registry.Counts()`. |
| `connected_phones` | int | Phones summed across all server-ids, from `Registry.Counts()`. |
| `uptime_seconds` | int64 | `time.Since(startedAt)` in whole seconds, floored at zero. |

The five-field set, key order, and `application/json` content type are part of the public contract — `encoding/json` marshals struct fields in declaration order, so `healthzResponse`'s field order is the on-the-wire order.

Response body is bounded ≈135 bytes worst-case (six-digit counts, decade-scale uptime, a long version string). The 200-byte test budget guards against regression.

## API

Package `internal/relay` (`healthz.go`):

```go
func NewHealthzHandler(reg *Registry, version string, startedAt time.Time) http.Handler
```

Returned as `http.Handler`, not `http.HandlerFunc` — keeps construction factory-shaped so a future ticket adding per-handler state (logger, injectable clock) can do so without changing `main`'s call site.

Wired in `cmd/pyrycode-relay/main.go` after flag parsing:

```go
startedAt := time.Now()
reg := relay.NewRegistry()

mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))
```

`startedAt` is captured **after** `flag.Parse()` and the `--version` early-return so it reflects "began serving requests," not "binary started." `reg` is constructed here even though no producer pushes connections into it yet; the WS upgrade tickets (#4/#5/#16) will reuse this handle without refactoring `main`.

## Concurrency

Single goroutine per request. Handler holds no internal mutex. The only shared-state read is `reg.Counts()`, which takes the registry's RWMutex in read mode for the duration of one map iteration and releases before any response I/O. No goroutines spawned, no channels, no background timers.

`Counts()`'s consistency contract — one call is internally consistent; two concurrent calls may observe different values — is exactly the semantic a probe wants.

## Design notes

- **`Cache-Control: no-store`.** Defence-in-depth against an intermediate proxy or CDN caching live counts. Without it, a misconfigured edge could serve minutes-old data and mislead operators. Also blocks any cache-poisoning vector against other observers.
- **`time.Since` floor at zero.** Under normal operation `time.Now()` carries a monotonic clock reading, so `time.Since` cannot go negative. The floor is defence-in-depth against future refactors that pass a `startedAt` constructed without monotonic state (e.g. unmarshalled in a test). Cheap; eliminates a class of `"uptime: -3"` monitoring noise.
- **`json.Marshal` error discarded.** Marshalling a fixed-shape struct of primitives cannot fail (no maps, no `Marshaler` impls, no chan/func fields). Discarded error is documented inline; no fallback path.
- **Marshal-then-`Write`, not `json.NewEncoder(w).Encode`.** Atomic single `Write` — no torn frames, no trailing newline, `Content-Length` set automatically.
- **Method-agnostic.** Handler responds identically to every method. Restricting to `GET`/`HEAD` would add code without an observed failure mode; if a probe-storm or method-confused-client ticket lands later, that ticket adds the 405.

## What this deliberately does NOT do

- **No per-server-id breakdown.** Would leak server-ids; explicitly out of scope. The structural mitigation is using the aggregate `Counts()` API rather than a filter the handler could be coaxed into bypassing.
- **No `degraded` / `unhealthy` status values.** v1 is binary OK or no-response.
- **No latency histograms, request counters, or `/metrics` endpoint.** Separate ticket if/when needed.
- **No auth, no rate-limiting.** Probe-storm mitigation is an evidence-driven follow-up.
- **No structured log per healthz hit.** Would dominate logs at probe cadence.
- **No re-read of `Version` per request.** Captured at handler construction; `Version` is build-time-fixed.

## Adversarial framing

Unauthenticated and internet-exposed. Threats considered:

- **Information disclosure — aggregate counts.** Acknowledged tradeoff: operators value the graphable load signal more than they fear the small operational-intel leak. Per-server-id breakdown is structurally excluded.
- **Information disclosure — version string.** Already encoded in the deployed binary / GitHub releases / behaviour fingerprinting; explicit publication adds nothing meaningful for attackers and lets probes confirm a deploy.
- **No caller input reaches the response.** Method, headers, query, body are all ignored. No CRLF / JSON / `Content-Type` injection vectors.
- **Probe amplification.** One RLock-bounded `Counts()` (sub-microsecond, two small map iterations), one `json.Marshal` of five primitives, two header sets, one `Write`. Per-request work is dominated by TLS handshake, not handler body. `RWMutex` writer-preference prevents reader-flood from starving frame-routing writes.
- **Cache poisoning via intermediaries.** Mitigated by `Cache-Control: no-store`; any future weakening to `max-age=N` should be caught in review.
- **Header injection.** All header values are constant strings.
- **Method confusion.** Handler is stateless and ignores method/body — no security consequence.
- **Timing side channels.** Aggregate counts and constant-time math; no per-server-id branch to enumerate.
- **Supply chain.** No new dependencies (stdlib `encoding/json`, `net/http`, `time`).

Verdict from #10's security review: **PASS**. The only new protective response shape is `Cache-Control: no-store`; the rest is the structural minimum for an unauthenticated public health endpoint.

## Testing

`internal/relay/healthz_test.go`, `package relay`. End-to-end against the real handler via `httptest.NewRecorder` — no HTTP server needed, `http.Handler.ServeHTTP` is callable directly.

- `TestHealthz_ResponseShape` — status 200, `Content-Type: application/json; charset=utf-8`, `Cache-Control: no-store`, decodes into `healthzResponse` cleanly, all five fields well-typed, body under 200 bytes. Covers AC bullets 1–4, 6–8.
- `TestHealthz_TracksRegistryState` — populates the registry (claimed binaries via `ClaimServer` + phones via `RegisterPhone` using the `fakeConn` helper from `registry_test.go`); decoded counts match the registry's actual state. Covers AC 5 and 9.

Not tested: HTTP method dispatch (no method-restricted behaviour); clock skew (the floor is defence-in-depth, not observable); `json.Marshal` failure (unreachable).

## Related

- [Connection registry](connection-registry.md) — `Counts()` is the data source.
- [Threat model](../../threat-model.md) — operational surface and DoS framings.
- [Routing envelope](routing-envelope.md) — sibling pattern of "validate at boundary, marshal a fixed-shape struct, never echo input."
