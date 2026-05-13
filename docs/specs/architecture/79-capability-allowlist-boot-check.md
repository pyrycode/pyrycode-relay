# Spec: refuse to boot when Linux effective capabilities exceed allowlist

Ticket: [#79](https://github.com/pyrycode/pyrycode-relay/issues/79). Size S. Split from #42. Sibling of #77.

## Files to read first

- `cmd/pyrycode-relay/main.go` (whole file, 136 lines) — the only call site. Lines 45–51 hold the just-landed `CheckInsecureListenInProduction` wiring from #77; the new capability check slots in immediately after, before `startedAt := time.Now()` (line 53). Lines 62–127 show the listener-start branches the check must run *before*.
- `internal/relay/production.go` (whole file, 47 lines) — the canonical "boot-time refusal helper" pattern in this package, just merged via #77. The shape this spec commits to (single exported `Check…` returning `error`, sentinel branchable via `errors.Is`, injected test seam instead of mutating process state) mirrors it line-for-line; deviate only where Linux/non-Linux split forces it.
- `internal/relay/production_test.go` (whole file, 119 lines) — the table-driven, `t.Parallel()`-safe, `errors.Is`-against-sentinel test style for boot-check helpers. The fake-getter helper (`fakeGetenv` closure over `map[string]string`) is the exact pattern this spec uses for its `readStatus` seam.
- `internal/relay/tls.go:15-19` — the canonical sentinel-error declaration shape (`var ErrCacheDirInsecure = errors.New("relay: …")` plus a Go doc comment that names the contract). Mirror it.
- `internal/relay/tls.go:29-49` — the canonical wrapped-sentinel return shape (`fmt.Errorf("%w: %s …", sentinel, detail)`). The capability check uses this when including the offending bit list.
- `docs/specs/architecture/77-refuse-insecure-in-production.md` — the immediate precedent (sibling ticket, same parent #42). Same call-site, same sentinel-error contract, same testing approach. The "Why not a `Config` struct" section at line 111 covers the rationale for keeping each boot-check as its own exported `Check…` function rather than consolidating; that rationale carries over here.
- `docs/specs/architecture/9-autocert-tls.md` — the original `ErrCacheDirInsecure` rationale. The "fail loud, before any listener starts" framing is identical to this ticket's intent.
- `docs/specs/architecture/32-dockerfile-base-hardening.md:213-214` — the prior architect's note that "Binding `:80` and `:443` from uid 65532 is the host's problem (port mapping, `CAP_NET_BIND_SERVICE`, or a host-side proxy); the portable artifact stays uid-neutral." This determines the allowlist: `CAP_NET_BIND_SERVICE` is the *only* capability the relay's autocert mode legitimately needs.
- `Dockerfile:31-39` — the runtime image (distroless `:nonroot`, uid 65532, no `setcap` on the binary). Combined with the fly.toml `internal_port = 80 / 443` lines, this establishes that the running container needs `CAP_NET_BIND_SERVICE` (or equivalent host-side kernel sysctl) to bind privileged ports.
- `docs/PROJECT-MEMORY.md` § "Project-level conventions" — the "Sentinel errors + `errors.Is` branching", "Loud failure over silent correction", and "Tests live in the same package" rules apply directly.

## Context

Container runtimes can grant Linux capabilities via `--cap-add`, capability bounding sets, or default Docker profiles. A misconfigured deploy that grants the relay extra capabilities (CAP_SYS_ADMIN, CAP_NET_ADMIN, CAP_DAC_OVERRIDE, …) escapes CI build scans and runs with more privilege than intended.

The failure mode this ticket prevents is the silent-elevation class: a `--cap-add SYS_ADMIN` slipped into a deploy manifest does not fail the build; the container starts; `/healthz` returns 200; the relay processes traffic with capabilities it never needed. The defence must run *at boot, before any listener accepts a connection*, so a misconfigured deploy fails the health-check rather than serving traffic.

The implementation is a Linux-only parse of `/proc/self/status`'s `CapEff:` hex mask, compared against an explicit allowlist. On non-Linux platforms (darwin dev runs, future Windows/BSD CI) the check is a no-op with a single startup log line — the build-tag split keeps both the production path (Linux container) and the developer path (darwin) honest without runtime conditionals.

The allowlist is `{CAP_NET_BIND_SERVICE}` — the single capability the autocert mode legitimately needs to bind `:80` and `:443` from uid 65532 inside the distroless image. All other capabilities are unexpected.

This ticket establishes the `_linux.go` / `_other.go` build-tag convention for the repo. No prior file uses that split (`grep -r '//go:build linux' internal/ cmd/` returns nothing as of `main` at 15dc00f).

Capability bit positions are stable kernel ABI defined in `include/uapi/linux/capability.h`. The bit positions and symbolic names are frozen for the kernels the relay supports (Linux ≥ 4.x); future capability additions only ever extend the table, never renumber.

Related: #9 (`ErrCacheDirInsecure` set the boot-time-refusal precedent); #77 (the immediate sibling — production-mode env contract and `--insecure-listen` boot refusal, just merged); #32 (distroless `:nonroot` hardening that motivates this check); #42 (parent — was split into #77 / #78 / #79).

## Design

Three new files in `internal/relay/`, one new test file (cross-platform), one Linux-only test file, one ~7-line addition to `cmd/pyrycode-relay/main.go`. No existing code is refactored.

### File layout

| Path | Purpose | Build tag |
|---|---|---|
| `internal/relay/caps.go` | Sentinel, `Capability` type, `AllowedCapabilities`, name table, `parseCapEff`, `checkCapEffMask` (pure functions) | none (compiled on every GOOS) |
| `internal/relay/caps_linux.go` | `CheckCapabilities` Linux entry + `readProcSelfStatus` + `checkCapabilitiesWithReader` seam | `_linux.go` suffix (auto-applied for GOOS=linux) |
| `internal/relay/caps_other.go` | `CheckCapabilities` no-op + skip log | explicit `//go:build !linux` |
| `internal/relay/caps_test.go` | Tests for `parseCapEff` + `checkCapEffMask` (pure-function cases) | none |
| `internal/relay/caps_linux_test.go` | Tests for `checkCapabilitiesWithReader` (the seam) | `_linux_test.go` suffix |

**Convention chosen:** `_linux.go` (filename suffix that Go's build system auto-applies for GOOS=linux) and `_other.go` (with explicit `//go:build !linux`). The `_other` suffix is not a recognised GOOS, so the explicit build tag is required; the `_linux` half is tag-free for the same reason `os/file_unix.go` is tag-free in stdlib. Future per-GOOS splits in this repo should follow the same `_<goos>.go` / `_other.go` pairing.

### Sentinel error and allowlist (in `caps.go`, exported)

```go
// ErrUnexpectedCapability is returned by CheckCapabilities when the
// process's effective Linux capability set (CapEff) contains a bit
// outside AllowedCapabilities. Stray capabilities (CAP_SYS_ADMIN,
// CAP_NET_ADMIN, etc.) usually mean a misconfigured container runtime
// — --cap-add in a Docker run, an over-broad bounding set, or a
// default profile granting more than the relay needs. The relay
// refuses to start so the misconfiguration fails the deploy's
// health check rather than running with elevated privilege.
//
// Branchable via errors.Is. The wrapped error message identifies
// each unexpected bit (numeric position plus symbolic name where
// known) and the current allowlist.
var ErrUnexpectedCapability = errors.New("relay: process has unexpected effective Linux capabilities")

// Capability is a Linux capability bit and its symbolic name.
type Capability struct {
    Bit  uint  // 0-based bit position as defined in <linux/capability.h>.
    Name string // e.g. "CAP_NET_BIND_SERVICE". Empty for unknown bits.
}

// AllowedCapabilities is the explicit allowlist of effective Linux
// capabilities the relay legitimately needs.
//
// CAP_NET_BIND_SERVICE (bit 10) is included because the autocert mode
// binds :80 and :443 from uid 65532 inside the distroless image
// (Dockerfile, fly.toml). Hosts that drop this cap and instead lower
// net.ipv4.ip_unprivileged_port_start are fine — CapEff will be 0 and
// the check passes; hosts that grant it via Docker's default profile
// are also fine — CapEff has the bit set and it matches the allowlist.
//
// All other Linux capabilities are unexpected. To extend, add an entry
// and document the deployment shape that requires it.
var AllowedCapabilities = []Capability{
    {Bit: 10, Name: "CAP_NET_BIND_SERVICE"},
}
```

**Decision: allowlist as a slice of typed records, not a bitmask constant.** Two reasons: (a) the public allowlist is operator-facing — slog'ing it in the failure log needs `Name`s, not a hex int; (b) appending a new entry is a one-line diff with the symbolic name visible in code review, vs. a `1<<N |` bitmask change that requires the reader to cross-reference `capability.h`.

The bitmask form (`AllowedMask() uint64`) is computed once per call inside `checkCapEffMask` from the slice — derived state, not a separate API.

### Capability name table (in `caps.go`, package-private)

```go
// capabilityName returns the symbolic name for a Linux capability bit
// (e.g. "CAP_SYS_ADMIN" for bit 21), or the empty string if unknown.
// The table tracks include/uapi/linux/capability.h up to CAP_CHECKPOINT_RESTORE
// (bit 40, kernel 5.9+). New capabilities only append; bit positions are
// stable kernel ABI and never renumber.
func capabilityName(bit uint) string {
    if int(bit) >= len(capabilityNames) {
        return ""
    }
    return capabilityNames[bit]
}

// Indexed by bit position. The exact names below are the kernel's
// CAP_* macros without the CAP_ prefix removed.
var capabilityNames = []string{
    0:  "CAP_CHOWN",
    1:  "CAP_DAC_OVERRIDE",
    2:  "CAP_DAC_READ_SEARCH",
    3:  "CAP_FOWNER",
    4:  "CAP_FSETID",
    5:  "CAP_KILL",
    6:  "CAP_SETGID",
    7:  "CAP_SETUID",
    8:  "CAP_SETPCAP",
    9:  "CAP_LINUX_IMMUTABLE",
    10: "CAP_NET_BIND_SERVICE",
    11: "CAP_NET_BROADCAST",
    12: "CAP_NET_ADMIN",
    13: "CAP_NET_RAW",
    14: "CAP_IPC_LOCK",
    15: "CAP_IPC_OWNER",
    16: "CAP_SYS_MODULE",
    17: "CAP_SYS_RAWIO",
    18: "CAP_SYS_CHROOT",
    19: "CAP_SYS_PTRACE",
    20: "CAP_SYS_PACCT",
    21: "CAP_SYS_ADMIN",
    22: "CAP_SYS_BOOT",
    23: "CAP_SYS_NICE",
    24: "CAP_SYS_RESOURCE",
    25: "CAP_SYS_TIME",
    26: "CAP_SYS_TTY_CONFIG",
    27: "CAP_MKNOD",
    28: "CAP_LEASE",
    29: "CAP_AUDIT_WRITE",
    30: "CAP_AUDIT_CONTROL",
    31: "CAP_SETFCAP",
    32: "CAP_MAC_OVERRIDE",
    33: "CAP_MAC_ADMIN",
    34: "CAP_SYSLOG",
    35: "CAP_WAKE_ALARM",
    36: "CAP_BLOCK_SUSPEND",
    37: "CAP_AUDIT_READ",
    38: "CAP_PERFMON",
    39: "CAP_BPF",
    40: "CAP_CHECKPOINT_RESTORE",
}
```

The table is unexported because callers only ever consume it via the error message produced by `checkCapEffMask`. If a future ticket needs programmatic lookup, exporting is a one-line change.

### Pure parsing and check functions (in `caps.go`, package-private)

```go
// parseCapEff extracts the CapEff: hex mask from /proc/self/status
// content. The expected line shape is:
//   CapEff:	0000000000000400
// (whitespace separator may be tab or spaces; the value is 0–16 hex
// digits with no 0x prefix). Returns a wrapped error if the line is
// missing or the value does not parse as hex. Does NOT wrap
// ErrUnexpectedCapability — malformed input is a separate failure mode
// from "capabilities exceed allowlist".
func parseCapEff(procStatus string) (uint64, error) {
    // Iterate lines, look for "CapEff:" prefix, strconv.ParseUint(value, 16, 64).
    // Return fmt.Errorf("relay: parsing /proc/self/status CapEff: %w", err) on failure.
    // Return fmt.Errorf("relay: /proc/self/status missing CapEff line") if not found.
}

// checkCapEffMask returns ErrUnexpectedCapability (wrapped with the
// offending bit list and the allowlist contents) if the mask has any
// bit set outside AllowedCapabilities. Returns nil otherwise.
//
// The wrapped error message format is:
//   relay: process has unexpected effective Linux capabilities: \
//   CAP_SYS_ADMIN (bit 21), bit 63; allowlist: [CAP_NET_BIND_SERVICE (bit 10)]; \
//   drop with --cap-drop=ALL --cap-add=NET_BIND_SERVICE or equivalent
//
// Unknown bits (no entry in capabilityNames) are reported as "bit N".
func checkCapEffMask(mask uint64) error {
    allowed := allowedMask()      // bitwise OR of AllowedCapabilities[*].Bit
    unexpected := mask &^ allowed // bits set in mask but not in allowed
    if unexpected == 0 {
        return nil
    }
    // Build the bit list, format the message, wrap ErrUnexpectedCapability.
    return fmt.Errorf("%w: %s; allowlist: %s; drop with --cap-drop=ALL --cap-add=NET_BIND_SERVICE or equivalent",
        ErrUnexpectedCapability, formatBits(unexpected), formatAllowlist())
}
```

**Decision: report all unexpected bits, not just the first.** A misconfigured manifest often grants several caps in one breath (`--cap-add ALL` minus a handful); listing one bit at a time would make an operator restart the deploy loop N times. The error message lists every offending bit.

**Decision: include the operator fix string in the error message itself.** Mirrors `ErrCacheDirInsecure`'s wrapped form (`"%w: %s (mode %o)"`) and the AC's "structured log line that names the unexpected capability and the operator fix." The fix is fixed text — `--cap-drop=ALL --cap-add=NET_BIND_SERVICE or equivalent` — because the operator's runtime might be Docker, Kubernetes (`securityContext.capabilities.drop`), or Podman; "or equivalent" covers them all without naming each.

### Linux entry point (in `caps_linux.go`)

```go
// CheckCapabilities reads /proc/self/status's CapEff line and returns
// ErrUnexpectedCapability (wrapped) if the effective capability set
// contains any bit outside AllowedCapabilities. Returns nil otherwise.
//
// Intended to be called from main after flag parse, before any listener
// is started. Read errors on /proc/self/status are returned wrapped
// (not as ErrUnexpectedCapability) so callers can distinguish "kernel
// /proc gone weird" from "operator handed us extra caps."
func CheckCapabilities() error {
    return checkCapabilitiesWithReader(readProcSelfStatus)
}

func readProcSelfStatus() (string, error) {
    data, err := os.ReadFile("/proc/self/status")
    if err != nil {
        return "", fmt.Errorf("relay: reading /proc/self/status: %w", err)
    }
    return string(data), nil
}

// checkCapabilitiesWithReader is the test seam. Tests pass a closure
// returning canned /proc/self/status contents; production passes
// readProcSelfStatus.
func checkCapabilitiesWithReader(readStatus func() (string, error)) error {
    status, err := readStatus()
    if err != nil {
        return err
    }
    mask, err := parseCapEff(status)
    if err != nil {
        return err
    }
    return checkCapEffMask(mask)
}
```

**Decision: inject the *full status reader*, not just the parsed mask.** The seam at the `readStatus func() (string, error)` boundary lets the malformed-input test (case d) exercise `parseCapEff` end-to-end without a separate seam at the mask-injection layer. Injecting `mask uint64` directly would skip `parseCapEff` and leave AC case (d) uncovered by the seam.

`checkCapabilitiesWithReader` is package-private (lowercase) — tests live in the same package per the PROJECT-MEMORY "Tests live in the same package" rule, so they can call it directly. External callers go through `CheckCapabilities`.

### Non-Linux no-op (in `caps_other.go`)

```go
//go:build !linux

package relay

import (
    "log/slog"
    "runtime"
)

// CheckCapabilities is a no-op on non-Linux platforms. The /proc/self/status
// path the Linux variant reads exists only on Linux; on darwin/Windows/BSD
// there is no capability model to check, so the function returns nil and
// logs a one-line note that the check was skipped. The log emits via
// slog.Default(), so the caller's slog handler captures it alongside the
// rest of startup output.
func CheckCapabilities() error {
    slog.Default().Info("skipping linux-only capability check", "goos", runtime.GOOS)
    return nil
}
```

The Linux variant does *not* log a "running capability check" line on success — the absence of a log line is the success signal, mirroring `CheckInsecureListenInProduction` (which logs only on failure). The non-Linux variant logs because the "exactly once" skip note is the only signal the check ran at all.

### Wiring in `cmd/pyrycode-relay/main.go`

Insert immediately after the existing `CheckInsecureListenInProduction` block (lines 45–51 on `main` at 15dc00f), before `startedAt := time.Now()`:

```go
if err := relay.CheckCapabilities(); err != nil {
    logger.Error("refusing to start: unexpected Linux capabilities",
        "err", err,
        "fix", "drop extra capabilities (e.g. --cap-drop=ALL --cap-add=NET_BIND_SERVICE on docker, or securityContext.capabilities on kubernetes)")
    os.Exit(2)
}
```

**Three details:**

- **Exit code 2**, matching the existing flag-validation and production-mode guards. Exit 1 is for runtime listener failures; exit 2 is for boot-time configuration refusal. Distinct codes let ops dashboards split "deploy never started" from "deploy started and crashed."
- **No production-mode gating.** The AC is explicit: this check runs in every environment, including dev. The reasoning is that stray capabilities are *never* legitimate — a dev macOS box doesn't have Linux caps, a dev Linux container with extra caps is a misconfigured rehearsal of the production deploy. The check is uniformly on.
- **Placement: after `CheckInsecureListenInProduction`, before `relay.NewRegistry()`.** No side effects between flag-parse and `NewRegistry` on `main` as of 15dc00f, so the relative order between the two `Check…` calls is purely stylistic; lining them up adjacent makes a future "extract a `runStartupChecks` helper" refactor a one-block move.

### Why not also check CapPrm / CapBnd / CapInh

The PO body invites the architect to choose. The decision is **CapEff only**, for three reasons:

1. **CapEff is the load-bearing set.** The kernel consults CapEff (not CapPrm) when authorising a privileged syscall. A capability in CapPrm but not CapEff is held in reserve — it can be raised into CapEff via `capset(2)` but cannot be used until then. The relay binary never calls `capset`, never `setuid`s, never `exec`s into a setuid binary; CapPrm bits that aren't in CapEff are inert.
2. **CapBnd / CapInh broaden the false-positive surface.** CapBnd is the bounding set inherited from the container runtime; it can legitimately be a superset of CapEff (the runtime says "you *could* hold these if you raise them"). Refusing to boot on a wide CapBnd would reject Kubernetes pods running under default policy where CapBnd is broader than CapEff by design.
3. **Minimum surface = minimum noise.** Adding the other three masks now would flag legitimate deployments and force operators to over-grant. If a future incident shows CapPrm/CapBnd matters (e.g. a Go runtime change makes `capset(2)` reachable from a goroutine), follow-up ticket adds the additional checks with a clear motivating failure.

This decision is documented in the Go doc comment on `CheckCapabilities` so a future contributor extending the check has the rationale on hand.

### Why not gate behind PYRYCODE_RELAY_PRODUCTION

The PO body's last AC is explicit: this check has no production-mode predicate. Repeated here for the spec record: extra capabilities are never legitimate. A dev Linux box that picks up `--cap-add SYS_ADMIN` from a copy-pasted Docker compose stanza fails fast the same way prod would, which is the whole point of running the dev loop and prod from the same binary. The check is unconditional.

## Concurrency model

None. `CheckCapabilities` runs on the main goroutine before any listener is started. No locks, no channels, no goroutines. The file read (`os.ReadFile("/proc/self/status")`) is a single synchronous syscall; the kernel atomically snapshots the process's capability state for that read.

## Error handling

Three failure modes, each with a distinct return shape:

| Mode | Cause | Return | Branchable via |
|---|---|---|---|
| Allowlist violation | CapEff has bit outside `AllowedCapabilities` | `fmt.Errorf("%w: …", ErrUnexpectedCapability)` | `errors.Is(err, ErrUnexpectedCapability)` |
| Malformed `/proc/self/status` | Missing `CapEff:` line, non-hex value | `fmt.Errorf("relay: parsing /proc/self/status CapEff: …", …)` | — (treated as fatal by main) |
| `os.ReadFile` failed | `/proc` unmounted, permission denied | `fmt.Errorf("relay: reading /proc/self/status: %w", err)` | underlying `os.PathError` |

All three lead to `os.Exit(2)` in main with the same log line — operators don't need to distinguish (the `err` field carries the wrapped detail). The branchable sentinel exists so future callers (e.g. an integration test that wants to assert "this misconfiguration produces *this specific* refusal") can match on `ErrUnexpectedCapability` without string-matching the log.

No retry, fallback, or partial-state recovery. Boot-time refusal is total.

## Testing strategy

All tests live alongside the implementation and are `t.Parallel()`-safe. No `os.Setenv`, no `t.Setenv`, no touching real `/proc`.

### `caps_test.go` — cross-platform pure-function tests

These run on every GOOS the project builds for (linux, darwin, …). They exercise `parseCapEff` and `checkCapEffMask` directly. Helper:

```go
// Build a /proc/self/status fixture with a custom CapEff line.
func statusFixture(capEffHex string) string {
    return "Name:\trelay\nState:\tR (running)\nCapEff:\t" + capEffHex + "\nCapBnd:\tffffffffffffffff\n"
}
```

#### Test: `parseCapEff` value matrix

Table-driven over `(input string, want uint64, wantErr bool)`:

| input | want | wantErr |
|---|---|---|
| `statusFixture("0000000000000000")` | `0` | false |
| `statusFixture("0000000000000400")` | `0x400` (bit 10) | false |
| `statusFixture("00000000003fffffffff")` | `0x3fffffffff` (bits 0–37) | false |
| `statusFixture("ffffffffffffffff")` | `^uint64(0)` | false |
| `statusFixture("0")` (short value) | `0` | false |
| `"Name:\trelay\n"` (no CapEff line) | — | true |
| `statusFixture("not-hex")` | — | true |
| `""` (empty file) | — | true |
| `statusFixture("400 trailing junk")` | — | true (ParseUint rejects spaces in the field) |

The malformed cases cover AC (d). The "short value" case documents that the kernel does not zero-pad to 16 digits in older procfs versions.

#### Test: `checkCapEffMask` allowlist matrix

| mask | want |
|---|---|
| `0` (empty CapEff) | `nil` |
| `0x400` (only CAP_NET_BIND_SERVICE) | `nil` |
| `0x200000` (only CAP_SYS_ADMIN, bit 21) | `errors.Is(err, ErrUnexpectedCapability)` |
| `0x400 \| 0x200000` (allowed + disallowed) | sentinel; error message names `CAP_SYS_ADMIN` |
| `1 << 63` (unknown bit) | sentinel; error message contains `"bit 63"` (no symbolic name) |
| `^uint64(0)` (all bits) | sentinel; error message lists allowlist contents |

These cover AC (a), (b), (c). The unknown-bit row protects against a future kernel adding `CAP_CHECKPOINT_RESTORE+1` without an entry in `capabilityNames`.

#### Test: `ErrUnexpectedCapability` is branchable

A one-line test that `errors.Is(checkCapEffMask(0x200000), ErrUnexpectedCapability)` returns true. Locks in the `errors.Is` contract so a future "let me return `fmt.Errorf("relay: %s", ...)` instead" refactor breaks the test, not downstream callers.

### `caps_linux_test.go` — Linux-only seam test

Has the `_linux_test.go` suffix so it compiles only when building on Linux (CI on Ubuntu, plus any contributor on a Linux dev box). Exercises `checkCapabilitiesWithReader` with fake `readStatus` closures:

| `readStatus` returns | want |
|---|---|
| `(statusFixture("0000000000000400"), nil)` (only allowed) | `nil` |
| `(statusFixture("0000000000200000"), nil)` (CAP_SYS_ADMIN) | `errors.Is(err, ErrUnexpectedCapability)` |
| `("Name:\trelay\n", nil)` (missing CapEff line) | wrapped error (not sentinel), no panic |
| `("", errors.New("io: synthetic"))` | wrapped read error, no panic |

The seam test confirms (a) the parse + mask check is correctly threaded through `checkCapabilitiesWithReader`, and (b) read errors from the injected reader propagate without crashing — which is the production-relevant case for "what if /proc is unmounted in a stripped container."

### What is NOT tested

- The `main.go` wiring. Same rationale as #77's spec: `main` is a coordination function, and the unit tests on `CheckCapabilities` cover the behaviour. A fork-exec integration test for one `if err != nil { os.Exit(2) }` block is over-engineering. Code review of the diff is the gate.
- The non-Linux `CheckCapabilities` no-op. It is two lines (slog + return nil); the log message is operator-facing prose, not a downstream-machine-parsed format. Asserting on slog output requires capturing the handler, which is friction the line count does not earn.
- The real `/proc/self/status` on the CI runner. The seam test runs on Linux but with injected input; reading the *actual* `/proc/self/status` would couple the test to the CI runner's capability configuration (which varies between GitHub Actions standard runners and our distroless image). Production behaviour is verified by the failure-path's loud refusal — a CI environment whose CapEff contains an unexpected bit would cause `make test` to … pass (these are unit tests, not the binary running). The intended deployment-level check is "deploy fails health check," which is the explicit design.
- The exact format of the error message string. We test the *content* (sentinel branchable, contains "CAP_SYS_ADMIN", contains "bit 63") but not the exact whitespace or punctuation. The format is operator-facing prose; cosmetic changes should not break tests.

## Open questions

None. The allowlist contents (`CAP_NET_BIND_SERVICE` only), the build-tag file naming convention (`_linux.go` / `_other.go`), the choice to inject at the reader boundary rather than the mask boundary, the choice to check CapEff only, and the choice to keep the check unconditional (no production-mode gating) are all settled above.

The capability-bit name table tracks `include/uapi/linux/capability.h` through `CAP_CHECKPOINT_RESTORE` (bit 40, kernel 5.9, ~2020). The relay's deployment kernel (Fly's Firecracker-on-Linux fleet) is currently 5.x+. Future kernel additions (none queued as of 2026-05) only ever extend the table — appending new entries is a one-line follow-up.

## Security review

**Verdict:** PASS

This review was conducted per `architect/security-review.md` for the security-sensitive ticket #79. The spec was walked adversarially against each category. Findings:

- **[Trust boundaries]** The single input crossing into `CheckCapabilities` is `/proc/self/status`, sourced from the kernel — not from a network attacker, not from operator-supplied input. The kernel's procfs is part of the relay's TCB; if it lies about CapEff, the kernel itself is compromised and the relay's other defences (TLS, header validation, …) are also moot. No untrusted parser surface. No allocation driven by attacker-controlled length (the status file is bounded by kernel formatting). No findings.
- **[Tokens, secrets, credentials]** N/A. The ticket reads no credential material. The CapEff value is a state observation, not a secret. The log line carries the capability *name* (`CAP_SYS_ADMIN`) and the bit position, not anything operator-sensitive. No findings.
- **[File operations]** Single read of `/proc/self/status`, hard-coded path. No path concatenation, no TOCTOU window (the file is read once, parsed in memory, the result is consumed in the same syscall sequence). No write, no unlink, no permission change. No findings.
- **[Subprocess / external command execution]** None. No `exec.Command`, no `os.StartProcess`, no shell-out. The "operator fix" string in the error message is data, not a command we run. No findings.
- **[Cryptographic primitives]** N/A. No RNG, no hash, no comparison of attacker-controlled values. The bit-mask comparison is a bitwise `&^` on two `uint64`s; no timing side-channel risk because neither input is attacker-controlled (one is the kernel's view of the process, the other is a compile-time constant). No findings.
- **[Network & I/O]** The whole purpose of the ticket is to refuse to open listeners when the capability state is wrong. The check runs before any `http.Server` is started. Listener timeouts, frame caps, header gates — all unchanged. No findings.
- **[Error messages, logs, telemetry]** The wrapped error message contains: (a) the sentinel prose, (b) the unexpected capability *names* (or `bit N` for unknown), (c) the allowlist contents, (d) a fixed-text operator fix. None of these are user-controlled — they are constants drawn from `capabilityNames` and the literal allowlist slice. The main-side log line emits `err` (the wrapped detail) and `fix` (a fixed string). The CapEff hex value itself is not logged separately; it lives only inside the formatted error message via the symbolic names that derive from it. **Worth flagging to the developer:** do not extend the log line to add a `cap_eff` field with the raw hex value. The symbolic names already convey what an operator needs; the raw mask is forensic detail that, if logged, would clutter centralised logging for every misconfigured boot and would also leak (via the bit positions) any future capabilities we add to the allowlist before we've documented them. No findings against the spec as written.
- **[Concurrency]** N/A. `CheckCapabilities` runs on the main goroutine before any goroutine is spawned. No shared mutable state. The `capabilityNames` slice and `AllowedCapabilities` slice are package-level vars; both are initialised once at program start and only read thereafter. No data race surface. No findings.
- **[Build-tag correctness]** The Linux-only entry point lives in `caps_linux.go` (auto-applied filename suffix); the no-op lives in `caps_other.go` with explicit `//go:build !linux`. The two are mutually exclusive — no GOOS triggers both, no GOOS triggers neither. A future contributor who adds a `caps_freebsd.go` (matching `_freebsd.go` suffix) would auto-disable the `_other.go` no-op for that GOOS, which is the correct extension shape. The build-tag boundary itself is not a security boundary (it's a compile-time selector), but worth confirming the no-op is genuinely no-op: it has zero behavior beyond logging, returns nil unconditionally. No findings.
- **[Threat model alignment]** `docs/threat-model.md` § Deploy treats "operator misconfiguration" as the dominant failure class for a single-machine, internet-exposed relay. This ticket adds an in-binary defence against one specific shape of that class (over-broad capability grant). It complements `ErrCacheDirInsecure` (mode-bit drift), `ErrInsecureListenInProduction` (transport drift, just merged via #77), and a future #78 (uid-0 in production). All four share the "refuse to start; fail the health check; never serve traffic in this configuration" shape. No findings.

**One SHOULD FIX flagged inline** (Error messages, logs, telemetry): the developer must not extend the log line to include the raw CapEff hex value. The spec is written that way; code review must double-check.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-13
