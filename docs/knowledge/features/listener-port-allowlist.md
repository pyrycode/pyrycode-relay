# Listener port allowlist (boot-time refusal)

The relay refuses to start when the set of TCP ports the process is *about to bind* contains any port outside an explicit expected set derived from parsed flags. A stray `net/http/pprof` listener on `:6060`, an env-flipped debug port, or an accidentally-enabled metrics exporter fails the deploy's health check rather than silently exposing an unauthenticated HTTP surface on the public internet. Added by #81.

## The contract

- **Asymmetric.** Surplus listeners (`actual ⊋ expected`) are the threat shape and trigger refusal. Missing listeners (`actual ⊊ expected`) are *not* an error here — a failure to bind an expected port surfaces as a runtime listener error from `http.Server.ListenAndServe` (exit 1), which has its own clearer signal. Empty-on-both is permitted.
- **Port-only.** Interface binding (`127.0.0.1` vs `0.0.0.0` vs `::`) is intentionally not part of the contract. The threat model the check encodes is "a listener exists on this port and is reachable"; on a single-instance internet-exposed deploy, every listener is reachable. A future ticket adding management-only loopback listeners would extend the set type without breaking the port-set shape.
- **Expected set is flag-derived.** Autocert mode (`--domain` set): `{443, 80}`. Insecure mode (`--insecure-listen :<port>`): `{port}`. The expected set is built from the parsed flag values in `main`, not from a static literal — `--insecure-listen :8080` yields `{8080}`, not `{443}`.
- **Actual set is `http.Server.Addr`-derived.** The check runs after the `http.Server` literals are constructed but *before* either `ListenAndServe` call. The relay's only network ingress today is `http.Server`-mediated; a future non-HTTP listener (gRPC, raw TCP) would need a deliberate spec update to extend the actual-set source.
- **Port 0 is rejected.** `:0` means "pick an ephemeral port" in `net.Listen` semantics — accepting it would smuggle an unknown bound port past the actual-set construction and defeat the check. `ListenerPort` returns a wrapped error.

## API (`internal/relay/listeners.go`)

- `ErrUnexpectedListener` — exported sentinel, branchable via `errors.Is`. Wrapped error message names the surplus ports in ascending order plus the expected-set contents (also ascending) so the failure log is deterministic across runs and grep-friendly.
- `ListenerPort(addr string) (uint16, error)` — extracts the TCP port from any `http.Server.Addr` shape (`":443"`, `"127.0.0.1:8080"`, `"[::1]:443"`). Wraps `net.SplitHostPort` + `strconv.ParseUint(_, 10, 16)`; the upper-bound check is implicit in `ParseUint`'s `16`-bit width, and port 0 is rejected explicitly.
- `CheckListenerPorts(expected, actual map[uint16]struct{}) error` — pure function, no I/O. Returns `ErrUnexpectedListener` (wrapped) if any element of `actual` is absent from `expected`, nil otherwise. Reports **all** surplus ports in one shot (not just the first) so a manifest enabling several debug surfaces fails in one boot rather than N restart cycles.

The maps are `map[uint16]struct{}` rather than `[]uint16` so set-difference is O(actual). The ascending-sorted ordering is a deliberate contract: `map` iteration is randomised, which would make the failure log non-deterministic and complicate operator grepping.

## Wiring (`cmd/pyrycode-relay/main.go`)

The check is inserted into each listener branch separately — the two branches have different `http.Server` shapes and there is no shared structure to lift it out of without restructuring `main`. Both branches run the check *after* the `http.Server` literals exist and *before* either `ListenAndServe` call (in the autocert branch, before the goroutine that runs `httpSrv.ListenAndServe` is spawned).

**Insecure-mode branch.** Builds `expected = {port}` and `actual = {port}` from the parsed flag, runs the check, exits 2 on surplus.

**Autocert-mode branch.** Builds `expected = {443, 80}` and `actual = {ListenerPort(httpsSrv.Addr), ListenerPort(httpSrv.Addr)}`, runs the check, exits 2 on surplus.

Three details:

- **Exit code 2** = config-rejected-at-boot, matching #9 / #77 / #78 / #79. Exit 1 in this file is reserved for *runtime* listener failures (`ListenAndServe` returning a bind/accept error). Distinct codes let ops dashboards split "deploy never started because of misconfiguration" from "deploy started and then crashed." The AC body literally said `os.Exit(1)`; the architect's spec overrode to harmonise.
- **Structured log fields**: `err` (wrapped sentinel), `unexpected_ports` (`[]uint16`, ascending), `expected_ports` (`[]uint16`, ascending). The surplus / expected slices are recomputed in `main` (via the unexported `listenerPortLists` helper) so `CheckListenerPorts` stays a single error return — the duplicate pass is cheap at boot and runs at most once.
- **Malformed-address path is distinct.** A `ListenerPort` parse error (e.g. `--insecure-listen notaport`) is a flag-validation failure, not a surplus-listener failure; main exits 2 with `"refusing to start: invalid listener address"` and an `addr` field, separate from the `"unexpected listener"` log line.

## Build-graph defence (`cmd/pyrycode-relay/deps_test.go`)

`TestBinaryDoesNotImportPprof` is a compile-time companion to the runtime port check. It shells out to `go list -deps -json github.com/pyrycode/pyrycode-relay/cmd/pyrycode-relay`, decodes the streamed JSON, and fails if any entry's `ImportPath == "net/http/pprof"`. The two layers catch different shapes of the same threat:

- **Runtime check** catches a stray `:6060` *bind* — pprof's `go func() { http.ListenAndServe(":6060", nil) }()` shape.
- **Build-graph test** catches the `import _ "net/http/pprof"` *handler-registration* variant that attaches `/debug/pprof/*` to `http.DefaultServeMux` without opening a new port — would slip past the runtime port check entirely and expose unauthenticated profiler handlers on whatever mux the default mux is mounted on.

The build-graph layer spans transitive imports, not just direct ones — a third-party dep blank-importing `net/http/pprof` (which has happened in real Go ecosystems) is the realistic drift surface. The test `t.Skip`s with a logged reason if `go` is not in `$PATH`; CI runners always have it.

This is the "belt-and-suspenders means different fabric" rule from the project handbook: pairing a stochastic-ish runtime guard with a deterministic compile-time test. Both are needed; either alone leaves a known gap.

## Failure modes (three distinct return shapes)

| Cause | Source | Return | Branchable via |
|---|---|---|---|
| Surplus listener | `CheckListenerPorts` | `fmt.Errorf("%w: unexpected ports …; expected ports …", ErrUnexpectedListener)` | `errors.Is(err, ErrUnexpectedListener)` |
| Malformed listener address | `ListenerPort` | `fmt.Errorf("relay: parsing listener address %q: %w", addr, err)` (or empty/zero-port variants) | underlying `strconv.NumError` / `net.AddrError` |
| `net/http/pprof` in transitive imports | `TestBinaryDoesNotImportPprof` | `t.Fatalf` at CI time, naming the offending import path | — (test-time) |

All three runtime paths exit 2 in `main`. The build-graph defence fails the CI pipeline before the binary ever ships.

## Threat model alignment

`docs/threat-model.md` § Deploy treats operator misconfiguration as the dominant failure class for an internet-exposed relay. The listener-allowlist check joins the boot-time-refusal family:

- `ErrCacheDirInsecure` (#9) — autocert cache dir mode drift
- `ErrInsecureListenInProduction` (#77) — plaintext-in-prod transport drift
- `ErrRunningAsRoot` (#78) — uid-0 in production
- `ErrUnexpectedCapability` (#79) — over-broad capability grant
- `ErrInvalidConfigSentinel` (#80) — env-var typo / malformed value
- `ErrUnexpectedListener` (#81) — surplus listener bound by the process

All six share the "refuse to start; fail the health check; never serve traffic in this configuration" shape. With six instances of the pattern, the consolidation follow-up (Config.Validate() multi-error) is owed against the parent #42 epic.

## Out of scope (deferred)

- **Interface binding.** `127.0.0.1` vs `0.0.0.0` vs `::` is port-only-equivalent today. Revisit if a management-only loopback listener is ever added.
- **Non-HTTP listeners.** Raw `net.Listen` without an `http.Server` wrapper would slip through — the relay's only network ingress today is `http.Server`-mediated. A future protocol that adds (e.g.) gRPC would need a deliberate spec update to extend the actual-set source.
- **Consolidation under `Config.Validate()`.** Sixth boot-check in `internal/relay`. The consolidation work owns the multi-error API design and the migration of all six call sites; deferred to a follow-up against #42's epic.

## Cross-links

- [Codebase note #81](../codebase/81.md) — per-ticket implementation detail.
- [Production-mode contract](production-mode.md) — sibling boot-time refusals (#77, #78).
- [Capability allowlist](capability-allowlist.md) — sibling boot-time refusal (#79); immediate precedent for the explicit-allowlist shape.
- [Env-var config validator](env-config-validator.md) — sibling boot-time refusal (#80).
- [Autocert TLS](autocert-tls.md) — `ErrCacheDirInsecure` is the original boot-time-refusal sentinel (#9).
- [#42 — parent ticket](https://github.com/pyrycode/pyrycode-relay/issues/42) — split into #77 / #78 / #79 / #81.
