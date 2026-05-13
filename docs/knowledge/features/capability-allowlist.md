# Linux capability allowlist (boot-time refusal)

The relay refuses to start when its process's *effective* Linux capability set (`CapEff`) contains any bit outside an explicit allowlist. A container runtime that grants stray capabilities (`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `--cap-add ALL`, an over-broad bounding set, …) fails the deploy's health check rather than serving traffic with elevated privilege. Added by #79.

## The contract

- **Source of truth:** `/proc/self/status`'s `CapEff:` hex mask. CapEff is what the kernel consults when authorising a privileged syscall; CapPrm/CapBnd/CapInh are intentionally not checked (see "Why CapEff only" below).
- **Allowlist:** `AllowedCapabilities` — currently `{CAP_NET_BIND_SERVICE (bit 10)}` only. That single capability is needed because autocert mode binds `:80` and `:443` from uid 65532 inside the distroless image (Dockerfile, fly.toml). Hosts that drop the cap and lower `net.ipv4.ip_unprivileged_port_start` instead also pass (CapEff = 0 satisfies the allowlist).
- **Unconditional.** No production-mode gating, no env-var bypass. Stray capabilities are never legitimate; a dev container with `--cap-add SYS_ADMIN` slipped in from a copy-pasted Compose stanza fails the same way prod would.
- **Linux only.** Non-Linux GOOS (darwin dev runs, future Windows/BSD CI) skips the check with a one-line `slog` info log. Split at compile time via build tags (see ADR-0009).

## API (`internal/relay/caps.go` + `caps_linux.go` + `caps_other.go`)

- `ErrUnexpectedCapability` — exported sentinel, branchable via `errors.Is`. Wrapped error message names each unexpected bit (`CAP_NAME (bit N)` or `bit N` for unknown), lists the current allowlist contents, and embeds the operator fix string.
- `Capability{Bit uint; Name string}` — exported record type used by the allowlist.
- `AllowedCapabilities []Capability` — exported package-level slice. Operator-facing (formatted by name into the failure message), one-line diff to extend.
- `CheckCapabilities() error` — Linux entry point reads `/proc/self/status`, parses the `CapEff:` line, returns `ErrUnexpectedCapability` (wrapped) on violation or a separately-wrapped error on read / parse failure. On non-Linux GOOS the function is a no-op that returns nil and logs once at info.

Internal helpers (`parseCapEff`, `checkCapEffMask`, `capabilityName`, `allowedMask`, `formatBits`, `formatAllowlist`) live in `caps.go` (cross-platform, pure functions). The `checkCapabilitiesWithReader(readStatus func() (string, error))` seam lives in `caps_linux.go` so tests can inject canned `/proc/self/status` content without touching real `/proc`.

## Wiring (`cmd/pyrycode-relay/main.go`)

Immediately after the `CheckInsecureListenInProduction` block (#77), before `relay.NewRegistry()`:

```go
if err := relay.CheckCapabilities(); err != nil {
    logger.Error("refusing to start: unexpected Linux capabilities",
        "err", err,
        "fix", "drop extra capabilities (e.g. --cap-drop=ALL --cap-add=NET_BIND_SERVICE on docker, or securityContext.capabilities on kubernetes)")
    os.Exit(2)
}
```

- **Exit code 2** = config-rejected-at-boot, matching the boot-time-refusal convention (#77).
- **No `cap_eff` raw-hex log field.** The wrapped error already names every offending bit symbolically; logging the raw mask alongside would clutter centralised logging and could leak future-allowlisted bits before they're documented. Security review SHOULD-FIX from #79's spec — carry forward to any future extension.
- **`fix` field** lists Docker and Kubernetes resolutions explicitly; "or equivalent" inside the wrapped error covers Podman / nerdctl / Fly Machines.

## Failure modes (three distinct return shapes)

| Cause | Return | Branchable via |
|---|---|---|
| CapEff has bit outside allowlist | `fmt.Errorf("%w: …", ErrUnexpectedCapability)` | `errors.Is(err, ErrUnexpectedCapability)` |
| `/proc/self/status` missing `CapEff:` or non-hex value | `fmt.Errorf("relay: parsing /proc/self/status CapEff …", …)` | — |
| `os.ReadFile("/proc/self/status")` failed | `fmt.Errorf("relay: reading /proc/self/status: %w", err)` | underlying `os.PathError` |

All three lead to `os.Exit(2)` in main. The sentinel exists so future callers (integration tests asserting "this misconfiguration produces *this* refusal") can match without string-matching the log.

## Why CapEff only

CapEff is the load-bearing set: the kernel consults it (not CapPrm) when authorising privileged syscalls. The relay never calls `capset(2)`, never `setuid`s, never exec's into a setuid binary — CapPrm bits not in CapEff are inert. CapBnd is the bounding set inherited from the runtime; refusing on a wide CapBnd would reject Kubernetes pods running under default policy (CapBnd legitimately broader than CapEff there). Minimum surface = minimum noise. If a future incident shows CapPrm/CapBnd matters, a follow-up ticket extends the check with a clear motivating failure — rationale frozen in the `CheckCapabilities` doc comment so the next contributor has it on hand.

## Threat model alignment

`docs/threat-model.md` § Deploy treats operator misconfiguration as the dominant failure class for a single-machine internet-exposed relay. This check joins three siblings in the "refuse-to-boot on operator drift" family:

- `ErrCacheDirInsecure` (#9) — autocert cache dir mode drift
- `ErrInsecureListenInProduction` (#77) — plaintext-in-prod transport drift
- `ErrUnexpectedCapability` (#79) — over-broad capability grant
- *future #78* — uid-0 in production

All four share the same shape: detect at boot, refuse loudly, fail the health check, never serve traffic in the misconfigured state.

## Out of scope (deferred)

- **CapPrm / CapBnd / CapInh checks.** See "Why CapEff only" above; revisit on a motivating failure.
- **Real-`/proc` integration test.** Would couple test outcome to the CI runner's capability configuration (varies by GitHub Actions image vs. distroless). The seam test on the injected reader is the production-relevant assertion.
- **Programmatic export of `capabilityName`.** Package-private until a caller actually needs it; one-line change when one does.

## Cross-links

- [ADR-0009: Build-tag platform-split convention (`_linux.go` / `_other.go`)](../decisions/0009-build-tag-platform-split-convention.md) — convention this ticket established.
- [Codebase note #79](../codebase/79.md) — per-ticket implementation detail.
- [Production-mode contract](production-mode.md) — sibling boot-time refusal (#77).
- [Autocert TLS](autocert-tls.md) — `ErrCacheDirInsecure` is the original boot-time-refusal sentinel (#9).
- [Docker image](docker-image.md) — distroless `:nonroot` (uid 65532) deployment shape that motivates `CAP_NET_BIND_SERVICE` on the allowlist.
