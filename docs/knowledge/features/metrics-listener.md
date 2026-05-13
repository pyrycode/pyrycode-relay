# Metrics listener — localhost-only `/metrics`

The relay serves `/metrics` on a **separate** `http.Server` bound to a loopback IP literal (default `127.0.0.1:9090`). It is intentionally not multiplexed onto the public listener that serves `/healthz` and `/v1/{server,client}`: Prometheus metrics leak operational state (active-connection counts indicate whether anyone is using the relay; future upgrade/register counters reveal traffic patterns), and the relay is internet-exposed. Operators reach `/metrics` via SSH tunnel or sidecar.

The contract has three guarantees: the bind address must be a loopback **IP literal** (hostnames refused), an empty value is a structural opt-out (no listener bound, no goroutine started, no error), and any other value fails boot loudly with exit 2.

## Contract

- Flag: `--metrics-listen` (default `127.0.0.1:9090`).
  - Loopback IP literal + port → listener bound (`127.0.0.1:9090`, `127.0.0.5:1234`, `[::1]:9090` all accepted; whole `127.0.0.0/8` is loopback).
  - Empty string → no listener, no goroutine, no port in the allowlist set (#81). Structural opt-out.
  - Anything else → `os.Exit(2)` with a structured log naming the offending value and the fix string.
- Hostnames (including `localhost:9090`) are refused even when they currently resolve to a loopback IP — see the TOCTOU rationale below.
- Port 0 is refused (inherited from `ListenerPort`, #81). The ephemeral-port shape would smuggle an unknown bound port past #81's listener-port allowlist.

## API

Package `internal/relay` (`metrics_listen.go`):

```go
var ErrNonLoopbackBind = errors.New("relay: metrics listener bind address must be a loopback IP literal")

func CheckLoopbackBind(addr string) error
func NewMetricsServer(addr string, h http.Handler) (*http.Server, error)
```

- `CheckLoopbackBind` is the pure validator. Reuses `ListenerPort` for the port parse (1..65535, port 0 rejected) so the rule the rest of the codebase already obeys extends here without duplication. Returns wrapped `ErrNonLoopbackBind` for the loopback-rule failures (hostnames, non-loopback IPs, empty host) — branchable via `errors.Is`. Other shapes (port out of range, missing port, empty addr) return plain wrapped errors.
- `NewMetricsServer` is the opt-out-aware constructor:
  - `addr == ""` → `(nil, nil)`. The caller checks `srv != nil` before launching the goroutine or adding the port to the listener allowlist.
  - `CheckLoopbackBind(addr)` fails → `(nil, err)`.
  - otherwise → `*http.Server` with the public listener's timeout policy (`ReadHeaderTimeout: 10s`, `ReadTimeout: 60s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`) — duplicated rather than shared so either listener can drift if a future ticket has reason; today they match because that is the safest default.

The handler argument is `http.Handler`, not `*http.ServeMux`: the caller chooses whether to wire a bare `NewMetricsHandler(reg)` or wrap it in a mux. `cmd/pyrycode-relay/main.go` wraps in a `ServeMux` so `/metrics` is distinguishable from a future sibling path on the same listener.

## Why hostnames are refused (load-bearing)

A hostname resolves at validation time (the `net.LookupHost` a "helpful" validator would call) and again at bind time (the kernel's resolver hit inside `net.Listen`). Between the two, DNS rebinding, an `/etc/hosts` race, or a resolver reconfigure can make a hostname that validated as loopback bind to a non-loopback IP. The only TOCTOU-proof defence is to refuse the entire shape: the validator parses `net.SplitHostPort`'s host portion with `net.ParseIP` and rejects anything that fails to parse as an IP literal.

The docstring on `CheckLoopbackBind` restates this rationale so the next contributor does not "fix" the validator by adding `net.LookupHost`.

## Wiring (`cmd/pyrycode-relay/main.go`)

Three localised edits, both listener modes:

1. **Construction (lines 101–114):** build the metrics registry, register collectors (today just `NewConnectionsMetrics`, #61; sibling tickets #57 / #58 append here), wrap `NewMetricsHandler` in a `ServeMux`, and call `NewMetricsServer(*metricsListen, metricsMux)`. A non-nil error fails boot with exit 2 and a structured log carrying `err`, `value=<flag>`, and the operator fix string.
2. **Port-allowlist integration (insecure: lines 147–156; autocert: lines 223–232):** when `metricsSrv != nil`, parse its `Addr` via `ListenerPort` and add the port to **both** the `expected` and `actual` sets passed to `CheckListenerPorts` (#81). The metrics port is a declared listener, not a surplus one — it has to land on both sides of the asymmetric check.
3. **Goroutine launch (insecure: lines 165–173; autocert: lines 245–253):** `go metricsSrv.ListenAndServe()`. On error the goroutine logs `"metrics listener failed"` and calls `os.Exit(1)` — same shape as the autocert mode's http-01 listener. A relay that booted with metrics enabled but is silently not serving them would mislead operator scrapes into thinking the relay is healthy when its metrics surface is gone; loud failure over silent correction.

## Threat model alignment

- **Trust boundary defended structurally, not by runbook.** The bind shape is the gate. Loopback validation + IP-literal-only is enforced before any listener starts; no fallback, no warn-and-continue.
- **Same-host adversary is in scope but not a defence target.** A hostile process on the same host can scrape `/metrics` — that is the threat model's accepted shape. Off-host exposure is structurally impossible without a kernel-level routing bypass.
- **TLS deliberately out of scope.** Loopback is the entire defence; TLS would add no marginal security and would require autocert / static-cert plumbing not justified by the threat model.
- **No authentication / authorisation on `/metrics`.** Same rationale.
- **DoS:** `http.Server` timeouts cap scrape duration. `promhttp.HandlerFor`'s `MaxRequestsInFlight` is left at the library default (unbounded); acceptable because the loopback gate caps the threat population to same-host processes. A future ticket can cap it if a same-host operational-process bug causes scrape storms.
- **Log hygiene:** boot-refusal log lines emit the flag value (operator input, not network input) and the wrapped error. No token, no PII, no metric labels carry attacker-influenced values (the only collector wired here, `NewConnectionsMetrics`, is label-less by design — see [Connection-count gauges](connection-count-gauges.md)).
- **gosec G114** (`http.Server` without `ReadHeaderTimeout`): mitigated by `NewMetricsServer` setting all four timeouts explicitly.

## Concurrency

One new goroutine: the metrics listener's `srv.ListenAndServe()`. Started only after both flag validation and `CheckLoopbackBind` have succeeded. Exits only on `ListenAndServe` returning an error; the error path is `logger.Error` + `os.Exit(1)`. No coordination with the public listener's goroutines — they are independent.

`promhttp.HandlerFor` is documented safe for concurrent calls; `prometheus.Registry` uses internal locking. `NewConnectionsMetrics` reads `Registry.Counts()` on every scrape (#61's pull-based design), which already takes the right lock. No shared state is introduced.

## Failure modes

| Failure | Surface | Recovery |
| --- | --- | --- |
| `--metrics-listen` rejected by `CheckLoopbackBind` | `logger.Error` + `os.Exit(2)` (config-rejected-at-boot) | Operator fixes the flag |
| Metrics port collides with public listener | `ListenAndServe` returns `bind: address already in use` | Goroutine logs + `os.Exit(1)`. `CheckListenerPorts` does not catch this — both ports are in the expected set; the OS catches it at bind time |
| `ListenAndServe` returns mid-run (fd exhaustion, network reconfigure) | Goroutine logs + `os.Exit(1)` | Process restart |
| `--metrics-listen=""` | Structural skip — no listener, no goroutine, no port-set entry | n/a (opt-out) |

## Testing

`internal/relay/metrics_listen_test.go`:

- `TestCheckLoopbackBind_Matrix` — 12-row table covering every branch: IPv4 loopback (default + `/8` high address), IPv6 loopback (bracketed), non-loopback IPv4 / IPv6 (sentinel-wrapped), private IPs, hostname (`localhost:9090` rejected — the security-design anchor), empty host (`:9090` rejected), port-0, out-of-range port, missing port, empty addr. Sentinel-rule failures asserted with `errors.Is(err, ErrNonLoopbackBind)`; other failures asserted non-nil only.
- `TestNewMetricsServer_Matrix` — three rows: empty addr → `(nil, nil)`; non-loopback → `(nil, err)` with `errors.Is(_, ErrNonLoopbackBind)`; loopback → `(srv, nil)` with all four timeouts pinned to literal values (not a shared constant — the spec says they can drift).
- `TestMetricsServer_EndToEnd_HappyPath` — drives validator + constructor + `net.Listen` + actual HTTP round-trip over an ephemeral port. Constructs the server at `127.0.0.1:9090` (validator passes), then serves on `net.Listen("tcp", "127.0.0.1:0")` because `http.Server.Serve` ignores `Addr` once a listener is supplied — sidesteps the port-0 rule in `ListenerPort` without bypassing the factory. Asserts status 200 and Prometheus text-format `Content-Type` prefix; body parsing is `metrics_test.go`'s job (#59).

`make vet`, `make test -race`, `make build` clean.

## What this deliberately does NOT do

- TLS on the metrics listener — loopback is the entire defence (see *Threat model*).
- Authentication on `/metrics` — same rationale.
- Per-listener teardown logic baked into this file — graceful shutdown (#31) lives in `internal/relay/shutdown.go`, takes `servers ...*http.Server`, and the metrics server joins that variadic alongside the public listener(s). The "top-level local in `main`" shape that this feature established was the seam that made #31 a localised edit.
- A `--metrics-listen` unix-socket form — out of scope; would need its own threat-model review (file permissions, peer authentication).
- A second `/metrics` collector beyond #61's `NewConnectionsMetrics` — siblings #57 / #58 append more `NewXxxMetrics(metricsReg, …)` calls at the wiring site this ticket establishes.

## Related

- [Metrics registry (scaffolding)](metrics-registry.md) — the `*prometheus.Registry` + `NewMetricsHandler` factory this listener consumes (#59).
- [Connection-count gauges](connection-count-gauges.md) — the only collector wired here today (#61).
- [Listener port allowlist (boot-time refusal)](listener-port-allowlist.md) — the metrics port joins both the `expected` and `actual` sets (#81).
- [ADR-0008: Adopt `github.com/prometheus/client_golang`](../decisions/0008-prometheus-client-adoption.md) — scope-of-use rules.
- [Threat model](../../threat-model.md) — log-hygiene and DoS posture that this listener inherits.
