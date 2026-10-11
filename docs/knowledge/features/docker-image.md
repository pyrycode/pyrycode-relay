# Docker image — portable deploy artifact

Host-agnostic OCI image for the relay, produced by a multi-stage `Dockerfile` at the repo root. Image-layer hardening (small base, no shell, no package manager, non-root, digest-pinned bases, stripped static binary) is defence-in-depth on top of the runtime hardening already in place (autocert cache permission check, slow-loris timeouts, header-gate before WS upgrade, 256 KiB frame cap, opaque payload routing).

The image is intentionally **portable, not deployable on its own**: it exposes both `:80` and `:443` and declares a volume mount at `/var/lib/relay/autocert`, but TLS termination policy, port publishing, volume backing, single-instance enforcement, and healthcheck wiring are decisions the host manifest owns. The host wiring landed as `fly.toml` + CI auto-deploy in #38 ([Feature: Fly.io deploy](fly-deploy.md)); single-instance binary-level self-check in #65 (#39 / #42 deferrals).

## Build and verification

```bash
docker build -t pyrycode-relay:dev .
docker run --rm pyrycode-relay:dev --version   # zero-exit no-network startup form
```

The image is invoked with `--version` for smoke tests; `--help` exits `2` (Go's `flag` package treats usage prints as errors), and the bare `pyrycode-relay` invocation also exits `2` (missing required `--domain` / `--insecure-listen`). `--version` is the only zero-exit no-network startup form.

## Structure

### Build stage

`golang:1.26-bookworm@sha256:…` (digest-pinned). Tag tracks the Go toolchain version in `go.mod`. Builds with:

```
CGO_ENABLED=0 GOOS=linux
go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/pyrycode-relay ./cmd/pyrycode-relay
```

- `CGO_ENABLED=0` → fully-static binary that runs on distroless/static (no glibc on the runtime image).
- `-trimpath` → strips host build paths from the binary; defends against accidental disclosure of build-host directory structure via panic stack traces.
- `-s -w` → strips symbol table and DWARF; reduces post-exploitation reverse-engineering convenience and signals build hygiene.
- `-X main.Version=${VERSION}` → mirrors the `Makefile`'s `LDFLAGS`. Image builds default to `VERSION=dev` (matches the bare-binary default); release deploys override it with `--build-arg VERSION=<tag>` (see [`docs/deploy.md` § *Steady-state flow*](../../deploy.md#steady-state-flow)), so `/healthz` reports the release tag.

`go mod download` runs in a separate layer before `COPY . .` so source-only edits don't bust the dependency-cache layer.

### Runtime stage

`gcr.io/distroless/static-debian12:nonroot@sha256:…` (digest-pinned). The distroless `:nonroot` variant runs as uid `65532` upstream; the Dockerfile re-asserts `USER nonroot:nonroot` as belt-and-suspenders so a future base swap can't silently regress the invariant. The binary is the only thing in the runtime image — no shell, no package manager, no `apt`/`apk`, no `/etc/passwd` games.

- `EXPOSE 80 443` — `:80` for autocert ACME http-01 challenges, `:443` for WSS. The portable artifact exposes both; the host manifest chooses publish-both (autocert mode) or publish-neither (`--insecure-listen` behind a reverse proxy).
- `VOLUME ["/var/lib/relay/autocert"]` — documented mount point for the autocert cache. Without a mount, `--cert-cache` defaults to `/home/nonroot/.pyrycode-relay/certs` (degraded posture: cache vanishes on container restart, forces re-issuance). `fly.toml`'s `[[mounts]]` block (#38) wires a Fly volume `relay_autocert` to this path; `[processes] app = "... --cert-cache /var/lib/relay/autocert"` is set explicitly so autocert writes into the persistent volume rather than the unmounted default.
- `ENTRYPOINT ["/pyrycode-relay"]` — args at `docker run` go straight to the binary (`--domain …`, `--insecure-listen …`, etc.).

## Digest pinning convention

Both `FROM` lines pin the base image by `@sha256:…` and carry a `# Tracks: <upstream-tag>` comment naming the tag they were pinned from. Renovate keeps the digest fresh as the upstream tag moves; the comment lets a human reviewer (or Renovate's diff) sanity-check that the proposed digest still corresponds to the tracked tag. A malicious digest swap (digest changed, tag-comment unchanged) is structurally reviewable rather than relying on out-of-band trust.

Refreshing a pin (developer steps):

```bash
docker pull golang:1.26-bookworm
docker inspect --format='{{index .RepoDigests 0}}' golang:1.26-bookworm
# → paste the hex into the Dockerfile, keep the # Tracks: line in sync
```

## `.dockerignore`

Minimal, excludes `.git`, `bin/`, `dist/`, `coverage.txt`, `*.test`, and the Dockerfile / `.dockerignore` themselves. Keeps the build context bounded — `.git` is the largest item and a memory-constrained CI runner could OOM `docker build` with it included. `docs/` is **not** excluded; it lands in the build stage and is dropped at the multi-stage boundary, so excluding it would surprise a contributor running `docker build` from the same checkout they edit docs in.

## Threat-model contributions

The image layer contributes hardening to three operational surfaces (see `docs/threat-model.md`):

- **Deploy security — VPS compromise.** Non-root uid `65532`, no shell, no package manager. A compromised binary cannot `apt-get install`, cannot `exec` a shell, cannot escalate via setuid (none in the image).
- **Supply chain — Go dependencies.** Both base images digest-pinned with reviewable `# Tracks:` comments. Tag-swap attacks on `golang` or `distroless/static` cannot change what we build against between Renovate bumps. No `ADD` from URLs, no `RUN curl | sh`, no third-party scripts.
- **Cert & key handling.** No secrets baked into the image. `.git` excluded via `.dockerignore`. Docs enter the build stage but drop at the multi-stage boundary; they never reach the runtime layer.

## What this artifact deliberately does NOT do

- **No `HEALTHCHECK` directive.** `/healthz` (#10) is already exposed; platform health checks belong in the host manifest (`fly.toml`, #38), not in the portable artifact.
- **No host-specific config inside the image.** `fly.toml` (#38) sits at the repo root, not in the build context (its contents land at deploy time via `flyctl`, not as image bytes).
- **No single-instance enforcement.** The relay's binary-slot single-instance constraint is #39's problem; the image can be run N times, but only one will hold the slot.
- **No startup security-posture self-check.** #42 covers runtime self-validation.
- **No `--cert-cache` baked in.** The default (`/home/nonroot/.pyrycode-relay/certs`) is only relevant for `--version` smoke tests; real deployments pass `--cert-cache /var/lib/relay/autocert` via the host manifest.

## CI image scanning

PR-time image scanning was originally wired in `.github/workflows/ci.yml` as the `image-scan` job (#68), but that workflow was deleted (2026-05-24); there is no longer a Trivy scan gating PRs. The only image scanning now runs post-merge in `.github/workflows/security-scan.yml` (see below).

Repository image scanning lives in `.github/workflows/security-scan.yml` as the `image-scan` job, on a daily `0 6 * * *` UTC cron + `workflow_dispatch` (#72). It builds the image locally as `pyrycode-relay:${{ github.sha }}` and runs `aquasecurity/trivy-action` (commit-SHA pinned, `# Tracks: <upstream-tag>` comment alongside — same convention as the Dockerfile base-image digest pins) against the latest `main` SHA. The job fails on **fixable** CRITICAL/HIGH CVEs only (`ignore-unfixed: true`), covering `os,library` vuln types — distroless's OS package set plus Go-binary content Trivy re-derives. These binary findings are conservative module-version matches; the `govulncheck` job supplies source call-path evidence (see below). The repository results describe what `main` would ship. Daily scans let newly disclosed CVEs surface without waiting for the next dependency bump; production can still run an older image until an operator deploys.

### Deployed image scanning

The `deployed-image-scan` job (#176) runs only on `schedule` or
`workflow_dispatch` with `github.ref == 'refs/heads/main'`. It describes the
image serving at its observation time. The
[`security_scan.py` helper](../../../.github/workflows/security_scan.py) reads
Fly Machines for `pyrycode-relay`, validates every `started` Machine's resolved
`image_ref`, then requires exactly one distinct
`registry.fly.io/pyrycode-relay@sha256:<64 hex characters>` reference. Stopped
Machines do not select the image; a malformed started reference fails discovery
even beside a valid one. Multiple started Machines sharing a digest are accepted;
this scanner does not enforce the relay's single-machine constraint.

Discovery logs the immutable reference and UTC observation time. Download pulls
that digest and saves `deployed-image.tar`; Trivy scans those downloaded bytes
with the same fixable CRITICAL/HIGH, `ignore-unfixed`, `os,library` policy. It
does not rebuild or resolve a tag. Temporary registry credentials are removed
before scanning (see [Fly deploy privilege model](fly-deploy.md#deploy-privilege-model--operator-direct)).
A completed scan with no qualifying findings passes without a regression
artifact. Qualifying findings fail the job and upload
`regression-deployed-image-scan` containing `issue-title.txt` and `issue-body.md`
for `file-issue`. A deployed finding includes the digest, observation time and
run link in its body, so an older observation cannot establish what is serving
after a later deploy. [Verify a deploy](../../deploy.md#verify-a-deploy)
describes how the operator uses this result.

Trivy's vulnerability exit code is zero here so execution errors remain distinct
from completed findings. Rendering runs only after a successful scan and
validates the whole Trivy v2 report before writing any artifact. A malformed or
partial report fails without a finding artifact, even if it contains valid CVEs
earlier in the output. Fixed-version lists may contain comma-separated versions
and spaces for multiple release lines; rejecting spaces would misclassify valid
findings as infrastructure failures. The renderer writes the body first, then
atomically renames a temporary title into `issue-title.txt`: title existence is
the upload guard, so a partial title write must never qualify for upload.

The job runs [offline fixture/stub tests](../../../.github/workflows/test_security_scan.py)
before either credential-bearing step. They cover discovery validation,
download cleanup on login/pull/save failure, deterministic rendering, malformed
reports and partial writes. Issue-filing fixtures execute the actual workflow
shell with stubbed `gh`, including repeated titles and mixed finding/failure
results. These checks do not establish live Fly Machines or registry access;
the operator performs the initial manual run after provisioning the secret.

### Go source scanning

The `govulncheck` job uses pinned `govulncheck@v1.1.4` with `-json ./...`, then runs [`cmd/vulngate`](../../../cmd/vulngate/main.go) to gate the result (#175). JSON mode alone is not an exit-status gate: a successful scan exits 0 even with findings. The gate fails on a `finding` whose first trace frame has a non-empty `function`, providing source call-path evidence that the code calls a vulnerable function. This is the same symbol-level rule that makes text-mode `govulncheck ./...` exit 3. Bare `osv` records and module/package findings describing required or imported code do not qualify; a valid scan with only those records passes without an artifact.

The gate groups relevant findings by OSV id and joins their matching `osv` summaries. Each advisory is listed once with its module/version, fixed version (or `no fix`) and distinct called symbols. Sorted ids and symbols keep rendering deterministic; the title names the first sorted id and its Go module. The body displays at most 100 advisories and five symbols per advisory, with omitted counts, since a single stdlib advisory can have dozens of called symbols. `cmd/vulngate` is a separate standard-library-only command; the Dockerfile still builds only `cmd/pyrycode-relay`.

Parse the output with a streaming `json.Decoder`: govulncheck emits pretty-printed objects spanning multiple lines. Validation completes before artifact writes. Records must contain exactly one kind, including no duplicate keys; null `config`, `osv` and `finding` values are rejected. Pointer fields alone would confuse null with absence, and decoding into a map would hide duplicate keys. Validate every trace frame before deciding relevance, including caller frames and imported-only findings, so malformed data cannot appear clean. Standalone progress, SBOM and future record kinds remain ignored after the required initial v1 symbol-scan config.

Fixture tests in [`cmd/vulngate/main_test.go`](../../../cmd/vulngate/main_test.go), included by `make check`, cover clean and imported-only streams, multiple called findings for one advisory, deterministic rendering, output caps and malformed records. Error-path cases check that malformed data after valid called findings still leaves the artifact directory empty.

### Issue filing and scan failures

On a relevant Go finding, the gate writes `issue-body.md` before `issue-title.txt`, then exits 3. The workflow's `go run` reports any non-zero child exit as exit 1, so the upload guard uses title-file existence to distinguish findings from scan errors. It uploads both files as `regression-govulncheck` only on failure with a title present.

A malformed or truncated stream fails the gate without a regression artifact, even if earlier records contained called findings. A non-zero govulncheck exit fails the scan step and skips the gate, also producing no artifact. When no rendered finding reaches `file-issue`, it files `security-scan: scanner failed with no parseable findings` rather than inventing an advisory. A failed deployed-image job without its finding artifact also triggers this generic issue **alongside another scanner's findings**. Missing credentials, failed or ambiguous discovery, failed download, Trivy execution errors and unusable reports take this path. The other scanners' fallback remains limited to runs with no rendered finding. Read the linked run log to diagnose the scan failure.

The `file-issue` job (added in #73, scoped to `issues: write` at the job level only) consumes pre-rendered title+body files from the three `regression-*` artifacts and opens labelled GitHub issues via `gh issue create --body-file …`. All scanner jobs retain job-level `contents: read`; the privileged consumer never executes scanner code. Dedup compares actual titles exactly among open `security-sensitive` issues, rather than treating search hits as matches. The deployed title is `security-scan regression: <CVE> in deployed-image package <package>@<installed-version>`; CRITICAL findings sort before HIGH, then advisory/package/version order determines the primary finding. Digest, observation time and run identity stay in the body so a repeat retains the title. A repository-image issue for the same CVE or any different title cannot suppress it. Repeated Go findings and the generic failure title use the same exact-title dedup path.

## Responding to a scan regression

A red `security-scan` finding against stdlib is cleared by refreshing the build-stage digest (Trivy reads the stdlib version from the *builder's* toolchain, not from `go.mod`'s `go` directive), and a finding against a module by `go get <module>@<version>` + `go mod tidy` — the same two-edit shape in #103, #122, #126 and #165.

Clearing the repository result does not clear a deployed-image finding. The
operator must deploy the patched build, then check a completed deployed-image
result whose logged digest and observation time correspond to that deploy.

When a module bump's target version is itself in question, query OSV (`api.osv.dev/v1/querybatch`) over the resolved dependency set rather than assuming either extreme is right. Taking the latest release can be wrong (#126: the stated minimum left a sibling module one minor behind, carrying its own unfixed-at-the-time CVE — the current release happened to clear it too) and taking the minimum that clears the named CVE can also be wrong, for the opposite reason: MVS can still drag a sibling forward (#165: `x/net`'s minimum fix pulled nothing else, while the *latest* `x/net` would have moved `x/crypto`, `x/sys` and `x/text` with no advisory asking for it). Resolve the actual question — does the resolved set clear the named CVEs with no fixed advisory left over — rather than defaulting to "latest" or "stated minimum" on habit.

On a builder host with no Docker or Trivy, the substitute proof established in #126 and reused in #165: resolve the new base-image digest's `GOLANG_VERSION` via the registry API, build the Dockerfile's build line under that exact toolchain (`GOTOOLCHAIN=go<version>`), read the embedded module/stdlib set with `go version -m`, and run that set through OSV's `querybatch` — with a control query against the pre-change versions, to confirm the method would have reproduced the scan's original findings. The post-merge `security-scan` `workflow_dispatch` on `main` remains the real gate.

## Cross-links

- [Ticket #32 codebase notes](../codebase/32.md) — what landed in this ticket.
- [Ticket #68 codebase notes](../codebase/68.md) — PR-time Trivy image CVE scan in CI.
- [Spec: 32-dockerfile-base-hardening](../../specs/architecture/32-dockerfile-base-hardening.md) — architect's design and security review.
- [Threat model](../../threat-model.md) — § *Deploy security*, § *Supply chain*, § *Cert & key handling* are the surfaces this image layer hardens.
- [Autocert TLS](autocert-tls.md) — what the `:80` / `:443` exposure and `/var/lib/relay/autocert` mount feed.
- [`/healthz` endpoint](healthz.md) — what platform health checks would probe (no `HEALTHCHECK` wired by #38; available for future use).
- [Feature: Fly.io deploy](fly-deploy.md) — the host manifest + CI auto-deploy that wires this image to production (#38).
