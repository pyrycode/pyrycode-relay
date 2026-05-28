# Spec: tighten cert cache dir to 0700 when owned by relay's runtime uid

Ticket: [#95](https://github.com/pyrycode/pyrycode-relay/issues/95). Size S. Narrow exception to the "loud failure over silent correction" rule for the single substrate-handed-us-a-fresh-volume case.

## Files to read first

- `internal/relay/tls.go` (whole file, 88 lines) — the only place to change in production code. Lines 29–56 hold `NewAutocertManager`; lines 42–48 are the existing rejection branch this spec narrows. Lines 15–19 declare `ErrCacheDirInsecure` and define its contract ("TLS private keys live there; the relay refuses to start … rather than silently weakening the deployment") — this spec narrows that contract, not abolishes it.
- `internal/relay/tls_test.go:72-87` — the existing `TestNewAutocertManager_ExistingInsecureDirRejected` test that today exercises a 0755 dir owned by the test user. Its semantic role flips under this spec: the same setup now exercises the *tightening* path (AC1), not the rejection path. The current test is replaced, not extended.
- `internal/relay/tls_test.go` (whole file, 211 lines) — the package-internal testing convention (`package relay`, `errors.Is` against unexported sentinels, `t.Parallel`, `runtime.GOOS != "windows"` perm gates) every new test must follow.
- `internal/relay/caps_linux.go` and `internal/relay/caps_other.go` — the canonical `_linux.go` / `_other.go` platform-split pair this spec replicates. Read the file headers (build tag wiring, exported-function signature symmetry, doc-comment shape) and copy that structure verbatim for `owner_linux.go` / `owner_other.go`.
- `docs/knowledge/decisions/0009-build-tag-platform-split-convention.md` — the binding ADR for this repo's per-GOOS file layout. The pair `<base>_<goos>.go` + `<base>_other.go` is mandatory; do not reach for `_unix.go`, do not collapse to a single file with a `runtime.GOOS` branch.
- `cmd/pyrycode-relay/main.go:69-70` — establishes that `slog.SetDefault(logger)` is called at boot, so any in-package use of `slog.Default()` for an INFO log will inherit the production text handler without a constructor-signature change. Lines 257–261 are the single production call site of `NewAutocertManager` — no edits needed; the signature does not change.
- `docs/specs/architecture/9-autocert-tls.md` — original `ErrCacheDirInsecure` rationale and the wider autocert wiring. The "fail loud before any listener starts" framing this spec narrows.
- `docs/PROJECT-MEMORY.md` § "Project-level conventions" — the `Loud failure over silent correction` rule explicitly names `ErrCacheDirInsecure` as the canonical instance. This spec is a deliberate, scoped exception (ownership-gated). Do not generalise the exception; do not touch the project-memory file (it is read-only for agents per the file header).
- Ticket body — the precise statement of AC1–AC4. AC4 (real-Fly verification) is operator-side; the developer ships the code change. The current `fly.toml:27` already uses the mount root (no subdir workaround landed on `main`), so this spec does not modify `fly.toml`.

## Context

The relay refuses to start when its autocert cache directory exists with permissions broader than 0700 — TLS private keys live there. This is correct in principle and was deliberate (ADR-precedent #9, also cited as the canonical "loud failure" example in `docs/PROJECT-MEMORY.md`).

Fly.io's volume mount sets the destination to 0755 on every machine boot (operator-observed: `INFO Mounting /dev/vdc at /var/lib/relay/autocert w/ uid: 65532, gid: 65532 and chmod 0755`). `fly.toml`'s `[[mounts]]` block has no `mode` field. Result: every first-deploy and every restart after a volume detach/reattach trips `ErrCacheDirInsecure`. The current workaround — passing a subdir of the mount as `--cert-cache` so `os.MkdirAll` creates the leaf at 0700 — works but encodes substrate quirks into the deploy manifest with no co-located explanation.

The narrowing this spec specifies: when the broader-than-0700 dir is owned by the relay's own runtime uid, the relay tightens it to 0700 in-place (silent correction, one INFO log line) and continues startup. When it is owned by *any other* uid, the existing loud-rejection path is preserved untouched — that case still represents a structurally suspicious state (someone else laid TLS-key storage at the cache path) and must continue to fail closed.

The trust model that justifies the narrowing: a foreign-uid 0755 dir at the cache path means an attacker (or a misconfigured runtime) has staged a directory the relay is about to write TLS private keys into, and the directory is group/world-readable before the relay can even touch it. Bytes written under that dir would be readable by whatever group/world the owner placed there at creation time, and the foreign owner retains arbitrary write access (rename, swap files, observe). A same-uid 0755 dir means the kernel (via the substrate's mount step) handed the relay's own process a freshly mounted directory at standard volume-perms, and the relay can immediately self-correct before opening a single file under it.

Residual-safety dependency: `golang.org/x/crypto/acme/autocert`'s `DirCache` writes the per-domain cert + account-key files at file mode 0600. The narrowing here only relaxes the *directory* perm check; file perms inside the dir are independently set by autocert. A 0755 dir before the tightening chmod is still safe in practice because (a) on a fresh Fly mount the dir is empty, and (b) any pre-existing files (from a prior relay run on the same volume) were written by autocert at 0600. If autocert's file-perm contract ever changed, this assumption would silently break and the chmod alone would not be a complete defence — the relay would need to walk the dir and re-tighten file perms too. Worth noting in the spec so a future reader does not assume the dir-perm tightening is the entire safety story.

This is a single, narrow concession to substrate realities — not a general relaxation of the "loud failure" rule. The wording of `ErrCacheDirInsecure`'s godoc is updated to reflect the narrowed contract; the sentinel itself, its message text, and its `errors.Is` identity are preserved (one production caller — `cmd/pyrycode-relay/main.go:257-261` — and no external consumers).

## Design

Three changes:

1. Modify `internal/relay/tls.go` — narrow the rejection branch in `NewAutocertManager` to a uid-gated split: same-uid → chmod + INFO log + proceed; different-uid (or owner-unknown) → unchanged rejection.
2. Add `internal/relay/owner_linux.go` — Linux-only ownership probe via `syscall.Stat_t.Uid`. Filename suffix auto-applies the build tag per ADR-0009; no explicit `//go:build` line.
3. Add `internal/relay/owner_other.go` — non-Linux stub returning "owner unknown". Explicit `//go:build !linux` per ADR-0009.

No signature changes to `NewAutocertManager`. No edits to `cmd/pyrycode-relay/main.go`. No edits to `fly.toml`. No new exported symbols outside the package (the helper is unexported).

### File layout

| Path | Purpose | Build tag |
|---|---|---|
| `internal/relay/tls.go` | Existing — modify `NewAutocertManager` rejection branch only. Doc comment on `ErrCacheDirInsecure` updated to reflect the narrowed contract. | none |
| `internal/relay/owner_linux.go` | New — `fileOwnerUID(info os.FileInfo) (uint32, bool)` reading `syscall.Stat_t.Uid` | `_linux.go` suffix (auto-applied) |
| `internal/relay/owner_other.go` | New — `fileOwnerUID(_ os.FileInfo) (uint32, bool)` returning `(0, false)` | explicit `//go:build !linux` |
| `internal/relay/tls_test.go` | Existing — replace one test, add three; no other edits. | none |

The convention follows ADR-0009 verbatim. The Unix-shared `syscall.Stat_t.Uid` field would technically also work on darwin/BSD, but the ADR is binding: the production deployment target is Linux containers, and darwin dev runs accept the `_other.go` stub (which keeps the *existing* loud-rejection behaviour in place — strict superset of pre-#95 darwin behaviour, no regression).

### `internal/relay/owner_linux.go` (new, ~15 LOC)

```go
package relay

import (
	"os"
	"syscall"
)

// fileOwnerUID returns the numeric UID of the file's owner from a stat
// result, if discoverable. Linux-only impl reads syscall.Stat_t.Uid;
// see owner_other.go for the non-Linux fallback. ok=false means "the
// platform did not return a recognisable stat shape" — callers must
// treat that as "ownership unknown, do not silently relax checks".
func fileOwnerUID(info os.FileInfo) (uint32, bool) { /* type-assert info.Sys().(*syscall.Stat_t); return Uid, true */ }
```

The body is two lines: type-assert `info.Sys()` to `*syscall.Stat_t`, return `(st.Uid, true)` on success and `(0, false)` if the assertion fails (defensive against future stdlib changes; in practice this branch is unreachable on linux/amd64 and linux/arm64).

### `internal/relay/owner_other.go` (new, ~10 LOC)

```go
//go:build !linux

package relay

import "os"

// fileOwnerUID is unsupported on non-Linux platforms. Returning
// ok=false routes callers into their existing "ownership unknown"
// branch — for NewAutocertManager, that is the unchanged
// ErrCacheDirInsecure rejection path.
func fileOwnerUID(_ os.FileInfo) (uint32, bool) { return 0, false }
```

### Modified `NewAutocertManager` (in `internal/relay/tls.go`)

The existing `default:` branch (lines 42–48) currently rejects any broader-than-0700 perm. Narrow it: keep the `IsDir` check first; on the perm-too-broad condition, ask `fileOwnerUID` who owns the dir. If owner equals `os.Geteuid()`, chmod 0700, log INFO, fall through. Otherwise (different uid, or ownership unknown), return `ErrCacheDirInsecure` exactly as today.

Contract sketch (signature unchanged, body delta only):

```go
default:
    if !info.IsDir() { /* unchanged: return non-dir error */ }
    if mode := info.Mode().Perm(); mode&0o077 != 0 {
        // Ownership-gated narrow exception. Same-uid case: substrate
        // (Fly volume mount) handed our process a fresh dir at 0755;
        // we self-correct in place. Different/unknown owner: fall
        // through to the existing loud-rejection contract.
        if ownerUID, ok := fileOwnerUID(info); ok && ownerUID == uint32(os.Geteuid()) {
            // chmod 0700, INFO log, proceed
        } else {
            return nil, fmt.Errorf("%w: %s (mode %o)", ErrCacheDirInsecure, cacheDir, mode)
        }
    }
```

INFO log contract: `slog.Default().Info("tightened cert cache dir perms", "path", cacheDir, "from", fmt.Sprintf("%#o", mode), "to", "0700")`. The exact key names are part of the contract (operators may grep on them); the message string is `tightened cert cache dir perms`. Emit the log *after* the `os.Chmod` returns nil (so a failed chmod does not produce a misleading "tightened" line). If `os.Chmod` fails, return `fmt.Errorf("relay: tightening cert cache dir %s: %w", cacheDir, err)` (new, unwrapped error — the dir is in an undefined perm state and starting up would be wrong).

`os.Geteuid()` returns `int`; the cast to `uint32` is intentional and safe for any conceivable Linux uid (the syscall ABI itself uses 32-bit uids; the stdlib's `int` return is a portable convenience).

### `ErrCacheDirInsecure` doc-comment update (in `internal/relay/tls.go:15-19`)

Replace the existing doc with one that reflects the narrowed contract — the sentinel still fires, but only in the structurally suspicious case (foreign-owned cache dir with broader-than-0700 perms). The wire identity of the sentinel (`errors.Is` matching, the formatted message that includes path + mode) is unchanged. The godoc should explicitly call out the narrow same-uid exception so a future reader does not assume the loud-rejection rule is unconditional.

### Concurrency model

`NewAutocertManager` runs synchronously during boot, before any goroutine has been spawned and before any listener accepts a connection. No concurrency considerations. `os.Stat` / `os.Chmod` are single-syscall and atomic at the kernel level for this use case (no TOCTTOU window matters: the bootstrap is single-threaded, and even if it weren't, the foreign-uid case still goes to rejection regardless of any race).

### Error handling

| Scenario | Behaviour |
|---|---|
| Dir does not exist | Unchanged: `MkdirAll` at 0700, proceed. |
| `os.Stat` returns a non-`ErrNotExist` error | Unchanged: wrapped error returned. |
| Path is not a directory | Unchanged: non-dir error returned. |
| Dir exists, mode 0700 | Unchanged: proceed (no chmod attempted). |
| Dir exists, mode broader than 0700, owned by `os.Geteuid()` (Linux only) | **New:** `os.Chmod(cacheDir, 0o700)` then proceed. INFO log emitted on success. If chmod itself fails, return a new wrapped error (no sentinel). |
| Dir exists, mode broader than 0700, owned by foreign uid (Linux) | Unchanged: `ErrCacheDirInsecure` (wrapped with path + mode). |
| Dir exists, mode broader than 0700, ownership unknown (non-Linux) | Unchanged: `ErrCacheDirInsecure`. |

### Testing strategy

Test functions to keep unchanged: `TestNewAutocertManager_CreatesCacheDirWith0700`, `TestNewAutocertManager_ExistingSecureDirIsNoOp`, `TestNewAutocertManager_CacheDirPathNotADirectory`, `TestNewAutocertManager_RequiresDomainAndCacheDir`, `TestNewAutocertManager_HostPolicyAcceptsConfiguredDomain`, `TestNewAutocertManager_HostPolicyRejectsOtherDomains`, `TestEnforceHost`, `TestTLSConfig_PinsMinVersionToTLS12`.

Replace `TestNewAutocertManager_ExistingInsecureDirRejected` (lines 72–87) with three new tests:

- **`TestNewAutocertManager_OwnedInsecureDirIsTightened`** (Linux-only — `if runtime.GOOS != "linux" { t.Skip("ownership-gated tightening per ADR-0009 only enabled on linux") }`). Setup: `os.MkdirAll(cache, 0o755)` inside `t.TempDir()`; that dir is owned by the test process uid. Expect: `NewAutocertManager` returns nil error; post-call `os.Stat(cache).Mode().Perm() == 0o700`. Exercises AC1.
- **`TestNewAutocertManager_ForeignOwnedInsecureDirRejected`** (Linux + root only — `if runtime.GOOS != "linux" || os.Geteuid() != 0 { t.Skip("requires root to chown to a foreign uid") }`). Setup: 0755 dir in `t.TempDir()`, then `os.Chown(cache, 65534, 65534)` (nobody). Expect: `errors.Is(err, ErrCacheDirInsecure)`; post-call the dir's mode is *unchanged* (the chmod must not have run). Exercises AC2.
- **`TestNewAutocertManager_InsecureDirRejectedOnNonLinux`** (skip on Linux — `if runtime.GOOS == "linux" || runtime.GOOS == "windows" { t.Skip(...) }`). Setup: 0755 dir in `t.TempDir()`. Expect: `errors.Is(err, ErrCacheDirInsecure)`. Locks in that the `owner_other.go` stub keeps the pre-#95 behaviour intact on darwin dev runs — strict superset, no regression. Skips on Windows for the same reason the existing tests skip there (perm bits don't apply).

The existing `TestNewAutocertManager_CacheDirPathNotADirectory` already covers the non-dir rejection path; no edits. AC3's "symlink at cache path" is implicitly covered by the existing code's use of `os.Stat` (follows symlinks): a symlink to a 0700 dir owned by the test uid proceeds; a symlink to a 0755 dir owned by the test uid would now follow the new tightening path (acceptable — the chmod operates on whatever the symlink resolves to, and `os.Chmod` follows symlinks the same way). No new symlink-specific test is required; the AC's "existing rejection paths unchanged" is satisfied because we did not add or remove any symlink-handling logic. Document this stance in the spec but do not write a symlink test (over-specification for behaviour that has not changed).

Test assertions on the INFO log message are not required. The log line is observability surface, not a behavioural contract under test; a developer who wants to assert on it may wire a `slog.New(slog.NewJSONHandler(&buf, nil))` via `slog.SetDefault` in the test, but this spec does not mandate it.

### Open questions

- **None.** The same-uid vs. foreign-uid trust split is the design's core invariant and is settled in Context. The platform-split file layout is mandated by ADR-0009. The log-line key names are part of the contract above. The signature of `NewAutocertManager` is preserved. `fly.toml` is already at the correct state on `main` and does not need modification.

## Acceptance criteria — coverage map

| AC | Where covered |
|---|---|
| AC1 — same-uid 0755 dir → tightened to 0700, no error, relay continues | Production: tls.go chmod branch. Test: `TestNewAutocertManager_OwnedInsecureDirIsTightened`. |
| AC2 — foreign-uid 0755 dir → `ErrCacheDirInsecure` preserved | Production: tls.go else-branch unchanged. Test: `TestNewAutocertManager_ForeignOwnedInsecureDirRejected` (root-gated). |
| AC3 — symlink or non-dir at cache path → existing rejection paths unchanged | Production: no edits to `IsDir` check or `os.Stat` behaviour. Test: existing `TestNewAutocertManager_CacheDirPathNotADirectory` unchanged. Spec documents the symlink stance under Testing strategy. |
| AC4 — real-Fly deploy with mount-root cache dir starts cleanly | Operator-verified post-merge. Code change above is the enabling delta. `fly.toml:27` already uses the mount root on `main`; no `fly.toml` edit required in this PR. |

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] SHOULD FIX (low priority) — narrowed contract is justified by the same-uid trust invariant; foreign-uid case is preserved untouched. Residual safety depends on autocert's `DirCache` writing per-domain cert + account-key files at 0600 (the *directory* tightening alone does not re-perm files inside). Addressed inline in the new "Residual-safety dependency" paragraph in Context; no code-level change required because autocert's 0600 invariant has been stable since the relay adopted autocert in #9.
- [Tokens] No findings — INFO log carries only `path`, `from`, `to` (operator-known path + non-secret mode bits). chmod-failure error carries the same path; no secrets exposed.
- [File operations] SHOULD FIX (deferred) — there is a TOCTOU window between `os.Stat` and `os.Chmod` on a path the operator controls. Structurally infeasible on the Fly substrate (the parent `/var/lib/relay/` is owned by the relay's runtime uid; no other writer can race the swap). The pre-#95 code has the same `os.Stat` → `return error` shape and the same race profile, so this spec does not regress the threat surface. Hardening to an `os.OpenFile(... O_NOFOLLOW|O_DIRECTORY ...)` + `f.Chmod` form is left to a future ticket if/when an observed exploit warrants it; until then the deterministic-code defence (foreign-uid → reject) is the load-bearing control.

  Symlink at `--cert-cache` is operator-controlled input, not adversarial; AC3 preserves existing `os.Stat`-follows-symlinks behaviour. No new finding.
- [Subprocess] N/A — no subprocess execution.
- [Crypto] N/A — no cryptographic primitives introduced.
- [Network & I/O] N/A — `NewAutocertManager`'s signature and the downstream `autocert.Manager` wiring are unchanged.
- [Error messages, logs, telemetry] No findings — the INFO log line and the chmod-failure error both expose only operator-known values (path + mode bits). The existing `ErrCacheDirInsecure` wrap preserves its current shape.
- [Concurrency] No findings — `NewAutocertManager` runs single-threaded during boot, before any goroutine spawns or any listener accepts.
- [Threat model alignment] No findings — the same-uid trust invariant matches the Fly substrate's mount behaviour (uid 65532 = relay runtime uid). Non-Fly operator workflows (`mkdir; chown; chmod 0755`) get the same benign tightening. Foreign-uid case continues to fail closed.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-28

