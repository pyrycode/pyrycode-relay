# Spec — Structured-log key allowlist (test-time gate) (#36)

## Files to read first

- `internal/relay/client_endpoint.go:55-83` — three log call sites (`phone_register_no_server`, `phone_registered`, `phone_unregistered`); shape of multi-line `key, value` pairs the AST walker must traverse.
- `internal/relay/server_endpoint.go:60-95` — three log call sites (`server_id_conflict`, `server_claimed`, `server_released`); note `server_released` is a single-line call (`"server_id", serverID` inline with msg) — both shapes must work.
- `internal/relay/forward.go:38-72, 110-160` — eight log call sites across two forwarder loops; widest fan-out of keys. Source of the `binary_conn_id` key (distinct from `conn_id`).
- `docs/threat-model.md:46-55` — § Log hygiene. `MUST NOT` / `MAY` lists; the new doc cross-reference attaches at the end of the bullet list.
- `docs/lessons.md` § "Credentials the relay does not validate are presence-checked then discarded — never logged…" — the layered-defence posture this ticket adds the fourth layer to.
- `internal/relay/registry.go` — read enough to confirm it has zero `logger.*` call sites (registry has no logger dep); the test will assert it's silent only by absence of matches, not by an explicit check.
- Go stdlib reminder: `go/parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)` + `ast.Inspect` is the idiomatic walker; we do not need `go/types`.

## Context

`docs/threat-model.md` § Log hygiene lists fields the relay must never log (tokens, payload bodies, full headers/URLs) and fields it may log (server-id, conn-id, device-name, remote host, event types, close codes). Today the rule is enforced by code review and the structural absence of token-bearing code paths (see `docs/lessons.md` "Credentials the relay does not validate…"). One careless PR re-introduces a leak. This ticket adds a fourth defence layer: a Go test that fails when any non-test file in `internal/relay/` contains a `logger.{Info,Warn,Error,Debug}` call carrying a key not present in an in-repo allowlist.

The allowlist becomes the canonical machine-checkable form of the threat-model rule. The doc continues to enumerate intent (what may/must-not appear); the allowlist enumerates the exact key names. Adding a logged key requires editing the allowlist in the same commit — that diff is what reviewers (human + future linters) gate on.

## Design

Three artefacts:

### 1. `internal/relay/log_allowlist.go` (new, ~20 lines)

```go
package relay

// allowedLogKeys is the closed set of structured-log keys the relay is
// permitted to emit on a slog.Logger call. Adding a key here is the
// commit-time gate that pairs with the threat-model rule in
// docs/threat-model.md § Log hygiene. TestLogKeysAreAllowlisted reads
// from this map; never inline new keys at a call site without adding
// the key here in the same commit.
var allowedLogKeys = map[string]struct{}{
    "server_id":      {},
    "conn_id":        {},
    "binary_conn_id": {},
    "device_name":    {},
    "remote":         {},
    "binary_version": {},
    "err":            {},
}
```

- Unexported. Only the test in the same package reads it; no external consumer should depend on this set.
- Lives in a non-test `.go` file (not `_test.go`) so that the threat-model doc can point at a stable, non-fixture path and so `go vet` / IDEs surface it as production code that pairs with the doc.
- Comment line names the doc section and the test, so a developer adding a key sees the contract without leaving the file.

The seven initial entries are the exhaustive set produced by walking the package on `main` as of this ticket — every existing key must already be in the allowlist for the test to pass on the same commit. The ticket's "test passes against current main" AC is enforced by this file's content, not by the test logic.

### 2. `internal/relay/log_keys_test.go` (new, ~120 lines)

Single test, no fixtures, no testdata. Pure stdlib (`go/parser`, `go/ast`, `go/token`, `strconv`, `path/filepath`).

```go
func TestLogKeysAreAllowlisted(t *testing.T) { … }
```

Algorithm:

1. **Discover files.** `filepath.Glob("*.go")` from the test's working directory (which is `internal/relay/` when the test runs). Exclude names ending in `_test.go`. Excluding the test file itself is essential — it contains string literals that look like log keys (e.g. assertion messages).
2. **Parse each file.** `parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)`. A parse error fails the test with the file path.
3. **Walk for log calls.** `ast.Inspect` each file. Match `*ast.CallExpr` where:
    - `Fun` is `*ast.SelectorExpr`, and
    - `Sel.Name` is one of `{"Info", "Warn", "Error", "Debug"}`.
    No type resolution. False positives (a non-logger type with a method named `Info`) are not present in this package and are acceptable noise if introduced later — the violation message will name the file:line so a reviewer can disambiguate.
4. **Extract keys.** For each match, `args := call.Args[1:]` (skip the msg arg). Iterate `args` two at a time. For each pair at even index `i`:
    - `args[i]` is a `*ast.BasicLit` with `Kind == token.STRING` → `strconv.Unquote` to get the key.
    - Otherwise → record a violation: `"%s:%d: non-literal log key argument (dynamic key defeats the allowlist; use a literal string key)"`.
    - Odd-length argument list (a key with no value) → also a violation; same `non-literal-or-malformed` family. Stop walking that call's args.
5. **Compare to allowlist.** Each extracted literal key not in `allowedLogKeys` → violation: `"%s:%d: log key %q is not in allowedLogKeys (see internal/relay/log_allowlist.go and docs/threat-model.md § Log hygiene)"`.
6. **Report.** Accumulate all violations across all files (do not fail at first hit). At end of walk, if violations is non-empty, `t.Errorf` once per violation followed by a final `t.Fatalf("found %d log-hygiene violation(s)", n)`. Failure surface names file path, line, and offending key without `-v`.

Out-of-scope methods, captured as a guard not as full coverage:

- `Logger.With`, `Logger.LogAttrs`, `Logger.Log`, and any `slog.<Attr-constructor>` (`slog.String`, `slog.Any`, `slog.Group`, …) are not in the matched set. None are used in the current codebase.
- The walker emits a separate "unsupported log shape" violation if it encounters a `SelectorExpr.Sel.Name` in `{"With", "LogAttrs", "Log"}` on any receiver inside a non-test file. Message: `"%s:%d: %s.%s used; this method is not gated by TestLogKeysAreAllowlisted. Either convert the call to positional Info/Warn/Error/Debug or extend the walker."` This is the "future contributor switches to `With` and bypasses the gate" hedge — a deterministic loud failure that forces the next contributor to either extend the test or convert the call. Costs ~6 lines; pays its own freight the first time anyone touches log structure.

The walker is local to the test file (a small visitor func over `ast.Inspect` is enough; no need for a separate type or table). Each violation is a `string`; no need for a `Violation` struct.

### 3. `docs/threat-model.md` § Log hygiene — one-line addition

After the existing `MAY be logged` bullet, append a new paragraph:

> **Enforcement:** The canonical set of permitted keys lives in `internal/relay/log_allowlist.go`. `TestLogKeysAreAllowlisted` in `internal/relay/log_keys_test.go` AST-walks every non-test `.go` file in the package on each `make test` run and fails if any `logger.{Info,Warn,Error,Debug}` call carries a key absent from that set or uses a dynamic (non-string-literal) key. Adding a logged key requires editing the allowlist file in the same commit.

This converts the existing "Residual risk: future contributor adds a debug log statement…" sentence from a guarded risk into a partially-covered one. The "Future hardening" entry about a `slog` middleware remains valid — the test catches authoring-time drift; runtime redaction would catch operational drift (a different threat). Do **not** delete the residual-risk or future-hardening paragraphs; they cover the gaps the test does not (runtime, payload-bearing values misrouted to an allowlisted key like `err`).

## Concurrency model

None. The test is sequential, single-goroutine, no I/O outside `os.ReadFile` via `parser.ParseFile`. No timers, no contexts.

## Error handling

- Parse error on any walked file → `t.Fatalf("parse %s: %v", path, err)`. The test cannot proceed if it cannot read the package; this is a hard fail.
- Glob error → same. Glob does not return errors for empty matches; an empty match is itself a `t.Fatalf` (something is wrong if `internal/relay/` has zero non-test `.go` files when the test runs).
- Allowlist violations → accumulated, reported in full, single trailing `t.Fatalf` with count. Rationale: the developer who introduces a misnamed key in one file probably misnames it in three; reporting them all at once shortens the fix loop.
- Non-literal key violations → same accumulation path as allowlist violations; one combined trailing count is fine. Each line is self-explanatory.

## Testing strategy

The test is the test. Two meta-checks:

1. **Self-check on current `main`.** Spec passes when the seven enumerated keys cover every existing call site. Developer verifies by running `go test ./internal/relay/ -run TestLogKeysAreAllowlisted` and observing pass before adding any other change. If pass fails, the allowlist content (not the test logic) is wrong — fix the allowlist, do not relax the matcher.
2. **Negative-path manual check.** Before committing, the developer temporarily edits one call site (e.g. add `"forbidden_key", "x"` to a `logger.Info`), re-runs the test, confirms failure message names the right file:line and key, reverts the edit. Document the manual check in the test's leading doc comment so a future contributor can repeat it; do not commit a permanent negative-path test that would itself be a "logs a forbidden key" pattern.

No table-driven cases. No fixture files. The package's own source is the corpus.

## Open questions

1. **Glob vs. `filepath.WalkDir`.** Spec says `filepath.Glob("*.go")` because the package is flat (no subdirectories in `internal/relay/`). If a future refactor introduces a subpackage under `internal/relay/`, the test will silently miss it. Mitigation deferred to that future ticket; a one-line `WalkDir` swap is cheap. Not worth pre-emptive complexity now.
2. **`slog.SetDefault` / package-level `slog.Info`.** None used today. The walker matches `Sel.Name`, so `slog.Info("msg", "key", v)` (where `Fun` is `slog.Info`) is structurally identical to `logger.Info(...)` and *would* be caught — the receiver name is irrelevant to the AST match. No spec change needed; record here so the developer knows the matcher is broader than its docstring suggests and writes the docstring accurately.
3. **`docs/lessons.md` add-on.** Not in AC. The pattern this ticket establishes — "stochastic spec → deterministic test" — is worth a lesson entry, but per AC it is out of scope for this ticket. The dispatcher's documentation agent (post-merge) handles knowledge-base capture; the architect should not write into `lessons.md` from this spec.

## Security review

See § "Security review" below; appended after PASS.

---

## Security review (security-sensitive ticket)

### Threats considered

1. **Bypass via dynamic key.** A future PR writes `logger.Info("msg", dynamicKey, value)` where `dynamicKey` is a variable carrying token bytes. The allowlist literal-key check would be irrelevant.
    - **Mitigation in spec:** The walker rejects non-`*ast.BasicLit` key arguments with a distinct violation. The error message explicitly names the failure mode ("dynamic key defeats the allowlist"). This converts the attack from "silent bypass" to "build-time failure with a clear remediation."
2. **Bypass via unsupported method.** A future PR converts a call to `logger.With("token", t).Info("msg")` or `logger.LogAttrs(ctx, level, msg, slog.String("token", t))`. The visitor matches only `Info/Warn/Error/Debug`.
    - **Mitigation in spec:** The walker also flags `With`/`LogAttrs`/`Log` selectors as "unsupported log shape" violations. The PR's CI fails; the developer must either convert to positional form (gated) or extend the walker in the same PR (which itself is a security-sensitive change that gets review).
3. **Bypass via attr constructors at the call site.** `logger.Info("msg", slog.String("token", t))` — `slog.String` returns an `slog.Attr`, which `slog.Logger.Info` accepts as a positional arg. The walker treats `slog.String(...)` (a `*ast.CallExpr`, not a `*ast.BasicLit`) as a non-literal key argument and rejects it.
    - **Outcome:** caught by the dynamic-key rule, no extra spec needed. Worth calling out so the developer's docstring lists this case explicitly.
4. **Bypass via `fmt.Sprintf`-built key.** `logger.Info("msg", fmt.Sprintf("token_%s", id), val)` — non-`*ast.BasicLit`, caught by the dynamic-key rule. Same outcome as #3.
5. **Bypass via package-level `slog.Info`.** Walker matches by `Sel.Name`, not receiver type. `slog.Info("msg", "token", t)` is gated identically. Not a bypass — recorded in Open Questions #2 so the developer's docstring is accurate.
6. **Bypass via a *value* argument containing secrets under an allowlisted key.** `logger.Info("msg", "err", fmt.Errorf("bad token %s: …", token))`. The key is `err` (allowlisted); the value carries the secret. The spec does NOT mitigate this — the test only inspects keys, not values, and inspecting values would require dataflow analysis the AST walk cannot do.
    - **Decision:** Out of scope; deliberate. The threat-model doc's `MUST NOT be logged` list addresses this at the human-review layer ("`x-pyrycode-token` values or any header that carries a token"). The "Future hardening" entry (runtime `slog` middleware that scans Attr values) remains the right place to land this — runtime is the only layer that can see formatted strings. The spec's threat-model edit must NOT claim the test covers value-side leaks. (Confirmed: the proposed enforcement paragraph says "carries a key absent from that set" — keys only, not values.)
7. **Allowlist tampering.** A malicious PR adds `"token"` to `allowedLogKeys` to unblock a key that should never be allowed.
    - **Mitigation:** Code review. The allowlist file is one of the highest-leverage review targets in the repo; the doc paragraph explicitly names it as the canonical enforcement point so reviewers know to scrutinise diffs to it. Not a structural defence; that is acceptable for a contributor-trust threat.
8. **Test disabled / skipped.** A PR adds `t.Skip()` or builds without the `internal/relay` package's tests.
    - **Mitigation:** `make test` runs the whole module; the test cannot be locally skipped without an explicit `t.Skip` that shows up in diff. Not a structural defence; same trust model as #7.
9. **Cross-package leakage.** A future log call site appears in `cmd/pyrycode-relay/main.go` or another package; the test walks only `internal/relay/`.
    - **Decision:** Acknowledged in the ticket's "Out of Scope" block. `cmd/pyrycode-relay/main.go` has handler construction only, no `logger.{Info,Warn,Error,Debug}` calls today. If a log call lands there, the architect of that ticket extends the test (or carbons it). Not a current threat.
10. **Parse failure masks coverage.** A `.go` file with a syntax error → `t.Fatalf`. Could a tampering PR introduce an intentional parse error in a file with a forbidden log call?
    - The Go compiler would refuse to build the package; `make build` fails before `make test` runs. Not a viable bypass.

### Trust boundaries

- **Input to the test:** the package's own source files. Not adversary-controlled at test time — the corpus is the committed code.
- **Input to the production code (the relay):** unchanged by this ticket. No new attack surface introduced.
- **Allowlist file:** trust-boundary equivalent to the threat-model doc itself — a privileged config that code review owns.

### Verdict

**PASS.** The spec mitigates the two viable bypass classes (dynamic key, alternate method) with deterministic structural checks. The remaining bypasses (value-side leaks, tampering, off-package logs) are explicitly acknowledged in scope, mitigated at the threat-model doc / code-review layer, and aligned with the "Future hardening: runtime redaction middleware" entry that this ticket does not displace.
