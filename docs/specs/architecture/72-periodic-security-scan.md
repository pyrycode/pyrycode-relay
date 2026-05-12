# Spec: periodic security-scan cron workflow (#72)

## Files to read first

- `.github/workflows/ci.yml:1-9` — the workflow header pattern (triggers, top-level `permissions: contents: read`). The new file's header mirrors this except for the trigger set.
- `.github/workflows/ci.yml:26-47` — the `security` job. Specifically lines 35-43 (the `# Tracks:` comment + `install govulncheck` step pinned to `@v1.1.4`) and line 46-47 (`govulncheck ./...`). The new workflow's `govulncheck` job copies these lines verbatim. `gosec` (line 33-34, 44-45) is **not** ported — see § Design / Out of scope.
- `.github/workflows/ci.yml:49-84` — the `image-scan` job. The new workflow's `image-scan` job copies this job verbatim (job-level `permissions`, `actions/checkout@v6`, `docker build`, the SHA-pinned Trivy step with `ignore-unfixed: true`, `severity: CRITICAL,HIGH`, `vuln-type: os,library`, `exit-code: '1'`). The `# Tracks:` comment block on lines 63-68 carries across.
- `docs/specs/architecture/68-trivy-image-scan.md` § *Pinning the action*, § *Why `ignore-unfixed: true`*, § *Why `vuln-type: os,library`* — the rationale that justifies each Trivy flag. This spec inherits those decisions one-for-one; the periodic scan must match policy with the PR-time scan or it would produce a different definition of "vulnerable" depending on the trigger.
- `docs/specs/architecture/41-govulncheck-pin.md` § *Choice of version*, § *Why a semver tag, not a commit SHA or pseudo-version* — the rationale for the `@v1.1.4` semver pin (checksum-DB immutability). The periodic scan inherits the same pin.
- `docs/threat-model.md` § *Supply chain — Go dependencies* — the section the periodic scan extends. The existing prose names PR-time scanning as the control; the periodic re-scan closes the residual gap "CVEs disclosed after a PR merges, against bases that haven't changed."
- `docs/PROJECT-MEMORY.md` § *Patterns established* — for the loud-failure + pin-by-immutable-ref + comment-the-tracked-tag conventions that govern any CI workflow change.

No Go code is touched. Codegraph is not the right tool here; the artifact is a single new YAML workflow.

## Context

#68 (PR-time Trivy image scan) and #41 (pinned PR-time `govulncheck`) together cover the *PR moment*: every PR's diff is checked against both scanners' current vuln databases before merge. They do not cover the *post-merge moment*: a CVE disclosed today against a dependency that hasn't changed in three weeks produces no PR signal, no Actions run, no notification. The relay's Go modules and base-image digests will sit untouched for stretches; CVE disclosures against them are continuous; the gap between disclosure and the next dependency bump is the invisibility window.

The right control is a scheduled re-scan that runs the same two scanners against `main` on a cadence, independent of PR activity. A regression then surfaces as a red workflow run in the Actions list within the chosen cadence. This ticket lands the workflow; **filing a GitHub issue from a red run is deferred to #73** so this ticket has a single concern — schedule + execute.

The two scanners cover complementary surfaces (per #68 § Context):
- `govulncheck` — reachable vulns in Go modules linked into the binary, source-side.
- Trivy image scan — CVEs in OS packages and Go binary content of the runtime image, artifact-side.

The cron workflow must run **both**, with **identical policy** to the PR-time equivalents. A divergent policy would mean "what's vulnerable at PR time" and "what's vulnerable on cron" disagree, which is the kind of inconsistency that erodes trust in the signal.

## Design

### Scope

Add one new file: `.github/workflows/security-scan.yml`. No changes to `ci.yml`, no changes to `Dockerfile`, no changes to `go.mod`. No reusable workflow or composite action — per AC #5's Technical Notes, "optimize for clarity over DRY at v1." The two jobs duplicate ~30 lines each from `ci.yml`; that duplication is the cost of clarity at this size.

### Schedule cadence: daily

Pick `cron: '0 6 * * *'` (06:00 UTC, daily). Rationale:

- **CVE disclosure cadence is continuous, not weekly.** New CVEs are published to the NVD and the Go vuln DB on no fixed schedule; a weekly re-scan creates an up-to-six-day invisibility window between disclosure and detection. Daily caps that at ~24h.
- **Cost is negligible.** Each scan job finishes in ~2–3 minutes on `ubuntu-latest`; two jobs in parallel total ~3 minutes wall-clock, ~6 minutes total runner time. At one run per day this is ~3 hours/year of free-tier runner usage.
- **Off-peak slot.** 06:00 UTC is 02:00 EDT, 08:00 CEST, 14:00 JST — outside US business hours where PR-time CI peaks, so the cron run does not contend with the team's PR runner queue.
- **Pre-#73 noise tolerance.** Without auto-issue filing (#73), a regression appears only as a red row in the Actions list. Daily makes that row more recent and harder to scroll past; weekly would make the most recent row up to seven days old by the time anyone looks. The "no auto-paging" property the AC accepts for this ticket is a design choice for the *signal mechanism*, not an excuse to dampen the signal frequency.

`workflow_dispatch:` is also added for manual re-runs (AC #1). No `inputs:` — the workflow is monolithic and takes no parameters.

### File contents

Single new file `.github/workflows/security-scan.yml`. Authoritative version below; the developer copies it verbatim and then refreshes the two pin references at implementation time per § *Refreshing the pins* below.

```yaml
name: security-scan

# Re-runs the same two security scanners that gate PRs (ci.yml's
# `image-scan` job from #68 and `govulncheck` from `security` in #41)
# against the latest `main` SHA on a daily schedule. The goal is to
# surface CVEs disclosed against deps that have NOT changed since the
# last PR — without a periodic re-scan these are invisible until the
# next time someone bumps the affected dep. A failing run produces a
# red row in the Actions list; auto-issue filing on top of that signal
# lands in #73.
on:
  schedule:
    # 06:00 UTC daily. Off-peak for US/EU/JP and keeps the
    # disclosure-to-detection window under ~24h. See § Schedule
    # cadence in 72-periodic-security-scan.md for rationale.
    - cron: '0 6 * * *'
  workflow_dispatch:

# `contents: read` is the only permission needed: checkout + read-only
# scans. The `issues: write` widening that #73 needs lives in #73,
# scoped to its single issue-filing step, NOT here.
permissions:
  contents: read

jobs:
  image-scan:
    runs-on: ubuntu-latest
    # Belt-and-suspenders mirror of ci.yml's image-scan job: even
    # though the workflow-level block already grants only
    # `contents: read`, a future top-level escalation must not
    # silently widen this job. Same convention as ci.yml.
    permissions:
      contents: read
    steps:
      # Explicit `ref: main` so a future change to the default-branch
      # name or a manual `workflow_dispatch` invocation from a feature
      # branch still scans `main` (the ticket's AC #2 names `main`).
      - uses: actions/checkout@v6
        with:
          ref: main
      - name: build image
        # Same artifact construction as ci.yml's image-scan job.
        run: docker build -t pyrycode-relay:${{ github.sha }} .
      # Tracks: aquasecurity/trivy-action@v0.36.0
      # Pinned by commit SHA so a tag-swap upstream cannot change
      # what runs against our image between Renovate bumps. Mirror
      # of ci.yml's image-scan pin — must move in lockstep with that
      # file when Renovate bumps the action.
      - name: trivy image scan
        uses: aquasecurity/trivy-action@ed142fd0673e97e23eac54620cfb913e5ce36c25
        with:
          image-ref: pyrycode-relay:${{ github.sha }}
          format: table
          severity: CRITICAL,HIGH
          ignore-unfixed: true
          vuln-type: os,library
          exit-code: '1'

  govulncheck:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v6
        with:
          ref: main
      - uses: actions/setup-go@v6
        with:
          go-version: '1.26.x'
      # Tracks: golang.org/x/vuln/cmd/govulncheck@v1.1.4
      # Mirror of ci.yml's `security` job pin — must move in lockstep
      # with that file when Renovate bumps the tag. Same rationale
      # for a semver tag vs commit SHA documented in #41's spec
      # (Go module checksum-DB makes a tag effectively immutable for
      # `go install`).
      - name: install govulncheck
        run: go install golang.org/x/vuln/cmd/govulncheck@v1.1.4
      - name: govulncheck
        run: govulncheck ./...
```

Two jobs, parallel on independent runners. No cross-job dependencies (`needs:`).

### Why two parallel jobs, not one sequential job

The `image-scan` step and the `govulncheck` step audit different surfaces and produce independent signals. Running them in one job would conflate the failure reason: a red row in the Actions list could mean either scanner; the operator has to open the run to see which. Two jobs give two distinct check entries, so "image had a CVE" vs "Go code had a reachable vuln" is visible from the Actions list itself. This mirrors the structure in `ci.yml` where `image-scan` and `security` are separate jobs.

### Refreshing the pins at implementation time

The two pinned references in this spec (`aquasecurity/trivy-action@ed142fd0…` and `govulncheck@v1.1.4`) are intentionally copied from `ci.yml`'s current state at spec-writing time. They MUST remain in lockstep with `ci.yml`. The developer at implementation time should:

1. Open `.github/workflows/ci.yml`.
2. Copy the Trivy action SHA from line 70 verbatim into the new workflow.
3. Copy the `# Tracks: aquasecurity/trivy-action@<tag>` comment from line 63 verbatim.
4. Copy the `@v1.1.4` version from line 43 verbatim into the new workflow.
5. Copy the `# Tracks: golang.org/x/vuln/cmd/govulncheck@v1.1.4` comment from line 35 verbatim.

If `ci.yml` has been updated by a Renovate bump between this spec being written and the implementation PR landing, the developer copies the *current* values from `ci.yml`, not the values pasted into this spec. The spec values are the snapshot at writing; `ci.yml` is the source of truth.

### Why not a reusable workflow / composite action

Three options were on the table:

1. **Duplicate YAML** (this spec's choice).
2. **Reusable workflow** — extract the `image-scan` job and the `govulncheck` portion of the `security` job into a `.github/workflows/_security-scans.yml` reusable, call it from both `ci.yml` and `security-scan.yml`.
3. **Composite action** — same idea at the action level.

Picked #1 for v1 because:

- **AC #5 Technical Notes explicitly says "optimize for clarity over DRY at v1."** This is the architect's standing instruction.
- **The duplication is ~25 lines per scanner.** Below the threshold where the abstraction pays for itself; a reader of either workflow can understand the whole file without jumping to a third file.
- **The reusable-workflow path requires deciding what the parameter surface is.** Both callers pass identical inputs today, so the parameter list would be empty — at which point the abstraction is pure indirection.
- **Renovate handles bumps fine across two files.** Renovate detects `uses:` and `go install …@vX.Y.Z` lines in any `.github/workflows/*.yml` file; a single bump produces one PR that updates both files atomically.

If a third caller appears (e.g. a per-release branch scan), or if the parameter surface diverges (e.g. cron wants `--show-traces` but PR-time doesn't), the abstraction is a follow-up ticket. Drift between the two pinned versions is the failure mode the lockstep convention guards against; if drift is observed in practice (e.g. someone bumps `ci.yml` and forgets `security-scan.yml`), that's the trigger for the reusable-workflow refactor.

### Out of scope

- **`gosec` in the cron workflow.** The PR-time `security` job runs both `gosec` and `govulncheck` (line 44-47 of `ci.yml`). The ticket body names only "the reachable-Go-vuln `govulncheck` scan from ci.yml's `security` job," not `gosec`. `gosec` is a static-analysis linter against the source tree; its findings depend on the *code*, not on disclosed CVEs against unchanged code. A periodic re-run against unchanged `main` produces the same `gosec` output every day until the code changes, which is the inverse of this ticket's value proposition. If the team later wants a periodic `gosec` cron, that's its own ticket.
- **Auto-issue filing on a red run.** Lives in #73 (depends on this ticket).
- **Slack/email notifications on red runs.** Out of scope for this ticket and arguably for #73 too — the auto-filed GitHub issue is the notification mechanism. Slack/email is a separate decision.
- **SARIF upload to GitHub Security tab.** Same deferral as in #68's spec — would require `security-events: write`, widening the minimum-permissions posture this ticket explicitly hardens. Follow-up if/when the team wants Security-tab visibility.
- **Per-release-branch scans.** Only `main` is scanned. The relay has no concept of long-lived release branches yet; revisit if it does.
- **Scan-skip on `paths:` or `paths-ignore:`.** Inapplicable to `schedule` triggers (they run on time, not on push diff). The workflow runs unconditionally on each cron tick.

## Concurrency model

Not applicable in the Go-concurrency sense. At the workflow level:

- **Two GitHub Actions jobs** (`image-scan`, `govulncheck`) run in parallel on independent ephemeral runners. No shared state.
- **Cron + workflow_dispatch overlap.** If a manual `workflow_dispatch` runs while a scheduled run is in flight (e.g. operator triggers a re-scan minutes before 06:00 UTC), GitHub Actions schedules both runs independently. Each gets its own runner; no shared filesystem; no race. Add no `concurrency:` block — the AC does not require it, and the cost of two parallel runs is two free-tier runner-minutes. If duplicate runs become a noise problem (e.g. after #73 lands and dedup matters), `concurrency: group: security-scan` is a one-line follow-up.

## Error handling

- **`docker build` fails** (e.g. base image pulled by digest is no longer fetchable). Loud, local, fails the `image-scan` job at the build step. The Trivy step does not execute. Same failure mode as PR-time.
- **Trivy DB download fails** (transient). Action's default behaviour: non-zero exit, step fails. Re-run from the Actions UI. Same as PR-time.
- **Trivy finds a fixable CRITICAL/HIGH.** AC #2: `exit-code: '1'` fails the job, CVE table prints to the step log, run goes red in the Actions list. Operator triage: bump the offending base-image digest (Renovate or manual) and verify the next cron tick is green. Without #73, there is no automatic owner assignment — that's accepted per AC #5.
- **Trivy finds an unfixable CRITICAL/HIGH.** `ignore-unfixed: true` keeps the job green; the CVE is informational in the step log. Same policy as PR-time per #68's rationale.
- **`go install` of `govulncheck@v1.1.4` fails** (proxy.golang.org outage, checksum mismatch, tag removed upstream). `go install` exits non-zero, job fails red, no fallback. Same failure mode as PR-time.
- **`govulncheck ./...` finds a reachable Go vuln.** Non-zero exit, job fails red, vuln summary in step log. Operator triage: bump the offending Go dependency and verify next cron tick. Same as PR-time.
- **Pinned Trivy action SHA stops resolving** (action repo deleted or force-pushed). GitHub Actions fails the step with a manifest-fetch error. Loud, no silent fallback. Same as PR-time; same fix (Renovate or manual bump).
- **Schedule fires while `main` is broken on an unrelated axis** (e.g. someone merged a Dockerfile that doesn't build). `docker build` fails in `image-scan`; `govulncheck` may still pass independently. The red row signals "main has a problem" — accurate, and the operator's next look at the run identifies which scanner / which step.

No new failure modes are introduced beyond what the PR-time equivalents already exhibit; each is a known shape with an established triage path.

## Testing strategy

AC #6 explicitly scopes verification to the green path; the vulnerable-state path is exercised by #73. Two verification steps live in the PR description, not as committed test fixtures.

1. **Manual `workflow_dispatch` against current `main` after merge** (AC #6). After the PR ships, the developer opens the Actions tab, picks the `security-scan` workflow, hits "Run workflow" on the `main` branch, and observes both jobs complete green. Both step logs should match what the equivalent PR-time jobs produced on the most recent merged PR (modulo CVE-database timestamps).
2. **Pre-merge dry run via on-branch trigger.** Optional but recommended: add a temporary `pull_request:` trigger to the file in the implementation PR, push, observe both jobs run green on the PR check list, then remove the `pull_request:` trigger before merging. This validates the workflow YAML parses and the two jobs wire up correctly without waiting until after merge. The temporary trigger MUST be removed before merge — leaving it in would re-run the periodic scan on every PR and inflate PR check noise.

   Alternative without trigger editing: push the workflow file to a branch and use `gh workflow run security-scan.yml --ref <branch>` — `workflow_dispatch` works on any branch that contains the file (the dispatch UI defaults to `main` but accepts a `--ref` override via the CLI). This avoids edit-then-revert churn.

Neither verification step requires a code change to `main`; both are exploratory invocations of the workflow YAML itself.

No unit tests; this is a CI workflow change.

## Open questions

- **Whether the lockstep convention should be enforced by a CI lint.** If `ci.yml`'s Trivy SHA and `security-scan.yml`'s Trivy SHA ever diverge silently, the two workflows scan against different scanner versions and "vulnerable at PR time" and "vulnerable at cron time" mean different things. A two-line lint (`grep -E 'trivy-action@[a-f0-9]{40}' .github/workflows/{ci,security-scan}.yml | sort -u | wc -l` must equal 1) is the deterministic safety net per the playbook's "belt-and-suspenders means different fabric" principle. **Deferred** to a follow-up: this ticket's value lands without the lint, and the failure mode (silent drift) hasn't been observed yet. Per the playbook's *Evidence-Based Fix Selection* — don't ship a defense for a failure mode that hasn't happened. If a Renovate bump in practice updates one file but not the other, that's the trigger.
- **Whether the reusable-workflow refactor pays off now.** Considered and deferred to clarity-over-DRY (§ Why not a reusable workflow). Re-evaluate if a third caller or a divergent parameter surface appears.
- **Cron cadence tuning.** Daily is the v1 choice; if it generates too much runner-time pressure on the org's free-tier minutes (it won't at this volume, but worth naming), `0 6 * * 1` (weekly Monday) is a one-line tweak. No decision needed until usage signal exists.
- **Timezone of the cron expression.** GitHub Actions cron is UTC-only; no timezone parameter. `0 6 * * *` is anchored at UTC. If the team wants a different anchor (e.g. always pre-Monday-morning-standup), the cron expression encodes that — no design change needed.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings. The workflow has two trust boundaries: (a) GitHub's scheduler firing the cron tick, and (b) the Trivy action + Go module proxy as upstream code-execution surfaces. Both are pre-existing trust boundaries inherited from `ci.yml`; neither is widened by this ticket. The `actions/checkout@v6` step reads `main` — a branch only writable via the normal PR review flow — so the checked-out content is the same content the existing `ci.yml` scans on every push to main.
- **[Tokens, secrets, credentials]** No findings. Workflow-level `permissions: contents: read` and job-level `permissions: contents: read` on each job — strictly read access. `GITHUB_TOKEN` is implicitly granted at this minimum and is used only by `actions/checkout` to fetch the repo; the token is never read, logged, compared, or passed downstream. No registry pushes (`docker build` only, no `docker push`). No secrets-context variables (`${{ secrets.* }}`) referenced anywhere. The `issues: write` permission needed for #73's auto-filing is explicitly deferred to that ticket and scoped to that step — confirmed against AC #4 and #73's body.
- **[File operations]** No findings. The workflow runs on GitHub-hosted ephemeral runners with a per-run filesystem. No paths cross from user-controlled input (no `workflow_dispatch.inputs.*`). No writes to repo-tracked paths. `docker build` and `go install` write only to runner-local locations (`/var/lib/docker`, `$GOPATH/bin`) discarded at runner teardown.
- **[Subprocess / external command execution]** No findings. Two shell invocations: `docker build -t pyrycode-relay:${{ github.sha }} .` and `go install golang.org/x/vuln/cmd/govulncheck@v1.1.4` and `govulncheck ./...`. Each is a fixed command string with one GitHub-provided interpolation (`${{ github.sha }}` — a 40-char hex SHA, shell-safe). No `sh -c`, no user-controlled values, no environment-variable expansion of attacker-influenced content. The Trivy action runs inside its own container; YAML inputs (`image-ref`, `format`, etc.) are passed as action inputs, not shell-interpolated.
- **[Cryptographic primitives]** Not applicable directly. The workflow inherits two pre-existing crypto controls — Trivy action commit-SHA pin (immutable git ref) and `govulncheck` semver pin verified by Go's module checksum database (`sum.golang.org`). No new crypto is introduced; both are the same controls in force at PR time.
- **[Network & I/O]** No findings. The workflow's outbound network egress: `github.com` (checkout), `proxy.golang.org` + `sum.golang.org` (`go install`), Trivy action's bundled vuln-DB registry (action default), and the Docker daemon's local socket (`docker build` against the Dockerfile's pinned base-image digests). Each destination is identical to what `ci.yml` already reaches; cron simply triggers the same egress on a schedule. No listener is opened. No user-controlled URLs.
- **[Error messages, logs, telemetry]** No findings. CVE-finding logs contain CVE IDs, package names, severity, and fixed-version metadata — all public NVD-style data. `go install` and `govulncheck` step logs contain version strings and module paths — all public. No secrets, no internal relay state, no payloads (the relay's runtime traffic is not exposed to this workflow — it operates against build-time artifacts only). Log audience is anyone with read access to the Actions tab, which on a public repo is the world; that audience is the intended audience for a CVE-disclosure signal.
- **[Concurrency]** Not applicable. Two parallel jobs on independent runners with no shared filesystem, no shared service, no shared state. Overlapping `schedule` + `workflow_dispatch` invocations create independent runs on independent runners; no race.
- **[Threat model alignment]** Addresses the residual-risk paragraph in `docs/threat-model.md` § *Supply chain — Go dependencies* — specifically the post-merge invisibility window for CVEs disclosed against unchanged deps. The threat-model prose names PR-time scanning as the control; this ticket adds the periodic complement and closes the window from "until the next dependency bump" to "≤24h." The threat-model document itself does NOT need editing this ticket — the existing prose is about the control's *intent* (gate merges on known-vuln deps), and the periodic re-scan reinforces the same intent on a different cadence. A documentation-phase pass can refresh the section to cite both #68/#41 (PR-time) and #72/#73 (periodic) as a single supply-chain section if/when #73 also lands.

**Adversarial scenarios considered:**

- *Attacker bumps the Trivy SHA in `security-scan.yml` to a malicious commit while leaving `ci.yml` untouched.* Caught at code review on the PR diff — the lockstep convention makes a divergent SHA a visible smell (and the architect spec's § *Refreshing the pins* + § *Open questions* both name the lint that would mechanise the check). The current safety is reviewer attention; this is the same residual risk #68 names for `ci.yml`.
- *Attacker triggers `workflow_dispatch` repeatedly to exhaust free-tier runner minutes.* `workflow_dispatch` is restricted to users with `write` access to the repo (GitHub's built-in policy); a hostile maintainer is a wholly different threat model the relay's policy explicitly does not defend against (per `docs/threat-model.md`'s scope). External users cannot trigger `workflow_dispatch`.
- *Attacker pushes a commit to `main` between cron tick and `actions/checkout`.* The checkout pins `ref: main` resolved at checkout time, so a race here means the scan runs against the new HEAD — which is the desired behaviour (always scan the freshest `main`). There is no security regression; the scan target is more current, not less.
- *Schedule fires while the GitHub Actions service is degraded and the run silently fails to start.* GitHub's status page is the operator's signal; this is outside the relay's control. Not a defect of this design. If "the cron didn't fire" becomes an observed failure mode, a separate "cron-of-the-cron" health check is a follow-up; until then, evidence-based-fix-selection says don't pre-build it.
- *Renovate bumps the Trivy SHA in only one file due to a parsing bug, drifting `ci.yml` and `security-scan.yml`.* This is the silent-drift case the § Open questions lint addresses. Defended-in-depth by: (a) the `# Tracks:` comment on each pin (reviewer can sanity-check), (b) the spec's lockstep convention (named explicitly, so a reviewer or future architect can cite it), and (c) the deferred lint as the deterministic safety net once observed. Pre-shipping the lint without observed drift is the playbook's anti-pattern (`Evidence-Based Fix Selection`).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-12
