# Spec — `/healthz` returns JSON with version + connection counts (#10)

## Files to read first

- `cmd/pyrycode-relay/main.go:18-46` — current `/healthz` plain-text handler, `mux` setup site, the `Version` package var (already overridden via `-ldflags`). Wiring lands here.
- `internal/relay/registry.go:48-64` — `Registry` struct and `NewRegistry()`. The handler holds a `*Registry` reference.
- `internal/relay/registry.go:175-188` — `Counts() (binaries, phones int)`. The doc-comment explicitly says one call is internally consistent; two concurrent calls may observe different values. That is the right semantic for a health probe.
- `internal/relay/envelope.go:10-19` — sentinel-error idiom and doc-comment style. Match it for any new exported symbol.
- `internal/relay/registry_test.go:24-50` — `fakeConn` shape, established test pattern (`package relay`, table-style assertions). Reuse for registry-population tests.
- `docs/PROJECT-MEMORY.md:22-40` — established patterns: stdlib-default, "loud failure over silent correction," package-internal tests, "interface methods called under the lock are documented as non-blocking getters."
- `docs/threat-model.md` — the section on operational surface + DoS resistance applies because `/healthz` is unauthenticated and internet-exposed.
- `docs/specs/architecture/15-wsconn-adapter.md` — recent example of the spec/test style and security-review template this repo uses.
- `Makefile` — `make test` runs `go test -race ./...`; `make lint` runs `gosec` + `govulncheck`. New code must pass both.

## Context

Today's `/healthz` returns `"ok\n"` plain-text from an inline closure in `cmd/pyrycode-relay/main.go:44-47`. The endpoint is public-readable by design; off-host probes (Uptime Kuma, Healthchecks.io, custom Prometheus collectors later) need a structured signal beyond "200 OK." This ticket replaces the inline closure with a JSON handler that returns five fields: `status`, `version`, `connected_binaries`, `connected_phones`, `uptime_seconds`.

The registry from #3 already exposes `Counts() (binaries, phones int)` — the only piece of state the handler reads. `cmd/pyrycode-relay/main.go` does not yet construct a `*relay.Registry` (no WS upgrade handlers exist yet), so this ticket also instantiates the registry and threads it into the handler, even though no producer pushes connections into it in v1. Counts will be `(0, 0)` in production until the WS upgrade tickets land — that is correct and intentional; the handler reads whatever the registry currently holds.

The ticket's security surface is small but real: the endpoint is unauthenticated and internet-exposed, so the handler must not introduce probe-amplification cost, must not echo caller input (no CRLF injection vector), and must not leak per-server-id information. The accepted tradeoff (per the ticket body) is exposing aggregate counts.

## Design

### Package & files

- New file: `internal/relay/healthz.go` — `package relay`, exports `NewHealthzHandler`.
- New test file: `internal/relay/healthz_test.go` — `package relay`, follows the established `envelope_test.go` / `registry_test.go` convention.
- Edit: `cmd/pyrycode-relay/main.go` — capture `startedAt`, construct `*relay.Registry`, wire the new handler.

No other files touched. No new dependencies (stdlib `encoding/json` + `net/http` + `time` only).

### Types & API

```go
package relay

import (
    "encoding/json"
    "net/http"
    "time"
)

// healthzResponse is the JSON shape returned by the /healthz handler.
// Field order is significant: encoding/json marshals in declaration order
// and the AC pins the on-the-wire key order.
//
// Unexported because the shape is an implementation detail of the handler;
// callers consume the JSON, not the Go type.
type healthzResponse struct {
    Status            string `json:"status"`
    Version           string `json:"version"`
    ConnectedBinaries int    `json:"connected_binaries"`
    ConnectedPhones   int    `json:"connected_phones"`
    UptimeSeconds     int64  `json:"uptime_seconds"`
}

// NewHealthzHandler returns an http.Handler that responds to every request
// with a small JSON body containing the relay's status, version, current
// connection counts, and uptime. The handler is intended to be served
// unauthenticated on /healthz.
//
// reg is the live connection registry; the handler reads it via Counts()
// on every request. version is the build-time version string (the same
// value --version prints). startedAt is the moment the relay began serving
// requests; uptime is reported as time.Since(startedAt) rounded to whole
// seconds.
//
// Safe for concurrent use: holds no per-request state, spawns no goroutines,
// performs one read-locked Counts() call per request.
func NewHealthzHandler(reg *Registry, version string, startedAt time.Time) http.Handler
```

The handler is returned as `http.Handler` (not `http.HandlerFunc`) so its construction stays factory-shaped: a future ticket that needs to add per-handler state (a logger, a clock injection for tests) can do so without changing the call site in `main`.

### Algorithm

The handler body, in full:

```go
func NewHealthzHandler(reg *Registry, version string, startedAt time.Time) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        binaries, phones := reg.Counts()
        uptime := int64(time.Since(startedAt).Seconds())
        if uptime < 0 {
            uptime = 0
        }

        body, _ := json.Marshal(healthzResponse{
            Status:            "ok",
            Version:           version,
            ConnectedBinaries: binaries,
            ConnectedPhones:   phones,
            UptimeSeconds:     uptime,
        })

        w.Header().Set("Content-Type", "application/json; charset=utf-8")
        w.Header().Set("Cache-Control", "no-store")
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write(body)
    })
}
```

Notes on each step:

- **`reg.Counts()`** — single RLock-bounded read; releases before any I/O. The registry's doc-comment already names the consistency model (one call is internally consistent), which is exactly the semantic a probe wants.
- **`time.Since` floor at zero** — `time.Now()` carries a monotonic clock reading, and `time.Since` uses the monotonic component, so under normal operation this can never go negative. The floor is defence-in-depth against future refactors that pass a `startedAt` constructed without monotonic state (e.g. unmarshalled from JSON in a test). Cheap to keep; eliminates a class of "uptime: -3 seconds" bug that would surface as monitoring noise.
- **`json.Marshal` of a fixed-shape struct of primitives cannot fail** — the discarded error is correct, not a smell. We marshal then write (rather than `json.NewEncoder(w).Encode`) so the response is atomic: one `Write` call, no torn frames, no trailing newline.
- **`Cache-Control: no-store`** — defence-in-depth against an intermediate proxy or CDN caching live counts. Without it, a misconfigured edge could serve stale data for minutes. One line; clearly scoped; matches "loud failure over silent correction" by stating the intent on the wire.
- **Method handling** — the handler responds identically to every HTTP method. Matches the existing inline handler's behaviour. The AC is "GET returns 200 + JSON"; not adding a 405 path is *Don't add features… beyond what the task requires* — restricting methods is a separate, evidence-driven ticket if probe storms or method-confused clients ever surface.
- **Body size budget** — with a realistic upper bound (`version` ~ 30 bytes, counts ≤ 6 digits, uptime ≤ 10 digits), the marshalled body is well under 200 bytes. Worst-case math sketched in *Testing strategy* below.

### Wiring in `main`

The four edits to `cmd/pyrycode-relay/main.go`:

1. After `flag.Parse()` and before constructing `mux`, capture the start moment and construct the registry:

   ```go
   startedAt := time.Now()
   reg := relay.NewRegistry()
   ```

   `startedAt` is captured **after** flag parsing and the `--version` early-return so it reflects "began serving requests" rather than "binary started," matching the ticket's specification.

2. Replace the inline closure in `mux.HandleFunc("/healthz", ...)` with `mux.Handle("/healthz", relay.NewHealthzHandler(reg, Version, startedAt))`.

3. The registry has no producers in v1 (no WS upgrade handlers yet); the handle sits idle until #4/#5/#16 wire it up. Holding it now means those tickets don't have to refactor `main` — they just take the existing `reg` and pass it into the upgrade handlers.

4. No change to autocert wiring, listeners, or timeouts.

### Concurrency model

Single goroutine per request, as for any `http.Handler`. The handler holds no internal mutex. The only shared-state read is `reg.Counts()`, which takes the registry's RWMutex in read mode for the duration of one map iteration. Counts of currently registered binaries and phones are released before any response-writing I/O, so the registry's RLock is never held across a network write. This matches the established pattern (`PhonesFor` does the same — copy under lock, do slow work outside).

No goroutines are spawned. No background timer. No channel.

### Error handling

- `json.Marshal` of `healthzResponse` cannot fail (no maps, no `json.Marshaler` implementations, no `chan`/`func` fields). The discarded error is documented inline; no fallback path is added because the failure mode is unreachable.
- `w.Write` errors (client disconnected mid-response) are intentionally ignored — same convention as the existing inline handler and as #15's `WSConn.Close`. There is no useful recovery for a probe that drops mid-response; logging would only generate noise.
- No new sentinel errors (no protocol-level distinctions to expose).
- No panics. No nil checks on `reg`, `version`, or `startedAt` — passing zero values is a programmer error, not a runtime case.

## Testing strategy

`internal/relay/healthz_test.go`, `package relay`. End-to-end against the real handler via `httptest.NewRecorder` — no HTTP server needed because `http.Handler.ServeHTTP` is callable directly.

### Tests (1:1 with AC bullets)

1. **`TestHealthz_ResponseShape`** — construct `NewHealthzHandler(NewRegistry(), "test-version", time.Now().Add(-30*time.Second))`; serve a `GET /healthz` request via `httptest.NewRecorder`; assert:
   - status code is 200
   - `Content-Type` header is `application/json; charset=utf-8`
   - `Cache-Control` header is `no-store`
   - decoding the body into `healthzResponse` yields no error
   - all five fields are present and well-typed (`status == "ok"`, `version == "test-version"`, both counts are non-negative, `uptime_seconds >= 30`)
   - response body length is under 200 bytes
   
   This single test covers AC bullets 1, 2, 3, 4, 6, 7, 8.

2. **`TestHealthz_TracksRegistryState`** — populate a fresh registry (claim 2 binaries via `ClaimServer` with `fakeConn` stand-ins; register 5 phones across 2 server-ids via `RegisterPhone`); construct the handler; serve a request; decode the body; assert `connected_binaries == 2` and `connected_phones == 5`. Reuse the `fakeConn` test helper from `registry_test.go` (already in the same package — no re-export needed).
   
   Covers AC bullet 5 and 9.

3. *(Not added — listed for the developer's awareness)* No test of HTTP method dispatch (no method-restricted behaviour to verify); no test of clock-skew (the floor is defence-in-depth, not observable behaviour); no test of `json.Marshal` failure (unreachable).

### Body-size budget

Realistic upper bound for the JSON body, character-counted:

| Field | Max value | JSON length |
|---|---|---|
| `"status":"ok"` | constant | 13 |
| `"version":"99.99.99-rc.99-pre.99"` | ~25 chars | ~37 |
| `"connected_binaries":999999` | 6 digits | 27 |
| `"connected_phones":999999` | 6 digits | 25 |
| `"uptime_seconds":9999999999` | ~10 digits (≈317 yr uptime) | 27 |
| Punctuation (4 commas, 2 braces) | constant | 6 |
| **Total worst-case** | | **~135 bytes** |

Comfortably under the 200-byte bound. The `TestHealthz_ResponseShape` assertion fixes this against regression.

### Lint expectations

`make vet`, `make test` (under `-race`), `make build` all clean. `gosec ./...` clean — the only function with security review attention here is `w.Header().Set` (constant strings, no injection vector). `govulncheck ./...` clean — no new dependencies.

## Open questions

1. **Should the handler restrict to `GET`/`HEAD`?** No. The current handler accepts every method; the AC names only `GET`. Restricting adds code without an observed failure mode. If a future probe-storm ticket lands, that ticket adds the 405.

2. **Should `version` be re-read on every request, in case it ever becomes mutable?** No. `Version` in `cmd/pyrycode-relay/main.go` is a `var` overridden at build time via `-ldflags` and never reassigned at runtime. Capturing it once at handler-construction is correct; reading on every request adds nothing and would invite a future refactor to "make `Version` a function."

3. **Should `startedAt` be replaceable for tests?** Pass-through in the constructor already gives tests full control (they pass any `time.Time`). No clock interface needed.

## Out of scope (re-stated, for the developer)

- No per-server-id breakdown — would leak server-ids; explicitly excluded by the ticket.
- No `degraded`/`unhealthy` status values — v1 is binary OK.
- No latency histograms or request counters; no `/metrics` endpoint.
- No auth, no rate-limit. Probe-storm mitigation is a separate, evidence-driven ticket.
- No structured logging of healthz hits (would dominate logs at probe cadence).
- No edits to `internal/relay/registry.go`, `internal/relay/envelope.go`, autocert wiring, or the threat model.

## Done means

- `internal/relay/healthz.go` exists with `NewHealthzHandler` and the unexported `healthzResponse` struct. Doc-comment on `NewHealthzHandler` names its concurrency contract and the unauthenticated-by-design intent.
- `internal/relay/healthz_test.go` covers the two functional tests above.
- `cmd/pyrycode-relay/main.go` constructs `startedAt` and `reg`, and the `/healthz` route is `relay.NewHealthzHandler(reg, Version, startedAt)`. No other behavioural change.
- `make vet`, `make test` (`-race`), `make build` clean. `gosec ./...` and `govulncheck ./...` clean.
- One commit on `feature/10`: `feat(relay): JSON /healthz with version and connection counts (#10)`.

---

## Security review (security-sensitive label)

### Threat surface for THIS ticket

`/healthz` is unauthenticated and internet-exposed: anyone on the public internet can hit it as often as they like. The handler reads from `Registry.Counts()` (a brief RLock against the live in-memory state used by every routed frame) and emits a small JSON body. Two distinct concerns: amplification cost on the relay (every probe touches the lock that frame routing also needs) and information leakage (counts are sent to unauthenticated callers).

### Categories walked

- **Trust boundaries.** The handler reads zero bytes of caller-controlled input. It ignores method, headers, query string, and body. There is no path by which an adversary's bytes appear in the response, so there is no CRLF injection, no JSON-injection, no `Content-Type` confusion vector. The two arguments to the constructor (`version`, `startedAt`) come from `main`'s build-time/process-start state and are trust-elevated. **Finding:** no new trust boundary; no input-validation work needed.

- **Information disclosure — aggregate counts.** Exposing `connected_binaries` and `connected_phones` to anonymous callers is an explicit, ticket-acknowledged tradeoff: the operator value (graphable load over time, off-host probes without secret distribution) is judged greater than the small operational-intel leak (an attacker learns "the relay is currently routing N phones"). The mitigation against deeper leakage — *no per-server-id breakdown* — is enforced by using the aggregate `Counts()` API, not by a filter the handler could be coaxed into bypassing. **Finding:** matches threat-model intent; the API shape (no server-id input, no server-id output) makes the leak structurally bounded.

- **Information disclosure — version string.** The relay's version is already revealed indirectly via deployment artefact / Github releases / behaviour fingerprinting. Echoing it on `/healthz` makes it explicit, which is operationally desirable (probes can confirm the deploy succeeded) and not a meaningful uplift for attackers — the public release line is, definitionally, public. **Finding:** acceptable; no new exposure beyond what the published binary already encodes.

- **Resource exhaustion / probe amplification.** The handler does one RLock-bounded `Counts()` call (sub-microsecond, two map iterations on small maps), one `json.Marshal` of five primitive fields (sub-microsecond), two header sets, two writes. Cost per request is dominated by the TLS handshake on `:443`, not by the handler body. The registry's RLock contends with frame-routing writes only when the latter takes the *write* lock (`ClaimServer`, `ReleaseServer`, `RegisterPhone`, `UnregisterPhone`); under steady-state traffic those writes are infrequent compared to the `Counts()` read. **Finding:** no amplification factor; per-request work is bounded by O(1) header writes and O(1) map iterations on a state that is itself bounded by future per-IP connection caps. The ticket explicitly notes that probe-storm rate-limiting is a separate evidence-driven ticket; that decision stands here.

- **Resource exhaustion / response size.** The body is bounded under 200 bytes by the field set; the worst-case math is in the testing strategy. There is no path for an attacker's input to grow the response (no echoing). **Finding:** bounded by construction.

- **Cache poisoning / stale data via intermediaries.** A health endpoint with live counts must not be cached by an intermediate proxy or CDN; otherwise an operator could be misled by a stale reading. Mitigated by `Cache-Control: no-store`. This also blocks an adversary's ability to cache-poison the response for other observers (a request smuggling vector exists in some intermediaries; `no-store` makes this entry uncacheable). **Finding:** mitigated.

- **Race conditions / data corruption.** No mutable state shared across requests. `reg.Counts()` takes a read-lock and copies primitive ints out before returning. The handler closes over `reg`, `version`, `startedAt` — all read-only after construction. **Finding:** none observed; `-race` will catch regressions.

- **Lock-order / deadlock.** Single lock involved (`Registry.mu`, RLock-only). Held only by the registry method; released before any response I/O. **Finding:** no deadlock path.

- **Panics / nil-deref.** Constructor accepts arguments by value; no nil checks. A nil `*Registry` would panic on `reg.Counts()` — but `main` is the only caller, and `main` constructs `reg` non-nil before passing it. Defensive nil checks would only mask programmer error. **Finding:** none observed.

- **Error leakage to the wire.** No error path returns text to the caller. Even the unreachable `json.Marshal` failure would write nothing back. **Finding:** none.

- **Header injection.** `Content-Type` and `Cache-Control` are set with constant strings. No caller-controlled value reaches a header. **Finding:** none.

- **Method confusion.** The handler responds identically to every method, so a `POST /healthz` with a body returns the same JSON. The body is not parsed and is discarded by `net/http`. There is no state mutation, so method confusion has no security consequence. **Finding:** none.

- **Timing side channels.** Counts are aggregate. There is no per-server-id branch, so an attacker cannot use response-time differences to enumerate specific server-ids. The `time.Since` math is constant-time. **Finding:** none.

- **Supply chain.** No new dependencies. Stdlib `encoding/json`, `net/http`, `time` only. **Finding:** none.

### Adversarial framings considered

- *"What if an attacker scrapes `/healthz` every 100ms to track connection counts over time?"* — They learn the same time-series an operator would graph: load, daily patterns, deploy moments. The ticket accepts this. Per-IP rate limiting is an explicit follow-up if probe storms become a problem.

- *"What if `/healthz` is hit hard enough to starve frame routing of the registry's write lock?"* — `Counts()` takes only RLock; readers do not block other readers. Writers (frame-routing connect/disconnect) are not blocked by readers either, except on the brief moment a writer holds the write lock — and that moment is bounded by the registry's own internal map operations. There is no path by which read-side `/healthz` traffic prevents a writer from eventually acquiring the write lock. (Go's `sync.RWMutex` has writer-preference behaviour that prevents starvation.)

- *"Can an attacker use the count fields to time their attack to a low-traffic moment?"* — Yes; aggregate visibility includes "the relay currently has 0 binaries connected." Operators value this signal more than they fear it; the ticket explicitly chose this tradeoff.

- *"Can an attacker fingerprint specific binaries via count delta after a targeted action?"* — Coarsely: yes, an attacker who can also force a specific binary to disconnect would see `connected_binaries` decrement by 1. But the attacker needs out-of-band influence over the binary; the relay's `/healthz` only confirms the disconnect happened. The information leak is not the relay's to mitigate.

- *"What if a future refactor adds a `Cache-Control: max-age=N` header?"* — Live counts would become stale at the edge. `no-store` is the correct value; any future refactor that weakens it should be flagged in review.

- *"What if `version` is empty (the binary was built without `-ldflags`)?"* — The `Version` package var defaults to `"dev"`, never empty, so the JSON field is always populated. If a future build accidentally produces `Version = ""`, the JSON field is `"version":""` — ugly but not a vulnerability.

- *"What if `time.Since(startedAt)` overflows int64 seconds?"* — Would require ~292 years of uptime. Not a real failure mode.

- *"Is there a request-smuggling concern when the response is so small?"* — Go's `net/http` server writes `Content-Length` automatically when the body is buffered before the first `Write`. We `json.Marshal` then `Write` once, so `Content-Length` is set correctly and the framing is unambiguous. `Transfer-Encoding: chunked` is not used.

### Verdict: PASS

The handler reads no caller input, returns a bounded constant-shape body, takes no lock except a brief read-lock that is released before I/O, and matches the ticket's stated information-leak tradeoff (aggregate counts only, no per-server-id breakdown). `Cache-Control: no-store` is the one new piece of protective response shape; everything else is the structural minimum for an unauthenticated public health endpoint. No code-level mitigations are deferred; the only deferred mitigations are operational (per-IP rate limiting), correctly scoped to a future ticket.
