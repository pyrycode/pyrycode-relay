# Spec — localhost-only /metrics listener with bind-address validation (#60)

## Files to read first

- `cmd/pyrycode-relay/main.go:26-211` — entire `main` body. Two things to
  extract:
  1. The existing public-listener pattern (timeouts at lines 118-121 / 156-159
     / 168-172, goroutine launch at lines 200-205 for the http-01 listener,
     bare `ListenAndServe` at line 139 for insecure mode). The metrics
     listener mirrors these exactly.
  2. The `CheckListenerPorts` wiring at lines 123-138 (insecure mode) and
     174-195 (autocert mode). The metrics port MUST land in both the
     `expected` and `actual` sets when the metrics listener is enabled, or
     `#81`'s boot-time port allowlist refuses to start.
- `internal/relay/listeners.go:35-86` — `ListenerPort` extracts a `uint16`
  from an `http.Server.Addr` string; `CheckListenerPorts` is asymmetric
  (surplus is the failure mode, missing is not). Use `ListenerPort` for the
  metrics addr the same way main.go already uses it for `:443`/`:80`/insecure.
- `internal/relay/metrics.go:21-51` — `NewMetricsRegistry()` and
  `NewMetricsHandler(reg)` from #59's scaffolding. Consume both. Do NOT add
  new exported surface to this file — the seam is fixed.
- `internal/relay/metrics_connections.go:37-51` — `NewConnectionsMetrics(reg,
  src)` from #61. The wiring site (this ticket) calls it once at boot
  against the relay's `*Registry` (constructed at `main.go:98`). Future
  sibling tickets (#57, #58) will add similar `NewXxxMetrics` calls
  alongside.
- `docs/specs/architecture/59-metrics-scaffolding.md:165-169` — the
  "package-vars-vs-struct" pick and the implication for this ticket: each
  per-concern collector is a constructor call at the wiring site. Adding
  one collector here (`NewConnectionsMetrics`) is the pattern; #57 and #58
  will append.
- `docs/specs/architecture/81-listener-port-allowlist-boot-check.md` (whole
  file) — the asymmetric `expected ⊋ actual ⇒ refuse boot` semantics this
  ticket extends. The metrics port joins the expected set; that is the
  whole interaction.
- `docs/PROJECT-MEMORY.md` line 27 — *"Loud failure over silent correction."*
  Bind-address validation refuses to start on non-loopback / hostname / bad
  port. No silent fallback to localhost; no "warn and continue".
- `docs/threat-model.md` § *Log hygiene* (extends to metric labels per #59's
  spec) — relevant for the security review below: `/metrics` labels are an
  exfiltration channel. The collectors wired in this ticket
  (`NewConnectionsMetrics`) carry no labels by design.

## Context

The relay is internet-exposed. `/metrics` exposes operational state (active
connection counts indicate whether anyone is using the relay; future
upgrade/register counters reveal traffic patterns). Putting `/metrics` on
the same listener as `/healthz` (#10) and `/v1/{server,client}` would
publish that state to anyone who can reach the public listener.

The ticket body locks the exposure decision: `/metrics` runs on a separate
**localhost-only** listener (default `127.0.0.1:9090`). Operators scrape
via SSH tunnel or sidecar. The bind-address validation (loopback IP
literals only, hostnames refused) is part of the contract — not the
operator's responsibility to get right via runbook discipline.

This ticket is the wiring slice. #59 landed the registry and handler
factory; #61 landed `NewConnectionsMetrics`. #57 and #58 are concurrent
sibling tickets adding more collectors; they will append more
`NewXxxMetrics(metricsReg, …)` calls at the wiring site this ticket
establishes, but they are out of scope here — whatever is in the registry
at scrape time is what gets served.

## Design

Two artefacts: a new helper file in `internal/relay/` and edits to
`cmd/pyrycode-relay/main.go`. No edits to `internal/relay/metrics.go` —
the #59 seam is fixed.

### 1. `internal/relay/metrics_listen.go` (new)

Two exported functions and one sentinel error. Total ≤60 production lines
including doc comments.

```go
package relay

// ErrNonLoopbackBind is returned by CheckLoopbackBind / NewMetricsServer
// when --metrics-listen names a host that does not parse as a loopback
// IP literal. The relay refuses to start so a misconfiguration
// (typo, copy-paste from a non-loopback bind doc, accidental "0.0.0.0")
// does not silently publish operational state on the internet.
var ErrNonLoopbackBind = errors.New("relay: metrics listener bind address must be a loopback IP literal")
```

**`CheckLoopbackBind(addr string) error`** — pure validator.

- Behaviour contract (full enumeration; the test file is the executable spec):
  - `addr == ""` → returns an error naming the empty case
    (caller short-circuits *before* calling; this is a defence-in-depth
    return rather than a feature). The empty-disable path lives in main.go
    structurally, not in this helper.
  - `addr` not parseable by `net.SplitHostPort` → wrapped error naming
    the input.
  - port portion not a valid TCP port (per `ListenerPort` semantics:
    1..65535, no port 0) → wrapped error from `ListenerPort` so the
    surrounding pattern (port-0 refusal is already documented in
    listeners.go) extends to this listener.
  - host portion does not parse with `net.ParseIP` (hostname, empty,
    malformed) → wrapped `ErrNonLoopbackBind` with a message naming the
    offending value and the rule ("hostnames are rejected to avoid
    DNS-time TOCTOU; provide a loopback IP literal").
  - parsed IP, `IsLoopback() == false` → wrapped `ErrNonLoopbackBind`
    naming the offending IP.
  - parsed IP, `IsLoopback() == true` → return nil. Both IPv4 (`127.0.0.0/8`)
    and IPv6 (`::1`) loopback addresses are accepted.

- The hostname-refusal rationale (DNS TOCTOU) is the load-bearing security
  design choice — restate it in the docstring so the next reader does not
  "fix" the validator by adding `net.LookupHost`. A hostname can resolve
  to a loopback IP at validation time and a non-loopback IP at bind time
  (DNS rebinding, `/etc/hosts` race, resolver reconfigure). The only safe
  validation is to refuse the entire shape.

- Reuse `ListenerPort` for the port parse: it already wraps the same
  error families and refuses port 0. Do NOT duplicate the port logic.

**`NewMetricsServer(addr string, h http.Handler) (*http.Server, error)`** —
opt-out-aware constructor.

- Behaviour contract:
  - `addr == ""` → return `(nil, nil)`. This is the operator opt-out
    path: no listener, no goroutine, no error. Caller checks
    `srv != nil` to decide whether to enter the wiring branch.
  - `CheckLoopbackBind(addr)` returns non-nil → return `(nil, err)`. Caller
    logs and exits.
  - otherwise → return an `*http.Server` with:
    - `Addr: addr`
    - `Handler: h`
    - `ReadHeaderTimeout: 10 * time.Second`
    - `ReadTimeout: 60 * time.Second`
    - `WriteTimeout: 60 * time.Second`
    - `IdleTimeout: 120 * time.Second`
  - Timeout values are copied from the public listener (`main.go:118-121`).
    Centralised here so a future change to either listener's timeout
    policy does not silently diverge — but they are NOT exported as
    constants (each listener can drift independently if a future ticket
    has reason; today they match because that is the safest default).
  - The handler argument is `http.Handler`, not `http.ServeMux` — the
    caller chooses to wire either a bare `NewMetricsHandler(reg)` or a
    mux. Current main.go uses a `ServeMux` so the path `/metrics` is
    distinguishable from a future `/healthz` on this listener; that
    decision lives in main.go, not here.

### 2. `cmd/pyrycode-relay/main.go` edits

Three localised edits. All other code in `main` is untouched.

**Edit A — flag definition.** Add to the `flag.String` block at the top
of `main`:

```go
metricsListen = flag.String("metrics-listen", "127.0.0.1:9090",
    "Listen address for the /metrics endpoint. Must be a loopback IP "+
    "literal (e.g. 127.0.0.1:9090, [::1]:9090). Empty disables.")
```

**Edit B — metrics-listener wiring block.** After the existing
`startedAt` / `reg := relay.NewRegistry()` lines (~line 98) and before
the `mux := http.NewServeMux()` for the public listener (~line 106),
insert a block of the shape:

```go
metricsReg := relay.NewMetricsRegistry()
relay.NewConnectionsMetrics(metricsReg, reg)

metricsMux := http.NewServeMux()
metricsMux.Handle("/metrics", relay.NewMetricsHandler(metricsReg))

metricsSrv, err := relay.NewMetricsServer(*metricsListen, metricsMux)
if err != nil {
    logger.Error("refusing to start: invalid --metrics-listen address",
        "err", err,
        "value", *metricsListen,
        "fix", "use a loopback IP literal such as 127.0.0.1:9090 "+
            "or [::1]:9090, or pass --metrics-listen= to disable")
    os.Exit(2)
}
```

- `metricsSrv` is a local in `main`, kept in scope for the goroutine
  launch below and for the port-allowlist set construction.
- When `*metricsListen == ""` the constructor returns `(nil, nil)` and
  no error path runs — `metricsSrv` is nil thereafter; the goroutine
  launch and the port-set inclusion both short-circuit on `metricsSrv ==
  nil`. The opt-out is structural, not flag-tested.

**Edit C — port-allowlist integration and goroutine launch.** Two
sub-edits, one per public-listener mode.

*Insecure mode (`*insecureListen != ""` branch, ~line 113):*

- After `port, err := relay.ListenerPort(srv.Addr)` and BEFORE the
  `expected := map[uint16]struct{}{port: {}}` line, parse the metrics
  port (only if `metricsSrv != nil`) and add it to the expected/actual
  sets:

  ```go
  expected := map[uint16]struct{}{port: {}}
  actual := map[uint16]struct{}{port: {}}
  if metricsSrv != nil {
      mp, err := relay.ListenerPort(metricsSrv.Addr)
      if err != nil { /* same os.Exit(2) shape as the existing block */ }
      expected[mp] = struct{}{}
      actual[mp] = struct{}{}
  }
  // existing CheckListenerPorts call unchanged
  ```

- BEFORE `srv.ListenAndServe()`, launch the metrics listener in a
  goroutine (only if `metricsSrv != nil`):

  ```go
  if metricsSrv != nil {
      logger.Info("starting metrics listener", "listen", metricsSrv.Addr)
      go func() {
          if err := metricsSrv.ListenAndServe(); err != nil {
              logger.Error("metrics listener failed", "err", err)
              os.Exit(1)
          }
      }()
  }
  ```

*Autocert mode (the second branch, ~line 146 onwards):*

- Same shape. The existing `expected := map[uint16]struct{}{443: {}, 80:
  {}}` and `actual := …` lines extend by the metrics port the same way.
- The metrics goroutine launches alongside the existing `go func()`
  for the http-01 listener (~line 200), before `httpsSrv.ListenAndServeTLS`.

**Why a goroutine and not foreground:** the existing public listener
runs foreground (`ListenAndServe` blocks `main`); the http-01 listener
in autocert mode runs in a goroutine. The metrics listener is a
secondary listener, never the main exit point — goroutine is the right
choice. If `ListenAndServe` returns (typically because the port is
already bound or the address is rejected by the OS), the goroutine
calls `os.Exit(1)` to surface the failure rather than serving the public
listener with a missing metrics surface.

**Why no graceful shutdown wiring here:** the existing public listener
has none — process exits on signal naturally. #31 (open) will retrofit
SIGTERM-driven shutdown for both listeners; structuring `metricsSrv` as
a top-level local in `main` makes that retrofit a localised edit. This
ticket does not anticipate #31's design.

### 3. `internal/relay/metrics_listen_test.go` (new)

Table-driven coverage of the validator + constructor + an end-to-end
happy-path roundtrip. Three test functions; total ~120 lines.

- **`TestCheckLoopbackBind_Matrix`** — table test covering every branch
  of `CheckLoopbackBind`. Cases:
  - `127.0.0.1:9090` → nil
  - `127.0.0.5:1234` → nil (whole `127.0.0.0/8` is loopback)
  - `[::1]:9090` → nil
  - `0.0.0.0:9090` → wraps `ErrNonLoopbackBind`
  - `192.168.1.10:9090` → wraps `ErrNonLoopbackBind`
  - `[2001:db8::1]:9090` → wraps `ErrNonLoopbackBind`
  - `localhost:9090` → wraps `ErrNonLoopbackBind` (hostname, not an IP literal — the security-design rationale)
  - `127.0.0.1:0` → error (port 0 refusal flows from `ListenerPort`)
  - `127.0.0.1:99999` → error (out-of-range from `ListenerPort`)
  - `127.0.0.1` → error (no port, `net.SplitHostPort` fails)
  - `""` → error (empty addr; defence-in-depth case)
  - `:9090` → wraps `ErrNonLoopbackBind` (empty host doesn't parse as IP)

  Use `errors.Is(err, ErrNonLoopbackBind)` to branch on the loopback-rule
  failures specifically; other error cases assert non-nil only.

- **`TestNewMetricsServer_Matrix`** — three rows:
  - `addr=""` → returns `(nil, nil)`
  - `addr="0.0.0.0:9090"` → returns `(nil, err)` with `errors.Is(err, ErrNonLoopbackBind)`
  - `addr="127.0.0.1:9090"` → returns `(srv, nil)` with `srv.Addr == "127.0.0.1:9090"`, all four timeouts matching the public-listener policy literally (do NOT compare against a shared constant — the spec says they can drift independently; assert on the actual `time.Second` values).

- **`TestMetricsServer_EndToEnd_HappyPath`** — satisfies AC (a) against an
  actual listener, not just `httptest.NewRecorder`. Scenario:
  - Construct `reg := NewMetricsRegistry()`, register one trivial counter
    (so the response body is non-empty — the assertion is on
    `Content-Type` and status, not on the counter value, but a non-empty
    body shakes out any "handler is a no-op" regression).
  - `mux := http.NewServeMux(); mux.Handle("/metrics", NewMetricsHandler(reg))`.
  - `srv, _ := NewMetricsServer("127.0.0.1:0", mux)`.
  - Open a listener with `net.Listen("tcp", srv.Addr)` and call
    `go srv.Serve(l)` (not `ListenAndServe`, so the ephemeral port is
    knowable through `l.Addr()`). `t.Cleanup(func(){ srv.Close() })`.
  - `http.Get("http://" + l.Addr().String() + "/metrics")`.
  - Assert status == 200 and `Content-Type` matches the Prometheus text
    format (`text/plain; version=0.0.4; charset=utf-8`). Body is not
    parsed — that is `metrics_test.go`'s job (#59).

  This test is the AC (a) anchor. It exercises the validator + constructor
  + actual `net.Listen` + actual HTTP round-trip — the path main.go runs
  at boot, minus the flag plumbing.

**Note on the `127.0.0.1:0` happy-path test.** `0` is normally rejected
by `CheckLoopbackBind` (port-0 refusal inherited from `ListenerPort`).
The test uses `127.0.0.1:0` precisely so the validator rejects it —
which means the happy-path test cannot construct the server through
`NewMetricsServer("127.0.0.1:0", …)`. Two acceptable resolutions; the
spec lets the developer pick:

  1. **Preferred.** Construct the server with `NewMetricsServer("127.0.0.1:9090", mux)` (validator passes), then overwrite `srv.Addr = ""` and pass `l` from `net.Listen("tcp", "127.0.0.1:0")` into `srv.Serve(l)`. `http.Server.Serve` ignores `Addr` once a listener is supplied.
  2. Bypass `NewMetricsServer` and assemble the `*http.Server` directly in the test (timeouts copied from the spec). This trades coverage of `NewMetricsServer` for ergonomics; do this only if option 1's `srv.Addr = ""` step reads worse to a reviewer than constructing inline.

  Either is fine; the AC is about the listener+handler round-trip, not
  about which factory function the test calls.

### 4. Test-time port collision

The happy-path end-to-end test uses `127.0.0.1:0` to pick an ephemeral
port — no collision with anything the developer's machine might already
bind to. Tests do not use the default `127.0.0.1:9090` literal.

### 5. Out of scope

- TLS for the metrics listener. Loopback-only is the entire defence;
  TLS would add no marginal security and would require autocert /
  static-cert plumbing not justified by the threat model.
- Authentication / authorisation on `/metrics`. Same rationale: the
  bind shape is the gate.
- A second `/metrics` collector beyond the one wired by #61
  (`NewConnectionsMetrics`). #57 and #58 land theirs on this same wiring
  site by appending more constructor calls — that is the seam this
  ticket establishes, not work this ticket does.
- Graceful shutdown of either listener. #31's job.
- The `docs/knowledge/codebase/60.md` summary entry and the
  `docs/knowledge/INDEX.md` append. Both are documentation-phase
  territory (architect's "Never Update" rule); the AC is satisfied at
  the ticket level by the documentation pass, not by this developer
  run. Recorded so the developer does not chase an AC item that is not
  theirs.

## Concurrency model

One new goroutine: the metrics listener's `srv.ListenAndServe()` call.
Lifecycle:

- Started after both flag validation and `CheckLoopbackBind` have
  succeeded (so the goroutine never starts with a bad config).
- Exits only on `ListenAndServe` returning an error. The error handling
  is `logger.Error` + `os.Exit(1)` — same shape as the existing http-01
  goroutine in autocert mode. Treating a metrics-listener failure as a
  process-fatal event is intentional: a relay that booted with metrics
  enabled but is silently not serving them would mislead operator
  scrapes into thinking the relay is healthy when its metrics surface
  is gone. Loud failure over silent correction.
- No coordination with the public listener's goroutine. They are
  independent.

Concurrency on the handler side: `promhttp.HandlerFor` is documented
safe for concurrent calls; `prometheus.Registry` uses internal locking.
`NewConnectionsMetrics` reads `Registry.Counts()` on every scrape (#61's
pull-based design), which already holds the right lock on the relay's
`*Registry`. No shared state introduced here.

## Error handling

| Failure mode | Surface | Recovery |
| --- | --- | --- |
| `--metrics-listen` rejected by `CheckLoopbackBind` | `logger.Error` + `os.Exit(2)` (config-error, not runtime) | Operator fixes the flag |
| `--metrics-listen` port collides with public listener | `srv.ListenAndServe` returns `bind: address already in use` (OS-level) | `os.Exit(1)` from the metrics goroutine. Loud-failure-over-silent. The CheckListenerPorts pre-check does NOT catch this because both ports are in the expected set; the OS catches it at bind time. Acceptable. |
| `--metrics-listen` port not in expected set when port allowlist check runs | impossible by construction (we add the metrics port to both expected and actual sets in the same block) | n/a |
| `metricsSrv.ListenAndServe` returns mid-run (network reconfigure, fd exhaustion) | metrics goroutine logs + `os.Exit(1)` | Process restart |
| `--metrics-listen=""` | structural skip; no listener, no goroutine, no port-set entry | n/a — opt-out |

## Testing strategy

- **`internal/relay/metrics_listen_test.go`** (above) is the gate. It
  satisfies ACs (a), (b), and (c) at the package level.
- **No tests in `cmd/pyrycode-relay/`** are added by this ticket. The
  main-package logic is a thin wiring layer over the tested helpers;
  asserting it would require either a `go test -tags integration` style
  process-launch harness or a refactor of `main` into a testable shape
  neither of which the AC asks for. `deps_test.go` is unaffected.
- **`make test -race`** runs the new tests under the race detector. The
  end-to-end test starts a goroutine (`srv.Serve(l)`); the race detector
  surfaces any data race in the (third-party) handler stack. No relay-
  side shared state to exercise.
- **No change to e2e tests.** `/metrics` is not on the public-listener
  surface that the e2e harness drives.

## Open questions

1. **Should the metrics port also pass through `CheckRunningAsRoot` /
   `CheckCapabilities` / `CheckEnvConfig`?** No — those are
   process-wide pre-flight checks already gating both listeners; they
   run before any `metricsSrv` construction. No interaction.
2. **Should `--metrics-listen` accept a unix-domain socket path (e.g.
   `/var/run/pyrycode-relay.sock`)?** Out of scope. The ticket body
   names IP literals only. If a future operator wants unix-socket
   metrics, that is a separate ticket with its own threat-model review
   (file-permission surface, peer authentication, etc.).
3. **Should the metrics goroutine attempt to register at a different
   port if the configured port is busy?** No — that would be silent
   correction. Refuse to start.

## Security review (security-sensitive ticket)

### Mindset

Re-reading the spec as an adversary against the relay process. Default
verdict FAIL until each applicable category below has either a concrete
finding or an explicit design decision that makes the category not
applicable. The label is the gate; I do not skip the pass because the
ticket "feels small".

### 1. Trust boundaries

- **Inbound trust boundary introduced:** yes. A new HTTP listener binds
  to `127.0.0.1:9090` (default) and accepts unauthenticated GET requests
  for `/metrics`. The boundary defence is the bind shape — loopback IP
  literals only, hostnames refused, non-loopback IPs refused, port 0
  refused, all enforced at boot before the listener starts.
- **Why hostnames are refused (load-bearing):** a hostname resolves at
  validation time and again at bind time (different syscall paths).
  Between the two, DNS rebinding, `/etc/hosts` race, or resolver
  reconfigure can make a hostname that validated as loopback bind to a
  non-loopback IP. Refusing the entire shape is the only TOCTOU-proof
  defence. Documented in the `CheckLoopbackBind` docstring and in the
  spec's *Design* § 1 so a future contributor does not "fix" the
  validator by adding `net.LookupHost`.
- **Internal boundary unchanged:** the `prometheus.Registry` private to
  the relay (per ADR-0008 § Scope of use) is not touched by this ticket.
  `NewMetricsRegistry()` is called once at the wiring site;
  `DefaultRegisterer` is never referenced.

### 2. Tokens, secrets, credentials

- The relay's only handled secret is `X-Pyrycode-Token` at `/v1/client`
  (#5). This ticket adds no code path that touches a token.
- The metrics labels emitted by the only collector wired here
  (`NewConnectionsMetrics`, #61) are label-less by design — `pyrycode_relay_connected_binaries`
  and `pyrycode_relay_connected_phones` are scalar gauges. The
  `{server="<x-pyrycode-server header>"}` shape that #61's security
  review forbade is structurally absent. Sibling tickets (#57, #58) that
  add labelled collectors must enforce the same rule in their own specs;
  this ticket's wiring carries no labels and therefore no exfiltration
  channel. The constraint is recorded in #59's spec for sibling
  architects.

### 3. File operations

- None. The metrics listener performs no file I/O. The `--metrics-listen`
  flag is parsed as a string, not as a path.

### 4. Subprocess / external command execution

- None.

### 5. Cryptographic primitives

- None. The metrics listener is plaintext HTTP on loopback. TLS is
  intentionally out of scope (see *Out of scope* § 5) — loopback is
  the defence, not TLS.

### 6. Network & I/O

- **Listener exposure:** localhost-only by validation. A hostile
  process on the same host can scrape `/metrics` — that is in-scope
  for the threat model: same-host adversary is assumed to have at
  least equal privilege to the relay process and metrics is not a
  defence boundary against them.
- **Off-host exposure:** structurally impossible without a kernel-level
  routing bypass. `net.ParseIP(host).IsLoopback()` is the gate; the
  kernel enforces that loopback-bound sockets are not reachable from
  non-loopback paths.
- **DoS via scrape flooding:** the listener uses the same timeouts as
  the public listener (`ReadHeaderTimeout: 10s`, `ReadTimeout: 60s`,
  `WriteTimeout: 60s`, `IdleTimeout: 120s`). `promhttp.HandlerFor`'s
  `MaxRequestsInFlight` defaults to zero (unbounded) — acceptable
  because the loopback gate caps the threat population to same-host
  processes. A future ticket can cap it if a same-host
  operational-process bug causes scrape storms, but the present threat
  model does not justify a cap.
- **`promhttp.HandlerOpts.Timeout`:** zero (no per-handler timeout).
  The `http.Server.WriteTimeout` is the upper bound on scrape duration.
  No exposure here.
- **gosec G114 (`http.Server` without ReadHeaderTimeout):** mitigated.
  `ReadHeaderTimeout: 10 * time.Second` is set explicitly in
  `NewMetricsServer`.
- **Port-allowlist interaction (#81):** the metrics port joins the
  `expected` and `actual` sets at the wiring site. A typo'd flag (e.g.
  `--metrics-listen=127.0.0.1:6060`) would put a debug-shaped port in
  the expected set — that's the operator's choice and the boot check
  cannot second-guess it. The asymmetric check catches *surplus*
  listeners (a stray import bound to `:6060` that the operator did not
  declare); a deliberately-declared metrics port at `:6060` is not
  surplus and not caught. Acceptable; operators are accountable for
  the flag value within the loopback constraint.

### 7. Error messages, logs, telemetry

- **Boot-failure log line on `CheckLoopbackBind` rejection:** logs
  `value=<flag-string>` and the wrapped error. The flag value is
  operator input, not attacker input — `--metrics-listen` is a
  command-line flag, not a network-supplied string. Logging it is safe.
  No token / no secret / no PII flows through this path.
- **Metrics-goroutine `ListenAndServe failed` log:** the error from
  `ListenAndServe` is library-internal (`bind: address already in use`,
  `accept: too many open files`, etc.). No relay state, no token, no
  PII.
- **`promhttp` self-instrumentation counters** (registered by
  `HandlerOpts.Registry` per #59) — published on the loopback `/metrics`
  surface. Same trust boundary as the rest of the metrics; not a leak
  to non-loopback callers.
- **No new label-bearing metrics in this ticket.** The label-as-channel
  rule recorded in #59 propagates here automatically: nothing this
  ticket emits has labels.

### 8. Concurrency

- **One new goroutine** (`go srv.ListenAndServe()`); no shared mutable
  state with the public listener's goroutines. The `prometheus.Registry`
  the metrics goroutine reads is constructed once before either listener
  starts; `NewConnectionsMetrics` registers a `Collector` whose
  `Collect` method reads `Registry.Counts()` (already locked by #61).
  No new lock, no new channel, no goroutine-leak vector — `os.Exit(1)`
  on listener failure tears the process down before any leak matters.
- **Test-side goroutine** in `TestMetricsServer_EndToEnd_HappyPath`:
  `srv.Serve(l)`. `t.Cleanup(func(){ srv.Close() })` ensures the
  goroutine exits between subtests. `go test -race` covers the handler
  stack's internal concurrency.

### 9. Threat model alignment

- **`docs/threat-model.md` § Log hygiene:** unchanged by this ticket;
  no new log lines carry user-controlled input (the flag value is
  operator-controlled, not network-controlled).
- **`docs/threat-model.md` § DoS:** the metrics scrape surface is
  loopback-only. Same-host scrape storms are bounded by the
  `http.Server` timeouts. No internet-reachable surface added.
- **`docs/threat-model.md` § Supply chain:** unchanged. No new direct
  dep — `prometheus/client_golang` came in with #59; this ticket
  consumes it.
- **`docs/threat-model.md` § TLS:** unchanged. The public listener's
  TLS surface (`:443` in autocert mode) is untouched.
- **Protocol spec** (`pyrycode/pyrycode/docs/protocol-mobile.md`):
  unaffected. `/metrics` is operational, not on the wire protocol.

### Findings

- **[Trust boundaries]** No MUST FIX. The boundary is defended
  structurally by `CheckLoopbackBind`. Hostname refusal is the
  load-bearing TOCTOU defence; it is documented in the validator's
  docstring and asserted by `TestCheckLoopbackBind_Matrix`.
- **[Tokens]** No findings — no token-carrying code path is touched.
- **[File operations]** No findings — none performed.
- **[Subprocess]** No findings — none performed.
- **[Cryptographic primitives]** No findings — TLS deliberately out
  of scope; loopback is the defence.
- **[Network & I/O]** No MUST FIX. Loopback validation +
  per-listener timeouts cover the cases the threat model specifies.
  `MaxRequestsInFlight` left at default; acceptable on loopback.
- **[Errors / logs / telemetry]** No findings — operator-controlled
  flag value is the only thing logged from this ticket's new paths.
- **[Concurrency]** No findings — one new goroutine, no shared
  mutable state, `os.Exit(1)` on failure.
- **[Threat model]** No findings unique to this ticket.

### Verdict

**PASS.** No MUST FIX findings. The design's load-bearing security
choice (refuse hostnames, not just non-loopback IPs) is structurally
defended in `CheckLoopbackBind` and asserted by the matrix test.
Operator-misconfiguration paths fail loudly at boot rather than serving
on the wrong interface.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
