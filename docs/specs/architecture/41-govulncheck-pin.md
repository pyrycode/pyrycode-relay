# Spec: pin `govulncheck` install version in `ci.yml` (#41)

## Files to read first

- `.github/workflows/ci.yml:33-40` — the `security` job's `install govulncheck` step. The single line that changes is line 36. Note the `gosec` install on line 34 also uses `@latest`; explicitly out of scope per the ticket body.
- `.github/workflows/ci.yml:42-77` — the adjacent `image-scan` job (#68) demonstrates the established pinning convention for this repo: an immutable reference plus a `# Tracks:` comment naming the upstream tag a reviewer can sanity-check. The govulncheck pin mirrors that posture in the form `go install` supports.
- `docs/specs/architecture/68-trivy-image-scan.md` § *Pinning the action* — the rationale this spec inherits: pin to an immutable reference, comment the tracked tag in lockstep, refresh both atomically on Renovate bumps.
- `docs/threat-model.md` § *Supply chain — Go dependencies* (lines 22-30) — names `govulncheck` as the gate for `make lint` + merge. The pin makes that gate's behaviour deterministic across runs; the threat-model prose itself is unchanged by this ticket (the residual-risk paragraph already names malicious-authentic-maintainer release as the gap that remains, which a version pin does not close — Renovate does).

No Go source is touched. Codegraph is not the right tool here; the artifact under test is a single YAML line.

## Context

#68 shipped a Trivy image-scan job pinned by commit SHA, on the principle that *the supply-chain scanner is itself part of the supply chain and must not float on a moving reference*. The same logic applies one-for-one to `govulncheck`, but the install step was left at `@latest` from the initial scaffold (commit e8a91b6).

`@latest` on a `go install` line means every CI run resolves the version at run time against `proxy.golang.org`. A compromised or buggy upstream release would silently land in CI on the next workflow trigger with no code change, no PR, and no reviewer signal. For a tool whose entire job is to flag supply-chain risk on an internet-exposed service, this is the exact anti-pattern the tool is meant to guard against.

The fix is mechanical: replace `@latest` with an explicit semver tag. Renovate is already configured for this repo (per #32 / #68 posture) and will surface future bumps as ordinary PRs in the same flow as Dockerfile digest bumps.

## Design

### Scope

Edit one line in `.github/workflows/ci.yml`. No other files touched. No `.renovaterc` change required — Renovate's default `gomodTags` / GitHub Actions managers already track `go install` lines with explicit versions; only `@latest` was opaque to it.

### The change

Line 35-36 currently reads:

```yaml
      - name: install govulncheck
        run: go install golang.org/x/vuln/cmd/govulncheck@latest
```

Replace with:

```yaml
      # Tracks: golang.org/x/vuln/cmd/govulncheck@v1.1.4
      # Pinned to an explicit version so a compromised or buggy upstream
      # release cannot silently land in CI between Renovate bumps. The
      # tracked tag in this comment must move in lockstep with the
      # `@vX.Y.Z` below — same convention as the Trivy action pin in the
      # `image-scan` job (#68) and the Dockerfile base-image digest pins
      # (#32).
      - name: install govulncheck
        run: go install golang.org/x/vuln/cmd/govulncheck@v1.1.4
```

### Why a semver tag, not a commit SHA or pseudo-version

The Trivy job (#68) pins by commit SHA because GitHub Action `uses:` references can be tag-swapped by a malicious maintainer pointing the tag at a different commit. The Go module ecosystem has a stronger guarantee: every published module version, including every semver tag, is content-addressed in `go.sum` once observed and immutably mirrored by `proxy.golang.org` under the [Go module checksum database](https://sum.golang.org). A tag swap on `golang/vuln` does not change what `go install …@v1.1.4` resolves to once `proxy.golang.org` has cached the original — the checksum DB would reject the swap.

This means a semver tag *is* an immutable reference for `go install`, given:

- The build runs against `proxy.golang.org` (default; the workflow does not set `GOPROXY=direct`).
- The build does not set `GOSUMDB=off` (it doesn't).

Both hold for this workflow. So `@v1.1.4` is materially equivalent to a commit SHA here, with the readability advantage of naming the version explicitly. Renovate also tracks semver tags more cleanly than pseudo-versions or SHAs.

### Choice of version

`v1.1.4` is the current latest stable release of the `golang/vuln` repository's `govulncheck` binary (as of 2026-05-12; verified via the GitHub releases API). It is the version the ticket body cites as an illustrative example. The developer at implementation time should:

1. Visit `https://github.com/golang/vuln/releases` and confirm `v1.1.4` (or a newer stable tag) is the current latest non-prerelease.
2. If a newer tag exists, bump to it in both the `run:` line and the `# Tracks:` comment, and note the version chosen in the PR description so the reviewer can sanity-check.
3. The same `govulncheck ./...` invocation works across the entire `v1.x` series — no flag changes between `v1.0` and `v1.1.4`. Any current `v1.x` release is acceptable.

Pinning to the *current* latest at PR time, rather than the spec-time `v1.1.4`, is preferred — it avoids landing pre-aged and minimises the immediate Renovate churn.

### Out of scope

- **`gosec` on line 34.** Same `@latest` anti-pattern; explicitly deferred to a follow-up ticket per the ticket body's *Technical Notes*. Do not touch it in this PR.
- **`setup-go` action version.** Already pinned (`actions/setup-go@v6`); no work required.
- **`go install` cache layer.** Could speed up the security job, but is independent of the pinning concern and adds unrelated YAML. Out of scope.
- **Periodic re-scan-on-cron.** Lives in #69.
- **Image-side CVE scanning.** Lives in #68 (already shipped).

## Concurrency model

Not applicable. Single YAML line edit; no runtime code paths affected.

## Error handling

- **Pinned version no longer exists / module proxy removes it.** `go install` fails loudly with a non-zero exit and a `proxy.golang.org` error message; the `security` job goes red. Loud and local — no silent fallback, no retry-with-`@latest`. This is the *desired* failure mode: a removed upstream release should block merges until a human investigates and bumps the pin, not silently degrade to "whatever's current."
- **Checksum-DB mismatch (theoretical tag swap).** `go install` fails with a `verifying module: checksum mismatch` error. Same loud failure as above; same correct response (human investigation).
- **`govulncheck ./...` itself finds a reachable CVE.** Pre-existing behaviour, unchanged by this ticket — non-zero exit fails the job, CVE summary prints to the step log, PR check goes red. Fix is the same as today: update the offending Go dependency, push again.

## Testing strategy

The acceptance criteria are self-verifying via the CI run on the PR that ships this change:

1. **Positive (the ticket's AC #2).** Open a PR with only the line-36 change. The `security` job runs, installs `govulncheck@v1.1.4`, and `govulncheck ./...` completes successfully against the current `go.mod`. PR check goes green. This proves the pinned tag exists, is fetchable through `proxy.golang.org`, and is functionally compatible with the current source tree.
2. **Spec-time confirmation that the tag exists.** Architect-side, the GitHub releases API was queried (`api.github.com/repos/golang/vuln/releases`) and `v1.1.4` appears as a published, non-prerelease tag. The `proxy.golang.org` version listing also includes `v1.1.4`. The developer should re-verify at implementation time per § *Choice of version* in case a newer tag is preferred.

No unit tests; this is a CI workflow change. No `make` target change; the existing `make lint` invokes `govulncheck` from the developer's local install path, not from CI's pinned install, and is intentionally left alone (local-side pinning is a separate concern with different ergonomics).

## Open questions

- **Whether to also pin via `tools.go` and `go.mod`.** The Go-idiomatic alternative is to add `_ "golang.org/x/vuln/cmd/govulncheck"` to a `tools.go` file under a `//go:build tools` constraint, list it in `go.mod`, and run `go install` against the `go.mod`-resolved version (`go install golang.org/x/vuln/cmd/govulncheck` with no `@version`). This puts the pin in the same file Renovate already tracks for runtime deps and removes the duplicate-source-of-truth in the workflow. Considered and **deferred**:
  - The ticket scopes the change to `ci.yml` line 36 and AC#1 names the workflow line explicitly. Introducing a `tools.go` is a structurally larger change (new file, build-tag conventions, `go.mod` churn) than the AC permits.
  - The repo currently has no `tools.go` pattern established; adopting it is a project-wide convention worth its own ticket (which would also cover `gosec`, and any future `goimports` / `mockgen` / etc.).
  - Surface this as a follow-up if/when a third tool needs pinning; two pins inline is below the threshold where the abstraction pays for itself.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** Addressed by the change itself. Before: each CI run trusted whatever `proxy.golang.org` returned as `latest` for `golang.org/x/vuln/cmd/govulncheck` at workflow-trigger time. After: each CI run trusts the explicit `v1.1.4` artifact, whose content is checksum-verified against `sum.golang.org` (the workflow does not disable `GOSUMDB`). The boundary tightens; it does not widen. The supply-chain risk that *remains* — a malicious release tagged by an authentic upstream maintainer — is the same residual risk named in `docs/threat-model.md` § *Supply chain* and is unchanged by this ticket; the threat-model prose remains accurate.
- **[Tokens, secrets, credentials]** No findings. The `security` job's permissions are governed by the workflow-level `permissions: contents: read` block (line 8-9); no job-level escalation. No tokens are minted, read, or compared by `go install` or `govulncheck`. `GOPROXY` traffic uses no credentials.
- **[File operations]** No findings. `go install` writes to `$GOPATH/bin` inside the ephemeral runner. No repo-tracked paths are touched.
- **[Subprocess / external command execution]** The `run:` line is a fixed shell command with no `${{ … }}` interpolation; no shell-injection surface. The pinned version string is a YAML literal, not a user-controlled value. The change strictly *reduces* the set of binaries that can execute in this step (one specific build of `govulncheck` vs. "whatever's latest").
- **[Cryptographic primitives]** Not applicable directly. The pinning posture leans on the Go module ecosystem's pre-existing crypto primitives — module-tree hashes verified against `sum.golang.org` — which are operationally sufficient for this control. No new crypto is introduced by this ticket.
- **[Network & I/O]** Unchanged in shape: the `go install` step still fetches one module from `proxy.golang.org`. The change replaces a moving target (`@latest`) with a fixed target (`@v1.1.4`); the network egress count and destinations are identical. No new listener, no new outbound destination.
- **[Error messages, logs, telemetry]** No findings. `go install` and `govulncheck` step logs contain version strings, module paths, and CVE-database metadata — all public. No secrets, no internal state. Log audience is the PR's check viewers, which is the intended audience.
- **[Concurrency]** Not applicable. Single-step change inside an already-sequential job.
- **[Threat model alignment]** Closes a concrete delta against `docs/threat-model.md` § *Supply chain — Go dependencies*: that section names `govulncheck` as the merge gate, which is load-bearing only if `govulncheck` itself is deterministically the audited version. The threat-model prose itself does not need editing — it does not claim a property the pre-pin code failed to deliver; it names a control and this ticket tightens that control's implementation. A future pass that documents the pinning convention holistically (Trivy SHA + govulncheck semver + Dockerfile digests + Go module checksums) could cite all four as a single section; that is a documentation-phase concern, not an architecture one.

**Adversarial scenarios considered:**

- *Attacker tampers with `proxy.golang.org` to serve a malicious `v1.1.4`.* Defeated by `sum.golang.org` checksum verification on the runner; `go install` aborts with a mismatch error and the job fails red. Same trust model as every other `go install`.
- *Attacker compromises the `golang/vuln` maintainer's signing key and publishes a malicious `v1.1.5`.* Not blocked by this pin per se — `v1.1.4` still resolves correctly. Renovate would surface `v1.1.5` as a PR; the reviewer is the line of defence at that point. This is the residual-risk paragraph the threat model already names.
- *Attacker submits a PR that bumps the pin to a known-bad version.* Caught at code review on the PR diff. The `# Tracks:` comment plus the explicit `@v1.1.4` makes the bump self-describing; an inconsistent comment-vs-version pair (e.g. comment says `v1.1.4`, command says `v0.0.0-20231115...`) is an obvious smell at review time. This is the reason the `# Tracks:` comment is required, not optional.
- *Attacker submits a PR that re-introduces `@latest`.* Caught at code review. Worth a follow-up if a programmatic enforcer (a CI lint that rejects `@latest` in `.github/workflows/*.yml`) is wanted; out of scope here per the one-concern-per-ticket rule.

**Reviewer:** architect (self-review per the architect playbook's security-review step)
**Date:** 2026-05-12
