# Spec: auto-file GitHub issue when periodic security-scan finds a regression (#73)

## Files to read first

- `.github/workflows/security-scan.yml` (entire file, 64 lines) — the workflow this ticket extends. Both scan jobs (`image-scan`, `govulncheck`) keep their current bodies; the changes are (a) re-shaping each scanner step's output flag for machine parsing, (b) two new `if: failure()` post-steps per job to render and upload an artifact, and (c) one new `file-issue` job appended.
- `.github/workflows/ci.yml:49-84` — the PR-time `image-scan` job. The cron job's scanner *step* must continue to match PR-time policy (`severity: CRITICAL,HIGH`, `ignore-unfixed: true`, `vuln-type: os,library`, `exit-code: '1'`). Only the output flags (`format`, `output`) change in the cron version. The PR-time job is **not** touched by this ticket.
- `docs/specs/architecture/72-periodic-security-scan.md` § *File contents*, § *Refreshing the pins at implementation time*, § *Out of scope* (the "Auto-issue filing on a red run. Lives in #73 (depends on this ticket)" bullet) — establishes which file is authoritative, the lockstep-pin convention, and the deferral this ticket lands.
- `docs/specs/architecture/68-trivy-image-scan.md` § *Pinning the action*, § *Why `ignore-unfixed: true`*, § *Why `vuln-type: os,library`* — Trivy policy rationale. The JSON-output addition this spec introduces does not change any of those flags.
- `docs/specs/architecture/41-govulncheck-pin.md` § *Choice of version*, § *Why a semver tag, not a commit SHA* — `govulncheck@v1.1.4` rationale. This spec adds a `-json` flag to the invocation; the pin itself is unchanged.
- `docs/threat-model.md` § *Supply chain — Go dependencies* (line 22-30) — the residual-risk paragraph this ticket's auto-issue step closes by attaching an owner to red cron runs. The threat-model prose does NOT need editing for this ticket: line 26 already cites `security-scan.yml (#72)` as the periodic re-scan; the auto-filed issue is the actionability layer on top, not a new control surface.
- `docs/PROJECT-MEMORY.md` § *Patterns established* — loud-failure, pin-by-immutable-ref, comment-the-tracked-tag conventions that govern any CI workflow change. The new job inherits all three.

No Go code is touched. Codegraph is not the right tool here; the artifact is a YAML edit. `codegraph_context` was not run (empty result expected — codegraph parses `.go` files, not workflows). Falling back to direct file reads above.

## Context

#72 landed the periodic re-scan workflow: two jobs (`image-scan`, `govulncheck`) run daily against `main`, each fails the workflow on a finding. A red workflow run, by itself, is a *passive* signal — it appears in the Actions tab, but it does not page anyone, does not show up in PR review, and has no owner. Until someone notices the red row, a newly-disclosed CVE against a dependency we shipped weeks ago remains unactioned.

The fix is to lift "red row in Actions" to "open issue with a security-sensitive label." A GitHub issue is the canonical work-item shape in this repo — it gets a number, a status column on the project board, a labeled triage queue, and (via the `security-sensitive` label) gets routed through the security-review workflow on the implementation side. The auto-filed issue is intentionally terse: title names the primary CVE/vuln id and the affected component; body lists all findings, the workflow run URL, and a short triage hint. It's the *starting point* for human triage, not its final form.

Two design pressures shape the architecture:

1. **AC #3 forbids granting `issues: write` to the scanner jobs.** The scanner jobs run untrusted code (Trivy action, Go module install) and must keep the minimum `contents: read` surface. So issue filing happens in a separate job that does *not* run the scanners and *does* have `issues: write`.
2. **The filing job must know what the scanners found** without re-running them. Re-running the scanners in the filer job would (a) double runner time and (b) risk a different result if the vuln database updates between the two runs (the scanner could fail, then the re-run pass — filing nothing — or vice versa, filing a CVE that the original run didn't see). The clean answer is artifact handoff: each scanner job, on its own failure, writes a pre-baked title + body to the runner's filesystem and uploads it; the filer job downloads and posts.

The handoff makes the privilege split clean: scanner jobs prepare content with `contents: read` only; filer job posts content with `issues: write` and does no scanning work.

## Design

### Scope

Modify one file: `.github/workflows/security-scan.yml`. No changes to `ci.yml`, no changes to `Dockerfile`, no changes to `go.mod`, no new files. No reusable workflow or composite action — same clarity-over-DRY argument as #72.

### Architecture: three jobs

```
┌──────────────────┐   ┌──────────────────┐
│   image-scan     │   │   govulncheck    │   ← contents: read only
│                  │   │                  │
│  trivy → trivy.  │   │  govulncheck →   │
│  json (fail on   │   │  vulns.json      │
│  fixable HIGH/   │   │  (fail on any    │
│  CRITICAL)       │   │  finding)        │
│                  │   │                  │
│  on failure:     │   │  on failure:     │
│  render          │   │  render          │
│  issue-image.    │   │  issue-          │
│  {title,body}    │   │  govuln.{title,  │
│                  │   │  body}           │
│  upload artifact │   │  upload artifact │
│  regression-     │   │  regression-     │
│  image-scan      │   │  govulncheck     │
└────────┬─────────┘   └────────┬─────────┘
         │                      │
         └──────────┬───────────┘
                    │  needs: [image-scan, govulncheck]
                    │  if: failure()
                    ▼
         ┌──────────────────┐
         │   file-issue     │   ← contents: read, issues: write
         │                  │
         │  download both   │
         │  artifacts       │
         │  (whichever      │
         │  exist)          │
         │                  │
         │  for each:       │
         │    gh issue list │
         │    --search …    │
         │    → dedup       │
         │                  │
         │    gh issue      │
         │    create …      │
         │    --label       │
         │    security-     │
         │    sensitive     │
         └──────────────────┘
```

Three jobs, two trust tiers. The two scanner jobs keep their current permissions surface verbatim. The new `file-issue` job is the *only* place `issues: write` is granted, and it is granted at the **job** level — not workflow level — per AC #3.

### Scanner output: JSON, not table

Both scanners currently produce table / text output. AC #1 requires the issue body to contain "the scanner output excerpt, the offending CVE id(s) or Go vuln id(s), the affected dependency (Go module + version) or image layer." Table output is human-readable but a pain to parse with `awk` / `sed` — every column-width drift breaks the regex. JSON is parseable with `jq` in one line.

Both scanners support JSON natively:

- **Trivy:** change `format: table` → `format: json` and add `output: trivy.json`. The action writes the file and still exits non-zero on findings (`exit-code: '1'` is unchanged). The job step log loses the pretty table but gains a structured file the filer can `jq` against. (Operators reading the run can still `cat trivy.json | jq …` in the step log, which the filer pre-bakes into the issue body.)
- **govulncheck:** change `govulncheck ./...` → `govulncheck -json ./... > govulncheck.json`. The `-json` flag is supported as of `v1.1.4` (it has been in `govulncheck` since `v1.0.0`); the format is newline-delimited JSON records. The step exits non-zero on findings (unchanged). The wrapper redirect captures stdout to a file regardless of exit status.

  Wrap with `set -o pipefail` so a non-zero exit from `govulncheck` is still propagated past the redirect. Concretely:
  ```yaml
  - name: govulncheck
    run: |
      set -o pipefail
      govulncheck -json ./... > govulncheck.json
  ```
  Without `pipefail`, the redirect would swallow the non-zero exit (`>` is a builtin, so the exit code is propagated even without `pipefail`, but the explicit `set -o pipefail` documents intent and protects future refactors that pipe through `tee` for log visibility).

Policy flags (`severity: CRITICAL,HIGH`, `ignore-unfixed: true`, `vuln-type: os,library`, `exit-code: '1'`) are unchanged — same definition of "vulnerable" as PR-time and as #72 currently emits. This ticket only adds parseable output; it does not redefine what counts as a finding.

### Render step (per scanner job, `if: failure()`)

After the scanner step, add a render step that:

1. Runs only on previous-step failure: `if: failure()`.
2. Parses the JSON output with `jq` to extract the primary CVE/vuln id, the affected component, and a summary excerpt.
3. Writes two files to the runner filesystem:
   - `issue-title.txt` — single-line deterministic title (see § *Dedup* for format).
   - `issue-body.md` — markdown body containing the title, all CVE/vuln rows, the affected component(s), a fenced-code excerpt of the JSON (truncated), and the workflow run URL.

The renderer uses only tools pre-installed on `ubuntu-latest` runners: `bash`, `jq`, `printf`. No `npm install`, no extra action.

Authoritative renderer for `image-scan` (developer copies into the workflow):

```yaml
- name: render issue from trivy output
  if: failure()
  run: |
    set -euo pipefail
    primary_cve=$(jq -r '
      [ .Results[]?.Vulnerabilities[]? ]
      | sort_by(.Severity) | reverse | .[0].VulnerabilityID // empty
    ' trivy.json)
    primary_pkg=$(jq -r '
      [ .Results[]?.Vulnerabilities[]? ]
      | sort_by(.Severity) | reverse | .[0]
      | "\(.PkgName // "unknown")@\(.InstalledVersion // "unknown")"
    ' trivy.json)
    # AC #4: if trivy.json has no .Vulnerabilities entries (e.g. the
    # action failed before producing output, or produced an empty
    # results array), skip artifact upload — there is nothing to file.
    # An empty primary_cve means parsing produced nothing actionable.
    if [ -z "$primary_cve" ]; then
      echo "no parseable CVEs in trivy.json — skipping issue render"
      exit 0
    fi
    printf 'security-scan regression: %s in image package %s\n' \
      "$primary_cve" "$primary_pkg" > issue-title.txt
    {
      printf '## Periodic security-scan regression\n\n'
      printf '**Scanner:** Trivy (image scan)\n'
      printf '**Primary finding:** `%s` in `%s`\n' "$primary_cve" "$primary_pkg"
      printf '**Workflow run:** %s/%s/actions/runs/%s\n\n' \
        "${GITHUB_SERVER_URL}" "${GITHUB_REPOSITORY}" "${GITHUB_RUN_ID}"
      printf '### All findings\n\n```\n'
      jq -r '
        .Results[]?.Vulnerabilities[]?
        | "\(.Severity)\t\(.VulnerabilityID)\t\(.PkgName)@\(.InstalledVersion)\t\(.FixedVersion // "no fix")"
      ' trivy.json | sort -u | head -100
      printf '```\n\n### Triage\n'
      printf -- '- Bump the offending dependency or base-image digest.\n'
      printf -- '- Verify the next `security-scan` workflow run is green.\n'
      printf -- '- Close this issue once the next run passes.\n'
    } > issue-body.md
```

Authoritative renderer for `govulncheck`:

```yaml
- name: render issue from govulncheck output
  if: failure()
  run: |
    set -euo pipefail
    # govulncheck -json emits newline-delimited records; pick out
    # the OSV.id from the "osv" record kind.
    primary_id=$(jq -rs '
      [ .[] | select(.osv?.id?) | .osv.id ] | .[0] // empty
    ' govulncheck.json)
    primary_mod=$(jq -rs '
      [ .[] | select(.osv?.affected?) | .osv.affected[0].package.name ]
      | .[0] // "unknown"
    ' govulncheck.json)
    if [ -z "$primary_id" ]; then
      echo "no parseable vulns in govulncheck.json — skipping issue render"
      exit 0
    fi
    printf 'security-scan regression: %s in Go module %s\n' \
      "$primary_id" "$primary_mod" > issue-title.txt
    {
      printf '## Periodic security-scan regression\n\n'
      printf '**Scanner:** govulncheck (Go modules)\n'
      printf '**Primary finding:** `%s` in `%s`\n' "$primary_id" "$primary_mod"
      printf '**Workflow run:** %s/%s/actions/runs/%s\n\n' \
        "${GITHUB_SERVER_URL}" "${GITHUB_REPOSITORY}" "${GITHUB_RUN_ID}"
      printf '### All findings\n\n```\n'
      jq -rs '
        .[] | select(.osv?.id?)
        | "\(.osv.id)\t\(.osv.affected[0].package.name // "unknown")"
      ' govulncheck.json | sort -u | head -100
      printf '```\n\n### Triage\n'
      printf -- '- Run `govulncheck ./...` locally against the named module to see call-site traces.\n'
      printf -- '- Bump the offending dependency.\n'
      printf -- '- Verify the next `security-scan` workflow run is green.\n'
      printf -- '- Close this issue once the next run passes.\n'
    } > issue-body.md
```

The `head -100` cap on the findings list is a safety net: a flood scenario (e.g. a base-image update that introduces hundreds of CVEs at once) should not produce a 10 MB issue body that hits GitHub's body-size limit (65536 bytes). 100 lines × ~80 bytes/line ≈ 8 KB, comfortably under the cap. If the cap is hit, the truncated body still names the primary CVE in the title and links the workflow run for full output.

### Upload-artifact step (per scanner job, `if: failure()`)

After the render step, an upload step:

```yaml
- name: upload issue artifact
  if: failure() && hashFiles('issue-title.txt') != ''
  uses: actions/upload-artifact@v5
  with:
    name: regression-image-scan   # or regression-govulncheck
    path: |
      issue-title.txt
      issue-body.md
    retention-days: 7
    if-no-files-found: ignore
```

The `hashFiles(...) != ''` guard skips upload when the render step early-exited because there was nothing to file (the "no parseable CVEs" path above). `retention-days: 7` matches the cron cadence with slack — operators have a week to investigate before the artifact rotates out.

Use `actions/upload-artifact@v5` (current major as of 2026-05; this repo already uses `@v6` for `checkout` and `setup-go`, so a `@v5` here is the current pin for that action — the developer should verify the current major at implementation time and match `ci.yml`'s convention if `ci.yml` uses `upload-artifact` somewhere by then). Pin by major version; Renovate handles the rest.

### `file-issue` job

```yaml
file-issue:
  needs: [image-scan, govulncheck]
  if: failure()
  runs-on: ubuntu-latest
  permissions:
    # `issues: write` granted at the job level — per AC #3, NOT at
    # workflow level. `contents: read` is the implicit baseline for
    # GITHUB_TOKEN; we keep it explicit here for parity with the
    # scanner jobs' belt-and-suspenders block.
    contents: read
    issues: write
  steps:
    - name: download regression artifacts
      uses: actions/download-artifact@v5
      with:
        # Pattern matches both `regression-image-scan` and
        # `regression-govulncheck`. Missing artifacts (e.g. one
        # scanner failed for a non-CVE reason — Dockerfile didn't
        # build — and produced no artifact) are silently absent.
        pattern: regression-*
        path: artifacts
        merge-multiple: false
    - name: file issues
      env:
        GH_TOKEN: ${{ github.token }}
        REPO: ${{ github.repository }}
      run: |
        set -euo pipefail
        shopt -s nullglob
        for dir in artifacts/regression-*; do
          title=$(cat "$dir/issue-title.txt")
          body_file="$dir/issue-body.md"
          # Dedup search: AC #2 option (a). Search by the title's
          # deterministic prefix + the primary CVE id (the title
          # always contains "security-scan regression: <ID>"). Use
          # `in:title` to avoid matching body text in unrelated
          # issues. State filter excludes closed issues so a
          # resolved-and-closed CVE that regresses files a new
          # issue.
          existing=$(gh issue list \
            --repo "$REPO" \
            --state open \
            --label security-sensitive \
            --search "in:title \"$title\"" \
            --json number \
            --jq 'length')
          if [ "$existing" -gt 0 ]; then
            echo "duplicate suppressed for: $title"
            continue
          fi
          gh issue create \
            --repo "$REPO" \
            --title "$title" \
            --body-file "$body_file" \
            --label security-sensitive
        done
```

Two security-relevant invariants in this step:

1. **`gh issue create` uses `--body-file`, not `--body "<...>"`.** This avoids passing scanner-derived content (CVE descriptions, package names) through shell-argument expansion. Issue bodies can contain backticks and `$` from package version strings; `--body-file` reads the file verbatim. Same reason `--title` is sourced from a file via `$(cat …)`: the title is a plain ASCII line we just rendered.
2. **`GH_TOKEN` is the workflow-scoped `github.token`,** not a PAT, not a `secrets.GITHUB_TOKEN` reference (functionally identical but `github.token` is the documented idiom for the auto-issued token). The token's permissions at runtime are whatever the job's `permissions:` block grants — `issues: write` plus `contents: read`. No additional secrets surface.

### Dedup: title pattern + search query

AC #2 permits either machine dedup (option a) or a deterministic-title convention (option b). This spec chooses (a) — but the title pattern is also deterministic so option-b dedup-by-eye works as a backstop.

Title pattern (one line, ASCII only):

```
security-scan regression: <CVE-or-vuln-id> in <component-descriptor>
```

Examples:

- `security-scan regression: CVE-2026-12345 in image package openssl@3.0.2-0ubuntu1`
- `security-scan regression: GO-2026-1234 in Go module golang.org/x/crypto`

Search query (passed to `gh issue list`):

```
in:title "security-scan regression: <full-title-string>"
```

Why match the full title string and not just the CVE id: a single CVE id can affect multiple components (e.g. the same OpenSSL CVE on two layers) and we'd file one issue per *component pair* if that ever happens, which is more useful than collapsing them into one. The `in:title` qualifier prevents accidental matches against an issue whose body mentions the CVE id (e.g. a triage comment referencing a related ticket). The `--state open` filter lets a regression of a previously-resolved CVE file a fresh issue.

**Dedup edge cases (intentionally accepted, not defended):**

- **Title case drift.** If a future architect edits the title format, in-flight open issues filed under the old format won't match the new search. Acceptable: one duplicate issue per scanner during the transition; the spec's dedup convention is named explicitly so a careful reviewer catches the change.
- **Two cron runs file the same issue simultaneously.** GitHub Actions doesn't atomically gate the search-then-create sequence; a perfectly-timed concurrent run could file two identical issues. Acceptable per AC #2 ("perfect dedup is not required; non-spammy is"). The daily cadence and the `concurrency:` block below make this vanishingly unlikely.
- **The primary CVE changes between runs.** If Trivy's severity ordering shifts (a CRITICAL gets superseded by a new CRITICAL in the same package), the title changes and a new issue is filed. Acceptable: the old issue's body still names the old finding; the new issue names the new one. Operator closes both when fixed.

### Concurrency

Add a workflow-level `concurrency:` block to coalesce overlapping runs:

```yaml
concurrency:
  group: security-scan
  cancel-in-progress: false
```

`cancel-in-progress: false` is the deliberate choice — a scheduled run firing while a manual `workflow_dispatch` is in flight (or vice versa) should *queue*, not cancel. Cancelling an in-flight scanner mid-run could leave artifacts in a half-uploaded state and produce no issue at all for a real regression. Queueing is the safe default.

(#72's spec § *Concurrency model* explicitly deferred this block to a follow-up; this ticket is that follow-up.)

### Out of scope

- **Slack/email notifications on a filed issue.** The filed GitHub issue is the notification mechanism. Slack/email is a separate decision and a separate ticket.
- **SARIF upload to the GitHub Security tab.** Same deferral as #68 and #72 — would require `security-events: write`, widening the minimum-permissions posture this ticket explicitly preserves.
- **Auto-close of issues when the next run passes.** A passing run does not file anything; it also does not close prior open issues. Closing is an operator action after they verify the bump landed. Auto-close would require state machinery this ticket avoids.
- **Per-PR auto-issue filing on the PR-time `ci.yml` scanners.** `ci.yml`'s scanners gate merge; a failing PR-time scan blocks the PR. The PR itself is the work-item. No issue needed.
- **Re-running the filer on a manual `workflow_dispatch` against clean `main`.** When neither scanner fails, the `file-issue` job does not run (its `if: failure()` is false). No issue is filed. This satisfies AC #4's green-path assertion.
- **Refactoring the existing scanner steps' invocation beyond the JSON-output flag.** Pin SHAs, action versions, severity flags, and `exit-code` are unchanged from #72.

## Concurrency model

Not applicable in the Go-concurrency sense. At the workflow level:

- **Three GitHub Actions jobs**: `image-scan` and `govulncheck` in parallel; `file-issue` sequenced after via `needs:`.
- **`if: failure()` on `file-issue`** fires when at least one of the needed jobs failed (default behaviour: `failure()` is the negation of all-success / not-cancelled).
- **Overlapping runs** are coalesced by the workflow-level `concurrency:` block (group `security-scan`). A second invocation queues until the first completes.
- **Artifact contention** is not possible: each run gets a unique `${{ github.run_id }}` in its artifact namespace; `actions/upload-artifact@v5` does not write into a shared registry.

## Error handling

- **One scanner fails, the other passes.** `file-issue` runs; only the failing scanner's artifact exists; the loop iterates over one entry; one issue is filed. The passing scanner's artifact step did not run (its `if: failure()` was false at the job level since the scanner step succeeded), so no `regression-govulncheck` (or `regression-image-scan`) artifact appears in the download. The `for dir in artifacts/regression-*; do` loop with `shopt -s nullglob` is empty-safe.
- **Both scanners fail.** Both artifacts exist; two issues are filed (one per scanner). This is the intended behaviour — image CVEs and Go vulns are independent regressions with independent triage paths.
- **Scanner fails before producing JSON output** (e.g. Docker build fails, `go install` of `govulncheck` fails, Trivy DB download fails). The render step's `if: failure()` fires, but `jq` produces no `primary_cve` and the renderer early-exits with `echo "no parseable …"`. The upload step's `hashFiles(...)` guard sees no `issue-title.txt`, skips upload. `file-issue` runs (because the scanner job failed) but the artifact loop iterates zero times — no issue filed. The red workflow run is still visible in the Actions tab as a fallback signal. This is the deliberate behaviour: scanner-infrastructure failures are not security regressions and should not produce labelled issues; they show up as red runs and get triaged through the Actions UI, same as any CI flake.
- **`gh issue list` fails** (rate limit, GitHub API outage). `set -euo pipefail` propagates the failure, `file-issue` job goes red, the red workflow run is the operator's signal. No issue is filed silently with a missed-dedup; loud failure preferred over silent duplicate.
- **`gh issue create` fails** (rate limit, permission misconfig). Same loud failure; job goes red. Operator notices the red `file-issue` job and re-runs (the next cron tick re-attempts; or manual `workflow_dispatch`).
- **Concurrent runs** (manual + scheduled overlap). Coalesced by `concurrency:` block; queued runs don't double-file.
- **Issue-body size cap exceeded** (>100 findings in one run). The `head -100` cap on the rendered findings keeps the body under GitHub's limit. Body still names the primary CVE; full output reachable via the workflow run URL link in the body.
- **Title-search false positive.** If the deterministic title pattern accidentally matches an unrelated open issue (vanishingly unlikely given the `security-scan regression:` prefix is a literal string nothing else uses), the new issue is suppressed as a duplicate. Operator notices the missing-but-expected issue, manually reviews the run, files manually. Acceptable per AC #2 ("non-spammy" beats "perfect").

No new failure modes introduced beyond what the scanner steps already exhibit at PR time. The new failure modes (`file-issue` job failures) are loud and bounded.

## Testing strategy

Three verification flows live in the PR description, not as committed test fixtures:

1. **Clean-`main` green path** (AC #4, first half). After the PR ships, the developer opens the Actions tab, picks the `security-scan` workflow, hits "Run workflow" on `main`, observes:
   - `image-scan` green.
   - `govulncheck` green.
   - `file-issue` did not run (skipped because `failure()` is false).
   - No new issue filed.

2. **Vulnerable-state path** (AC #4, second half). On a throwaway branch the developer creates (e.g. `verify/73-vulnerable-state`):
   - **For the `image-scan` job:** edit `Dockerfile` to pin the base image to a digest known to have a fixable CRITICAL or HIGH CVE. Two recipes (developer picks one):
     - Pin `FROM alpine` to an older `@sha256:...` digest such as Alpine 3.18.0 (`sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b`), which has known fixable CRITICAL CVEs in older `openssl` packages. (Verify the digest still resolves at the time of the test.)
     - Or use a curated test image: `FROM ghcr.io/aquasecurity/trivy-test-images:vulnerable-debian` or equivalent if Trivy ships test fixtures (developer to verify availability at run time).
   - **For the `govulncheck` job:** add a temporary import in `cmd/pyrycode-relay/main.go` of a Go module + function with a known GO-vuln advisory reachable from `main()`. A safe choice as of 2026-05: `golang.org/x/text v0.3.7` has reachable `GO-2022-1059`-style entries when used through `language.Parse`. Add `_ = language.MustParse("en")` to `main()` and pin `golang.org/x/text` to `v0.3.7` in `go.mod`. The developer verifies the specific GO id is still listed in the Go vuln DB at test time; if `golang.org/x/text v0.3.7` has been delisted, pick another module from `https://pkg.go.dev/vuln/list` with a reachable function.
   - Push the branch and run `gh workflow run security-scan.yml --ref verify/73-vulnerable-state`. Observe in the Actions UI:
     - The corresponding scanner job goes red.
     - The `file-issue` job runs.
     - One issue is filed with the deterministic title, the `security-sensitive` label, and a body containing the CVE/vuln id, the affected component, and a link to the workflow run.
   - Verify dedup: trigger a second run of the same workflow on the same branch. The second `file-issue` job should log `duplicate suppressed for: <title>` and exit clean. No second issue filed.
   - Manually close the test-filed issue. Delete the throwaway branch. No commits to `main`.

3. **Pre-merge dry run** (optional but recommended). In the implementation PR, add a temporary `pull_request:` trigger to `security-scan.yml`, push, observe the scan jobs run green on the PR's check list, then **remove** the `pull_request:` trigger before merging. The trigger must not ship — leaving it in would re-run the periodic scan on every PR and inflate PR check noise. (Alternative without trigger editing: push the file to a branch and use `gh workflow run security-scan.yml --ref <branch>` after the branch is pushed.)

No unit tests; this is a CI workflow change. The two verification steps above are sufficient to cover all four AC items.

## Open questions

- **Should the filer post a comment on the existing open issue when dedup suppresses a new file?** Today, a suppressed duplicate is a one-line log entry in the `file-issue` step. An alternative is `gh issue comment <number> --body "Cron run on YYYY-MM-DD also detected this finding (workflow run: <URL>)"`. The argument *for* is "operator sees the issue is still live without scrolling Actions"; the argument *against* is "every daily run posts a comment on every open security-sensitive regression issue, generating noise." Defer to first observed need. Current behaviour (dedup suppresses silently, with a step-log line for forensics) is acceptable per AC #2 "non-spammy is."
- **Should the filer assign the issue to anyone?** Today, the issue is unassigned. Assigning to `@<maintainer>` would force ownership but couples the workflow to a specific GitHub user. Deferred — the `security-sensitive` label is the routing mechanism the repo already uses.
- **Should the throwaway-branch verification recipe be committed as a runnable script** (e.g. `make verify-security-scan`)? Argued against here: it would import a known-vulnerable dependency into the repo (even guarded by a Make target), which itself trips PR-time `govulncheck` and is a worse failure mode than a one-off branch the developer creates and discards. The verification recipe lives in this spec + the PR description; that's sufficient.
- **Should `file-issue` itself fail loud (job goes red) when it can't post for any reason?** Current design: yes — `set -euo pipefail` propagates `gh` errors. A silent post failure would mean a red scan run went un-issued, defeating the ticket's purpose. The trade-off is that a transient `gh` API outage turns one red scan job into two red jobs in the Actions list, which is appropriate (the second red row is "we couldn't even file the issue"). No change needed.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** Scanner JSON output (Trivy + govulncheck) is a new piece of content this ticket *propagates* into GitHub Issues. The content originates from public CVE databases (NVD, OSV) and is parsed by `jq`. The trust boundary is **public CVE-database content → GitHub Issue body via `gh issue create --body-file`**. This is enforced by (a) using `--body-file` not `--body "<...>"` so scanner content never passes through a shell argument, (b) `set -euo pipefail` in every shell step so `jq` failures on malformed JSON propagate as loud failures rather than silent empty issues, and (c) the `head -100` cap on the findings list. No code execution path from CVE description to runner.
- **[Tokens, secrets, credentials]** `GITHUB_TOKEN` (referenced as `github.token`) is the only credential surface introduced. It is consumed by `gh issue list` and `gh issue create` via the `GH_TOKEN` env var in the `file-issue` step. Permissions are granted at the **job** level (`permissions: { contents: read, issues: write }`), not workflow level — matches AC #3 verbatim. The two scanner jobs retain `contents: read` only (unchanged from #72). No `secrets.*` references. No PAT. The token is never logged, never echoed, never compared, never passed to a third-party action. `actions/upload-artifact@v5` and `actions/download-artifact@v5` use the runner's built-in credentials, not the GITHUB_TOKEN.
- **[File operations]** The render steps write to three files in the runner's per-run filesystem (`trivy.json`, `govulncheck.json`, `issue-title.txt`, `issue-body.md`). All paths are workflow-defined constants; no path component comes from scanner output. `actions/upload-artifact@v5` uploads only the named paths; no path-traversal vector. No writes to repo-tracked paths. The `file-issue` job reads from `artifacts/regression-*/issue-{title,body}.{txt,md}` — the `regression-*` glob matches only artifact names we control. `cat "$dir/issue-title.txt"` is the only read of scanner-derived content; the file is treated as a single line of UTF-8.
- **[Subprocess / external command execution]** New shell invocations: `jq` (read-only JSON parsing, no `-r '@sh'` style command-generation), `cat`, `printf`, `gh issue list`, `gh issue create`. `gh` arguments are constructed from environment-controlled values (`$REPO` from `github.repository`, `$title` from a runner-local file we wrote ourselves, `$body_file` from a runner-local path we constructed). The dedup-search string `"in:title \"$title\""` is the only place a runner-local-but-CVE-derived value enters a `gh` argument; `gh` treats `--search` content as an opaque query string passed to the GitHub Search API, not as shell. **Critical invariant:** `gh issue create` uses `--body-file`, NEVER `--body "<...>"`. Argument quoting in `bash` would otherwise expand backticks and `$(…)` from CVE descriptions. The `--title` argument is the rendered single-line string we wrote ourselves — pure ASCII by construction.
- **[Cryptographic primitives]** Not applicable directly. The workflow inherits #72's two crypto controls (Trivy action commit-SHA pin, `govulncheck` semver pin verified by `sum.golang.org`). No new crypto. The `gh` CLI is preinstalled on `ubuntu-latest` and validated by the GitHub-Hosted Runner image build process (out of this ticket's scope).
- **[Network & I/O]** Egress destinations added by this ticket: `api.github.com` (called by `gh issue list` and `gh issue create`). Pre-existing egress (`github.com` for checkout, `proxy.golang.org` + `sum.golang.org` for `go install`, Trivy action's vuln-DB registry) unchanged. No new listener. No user-controlled URLs.
- **[Error messages, logs, telemetry]** New log surfaces: the render-step `echo` lines, the `file-issue` step's `gh` output, and the issue body itself (which is published in the GitHub Issues feed). Issue body contents: CVE IDs, package names, Go module names, severity strings, fixed-version metadata, workflow run URLs. All public NVD/OSV-style data. No secrets, no internal relay state, no payloads — same audience-vs-content alignment as #72's existing step logs.
- **[Concurrency]** Workflow-level `concurrency: group: security-scan, cancel-in-progress: false` coalesces overlapping runs. Within a single run, three jobs run on three independent runners with no shared filesystem. The `file-issue` job downloads artifacts only after both scanner jobs have completed (`needs:` enforced). No artifact contention; no race between search and create within a single run (sequential bash). Cross-run race (two runs scheduled minutes apart) is coalesced by `concurrency:`.
- **[Threat model alignment]** Extends `docs/threat-model.md` § *Supply chain — Go dependencies* in the same direction #72 does — closing the post-merge invisibility window for disclosed CVEs. #72 reduced the window from "weeks" to "≤24h via a red Actions row"; #73 attaches an owner to that row via a tracked issue, which lifts the signal from "passive (someone has to look)" to "active (the issues queue surfaces it during normal triage)." The threat-model prose does not need editing: line 26 already cites `security-scan.yml (#72)` as the periodic-scan control; the auto-issue is the actionability layer, semantically part of the same control. A documentation-phase pass can refresh the section to cite both tickets explicitly if/when desired.

**Adversarial scenarios considered:**

- *A maliciously-crafted CVE description embeds shell-meta or markdown-injection payloads.* The pipeline is: CVE DB → scanner JSON output → `jq` parse → render to title/body files → `gh issue create --body-file`. Shell expansion never sees the content (we use `--body-file`, not `--body "<...>"`). Markdown injection in the issue body is bounded to what GitHub's issue renderer accepts — GitHub's renderer is the threat-modelled surface (it sanitises HTML, blocks script tags). The worst-case impact is an ugly-looking issue body, not code execution. Accepted residual.
- *An attacker poisons the artifact-upload payload between scanner job and file-issue job.* GitHub Actions artifacts are scoped to a single workflow run (run-id namespace) and uploaded via the runner's authenticated channel to GitHub's artifact storage. There is no inter-run leakage and no third-party write path. Out of scope: an attacker who has compromised GitHub Actions infrastructure itself.
- *An attacker abuses the `issues: write` permission by triggering many cron runs with false positives.* `workflow_dispatch` is restricted to users with `write` access (GitHub built-in policy). Scheduled cron firings are not user-triggerable. Dedup prevents duplicate spam. The maximum issue-creation rate from this workflow is bounded by the daily cron + dedup ≈ at most 2 issues/day (one per scanner) under sustained-regression conditions, and zero/day under steady state.
- *The dedup search returns a false positive that suppresses a real regression.* The title prefix `security-scan regression: ` plus the CVE/vuln id is specific enough that a manually-filed issue with the same title is implausible. If it happens (e.g. an operator opens an issue titled identically to test dedup), the next cron run silently suppresses — the operator-filed issue still tracks the regression. No silent loss of signal.
- *The dedup search misses a real duplicate (false negative).* AC #2 explicitly accepts non-perfect dedup. Worst case: two issues filed for the same finding. Operator closes one. Bounded blast radius.
- *`set -euo pipefail` is forgotten on a future edit to one of the shell steps.* This is a maintenance hazard, not an attack surface, but worth naming. The spec mandates `set -euo pipefail` on all multi-line `run:` blocks. A future architect editing these steps should preserve it; a code reviewer should flag its absence.
- *The `file-issue` job runs even when the scanner job failed before producing JSON output* (e.g. `docker build` errored). The render step early-exits and produces no artifact; the upload step's `hashFiles(...)` guard skips upload; the `file-issue` job's `for dir in artifacts/regression-*` loop iterates zero times via `shopt -s nullglob`. No spurious issue filed. The red workflow run is still the operator signal — same as PR-time behaviour for infrastructure failures.
- *A future maintainer widens `permissions: issues: write` to the workflow level "for convenience."* The `# Tracks:`-style permissions block comment (added below — see § File contents) explicitly names the scope and the AC that mandates job-level granularity. A reviewer who reads the comment is on notice. Code-level enforcement (e.g. a lint that fails if any workflow has `issues: write` at top-level outside a known allowlist) is the deterministic safety net per the playbook's "belt-and-suspenders means different fabric" principle — deferred until the failure mode is observed.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-12
