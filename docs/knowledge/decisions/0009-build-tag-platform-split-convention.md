# ADR-0009: Build-tag platform-split convention — `_<goos>.go` / `_other.go`

**Status:** Accepted (#79, 2026-05-13)

## Context

Ticket #79 needed a Linux-only implementation of `CheckCapabilities` (parses `/proc/self/status`) and a no-op fallback on every other GOOS (darwin dev runs, future Windows/BSD CI). Prior to #79 no file in `internal/relay/` or `cmd/` used Go's per-GOOS build tags — `grep -r '//go:build' internal/ cmd/` returned nothing on `main` at 15dc00f. The PO body explicitly delegated the file-naming convention to the architect; locking it in now sets the shape for every future per-GOOS split in this repo.

## Decision

Per-GOOS files in this repo use the pair `<base>_<goos>.go` + `<base>_other.go`:

- `<base>_<goos>.go` — implementation for one specific GOOS. **No build tag.** Go's build system auto-applies a `//go:build <goos>` constraint based on the filename suffix when the suffix matches a recognised GOOS (`_linux.go`, `_darwin.go`, `_windows.go`, `_freebsd.go`, …). This mirrors stdlib (`os/file_unix.go`).
- `<base>_other.go` — implementation for every other GOOS. **Explicit `//go:build !<goos>` tag required** because `other` is not a recognised GOOS suffix, so the build system does not auto-constrain it.

First instance: `caps.go` (cross-platform pure code), `caps_linux.go` (Linux entry point, no tag), `caps_other.go` (`//go:build !linux`). Tests follow the same suffix rules: `caps_test.go` (cross-platform), `caps_linux_test.go` (Linux-only, suffix auto-applies).

## Rationale

**Why not a single file with runtime `runtime.GOOS` switching.** The Linux branch reads `/proc/self/status`; on darwin that path does not exist and the import (`os`, `strings`, `strconv`) would have to handle a syscall that can't happen. Compile-time elimination keeps the darwin binary free of dead /proc-handling code and the Linux binary free of "skip this, we're on darwin" runtime branches. It also keeps darwin builds honest — a future maintainer who breaks the Linux path won't accidentally hide it behind an untaken runtime branch on a darwin dev box.

**Why `_<goos>.go` tag-free, `_other.go` tag-explicit (asymmetric).** Symmetric explicit tags on both halves would work but duplicate information the filename already carries — `_linux.go` with `//go:build linux` is redundant noise. The asymmetry is forced by the language: `_other` is not a GOOS suffix and won't auto-constrain, so the tag has to be explicit there. This is the same shape stdlib uses (`os/file_unix.go` is tag-free; `os/file_plan9.go` is tag-free; the fallback layer `os/file_posix.go` uses an explicit build tag).

**Why `_other.go` rather than `_default.go` or `_stub.go`.** "Other" reads correctly at the call site: a future contributor reading `caps_other.go` understands "this is the non-Linux branch" without needing to map `_default`/`_stub` jargon to "anything not covered by a sibling file." If/when this repo grows a `_freebsd.go` sibling, `_other.go` automatically narrows (still `!linux`, but `_freebsd.go` claims FreeBSD via its own filename) without touching the tag.

**Why establish this as a *convention* and not just decide ad-hoc per ticket.** Capability checks, uid-0 checks (queued #78), and any future "this only makes sense on Linux containers" gate will all face the same split. Standardising on one shape now means code review can assert against a single rule rather than re-litigating the file layout per ticket; it also means cross-linking from CLAUDE.md / spec docs is stable.

## Alternatives considered

- **Symmetric explicit tags on both files** (`//go:build linux` on `caps_linux.go`, `//go:build !linux` on `caps_other.go`). Rejected — redundant with the filename suffix on the Linux half. The implicit constraint is a deliberate Go convention; opting out adds noise without adding clarity.
- **Single file, `runtime.GOOS` branch.** Rejected — see above; couples darwin builds to /proc-shaped code and adds an untaken runtime branch on every Linux invocation.
- **`_unix.go` + `_other.go`.** Rejected — `_unix.go` is recognised by Go as a synthetic build tag (linux/darwin/bsd), but the relay's deployment target is specifically Linux containers, not "any Unix." Conflating Linux with darwin here would mean darwin dev runs hit the `/proc` reader and fail; the dev-run experience would be objectively worse than the no-op log.

## Consequences

- **Going forward:** any per-GOOS implementation in this repo follows `<base>_<goos>.go` + `<base>_other.go`. Tests follow the same suffix rules. The architect spec for any such ticket should name the convention by reference to this ADR instead of re-deriving it.
- **Extension path:** if a second-tier GOOS needs a non-stub implementation (e.g. FreeBSD CI grows a `/compat/linux/proc` reader), add `<base>_freebsd.go` (tag-free, filename suffix auto-constrains); the `_other.go` half auto-narrows to "neither linux nor freebsd" without edits. If a third GOOS joins, the same pattern repeats.
- **Cost paid:** a contributor must remember the asymmetry (`_other.go` needs an explicit tag, `_linux.go` does not). The two new files in #79 carry doc comments that make the asymmetry visible; future PRs land via review on the same lines.
- **What this does NOT change:** the cross-platform pure functions (`parseCapEff`, `checkCapEffMask`, `capabilityName`) live in `caps.go` *without* a build tag — they compile on every GOOS and are tested on every GOOS. The split applies only to the platform-specific entry point.

## Cross-links

- [Capability allowlist (feature)](../features/capability-allowlist.md) — first consumer.
- [Codebase note #79](../codebase/79.md) — per-ticket implementation detail.
- Go reference: [Build constraints — `go/build` docs](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — the `_<goos>.go` / `_<goos>_<goarch>.go` filename suffix rules this ADR rides on.
