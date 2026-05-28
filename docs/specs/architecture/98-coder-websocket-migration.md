# Spec: migrate `nhooyr.io/websocket` → `github.com/coder/websocket`

Ticket: [#98](https://github.com/pyrycode/pyrycode-relay/issues/98). Size **S** (PO-authorized atomic — see ticket body § *Sizing Override*). Library-identity swap; no behavioural change in frame handling, close codes, or upgrade flow.

## Files to read first

- `go.mod:8` — current dependency line: `nhooyr.io/websocket v1.8.17`. Replace with `github.com/coder/websocket` at the latest released version. `go mod tidy` rewrites `go.sum`.
- `internal/relay/ws_conn.go` (whole file, 133 lines) — the entire adapter. The full API surface this migration depends on: `websocket.Conn`, `websocket.StatusCode`, `websocket.StatusNormalClosure`, `websocket.MessageBinary`, `Conn.Write(ctx, MessageBinary, msg)`, `Conn.Read(ctx)`, `Conn.Close(code, reason)`, `Conn.Ping(ctx)`, `Conn.SetReadLimit(n)`. Used in `SetReadLimit` at line 54 and every `w.conn.*` call.
- `internal/relay/heartbeat.go:54` — uses `websocket.StatusInternalError` for the 1011 "heartbeat timeout" close. Identifier name unchanged in coder.
- `internal/relay/shutdown.go:19` (the `gracefulCloser` interface), `:63` (`websocket.StatusGoingAway`) — `StatusCode` flows through this interface; identical type name in coder.
- `internal/relay/server_endpoint.go:57-63, 76` — `websocket.Accept` with `AcceptOptions{OriginPatterns: []string{"*"}}` and direct `c.Close(websocket.StatusCode(4409), …)` on the stillborn-WSConn path. Stillborn close-code pattern (ADR-0005) crosses library identities unchanged.
- `internal/relay/client_endpoint.go:57-62, 72, 80` — same shape: `websocket.Accept` + stillborn `c.Close(websocket.StatusCode(4404|4429), …)`.
- `internal/relay/heartbeat_test.go:150-156` — `errors.As(err, &ce)` against `websocket.CloseError`, then asserts `ce.Code == websocket.StatusInternalError`. The `CloseError` type-name and field shape must match in coder; verify before running the test.
- `cmd/pyrycode-relay/main.go:26, 232` — two doc-comment references to "nhooyr.io/websocket" (no `import` statement). Comments must be updated for accuracy; behaviour identical.
- `docs/lessons.md:59` (Close-safe-with-in-flight-Write), `:77` (5s close-handshake grace), `:85` (32 MiB default per-frame read limit) — three documented upstream properties the relay relies on. Confirm each survives the migration; record verdict in the new ADR's *Consequences* section. Lessons themselves are frozen (2026-05-11) and not edited.
- `docs/knowledge/decisions/0004-ws-library-and-adapter-context-strategy.md` (whole file, 54 lines) — the ADR being superseded. Its *Decision* paragraph names `nhooyr.io/websocket (v1.8.x)`; ADR-0010 names `github.com/coder/websocket`. ADR-0004 is **not** deleted — set its status to `Superseded by ADR-0010` and leave the body intact (this matches existing ADR conventions; see #79 if it ever supersedes #78).
- `docs/knowledge/features/ws-conn-adapter.md` (whole file, 117 lines) — pointer updates: every textual reference to `nhooyr.io/websocket` becomes `github.com/coder/websocket`. The "Dependency" section (~109-111) names the library by its module path and notes the vanity-path history; rewrite that paragraph to reflect the swap.
- `docs/knowledge/INDEX.md` — append a one-line pointer for ADR-0010 in the *Decisions* section, newest-on-top per the file header convention.

## Context

`nhooyr.io/websocket` (v1.8.x) was deprecated in favour of `github.com/coder/websocket` — Coder forked it to continue maintenance under a non-vanity import path. The library was not redesigned: the README, the public types, the constants, the method signatures, and the documented Close/Write/Read behaviour are preserved across the fork point. Migration is a mechanical import-path swap.

Why now: the QA agent stage on `pyrycode-relay-agents` (PR #4) runs `make check` as its mechanical gate. The plan (separate downstream ticket) is to add `staticcheck` to `make check` to match canonical pyrycode's QA gate. Doing so today would make every PR's QA run red-fail on the pre-existing SA1019 deprecation errors against `nhooyr.io/websocket`; QA's classifier treats staticcheck failures (no `--- FAIL:` line) as "infra failure" → operator triage on every PR. Migrating off the deprecated path now is a prerequisite for the staticcheck-in-QA work that follows.

Two pieces of existing project guidance contradict the change and must be explicitly superseded:

- `docs/lessons.md:63` says "do not opportunistically switch to the coder fork without an ADR." That lesson was written when the vanity path was actively published; upstream deprecation is the trigger the lesson reserves judgement for. ADR-0010 (this spec) is the ADR the lesson asked for; the lesson itself is frozen and stays as historical context.
- ADR-0004's *Consequences* paragraph names `nhooyr.io/websocket` and says "do not switch to `github.com/coder/websocket` opportunistically." ADR-0010 supersedes that constraint by recording the upstream-deprecation trigger.

Out of scope: any refactor to the adapter (no API change to `WSConn`); adding `staticcheck` to `make check` (separate follow-up); changing `writeTimeout`, the 256 KiB read-limit constant, or the heartbeat constants; any change to close-code semantics or the upgrade-handler shape.

## Design

The migration is one logical change applied across 13 `.go` files plus `go.mod`/`go.sum` plus three docs. There is no new code, no new type, no behavioural change. The Design section below documents the verification work the developer must perform — not new design.

### Library-identity swap

Every Go file that today writes `"nhooyr.io/websocket"` in its import block must instead write `"github.com/coder/websocket"`. The import alias stays `websocket` (default — the module's package name is `websocket` in both). The qualifier in every call site (`websocket.Accept`, `websocket.Dial`, `websocket.StatusCode`, `websocket.StatusNormalClosure`, `websocket.StatusGoingAway`, `websocket.StatusInternalError`, `websocket.MessageBinary`, `websocket.AcceptOptions`, `websocket.DialOptions`, `websocket.CloseError`) is unchanged because the imported package name is identical.

`go.mod` swaps the dependency line; `go mod tidy` rewrites `go.sum`. No `replace` directive; pull the latest released `github.com/coder/websocket` version.

The two doc-comment references in `cmd/pyrycode-relay/main.go` (lines 26 and 232) update the library name in the comment text; they do not gate compilation but they document behavioural assumptions and must stay accurate.

### Pre-swap API-surface verification

Before swapping a single import, the developer reads the relevant pages of the coder/websocket godoc and confirms each symbol below is present and signature-compatible with the nhooyr.io/websocket v1.8.x usage in this repo. Any divergence the developer finds triggers the failure-mode trip wire in the ticket body's § *Sizing Override*: halt, route back to PO via `needs-rework:po`. The list — pinned against current usage so the verification is one read, not a discovery loop:

| Symbol | Used at | What to confirm |
|---|---|---|
| `websocket.Accept(w, r, *AcceptOptions) (*Conn, error)` | `server_endpoint.go:57`, `client_endpoint.go:57`, four `_test.go` upgrade fixtures | Signature and `AcceptOptions.OriginPatterns` field present. |
| `websocket.Dial(ctx, url, *DialOptions) (*Conn, *http.Response, error)` | `ws_conn_test.go:51`, `shutdown_test.go:255`, `heartbeat_test.go:66`, `metrics_upgrade_test.go:97`, `client_endpoint_test.go:44`, `server_endpoint_test.go:34`, `main_e2e_test.go:45` | Signature unchanged; `DialOptions.HTTPHeader` field present. |
| `websocket.StatusCode` (named int type) | `ws_conn.go:115`, `heartbeat.go:54`, `shutdown.go:19,63`, `shutdown_test.go:26,38`, `server_endpoint.go:76`, `client_endpoint.go:72,80` | Same name, same underlying kind. The package-private numeric value of each constant is the wire close-code (1000, 1001, 1009, 1011) — verify by spot-check, not just by symbol name. |
| `StatusNormalClosure` (1000), `StatusGoingAway` (1001), `StatusInternalError` (1011), `StatusMessageTooBig` (1009) | adapter + shutdown + heartbeat + tests | Exact numeric value match (RFC 6455-defined; both libraries are RFC-conformant). |
| `websocket.MessageBinary` (`MessageType`) | `ws_conn.go:81`, `ws_conn_test.go:42` | Present; identical semantics. |
| `websocket.CloseError` (struct with `Code StatusCode; Reason string`) | `heartbeat_test.go:150-159`, `shutdown_test.go` patterns, `metrics_upgrade_test.go` | `errors.As(err, &ce)` works against it; field names are `Code` and `Reason`. |
| `(*Conn).Read(ctx) (MessageType, []byte, error)` | `ws_conn.go:92`, every test that reads | Signature unchanged. |
| `(*Conn).Write(ctx, MessageType, []byte) error` | `ws_conn.go:81`, `ws_conn_test.go:42` | Signature unchanged. |
| `(*Conn).Close(code StatusCode, reason string) error` | `ws_conn.go:118`, all stillborn-WSConn paths | Signature unchanged. The behavioural property (5s close-handshake grace) is verified separately — see *Behavioural properties* below. |
| `(*Conn).Ping(ctx) error` | `ws_conn.go:132` | Signature unchanged. |
| `(*Conn).SetReadLimit(n int64)` | `ws_conn.go:54` | Signature unchanged; default cap value (32 MiB) is a behavioural property — see below. |
| `(*Conn).CloseRead(ctx) context.Context` | `heartbeat_test.go:52,107`, `ws_conn_test.go` indirectly | Signature unchanged; documented as the way to keep auto-pong machinery running on a write-only side. |

If every row checks out, the swap is mechanical. If any row diverges, halt.

### Behavioural properties to verify (lessons 59 / 77 / 85)

Three properties of `nhooyr.io/websocket` are load-bearing in relay code today. The developer confirms each survives the swap **before** running the test suite; the verdicts go into the new ADR's *Consequences* section (one bullet each).

1. **Lesson 59 — `Close` is safe with an in-flight `Write`.** `WSConn.CloseWithCode` cancels `closeCtx` and then calls `conn.Close` without first taking `writeMu`. The whole non-deadlock design depends on the library allowing this. Verification: read the coder/websocket godoc for `(*Conn).Close` — the documented property is that `Close` may be called concurrently with `Write` and aborts the in-flight `Write`. The `-race` test `TestWSConn_ConcurrentSend_ProducesIntactFrames` plus the existing `TestShutdown_CloseIdempotentOnRealWSConn` are the runtime cross-check; both pass today on nhooyr and must pass on coder.
2. **Lesson 77 — 5-second close-handshake grace gates goroutine exit.** `conn.Close(code, reason)` writes the close frame promptly (5s write deadline) and then **waits up to 5s for the peer's reciprocal close** before tearing down the TCP. Tests rely on this: `TestHeartbeat_UnresponsivePeer_TriggersClose` (heartbeat_test.go:128-169) sleeps 7 seconds waiting for `done` to close — that 7s budget is the 5s handshake grace plus margin. If coder shortens the grace or removes it, the test still passes (faster goroutine exit) but `drainDeadline = 10 * time.Second` in `cmd/pyrycode-relay/main.go:31` was sized against 5s + headroom; if coder *lengthens* the grace, the test margin tightens. Verification: godoc check; runtime is the test pass.
3. **Lesson 85 — default per-frame read cap is `32 MiB` until `SetReadLimit` is called.** `NewWSConn` calls `SetReadLimit(maxFrameBytes)` (256 KiB in production) before returning, so the default never reaches a relay-routed frame on the production path. The default still matters for the `httptest` server-side conns inside tests (e.g. `startEcho`, `startHeartbeatPair`) where no `SetReadLimit` is applied — those rely on the 32 MiB default being large enough that no test frame trips it. Verification: godoc check; runtime is the test pass.

If any of the three properties diverge in the coder fork, that is *not* a "halt and route back to PO" situation by default — it goes into the new ADR's *Consequences* section as a documented divergence. The trip-wire (halt, `needs-rework:po`) is for *API* divergence that forces call-site rewrites in >2 files; a behavioural divergence that the tests catch is in-scope for this ticket's verification.

### File-level migration (production)

| File | Edit |
|---|---|
| `go.mod` | Replace `nhooyr.io/websocket v1.8.17` with `github.com/coder/websocket vX.Y.Z` (latest released). |
| `go.sum` | Rewritten by `go mod tidy`. |
| `internal/relay/ws_conn.go` | Import-path swap (line 8). Doc-comment at line 16 ("adapts a `*websocket.Conn` from `nhooyr.io/websocket`") and line 124 ("`nhooyr.io/websocket` serialises control frames…") update the library name to `github.com/coder/websocket`. |
| `internal/relay/heartbeat.go` | Import-path swap (line 7). No other change. |
| `internal/relay/shutdown.go` | Import-path swap (line 10). No other change. |
| `internal/relay/server_endpoint.go` | Import-path swap (line 22). Comment at line 61 ("nhooyr.io/websocket has already written a 4xx response") updates the library name. |
| `internal/relay/client_endpoint.go` | Import-path swap (line 21). No other comment references. |
| `cmd/pyrycode-relay/main.go` | Comments only — line 26 (`nhooyr.io/websocket.Conn.Close`) and line 232 (`nhooyr.io/websocket.Accept hijacks…`) update to the coder/websocket name. **No import change** — `main.go` does not import the WS library directly. |

### File-level migration (tests)

The seven test files in the grep output use the WS library directly for client-side dial / accept patterns and for type assertions (`websocket.CloseError`). Each gets the same one-line import swap; no test logic changes.

| File | Edit |
|---|---|
| `internal/relay/ws_conn_test.go` | Import swap (line 14). |
| `internal/relay/server_endpoint_test.go` | Import swap (line 15). |
| `internal/relay/client_endpoint_test.go` | Import swap (line 15). |
| `internal/relay/shutdown_test.go` | Import swap (line 15). |
| `internal/relay/heartbeat_test.go` | Import swap (line 12). |
| `internal/relay/metrics_upgrade_test.go` | Import swap (line 14). |
| `cmd/pyrycode-relay/main_e2e_test.go` | Import swap (line 12). |

If any test file references library symbols not on the *Pre-swap API-surface verification* table above, that is a discovery that invalidates the "uniformly mechanical" premise — halt and route back to PO per the ticket body's trip-wire.

### ADR-0010 — supersedes ADR-0004

Path: `docs/knowledge/decisions/0010-coder-websocket-migration.md`.

Shape (mirrors existing ADRs):

- **Status:** `Accepted (#98)`. Below the status line: `Supersedes: ADR-0004`. ADR-0004's own *Status* line is updated to `Superseded by ADR-0010 (#98)` — body of ADR-0004 left intact (historical context for *why* nhooyr was chosen in the first place).
- **Date:** the merge date (developer fills in).
- **Context:** upstream `nhooyr.io/websocket` was deprecated; canonical home moved to `github.com/coder/websocket`. ADR-0004 explicitly named this as the trigger condition for revisiting the choice ("revisit when v2 (or the coder fork) introduces a meaningful behavioural change" — the meaningful change here is the *deprecation of the original*, which removes the "vanity path continues to be published" assumption underpinning ADR-0004's *Consequences*). Two project artefacts forbade an opportunistic switch (`docs/lessons.md:63` and ADR-0004's last *Consequences* bullet); ADR-0010 is the ADR those artefacts reserved judgement for.
- **Decision:** the relay now imports `github.com/coder/websocket` at vX.Y.Z (latest released). The `WSConn` adapter, the upgrade handlers, the heartbeat goroutine, and the shutdown drain are otherwise unchanged. The context strategy from ADR-0004 (`closeCtx`-cancelled-by-`Close`, per-call `WithTimeout`) carries forward verbatim because the library API is unchanged.
- **Rationale:** Coder forked nhooyr/websocket for continued maintenance under a non-vanity import path with the same public API. The three behavioural properties the relay depends on (`docs/lessons.md:59,77,85`) are preserved in the fork — record the godoc-verification verdict for each as a bullet. Adding `staticcheck` to `make check` (separate ticket) becomes unblocked.
- **Consequences:** the relay has one WS library and one adapter (unchanged from ADR-0004). `govulncheck`'s scan surface shifts from `nhooyr.io/websocket` to `github.com/coder/websocket`. Any future migration trigger (a meaningful API redesign in coder, or a new fork point) requires another ADR; opportunistic library swaps remain forbidden. Three explicit behavioural-property bullets:
  - *Close-safe-with-in-flight-Write* preserved (lesson 59 verdict).
  - *5s close-handshake grace* preserved (lesson 77 verdict; the `drainDeadline = 10s` and `TestHeartbeat_UnresponsivePeer_TriggersClose`'s 7s wait remain correctly sized).
  - *32 MiB default per-frame read cap* preserved (lesson 85 verdict; production still wires 256 KiB via `SetReadLimit`).

The Rationale/Consequences sections are roughly two paragraphs total — this is not a deep design ADR (the *design* lives in ADR-0004); it is a migration ADR. Target length: ~50-70 lines, matching the lighter superseding-ADR shape rather than ADR-0004's original-decision shape.

### `docs/knowledge/features/ws-conn-adapter.md` pointer updates

The feature doc names the underlying library throughout. Edits (search-and-replace mechanical, but verify each in context):

- Replace `nhooyr.io/websocket` with `github.com/coder/websocket` wherever it appears as a module identifier (the file references it in the intro, the *Concurrency model* notes, the *What the adapter deliberately does NOT do* list, and the *Adversarial framing* section).
- The *Dependency* section (currently ~lines 107-117) is rewritten: name `github.com/coder/websocket` as the library, note the historical migration from `nhooyr.io/websocket` (1-line aside), and update the cross-reference from ADR-0004 to ADR-0010 in the *Related* list.
- The closing line "the vanity import path remains `nhooyr.io/websocket` even though the upstream module home moved to `github.com/coder/websocket`" is deleted; it documents a constraint this ticket removes.

### `docs/knowledge/INDEX.md` pointer update

Append one line under the *Decisions* heading (newest at top per file convention):

```
- ADR-0010: Migrate to github.com/coder/websocket (#98) — supersedes ADR-0004; upstream nhooyr.io path deprecated.
```

No other INDEX edits.

## Concurrency model

Unchanged. The `WSConn` adapter's one mutex + one one-shot guard, the `closeCtx`-cancelled-by-`Close` strategy, the heartbeat goroutine's clean-vs-timeout exit cases, and the shutdown drain's fan-out all rely on library properties that coder/websocket preserves. The migration touches imports and doc-comment library names only — no synchronisation primitive moves, no goroutine is added or removed, no channel closes change owner.

## Error handling

Unchanged. Existing call sites (e.g. `errors.Is(err, ErrServerIDConflict)`, `errors.As(err, &ce)` against `websocket.CloseError`) work against coder/websocket's identical type names. The library's wrapped error types reach test assertions through the same `errors.As`/`errors.Is` paths — see `heartbeat_test.go:150-159` for the canonical `CloseError` assertion shape.

## Testing strategy

- `make build` succeeds.
- `make check` (= `vet test`) passes.
- `go test -race ./...` is green and matches the existing 100% pass rate.
- Every test in the seven test files listed above passes without modification beyond the import swap. If any test fails after the swap, that is a behavioural divergence — diagnose against the *Behavioural properties* checklist above, record the finding, and either (a) document the divergence in ADR-0010 if the test can be adjusted without changing production semantics, or (b) halt and route back to PO if the divergence requires call-site rewrites in >2 production files.

The verification has no new tests because the migration has no new behaviour. The existing test suite is the verification surface: any behavioural divergence between nhooyr v1.8.x and coder/websocket would manifest as a test regression here.

A `grep -rn "nhooyr.io/websocket" --include="*.go" .` returning zero results is AC2's mechanical gate. A `grep -rn "nhooyr" go.mod go.sum` returning zero results is the supplementary gate on the module files.

## Open questions

None requiring resolution before implementation. Two trip-wire conditions:

- **API divergence:** if the *Pre-swap API-surface verification* table finds any symbol with a renamed identifier or a changed signature, the "uniformly mechanical" premise of the ticket body's *Sizing Override* is invalidated → halt and route back to PO. (The README evidence we have today suggests this will not occur, but the developer's first action is to verify, not to assume.)
- **Behavioural divergence:** if any of the three lessons (59/77/85) does not survive the swap, document in ADR-0010 *Consequences*, do not halt unless tests break in a way that requires production call-site rewrites in >2 files.

## Acceptance Criteria mapping

| AC | Spec sections that discharge it |
|---|---|
| AC1 — `go.mod` declares `github.com/coder/websocket` at latest released version; `go.sum` reflects the swap; no remaining `nhooyr.io/websocket` references in either. | *Library-identity swap*, *File-level migration (production)* row 1-2. |
| AC2 — `grep -rn "nhooyr.io/websocket" --include="*.go" .` returns zero. | *File-level migration (production)* + *File-level migration (tests)* + the two `main.go` comment lines. |
| AC3 — `make build` succeeds, `make check` passes, `go test -race ./...` is green. | *Testing strategy*. |
| AC4 — Existing WS-handshake / heartbeat / shutdown tests pass without modification beyond the import-path swap (and any uniform renames). | *Testing strategy* + *Pre-swap API-surface verification* + *File-level migration (tests)*. |
| AC5 — A new ADR supersedes ADR-0004, naming the upstream deprecation as the trigger; `docs/knowledge/features/ws-conn-adapter.md` and `docs/knowledge/INDEX.md` are updated to point at the new ADR and the new library identity. | *ADR-0010* + *`docs/knowledge/features/ws-conn-adapter.md` pointer updates* + *`docs/knowledge/INDEX.md` pointer update*. |

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the migration does not move any boundary. Header validation, frame-routing boundary, and the relay's "envelope-only validation" rule (`docs/PROJECT-MEMORY.md` § Project-level conventions) all sit at the same lines after the swap.
- [Tokens, secrets, credentials] No findings — the `X-Pyrycode-Token` presence-check-then-discard path in `internal/relay/client_endpoint.go:43-50` is untouched. No new logging, no new error-message text.
- [File operations] N/A — no file operations introduced or modified.
- [Subprocess / external command execution] N/A — none.
- [Cryptographic primitives] No findings — `crypto/rand` in `randHex8` (`internal/relay/server_endpoint.go:127-133`) is unchanged; TLS via `golang.org/x/crypto/acme/autocert` is unchanged.
- [Network & I/O] No findings — `SetReadLimit(256 KiB)` is explicitly listed in the *Pre-swap API-surface verification* table; the 32 MiB default (relevant only to test fixtures that don't override) is verified via lesson-85 in *Behavioural properties*. All `http.Server` timeouts in `cmd/pyrycode-relay/main.go` are outside the migration's surface. Write-side slow-loris cap (`writeTimeout = 10s`) survives because the `(*Conn).Write(ctx, MessageType, []byte) error` signature is on the verification table and the `Close`-safe-with-in-flight-`Write` property is verified separately (lesson 59).
- [Error messages, logs, telemetry] No findings — `errors.As(err, &ce)` against `websocket.CloseError` (`internal/relay/heartbeat_test.go:150-159`) is on the verification table; log fields and metric labels are unchanged.
- [Concurrency] No findings — the non-deadlocking `Close`/`Send` design depends on lesson 59, which the spec verifies via two independent signals: godoc read pre-swap, and the existing `-race` tests at runtime (`TestWSConn_ConcurrentSend_ProducesIntactFrames`, `TestShutdown_CloseIdempotentOnRealWSConn`). If coder/websocket regressed the property, the race detector would surface it as a deadlock or `DATA RACE`. Goroutine lifecycle (heartbeat exit cases, shutdown drain) is unchanged.
- [Threat model alignment] No findings — `docs/threat-model.md` § "Supply chain — Go dependencies" already names "any direct WS library" on the supply-chain path; the migration changes *which* module sits there but does not widen the surface. `govulncheck` (already in `make lint`) re-scans against the new module path. DoS surface — write-side slow-loris, per-frame read cap, header timeouts, per-IP rate limit, per-server-id phone cap — all unchanged.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-28
