# Docker image — portable deploy artifact

Host-agnostic OCI image for the relay, produced by a multi-stage `Dockerfile` at the repo root. Image-layer hardening (small base, no shell, no package manager, non-root, digest-pinned bases, stripped static binary) is defence-in-depth on top of the runtime hardening already in place (autocert cache permission check, slow-loris timeouts, header-gate before WS upgrade, 256 KiB frame cap, opaque payload routing).

The image is intentionally **portable, not deployable on its own**: it exposes both `:80` and `:443` and declares a volume mount at `/var/lib/relay/autocert`, but TLS termination policy, port publishing, volume backing, single-instance enforcement, and healthcheck wiring are decisions the host manifest owns (#38 / #39 / #42).

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
- `-X main.Version=${VERSION}` → mirrors the `Makefile`'s `LDFLAGS`. Image builds default to `VERSION=dev` (matches the bare-binary default); release tooling lands in #38 and overrides via `--build-arg VERSION=…`.

`go mod download` runs in a separate layer before `COPY . .` so source-only edits don't bust the dependency-cache layer.

### Runtime stage

`gcr.io/distroless/static-debian12:nonroot@sha256:…` (digest-pinned). The distroless `:nonroot` variant runs as uid `65532` upstream; the Dockerfile re-asserts `USER nonroot:nonroot` as belt-and-suspenders so a future base swap can't silently regress the invariant. The binary is the only thing in the runtime image — no shell, no package manager, no `apt`/`apk`, no `/etc/passwd` games.

- `EXPOSE 80 443` — `:80` for autocert ACME http-01 challenges, `:443` for WSS. The portable artifact exposes both; the host manifest chooses publish-both (autocert mode) or publish-neither (`--insecure-listen` behind a reverse proxy).
- `VOLUME ["/var/lib/relay/autocert"]` — documented mount point for the autocert cache. Without a mount, `--cert-cache` defaults to `/home/nonroot/.pyrycode-relay/certs` (degraded posture: cache vanishes on container restart, forces re-issuance). The host manifest (#38) wires a real backing store.
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

- **No `HEALTHCHECK` directive.** `/healthz` (#10) is already exposed; platform health checks belong in the host manifest (#38), not in the portable artifact.
- **No host-specific config.** No `fly.toml`, `compose.yaml`, k8s manifest, or systemd unit — those live in #38.
- **No single-instance enforcement.** The relay's binary-slot single-instance constraint is #39's problem; the image can be run N times, but only one will hold the slot.
- **No startup security-posture self-check.** #42 covers runtime self-validation.
- **No `--cert-cache` baked in.** The default (`/home/nonroot/.pyrycode-relay/certs`) is only relevant for `--version` smoke tests; real deployments pass `--cert-cache /var/lib/relay/autocert` via the host manifest.

## CI image scanning

PR-time scanning is wired in `.github/workflows/ci.yml` as the `image-scan` job (#68). Each PR builds the image locally as `pyrycode-relay:${{ github.sha }}` and runs `aquasecurity/trivy-action` (commit-SHA pinned, `# Tracks: <upstream-tag>` comment alongside — same convention as the Dockerfile base-image digest pins) against it. The job fails on **fixable** CRITICAL/HIGH CVEs only (`ignore-unfixed: true`); unfixed CVEs print but don't block. Covers `os,library` vuln types — distroless's OS package set plus Go-binary content Trivy re-derives, intentionally overlapping with `govulncheck`'s source-reachability view. Periodic re-scan (catches CVEs disclosed after merge against unchanged bases) is a follow-up ticket.

## Cross-links

- [Ticket #32 codebase notes](../codebase/32.md) — what landed in this ticket.
- [Ticket #68 codebase notes](../codebase/68.md) — PR-time Trivy image CVE scan in CI.
- [Spec: 32-dockerfile-base-hardening](../../specs/architecture/32-dockerfile-base-hardening.md) — architect's design and security review.
- [Threat model](../../threat-model.md) — § *Deploy security*, § *Supply chain*, § *Cert & key handling* are the surfaces this image layer hardens.
- [Autocert TLS](autocert-tls.md) — what the `:80` / `:443` exposure and `/var/lib/relay/autocert` mount feed.
- [`/healthz` endpoint](healthz.md) — what platform health checks will probe (wired in #38, not here).
