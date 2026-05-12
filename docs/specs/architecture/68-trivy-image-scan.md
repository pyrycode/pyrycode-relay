# Spec: PR-time Trivy image CVE scan in `ci.yml` (#68)

## Files to read first

- `.github/workflows/ci.yml:1-41` — the workflow this ticket extends. Note the workflow-level `permissions: contents: read` (line 8-9) and the two existing jobs: `test` (Go build + test) and `security` (gosec + govulncheck). The new image-scan job sits alongside them.
- `Dockerfile:1-56` — the artifact the scan targets. Build-stage base is `golang:1.26-bookworm@sha256:…` (line 7); runtime base is `gcr.io/distroless/static-debian12:nonroot@sha256:…` (line 33). The runtime layer is what the AC cares about ("base-image system libraries").
- `docs/specs/architecture/32-dockerfile-base-hardening.md` § *Pinning the digests*, § *Testing strategy* — establishes the `# Tracks: <tag>` comment pattern this spec mirrors for the action pin, and explicitly defers image-layer scanning to a future ticket (this one).
- `docs/threat-model.md:22-28` § *Supply chain — Go dependencies* — the existing supply-chain posture. The new scan is the runtime-image complement to `govulncheck` (audits Go source) and adds OS-package CVE coverage that `govulncheck` cannot see.
- `README.md:31-38` § *Docker* — the build invocation users run locally; the CI step mirrors it (`docker build`, no push).
- `docs/PROJECT-MEMORY.md` § *Patterns established* — for the loud-failure + pin-by-digest + comment-the-tracked-tag conventions.

No Go code is touched; codegraph is not the right tool for this ticket. The artifact under test is a YAML workflow + the existing `Dockerfile`.

## Context

#32 shipped a Dockerfile with both base images digest-pinned, and explicitly deferred image-layer CVE scanning ("Image-layer scanning (Trivy / Grype) lands in a separate ticket per the issue's *Out of scope* note"). That follow-up is #68.

The relay is internet-exposed and the image is part of the supply chain. Digest pinning protects against *tag-swap* attacks (a malicious registry actor replacing what `golang:1.26-bookworm` points to), but does nothing about CVEs *disclosed against the pinned content itself* — those accumulate the moment the digest is taken and grow until the next Renovate bump. PR-time scanning is the chokepoint that surfaces fixable CVEs before merge.

The Go-source half (`govulncheck` against reachable Go vulns) already runs in the `security` job (line 39-40) and is covered by #41. This ticket adds the OS-package half. The two scanners look at different artifacts and flag different things:

- `govulncheck` → reachable vulns in Go modules linked into the binary.
- Trivy image scan → CVEs in OS packages installed in the runtime image (distroless: `base-files`, `ca-certificates`, `netbase`, `tzdata`, `libc6-compat`-style minimal set), plus any Go-binary vulns Trivy can re-derive from the binary itself.

Overlap on the Go side is intentional belt-and-suspenders: `govulncheck` cares about source + reachability; Trivy cares about what physically shipped in the image. They will sometimes flag different sets even for the same underlying Go module — that's the design, not a bug.

The companion **periodic re-scan** (catches CVEs disclosed after a PR merges, against bases that haven't changed) is split out to a follow-up ticket and depends on this one landing.

## Design

### Scope

Edit one file: `.github/workflows/ci.yml`. Add one new job: `image-scan`. No other files touched (no `.trivyignore` — per AC, that scaffold is only added if a real false-positive surfaces during initial wiring).

### Why a new job rather than extending `test` or `security`

- `test` builds the host binary (`go vet`, `go test`, `make build`); it has no Docker context. Pulling `docker build` into it would inflate the most-frequent job's runtime for every contributor on every PR.
- `security` runs Go-source scanners (`gosec`, `govulncheck`). Mixing image-layer work alongside source-layer work in one job conflates two distinct review surfaces and makes a single red ✗ ambiguous in the PR check list.
- A dedicated `image-scan` job gives one clear failure signal ("the image has fixable CRITICAL/HIGH CVEs"), can be rerun in isolation, and lets future image-side work (Cosign verification, SBOM upload, periodic re-scan) bolt on without disturbing `test` or `security`.

The three jobs run in parallel; total wall-clock for the slowest job is unchanged.

### New job: `image-scan`

Appended after the `security` job in `ci.yml`:

```yaml
  image-scan:
    runs-on: ubuntu-latest
    # Belt-and-suspenders: the workflow-level permissions block (line 8-9)
    # already grants only `contents: read`, but a future top-level
    # escalation would silently widen this job's scope without the
    # explicit declaration. Pin the minimum here so a regression in the
    # workflow header doesn't grant the scanner write tokens.
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v6

      - name: build image
        # The scan operates on the same artifact a PR would publish.
        # Plain `docker build` (no buildx, no GHA cache layer) keeps the
        # step count minimal — the relay binary is small, the build is
        # a few seconds, caching would add more YAML than it saves.
        run: docker build -t pyrycode-relay:${{ github.sha }} .

      # Tracks: aquasecurity/trivy-action@<tag>
      # Pinned by commit SHA so a tag-swap upstream cannot change what
      # runs against our image between Renovate bumps. Refresh the
      # comment in lockstep with the SHA so a reviewer can sanity-check
      # which release the digest refers to. Same convention as the
      # Dockerfile base-image digest pins (#32).
      - name: trivy image scan
        uses: aquasecurity/trivy-action@<COMMIT-SHA>
        with:
          image-ref: pyrycode-relay:${{ github.sha }}
          format: table
          severity: CRITICAL,HIGH
          # Only fixable CVEs fail the build. CVEs with no upstream
          # patch produce no actionable work and would block every PR
          # until someone unrelated to the change fixes them — exactly
          # the failure mode AC #2 prohibits.
          ignore-unfixed: true
          # Cover both OS packages (distroless's small package set) and
          # language manifests / Go binary content Trivy can re-derive.
          # Some overlap with #41's govulncheck is intentional — see
          # § Context.
          vuln-type: os,library
          exit-code: '1'
```

### Pinning the action

The AC requires the action be pinned to a specific version (commit SHA or version tag), not floating on `@main` / `@latest`. Two choices:

- **Commit SHA** (e.g. `@abc123def456…`) — fully immutable; survives a tag re-point.
- **Version tag** (e.g. `@0.28.0`) — readable, but a malicious or compromised maintainer can re-publish the tag.

Pick **commit SHA** for parity with the Dockerfile's digest-pin posture (#32). The SHA is the load-bearing part; the `# Tracks: aquasecurity/trivy-action@<tag>` comment alongside names the upstream tag the SHA was taken from, so a reviewer can sanity-check that a Renovate bump's new SHA corresponds to the new tag.

Developer steps at implementation time:

1. Go to `https://github.com/aquasecurity/trivy-action/releases`, pick the latest stable release tag (current as of this spec's writing: a `0.x.y` series; the developer takes whatever is current at implementation time).
2. Resolve the tag to a commit SHA: `git ls-remote https://github.com/aquasecurity/trivy-action refs/tags/<tag>` — the first 40-char hex is the SHA.
3. Paste the SHA into `uses:` and the tag into the `# Tracks:` comment.
4. Verify the chosen version exposes the four inputs this spec uses: `image-ref`, `format`, `severity`, `ignore-unfixed`, `vuln-type`, `exit-code`. All have been stable inputs since at least the 0.16 series; any current release is fine.

Renovate is already enabled for this repo (per the ticket body and #32's posture), so future bumps surface as PRs in the same flow as the Dockerfile base bumps.

### Tagging the built image

AC #1 requires the just-built image be tagged `${{ github.sha }}`. `docker build -t pyrycode-relay:${{ github.sha }} .` satisfies this. The image is not pushed — the scan reads it from the local Docker daemon (`aquasecurity/trivy-action` defaults to local image lookup for unqualified refs).

Two-tag belt-and-suspenders (`pyrycode-relay:dev` + `pyrycode-relay:${{ github.sha }}`) is unnecessary; one tag suffices for the scan step. Keep it minimal.

### Why `ignore-unfixed: true`

AC #2 is unambiguous: "CVEs without a known fix do not fail the build." Three reasons this is the right policy, not just a literal AC read:

1. **Actionability.** An unfixed CRITICAL has no upstream patch the relay can pull. Failing PRs on unactionable findings produces dev friction with no security benefit — the only "fix" available is `.trivyignore`, which the AC explicitly says shouldn't be pre-seeded.
2. **Renovate is the right tool for unfixed CVEs.** When upstream ships a fix, Renovate bumps the base-image digest and the next PR's scan auto-rediscovers the CVE as *fixable* — the build then fails until the bump is merged. Loud failure at the right moment.
3. **Reduces noise pressure on `.trivyignore`.** Pre-seeding suppressions for unfixed CVEs is a worn-down door — the first one teaches the team to suppress, and discipline erodes from there. Defaulting to "unfixed = noisy, ignore at scanner level" keeps the allow-list pristine for genuine false-positives.

### Why `vuln-type: os,library`

`os` is the headline target — distroless's small package set, plus any future swap to a less-minimal base. `library` adds language-manifest scanning; for a static Go binary on distroless, this primarily means Trivy's Go-binary scanner re-deriving module info from the embedded build info. Some overlap with `govulncheck` (#41) is intentional:

- `govulncheck` runs against source; it sees what *could* be reached.
- Trivy runs against the built artifact; it sees what *was actually linked in*.

These set up two slightly different views of the same surface; either may flag a vuln the other misses (different vuln-db sources, different reachability heuristics, different inclusion rules). For an internet-exposed relay, the redundancy is worth a few seconds of CI time.

### Permissions

Job-level `permissions: contents: read`. The workflow header already declares this at the top, so the job-level block is belt-and-suspenders against a future top-level widening that wouldn't notice this job. Costs three lines; pays out the moment someone adds `id-token: write` at the workflow level for a separate concern.

No write tokens needed — the scan only reads the local image and the action's bundled vuln DB.

## Concurrency model

Not applicable. The new job is a single sequential pipeline of steps. The three CI jobs (`test`, `security`, `image-scan`) run in parallel on independent runners; they share no state.

## Error handling

- **`docker build` fails.** Loud, local, fails the job at the build step (red ✗ on the `build image` step). Pre-existing failure mode; the scan step doesn't execute.
- **Trivy DB download fails** (transient network issue). The action's default behaviour is to fail the step with a non-zero exit. Re-run from the GitHub Actions UI; no permanent state to clean up.
- **Trivy finds a fixable CRITICAL/HIGH.** AC-defined behaviour: job fails with `exit-code: 1`, the CVE table prints to the step log, the PR check goes red. The fix is to bump the offending base-image digest (Renovate-driven or manual) and re-push.
- **Trivy finds an unfixable CRITICAL/HIGH.** Surfaced as informational in the step log (Trivy still prints them), does not fail the job (`ignore-unfixed: true`). The PR proceeds; the residual CVE is the team's risk-acceptance signal.
- **Action SHA pin no longer resolves** (action repo deleted, force-pushed, etc.). GitHub Actions fails the step with a manifest-fetch error. Loud, local, no silent fallback. Treat the same as a Dockerfile base-image digest going stale: investigate and bump.

## Testing strategy

Two AC-defined verification paths the developer runs after wiring:

1. **Known-good build (positive).** Push the implementation as a normal PR. The `image-scan` job should pass with green status; the step log should show a Trivy report ending with `Total: 0` (or 0 in the CRITICAL/HIGH severity columns) in the fixed-CVE counts. Existing `Dockerfile` digests are recent enough that this is the expected outcome.

2. **Vulnerable image (negative).** On a throwaway branch, temporarily change `Dockerfile`'s build-stage `FROM` line to a known-old tag (e.g. `golang:1.16` or earlier — predates many years of CVE fixes), push, observe the workflow fails on the new job with the CVE list visible in the step log. Revert that commit before merging.

Both verification steps live in the PR description / completion notes, not as committed test fixtures — the negative case requires a deliberately-broken Dockerfile, which must not land on `main`.

No unit tests; this is a CI workflow change, not Go code. The job's existence + green/red status on the PR check list is the test signal.

### Why not also wire `make scan-image` as a local target

Tempting parity with the existing `make lint` (which runs the source-side scanners locally). Out of scope:

- Trivy isn't a Go binary installable via `go install`; it requires either a brew tap, a curl-pipe-sh installer, or a docker invocation. Adding a Makefile target means adding install instructions to the README, which adds a Trivy dependency for every contributor running `make lint` locally — many of whom don't build the image.
- The CI gate is the load-bearing part. Local convenience can land in a follow-up if contributors ask.

If a future ticket adds local-side image scanning, it can mirror this CI job's flags exactly so the two stay in sync.

## Open questions

- **Current Trivy action commit SHA.** Resolved by the developer running `git ls-remote` at implementation time (see § *Pinning the action*). Not a design question; just a transient lookup.
- **Whether to upload SARIF / GitHub code-scanning results.** Out of scope. Trivy supports `format: sarif` + `github/codeql-action/upload-sarif` for the Security tab. Considered and deferred: it requires `security-events: write` at the job permission level (widens the minimum-permissions posture this ticket explicitly hardens), and it conflates "fail the build on findings" with "publish findings to the org security dashboard" — different audiences, different cadences. If the team later wants Security-tab visibility, that's a follow-up ticket whose permission widening is debated in its own AC, not bundled here.
- **Whether to scan the build stage too.** No. The build stage's filesystem is discarded at the multi-stage boundary; only the runtime stage ships. Trivy scans the *image* (one layer chain, runtime only). The build stage's `golang:1.26-bookworm` base may carry CVEs that never reach production — auditing them is noise.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings. The scan operates on an artifact built from the repo's own `Dockerfile`; no external untrusted inputs cross into the workflow at scan time. The Trivy action itself is a trust boundary, mitigated by commit-SHA pinning + the `# Tracks:` comment so a malicious SHA swap is reviewable. Vuln-database downloads are sourced from the action's bundled defaults (ghcr.io/aquasecurity registries); a compromised DB could *miss* CVEs but cannot *invent* false positives that block PRs — the failure mode is silent under-reporting, which Renovate-driven base bumps would surface anyway on the next legitimate scan after the DB recovers.
- **[Tokens, secrets, credentials]** No findings. The job runs with `permissions: contents: read` only. No tokens are minted, stored, logged, or compared. `GITHUB_TOKEN` is not used by any step. The image is not pushed; no registry credentials are involved.
- **[File operations]** No findings. The job operates inside the GitHub-hosted runner's ephemeral filesystem. No paths cross from user input. No writes to repo-tracked paths.
- **[Subprocess / external command execution]** No findings. `docker build .` takes no user-controlled values; the `${{ github.sha }}` interpolation is GitHub-provided and shell-safe (40 hex chars). The Trivy action runs in its own container; no shell-interpretation surface in the workflow YAML.
- **[Cryptographic primitives]** Not applicable. The job performs no cryptographic operations; the Trivy action's internal vuln-DB signature verification is upstream concern.
- **[Network & I/O]** No findings. The job pulls the Trivy vuln DB from the action's default registries (one-time per run). No listener is opened. No user-controlled network paths.
- **[Error messages, logs, telemetry]** No findings. The CVE report printed to the step log contains CVE IDs, package names, severity, and fixed-version metadata — all public information from the NVD-style sources. No secrets, no internal state, no payloads. The log is visible to anyone who can view the PR's checks, which is the intended audience (maintainers reviewing the failure).
- **[Concurrency]** Not applicable. The job is a single sequential step chain on a single runner; no shared state with other jobs.
- **[Threat model alignment]** Addresses `docs/threat-model.md` § *Supply chain — Go dependencies* — specifically the residual-risk paragraph's gap that `go.sum` doesn't defend against vulns in the *runtime image's OS packages*. The new job closes that gap for OS packages and adds a second view onto Go-module vulns (complementing `govulncheck`). The image-layer scan is now a named control in the supply-chain section; the threat model itself does NOT need editing this ticket (no new threat introduced; an existing residual-risk paragraph already names this control's absence as a gap, and that paragraph remains accurate prose even after the gap closes — the residual risk it describes (malicious authentic-maintainer release) is unchanged by this ticket). A future ticket can refresh that paragraph if/when periodic re-scan also lands and the supply-chain posture is described holistically.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-12
