# Spec: refuse to boot when listener ports exceed expected set

Ticket: [#81](https://github.com/pyrycode/pyrycode-relay/issues/81). Size S. Split from #42. Sibling of #77 / #78 / #79.

## Files to read first

- `cmd/pyrycode-relay/main.go` (whole file, 178 lines) — the only call site. Lines 89–94 hold the most recent boot-check wiring (`CheckCapabilities`); the new listener-allowlist check slots in after `mux` is built (line 110) but before either listener branch starts. Lines 112–127 hold the insecure branch (single `http.Server.Addr = *insecureListen`), 135–155 hold the autocert branch (`httpsSrv.Addr = ":443"`, `httpSrv.Addr = ":80"`), 160–170 hold the two `ListenAndServe` calls the check must run *before*.
- `internal/relay/production.go` (whole file, 77 lines) — the canonical "boot-time refusal helper" pattern: single exported `Check…` function returning `error`, sentinel branchable via `errors.Is`, injected test seam, no init/global mutation. Lines 11–17 are the `ErrInsecureListenInProduction` shape this spec mirrors for `ErrUnexpectedListener`.
- `internal/relay/production_test.go` (whole file, 199 lines) — the table-driven, `t.Parallel()`-safe, `errors.Is`-against-sentinel test style for boot-check helpers. The 2×2 matrix in `TestCheckInsecureListenInProduction_Matrix` (lines 47–105) is the exact shape this spec's `CheckListenerPorts` matrix uses, swapping env/flag axes for expected/actual-set axes.
- `internal/relay/tls.go:15-19` — canonical sentinel-error declaration shape (`var Err... = errors.New("relay: …")` + Go doc that names the contract). Mirror it.
- `internal/relay/tls.go:46-47` — canonical wrapped-sentinel return shape (`fmt.Errorf("%w: %s (mode %o)", ErrCacheDirInsecure, …)`). The listener check uses this shape when naming the offending port(s).
- `docs/specs/architecture/79-capability-allowlist-boot-check.md` — the immediate precedent (sibling ticket, same parent #42). Same call-site neighbourhood, same sentinel-error contract, same testing approach. The "Why not also check …" framing (CapPrm/CapBnd) maps directly onto this ticket's "Why port-only, not interface" decision.
- `docs/specs/architecture/77-refuse-insecure-in-production.md:105-113` — the "Why not a `Config` struct" decision. The same rationale (one `Check…` per concern; consolidate when the count hits ~3 and the wiring boilerplate is a real cost) carries over here.
- `docs/PROJECT-MEMORY.md` § "Project-level conventions" — the "Sentinel errors + `errors.Is` branching", "Loud failure over silent correction", and "Tests live in the same package" rules apply directly.

## Context

The relay's intended listeners are exactly two shapes:

- **Autocert mode** (`--domain <host>`): TCP/443 (HTTPS terminus, autocert-managed cert) and TCP/80 (ACME HTTP-01 challenge + 404).
- **Insecure mode** (`--insecure-listen :<port>`): a single TCP listener on the operator-supplied port, fronted by an upstream proxy.

Anything else the process binds is suspect. The high-frequency leak shapes are:

- **`net/http/pprof` blank-import.** Importing the package for any reason registers `/debug/pprof/*` handlers on `http.DefaultServeMux` *and* (since Go 1.22) starts the server on `:6060` via `http.ListenAndServe(":6060", …)` if anything anywhere in the binary calls it. A single `import _ "net/http/pprof"` slipped into a debug-flagged file (or pulled transitively from a profiling library) opens an unauthenticated stack-trace / heap-dump endpoint on every box.
- **Env-flipped debug ports.** A future flag (`--debug-listen :<port>`) added as a developer convenience and forgotten in a deploy manifest.
- **Accidentally-enabled metrics exporters.** Prometheus client wiring (`promhttp.Handler` on `:9100`) is a one-line mistake that exposes process internals on the internet.

The defence this ticket adds is a boot-time set-comparison: the ports the process is *about to bind* (the `Addr` fields of the constructed `http.Server` values) compared against the ports it is *permitted to bind* (derived from parsed flags). Any actual port not in the expected set fails the deploy's health check before traffic flows.

The check is **asymmetric** by design. Surplus listeners (actual ⊋ expected) are the threat shape. Missing listeners (actual ⊊ expected) are not — a failure to bind an expected port will surface as `EADDRINUSE` (or similar) from `ListenAndServe` itself, and the relay will exit 1 on the runtime-error path. This split keeps the boot check focused on one observable failure mode.

The check is **port-only**. Interface binding (`127.0.0.1` vs `0.0.0.0` vs `::`) is out of scope per the AC's technical notes. The threat model the check encodes is "a listener exists on this port and is reachable"; on a single-instance internet-exposed deploy, every listener is reachable. A future ticket that adds management-only `127.0.0.1` listeners can extend the set type without breaking the port-set contract — the migration path is "wrap `uint16` with a richer record"; the migration is trivial because nothing outside `internal/relay` constructs these sets except `cmd/`.

Related: #9 (`ErrCacheDirInsecure` set the boot-time-refusal precedent); #77 (`ErrInsecureListenInProduction`); #78 (`ErrRunningAsRoot`); #79 (`ErrUnexpectedCapability` — same "explicit allowlist" pattern). All four share the "refuse to start; fail the health check; never serve traffic in this configuration" shape.

## Design

Three new files; one edit to `cmd/pyrycode-relay/main.go`. No existing code is refactored.

| Path | Purpose | New? |
|---|---|---|
| `internal/relay/listeners.go` | `ErrUnexpectedListener` sentinel, `ListenerPort` parser, `CheckListenerPorts` check | new |
| `internal/relay/listeners_test.go` | Unit tests for the parser and check (AC matrix lives here) | new |
| `cmd/pyrycode-relay/deps_test.go` | Build-graph test that `net/http/pprof` is not in the binary's transitive imports | new |
| `cmd/pyrycode-relay/main.go` | Build expected + actual sets, invoke check before `Listen*` calls | edit (~25 lines added) |

### Sentinel error and helpers (in `internal/relay/listeners.go`, exported)

Three exports plus one sentinel. The shapes:

```go
// ErrUnexpectedListener is returned by CheckListenerPorts when the set
// of ports the process is about to bind contains any port outside the
// expected set (derived from parsed flags). Stray listeners
// (net/http/pprof on :6060, a forgotten debug exporter, a metrics
// endpoint accidentally enabled) are the threat shape: an unauthenticated
// HTTP surface on the public internet. The relay refuses to start so the
// misconfiguration fails the deploy's health check rather than serving
// traffic on the surplus port.
//
// Branchable via errors.Is. The wrapped error names the offending
// port(s) and the expected-ports set.
var ErrUnexpectedListener = errors.New("relay: process is about to bind a TCP port outside the expected set")

// ListenerPort extracts the TCP port from an http.Server Addr value
// such as ":443", "127.0.0.1:8080", or "[::1]:443". Returns a wrapped
// error if addr is empty or does not contain a parseable port in
// 1..65535. Used by cmd/pyrycode-relay to canonicalise both the expected
// (flag-derived) and actual (http.Server.Addr-derived) port sets onto
// the same uint16 key.
func ListenerPort(addr string) (uint16, error)

// CheckListenerPorts returns ErrUnexpectedListener (wrapped, naming the
// offending port(s) in ascending order plus the expected-set contents)
// when any element of actual is absent from expected. Returns nil
// otherwise.
//
// The check is asymmetric: missing ports (actual ⊊ expected) are NOT an
// error — a failure to bind an expected port surfaces as a runtime
// listener error from http.Server.ListenAndServe, which has its own
// exit path. This check exists to catch the surplus-listener case
// (actual ⊋ expected) where an unauthenticated debug endpoint would
// otherwise be exposed on the internet.
//
// Both arguments are sets keyed by TCP port (uint16). Empty sets are
// valid inputs: CheckListenerPorts(∅, ∅) returns nil.
func CheckListenerPorts(expected, actual map[uint16]struct{}) error
```

### `ListenerPort` — algorithm

1. If `addr == ""` → return `0, fmt.Errorf("relay: listener address is empty")`.
2. Call `net.SplitHostPort(addr)`. On error → return `0, fmt.Errorf("relay: parsing listener address %q: %w", addr, err)`.
3. `strconv.ParseUint(port, 10, 16)` on the port substring. On error → wrap with the same prefix.
4. Reject `0` explicitly: `port == 0` means "pick an ephemeral port" in net.Listen semantics; the relay never wants that, and an actual-set entry of `0` would defeat the check. Return a wrapped error.
5. Return `uint16(parsed), nil`.

Bounds check on the upper end is implicit in `ParseUint(_, 10, 16)`: anything > 65535 fails the parse.

### `CheckListenerPorts` — algorithm

1. Compute `surplus := slices.Sorted(actual \ expected)` (ports in `actual` not in `expected`, ascending).
2. If `surplus` is empty → return `nil`.
3. Format the error message:
   ```
   relay: process is about to bind a TCP port outside the expected set: \
   unexpected ports [N, M, …]; expected ports [P, Q, …]
   ```
   Both lists are sorted ascending so the message is deterministic across runs (`map[uint16]struct{}` iteration order is randomised, which would make the failure log non-deterministic and complicate operator grepping).
4. Return `fmt.Errorf("%w: unexpected ports %v; expected ports %v", ErrUnexpectedListener, surplus, sortedExpected)`.

**Decision: report all surplus ports, not just the first.** A misconfigured manifest can enable several debug surfaces in one breath (`pprof` + `metrics` + a forgotten dev port); listing one port at a time would force the operator through N restart cycles. The error message names every offending port. Mirrors the `ErrUnexpectedCapability` decision in #79.

**Decision: do not return information about missing ports in the error.** The asymmetric contract is the whole point. If the operator's deploy somehow ends up with `actual ⊊ expected`, the next thing that happens is `http.Server.ListenAndServe` returns and the process exits 1 with a runtime "listen tcp :443: bind: ..." error — that's the clearer signal for that failure mode.

### Wiring in `cmd/pyrycode-relay/main.go`

The constructed-but-not-yet-bound `http.Server` values are the source of truth for the actual set. The natural insertion point is *after* both `http.Server` values exist *and* after `mux` is wired, but *before* either `ListenAndServe` call. The two listener branches have different shapes, so the simplest wiring keeps the check inside each branch — there is no shared structure to lift it out of without restructuring the function more than this ticket warrants.

**Insecure-mode branch** (currently lines 112–127). Insert after the `srv` literal, before `srv.ListenAndServe()`:

```
// 1. Build expected set: a single port from --insecure-listen.
// 2. Build actual set: ListenerPort(srv.Addr).
// 3. Call relay.CheckListenerPorts(expected, actual); on error,
//    logger.Error(... "unexpected_ports", surplus, "expected_ports", ...) + os.Exit(2).
```

**Autocert-mode branch** (currently lines 129–170). Insert after both `httpsSrv` and `httpSrv` literals exist, before the goroutine that runs `httpSrv.ListenAndServe()`:

```
// 1. Build expected set: {443, 80}.
// 2. Build actual set: ListenerPort(httpsSrv.Addr), ListenerPort(httpSrv.Addr).
// 3. Call relay.CheckListenerPorts(expected, actual); on error,
//    logger.Error(... "unexpected_ports", surplus, "expected_ports", ...) + os.Exit(2).
```

Three details:

- **Exit code 2**, matching #77 / #78 / #79. The AC body literally says `os.Exit(1)`; I am overriding that to keep the boot-check exit-code contract consistent across the four sibling checks. Exit 1 in this file is reserved for *runtime* listener failures (`http.Server.ListenAndServe` returning a bind/accept error). Exit 2 is *boot-time configuration refusal*. Distinct codes let ops dashboards split "deploy never started because of misconfiguration" from "deploy started and then crashed." The PO body did not consider the precedent set by #77's spec; the architect's job is to harmonise.
- **Structured log line fields:** `err` (the wrapped sentinel), `unexpected_ports` (the surplus list as `[]uint16`), `expected_ports` (the expected list as `[]uint16`). These satisfy the AC's "at minimum: the offending port(s), the expected-ports set, and an `err` field carrying the wrapped sentinel."
- **`ListenerPort` parse errors → also `os.Exit(2)`** with `logger.Error("refusing to start: invalid listener address", "err", err, "addr", addr)`. A malformed `--insecure-listen` value (e.g. `notaport`) is a flag-validation failure, not a surplus-listener failure; surface it distinctly so the operator sees the right fix. This happens before the `CheckListenerPorts` call, on either branch.

### Why a fresh helper per check, not a shared `Config.Validate()`

Same rationale as the precedent in #77's spec § "Why not a `Config` struct." `CheckListenerPorts` is the fourth `Check…` to land in `internal/relay`. The line of "consolidate when the count hits ~3 and the wiring boilerplate becomes a real cost" has been crossed *by counting*, but the wiring boilerplate in `main.go` is still tractable (each `Check…` is a 5-line `if err := … { logger.Error(…); os.Exit(2) }` block). The consolidation work is a separate follow-up that owns both the API design (multi-error return? short-circuit?) and the migration; doing it here would expand the ticket beyond the AC. **Action:** when this ticket closes, file a follow-up "consolidate boot-check helpers" issue against the parent #42's epic.

### Build-graph test in `cmd/pyrycode-relay/deps_test.go`

```go
package main

// TestBinaryDoesNotImportPprof asserts that net/http/pprof is not in the
// transitive import set of the cmd/pyrycode-relay package. A blank-import
// of net/http/pprof anywhere in the dependency graph registers /debug/pprof/*
// handlers on http.DefaultServeMux and (under Go 1.22+ default configs) can
// open :6060 unattended. The boot-time CheckListenerPorts guard catches the
// :6060-bind variant; this build-graph test catches the handler-registration
// variant that wouldn't bind a new port but would attach to an existing mux.
//
// Implementation: shell out to `go list -deps -json <import-path>` and
// inspect the ImportPath field of each entry. The import path is the
// canonical module form, not "." or a relative path — that makes the test
// CWD-independent (works under `go test ./...` from repo root and under
// `go test` from the package directory).
func TestBinaryDoesNotImportPprof(t *testing.T)
```

**Algorithm:**

1. Run `exec.LookPath("go")` → `exec.Command(goBin, "list", "-deps", "-json", "github.com/pyrycode/pyrycode-relay/cmd/pyrycode-relay")`. Capture stdout to a `bytes.Buffer`.
2. The output is a stream of concatenated JSON objects (one per dep). Use `json.NewDecoder(...).Decode(&pkg)` in a loop where `pkg` is `struct { ImportPath string }`.
3. Skip until EOF; if any decoded `ImportPath == "net/http/pprof"` → `t.Fatalf("net/http/pprof in transitive imports; remove the import or it will register debug handlers on http.DefaultServeMux")`.
4. `t.Fatalf` on any I/O or decode error.

**Decision: `go list -deps -json`, not a build-tag-gated compile check.** The AC offers either as acceptable. The `go list` approach has two concrete advantages:

1. **Catches transitive imports too.** A blank-import in a dependency we pull in (`golang.org/x/net/http2/h2c`-style accident) doesn't appear in our source but does appear in `-deps`. The threat is third-party drift; the check has to span the whole graph.
2. **Test fails on the offending CI run, not at runtime.** A build-tag check would only flag the issue when something explicitly references the pprof package. `go list -deps` is unconditional.

The cost is the test requires the `go` binary in `$PATH` — which is true on every host that runs `make test` and every CI runner. If a future CI environment hits this constraint, the test logs `t.Skip("go binary not in PATH")` and the developer fixes the env. No silent-skip path; the test is either run or explicitly skipped.

**`t.Parallel()`-safe?** Yes. The test shells out to `go list` but reads no global state and writes only to the test's own `*testing.T`.

## Concurrency model

None. `ListenerPort` and `CheckListenerPorts` are pure functions; the `main` wiring runs on the main goroutine before any listener goroutine is spawned (recall the autocert branch spawns `httpSrv.ListenAndServe` in a goroutine *after* the check runs, then runs `httpsSrv.ListenAndServeTLS` on the main goroutine). No locks, no channels.

## Error handling

Three distinct failure modes, each with its own log line and exit code in `main`:

| Mode | Source | Return shape | `main` action |
|---|---|---|---|
| Surplus listener | `CheckListenerPorts` returns `fmt.Errorf("%w: …", ErrUnexpectedListener)` | wraps sentinel | `logger.Error("refusing to start: unexpected listener", "err", err, "unexpected_ports", surplus, "expected_ports", expected)` + `os.Exit(2)` |
| Malformed address | `ListenerPort` returns wrapped parse error | not sentinel | `logger.Error("refusing to start: invalid listener address", "err", err, "addr", addr)` + `os.Exit(2)` |
| Empty actual set | (cannot happen — every branch constructs at least one `http.Server` before the check) | — | n/a |

All three paths are total — no retry, no fallback. Boot-time refusal is intentional and complete.

## Testing strategy

All tests live alongside the implementation and are `t.Parallel()`-safe. No `os.Setenv`, no `t.Setenv`, no real listener binds, no real `/proc` reads.

### `internal/relay/listeners_test.go` — pure-function tests

#### `TestListenerPort_Matrix`

Table-driven over `(addr string, wantPort uint16, wantErr bool)`:

- `":443"` → `443`, nil
- `":80"` → `80`, nil
- `"127.0.0.1:8080"` → `8080`, nil
- `"[::1]:443"` → `443`, nil (IPv6 bracket form)
- `"0.0.0.0:9000"` → `9000`, nil
- `""` (empty) → 0, error
- `"443"` (no colon) → 0, error
- `"127.0.0.1"` (no port) → 0, error
- `":0"` (zero port) → 0, error (explicit reject)
- `":99999"` (out of range) → 0, error
- `":notaport"` → 0, error
- `":-1"` → 0, error

The `:0` row is the load-bearing one: documents the explicit reject of the ephemeral-port placeholder.

#### `TestCheckListenerPorts_ACMatrix` — verbatim from the AC

The AC enumerates exactly four cases; this table replays them:

| name | expected | actual | want |
|---|---|---|---|
| `autocert_match` (case a, autocert-on shape) | `{443, 80}` | `{443, 80}` | nil |
| `insecure_match` (case a, insecure shape) | `{8080}` | `{8080}` | nil |
| `surplus_pprof` (case b) | `{443, 80}` | `{443, 80, 6060}` | `errors.Is(err, ErrUnexpectedListener)` AND `err.Error()` contains `"6060"` |
| `actual_subset_of_expected` (case c) | `{443, 80}` | `{443}` | nil |
| `empty_both` (case d) | `{}` | `{}` | nil |

Per the AC's technical note: case (a) is exercised in both the autocert-on shape (`{443, 80}`) and the insecure shape (`{8080}`); the rows above split them so a regression in either shape is named.

#### `TestCheckListenerPorts_SurplusListedSorted`

A focused test: `expected = {443}`, `actual = {443, 9000, 6060, 1234}`. Assert that the error message contains the surplus ports in ascending order (`"1234"` before `"6060"` before `"9000"`). Locks in the determinism contract — without it, `map` iteration order would make failure logs vary across runs.

#### `TestCheckListenerPorts_MultipleSurplus`

A focused test: `expected = {443, 80}`, `actual = {443, 80, 6060, 9090}`. Assert that **all** surplus ports appear in the error message (both `"6060"` and `"9090"`). Locks in the "report all, not just the first" contract.

#### `TestErrUnexpectedListener_IsBranchable`

A one-line `errors.Is` test, mirroring `TestErrInsecureListenInProduction_IsBranchable` in `production_test.go`. Locks in the `errors.Is` contract so a future refactor to `fmt.Errorf("relay: %s", …)` (dropping the `%w`) breaks the test, not downstream callers.

### `cmd/pyrycode-relay/deps_test.go` — build-graph test

#### `TestBinaryDoesNotImportPprof`

Single test. Algorithm above. Assertion: no decoded JSON object has `ImportPath == "net/http/pprof"`. On failure, `t.Fatalf` with the offending dep's full ImportPath (so the developer can grep their tree for the introducing import).

### What is NOT tested

- The `main.go` wiring itself. Same rationale as #77/#78/#79: `main` is a coordination function; its behaviour is observable through the unit tests on the helpers it calls. A fork-exec integration test for a 5-line `if err != nil { os.Exit(2) }` block is over-engineering. Code review of the diff is the gate.
- The exact log-line field names or ordering. The AC says "structured log line containing at minimum: the offending port(s), the expected-ports set, and an `err` field." The spec satisfies that literally; asserting on slog output requires a handler-capture seam, which is friction the surface does not earn.
- The exact format of the error message string (whitespace, punctuation). We test that the message contains the offending port numbers and is branchable via `errors.Is`; cosmetic format changes should not break tests.
- Other "extra surface" leak shapes (UNIX-domain sockets, raw `net.Listen` on a TCP address that isn't wired to an `http.Server`). These are caught only insofar as they show up as `http.Server.Addr` values in the actual set. A raw `net.Listen` without an `http.Server` wrapper would slip through — but the relay's only network ingress today is `http.Server`-mediated, and a future protocol that adds a non-HTTP listener (e.g. gRPC) will need a deliberate spec update to extend the actual-set source. Documented here so the gap is explicit rather than silent.

## Open questions

None blocking the developer. Two notes for future tickets:

1. **Exit-code drift from the AC.** The AC literally says `os.Exit(1)`; the spec uses `os.Exit(2)` to stay consistent with #77/#78/#79. If the PO disagrees with the harmonisation, surface as a comment on the ticket and the spec adjusts.
2. **Consolidation follow-up.** `CheckListenerPorts` is the fourth `Check…` helper in `internal/relay`. A follow-up to consolidate them under a `Config.Validate()`-shape multi-error helper is worth filing against the parent #42's epic once this ticket closes.

## Security review

**Verdict:** PASS

Per `architect/security-review.md` for the security-sensitive ticket #81. The spec was walked adversarially against each category. Findings:

- **[Trust boundaries]** Two inputs cross into the check: (1) `--insecure-listen` is operator-supplied, and `ListenerPort` is the parser — bounded by `net.SplitHostPort` + `strconv.ParseUint(_, 10, 16)`, both stdlib, both bounded. No allocation driven by attacker-controlled length (the input is the value of a process flag, not network-borne). (2) The `http.Server.Addr` fields the actual set draws from are package-internal — either literal strings (`":443"`, `":80"`) or the same operator-supplied value the parser already validated. No untrusted parser surface in the check itself. No findings.
- **[Tokens, secrets, credentials]** N/A. No credential material is read or written. The check operates on port numbers. The error message contains port numbers and the literal `relay: …` prefix — no operator-supplied strings beyond the (already operator-controlled) port digits. No findings.
- **[File operations]** N/A in `internal/relay/listeners.go`. The `cmd/pyrycode-relay/deps_test.go` test invokes `exec.Command("go", "list", "-deps", "-json", <hardcoded-import-path>)` — the import path is a compile-time string constant, not derived from any user input or env var. The test does not write any file, does not concatenate paths, does not read attacker-controlled data. The subprocess inherits the test runner's environment (which is fine for a test) and writes to a `*bytes.Buffer` in memory. No findings.
- **[Subprocess / external command execution]** The `go list` invocation in the test is the only subprocess. Arguments are hardcoded literals (`"list"`, `"-deps"`, `"-json"`, the import-path string constant). No `sh -c`, no shell metacharacters, no user-supplied substring in `argv`. The lookup uses `exec.LookPath("go")` (or `exec.Command("go", ...)` which delegates to `$PATH`) — a PATH-poisoning test environment could substitute a malicious `go`, but the same risk applies to every other Go test in the repo and is out of scope for this ticket. No findings.
- **[Cryptographic primitives]** N/A. No RNG, no hash, no constant-time comparison. The `actual \ expected` operation is a bitwise-comparable set-difference on `uint64` keys (via map lookup); not security-critical timing because neither input is attacker-controlled in a way that depends on the comparison's runtime. No findings.
- **[Network & I/O]** The entire purpose of the ticket is to *prevent* a listener from opening in a misconfigured state. The check runs before any `Listen*` call. Listener timeouts, frame caps, header gates — all unchanged. The check itself does not open any network socket; `net.SplitHostPort` is a pure string parse. No findings.
- **[Error messages, logs, telemetry]** The wrapped error message contains: (a) the sentinel prose, (b) the surplus port list as `[]uint16` formatted via `%v` (digits + brackets — no operator-supplied strings), (c) the expected port list as `[]uint16` (same shape). None are user-controlled in a way an attacker can reach: the surplus ports come from in-process `http.Server.Addr` literals or from the operator's own `--insecure-listen` value (and even there, the parser has already enforced `ParseUint(_, 10, 16)` plus the `0` reject, so the value is a number in `1..65535`). The `main`-side log line emits `err`, `unexpected_ports`, `expected_ports`, and (on the parse path) `addr`. The `addr` field on the parse-error path carries the operator's flag value verbatim; this is acceptable — operator flags are not secrets, and the diagnostic value of seeing the exact bad input outweighs a hypothetical "operator put a token in `--insecure-listen`" misuse. **Worth flagging to the developer:** do not extend the success path to log the expected/actual ports on every boot — only log on failure, mirroring the silence-on-success convention from #77/#78/#79. No findings against the spec as written.
- **[Concurrency]** N/A. The check runs on the main goroutine before any other goroutine is spawned (in the autocert branch, the goroutine that runs `httpSrv.ListenAndServe` is spawned only after the check passes; in the insecure branch, no goroutines are spawned at all). The `map[uint16]struct{}` instances live entirely on `main`'s stack frame. No shared mutable state. No findings.
- **[Threat model alignment]** `docs/threat-model.md` § Deploy treats "operator misconfiguration leaks an unauthenticated surface" as the dominant failure class for an internet-exposed relay. This ticket adds an in-binary defence against one specific shape (surplus listener). It complements `ErrInsecureListenInProduction` (transport drift), `ErrRunningAsRoot` (uid drift), `ErrUnexpectedCapability` (capability drift), and `ErrCacheDirInsecure` (file-mode drift). All five share the "refuse to start; fail the health check; never serve traffic in this configuration" shape. The build-graph `TestBinaryDoesNotImportPprof` test adds a *second* defence on a different layer (compile-time vs runtime) against the highest-frequency leak shape, which is exactly the "belt-and-suspenders means different fabric" guidance in the architect handbook. No findings.
- **[Asymmetric-check correctness]** Explicit walk: the asymmetric design (surplus = error, missing = nil) is correct because the missing case is observable through a distinct, louder failure path (`ListenAndServe` returns a bind error). A reviewer might worry that an attacker could exploit the asymmetry — *no*. The check inputs are not attacker-controllable (both sets are derived from operator flags and in-process literals), so the asymmetric direction has no adversarial axis. The asymmetry's only consequence is "what kind of operator misconfiguration is caught here vs caught at runtime." No findings.

**One SHOULD FIX flagged inline** (Error messages, logs, telemetry): the developer must not log the expected/actual port sets on the success path. The spec is written that way (silence on success); code review must double-check.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
