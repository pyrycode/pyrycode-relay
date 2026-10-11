# #175 — Make called-function govulncheck findings fail the daily security scan

## Files read

- `.github/workflows/security-scan.yml` → `govulncheck` job (`govulncheck -json ./... > govulncheck.json`, the jq render step, the `regression-govulncheck` upload) and `file-issue` (title dedup, generic "scanner failed with no parseable findings" fallback). The render step is what this ticket replaces.
- `Makefile` → `check` (`go vet ./...` + `go test -race ./...`) picks up any new package under `cmd/` with no edit; `build` builds only `./cmd/pyrycode-relay`.
- `Dockerfile` → the build stage compiles only `./cmd/pyrycode-relay`, so a second `main` package under `cmd/` never reaches the image.
- `docs/knowledge/features/docker-image.md` § *CI image scanning* → the issue-artifact contract (title + body files, privileged filer never runs scanner code).
- `golang.org/x/vuln@v1.1.4` `internal/govulncheck/govulncheck.go` → `Message`, `Config` (`protocol_version`, `scan_level`), `Finding` (`osv`, `fixed_version`, `trace`), `Frame` (`module`, `version`, `package`, `function`, `receiver`). Output is a stream of **pretty-printed, multi-line** JSON objects, not one object per line, so the parser must use a streaming `json.Decoder`, not a line scanner.
- `golang.org/x/vuln@v1.1.4` `internal/scan/text.go` `TextHandler.Flush` and `internal/scan/template.go` `isCalled` → text mode exits 3 at symbol scan level iff some finding has `Trace[0].Function != ""`. That is the relevance rule here.
- `golang.org/x/vuln@v1.1.4` `cmd/govulncheck/testdata/common/testfiles/source-call/source_call_json.ct` → confirms one reachable OSV emits a module-level, a package-level and one or more symbol-level finding records; the fixtures are trimmed from this shape.

Overlap: no other in-flight `feature/*` branch touches these files.

## Context

In JSON mode govulncheck exits 0 regardless of findings, so the `govulncheck` job is never red, its `if: failure()` render never runs, and a reachable Go advisory never becomes an issue. The render step would also over-report: it lists every `osv` record, and govulncheck emits one for every advisory against any required module.

No decision record needed; this restores the intended gate and follows govulncheck's own exit semantics.

## Design

New `main` package `cmd/vulngate` (standard library only, no `go.mod` change), run from the workflow with `go run ./cmd/vulngate`:

```
vulngate -in govulncheck.json -out <dir> -run-url <url>
```

Exit status:

| Input | Exit | Files written |
|---|---|---|
| Valid stream, ≥1 symbol-level finding | 3 (mirrors text-mode govulncheck) | `<dir>/issue-body.md`, then `<dir>/issue-title.txt` |
| Valid stream, no symbol-level finding | 0 | none |
| Malformed / truncated / missing config / unexpected scan level / bad flags | 1 | none |

Internal shape (unexported):

- `parse(r io.Reader) ([]vuln, error)` — decodes the stream with `json.Decoder` until `io.EOF`. Requires the first message to be a `config` with `protocol_version` `v1.*` and `scan_level == "symbol"` (otherwise a module- or package-level scan, or an empty/truncated file, would read as clean). Collects `osv` records by id and keeps finding records whose `trace[0].function` is non-empty. A finding with an empty `osv` or empty `trace` is malformed. `progress`, `SBOM` and unknown kinds are ignored. Returns one `vuln` per relevant OSV id, sorted by id.
- `vuln` — `ID`, `Module`, `Version` (from `trace[0]`), `Fixed` (`fixed_version`, empty → "no fix"), `Symbols` (distinct `pkg.Receiver.Function` of the vulnerable frame, sorted), `Summary` (the matching `osv` record's `summary`, empty when no record matched).
- `render(vulns []vuln, runURL string) (title, body string)` — title `security-scan regression: <first id> in Go module <its module>` (same shape the jq step wrote, so dedup keeps working across the change). Body: scanner, primary finding, run URL, then one line per relevant OSV id (id, module@version, fixed version, symbols, summary) inside a code block, and the triage bullets.
- `run(args []string, stdout, stderr io.Writer) int` — flag parsing, file read, `parse`, `render`, writes. `main` is `os.Exit(run(...))`.

Determinism: ids sorted, symbols sorted and de-duplicated, no timestamps in the title. Same input → same title → `file-issue` suppresses the repeat.

Hygiene of scanner-derived strings (ids, module paths, versions, symbols): each must be a "safe token" — non-empty, ≤ 200 bytes, printable ASCII with no space, `"`, `` ` ``, `$` or `\`; a violation is malformed input (exit 1). `Summary` is free text: collapse whitespace to single spaces, drop backticks, cap at 200 bytes.

Workflow (`govulncheck` job): keep the install and `govulncheck -json` steps (a non-zero govulncheck exit fails the step, the gate is skipped, no artifact). Replace the jq render step with a gate step running `go run ./cmd/vulngate` with `-out .` and the run URL built from `GITHUB_SERVER_URL`/`GITHUB_REPOSITORY`/`GITHUB_RUN_ID`. The upload step and its `hashFiles('issue-title.txt')` guard stay as they are. Fix the stale "mirror of ci.yml" comments in this job only.

## Concurrency model

None: a single-threaded CLI that reads one file and writes two.

## State transitions and identity reuse

None: no lifecycle or identity state. The only repeated event, a second run with the same finding, is covered by `TestRenderDeterministic`.

## Error handling

- Decode error, missing/invalid config, wrong scan level, malformed finding, unsafe token → exit 1 with a fixed message plus the wrapped error on stderr; nothing written. `file-issue` then files its generic scanner-failure issue.
- Write failures → exit 1. Body is written before title, so the `hashFiles('issue-title.txt')` upload guard never ships a title without a body.
- govulncheck itself failing → its step fails before the gate runs; same generic path.

## Testing strategy

`cmd/vulngate/main_test.go` (package `main`), fixtures under `cmd/vulngate/testdata/`, run by `make check`:

- `TestParse` table over fixtures: `clean.json` (config, progress, osv, no findings) → 0 vulns; `imported-only.json` (module + package findings only) → 0; `called.json` (one OSV with module, package and two symbol findings for different symbols, a second called OSV, and a third OSV that is package-level only) → exactly the two called ids, each once, with module, version, fixed version and both symbols; `truncated.json`, empty input, `module-scan.json` (scan_level module), no config → error.
- `TestRun` over the same fixtures in a temp dir: exit 3 and both files for `called.json`; exit 0 and no files for clean/imported-only; exit 1 and no files for truncated/empty.
- `TestRenderDeterministic`: running `called.json` twice gives the same title, and reversing record order does not change title or body.
- `TestUnsafeToken`: an id with a `"`/newline is rejected.

## Open questions

- Whether `go run` in the workflow needs network for module downloads: the package imports only the standard library and `govulncheck ./...` has already populated the module cache, so no. Confirm locally with `go run` on a fixture.

## Documentation handoff

Pending for the documentation stage:

- `docs/knowledge/features/docker-image.md` § *CI image scanning*: the `govulncheck` job fails only on findings that reach a called function (source call-path evidence, the same rule that makes text-mode govulncheck exit 3); JSON mode alone is not an exit-status gate, so `cmd/vulngate` turns the JSON stream into the exit status and issue artifact; a malformed or failed scan lands as the generic scanner-failure issue.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the only input is `govulncheck.json`, written in the same job by the pinned `govulncheck@v1.1.4` from the Go vulnerability database. `parse` is the single boundary; every string that reaches the title passes the safe-token check, and free-text `summary` is normalised before it reaches the body. No relay payload is involved; the relay binary and image are untouched (`Dockerfile` builds only `./cmd/pyrycode-relay`).
- [Trust boundaries] SHOULD FIX — the title is interpolated into `gh issue list --search "in:title \"$title\""` in `file-issue`; a `"` would break the search and defeat dedup. The safe-token rule rejects `"`, backtick, `$`, `\` and whitespace in every title component; `TestUnsafeToken` must assert it.
- [Tokens & secrets] No findings — the gate step gets no token; `GH_TOKEN` stays in `file-issue`, whose permissions are unchanged.
- [File operations] No findings — writes two fixed filenames into the job workspace with mode `0644`; body before title so a partial write never uploads a title alone.
- [Subprocesses] No findings — the parser spawns nothing; the workflow keeps `--body-file` so scanner text never passes through shell arguments.
- [Cryptography] Not applicable — no randomness, keys or comparisons with secrets.
- [Network & I/O] No findings — CI-only tool reading a local file produced in-job; no listener, no socket.
- [Errors & logs] No findings — stderr carries decode errors about a scanner file in a public-repo Actions log that already prints the same data; no secrets in scope.
- [Concurrency] Not applicable — single-threaded, no goroutines.
- [Threat model] No findings — no new `go.mod` dependency, endpoint or deploy target, so no re-review trigger. Job-level `issues: write` scoping from #73 is unchanged. The gate fails closed: every unexpected input shape exits non-zero without an artifact.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11

### Rework security review (2026-10-11)

**Verdict:** PASS after addressing verifier finding 1.

- [Trust boundaries] Resolved MUST FIX — decoding directly into pointer fields confused explicit null with absence and let a multi-kind record hide a called finding. `message.UnmarshalJSON` now checks the object and its single key before dispatch, rejects duplicate keys and null `config`/`osv`/`finding` values, and still ignores standalone progress, SBOM and unknown kinds. `finding.validate` rejects null trace frames and applies v1.1.4's `validateFindings` field dependencies to every frame before relevance is decided. OSV records require an id. `TestMalformedRecords` proves all 16 malformed fixtures fail with no artifact, both after config and after valid called findings; `TestParseIgnoredRecords` preserves forward compatibility.
- [File operations] No findings — parsing completes before either artifact file is written; a malformed late record discards previously accumulated findings and leaves the output directory empty. The remaining security-review categories and workflow permissions are unchanged.

## Revisions

- 2026-10-11 (build): run against real `govulncheck@v1.1.4 -json ./...` output for this repo (709 finding records, 19 called stdlib advisories under local go1.26.2) showed one advisory can list 70+ called symbols. `render` now caps each line at 5 symbols (`(+N more)`) and the list at 100 advisories, keeping the body far under GitHub's issue-body limit; `TestRenderCapsSymbols` asserts it.
- 2026-10-11 (build): `go run` reports any non-zero child exit as its own exit 1, so in the workflow exit 3 and exit 1 are indistinguishable. Nothing depends on the difference: the upload guard keys on `issue-title.txt` existing, which only the findings path writes. `run` still returns 3, and the tests assert the distinction.
- 2026-10-11 (build): the planned `TestUnsafeToken` landed as the `quote in id` and `newline in mod` cases of `TestParseMalformedFinding`.
- 2026-10-11 (rework, verifier finding 1): record validation now precedes dispatch in `message.UnmarshalJSON`: each record is an object with exactly one kind, including no repeated keys, and processed kinds cannot be null. Trace entries are pointers so null frames remain distinguishable; `finding.validate` checks the entire trace, including imported-only traces and caller frames, against the pinned v1.1.4 field dependencies before testing `Trace[0].Function`. Any malformed record invalidates the whole stream even after a called finding, returning exit 1 with no artifact. Progress, SBOM and future standalone kinds remain ignored. Tests were written and observed failing before these fixes; `TestMalformedRecords` covers parser errors, CLI exit status and absent files in 32 cases, and `TestParseIgnoredRecords` covers clean and relevant streams.
- 2026-10-11 (rework): resolved the offline execution question: `GOPROXY=off go run ./cmd/vulngate` on `clean.json` succeeds without module downloads and returns the clean result.
