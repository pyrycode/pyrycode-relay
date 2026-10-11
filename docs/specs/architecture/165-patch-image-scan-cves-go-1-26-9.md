# #165 — Patch image-scan CVEs: Go 1.26.9 builder, x/net v0.60.0

Short plan: two pinned-version edits, no Go source change, no new type, state or failure mode. Same shape as #126.

## Files read

- `Dockerfile` → the build-stage `FROM golang:1.26-bookworm@sha256:…` pin and its `Tracks:` comment. The digest decides the stdlib Trivy reports, because the scanned binary is built by that image's toolchain (`GOTOOLCHAIN=local`), not by the `go` directive.
- `go.mod` / `go.sum` → `golang.org/x/net` (indirect, v0.59.0) and the `golang.org/x` modules MVS could move with it.
- `.github/workflows/security-scan.yml` → `image-scan` job: the Trivy policy (`CRITICAL,HIGH`, `ignore-unfixed`, `os,library`, exit-code 1) the last AC mirrors.
- `docs/specs/architecture/126-patch-image-scan-cves-go-1-26-8.md` and PR #131 → the analogue, including its substitute proof when the host has no Docker or Trivy, and its lesson to re-check the resolved set against current advisories.
- `docs/threat-model.md` § *Supply chain* → lists direct modules only; `x/net` is indirect.

No other in-flight `feature/*` branch touches `Dockerfile`, `go.mod` or `go.sum`.

## Change

**Dockerfile.** Replace the build-stage digest `sha256:a688600c…` (Go 1.26.8) with the current `golang:1.26-bookworm` OCI index digest `sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c`. Resolved from Docker Hub's registry API on 2026-10-11: its linux/amd64 and linux/arm64 image configs carry `GOLANG_VERSION=1.26.9` and `GOTOOLCHAIN=local`, created 2026-10-08. The `Tracks: golang:1.26-bookworm` comment stays accurate. The distroless runtime digest does not change.

**go.mod.** `go get golang.org/x/net@v0.60.0`, then `go mod tidy`. Only `x/net` moves; `x/crypto`, `x/sys` and `x/text` stay where #126 left them. The `go 1.26.2` directive does not move.

Why v0.60.0 rather than the latest v0.61.0: v0.61.0 would also move `x/crypto` to v0.58.0, `x/sys` to v0.49.0 and `x/text` to v0.43.0, with no advisory driving it. An OSV query (below) shows the v0.60.0 set has no fixed advisory left, so the minimum is enough and keeps the change to one module.

Advisory mapping, from OSV: CVE-2026-97031 is GO-2026-6607 and CVE-2026-78667 is GO-2026-6609, both stdlib, fixed in 1.26.9. The control query for `stdlib@1.26.8` returns 13 advisories, five of which (GO-2026-6603/6610/6611/6612/6617) also hit `x/net` v0.59.0 and are fixed in `x/net` v0.60.0.

## Testing strategy

No new logic, so no new test. The existing suite covers the bump: `go test -race ./...`, `go vet ./...` and `go build`.

The last AC asks for a local `docker build` plus a Trivy scan. As on #126, this builder host has no Docker or Trivy, and the builder role does not install scanners. The substitute proof:

1. Registry API: the new digest's image configs report `GOLANG_VERSION=1.26.9`.
2. `GOTOOLCHAIN=go1.26.9 CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags=…`, the Dockerfile's build line, then `go version -m`. That is the stdlib and module set the image's binary embeds, which Trivy's Go-binary analyser reads.
3. An OSV `querybatch` over that set plus `stdlib@1.26.9` returns only GO-2026-5932 (`x/crypto/openpgp` unmaintained, no fixed version, so `ignore-unfixed` drops it; the relay does not import it).
4. Control: the same query for `stdlib@1.26.8` and `x/net@0.59.0` returns 13 and 5 advisories, including both CVEs named by the scan.

The definitive Trivy check is a post-merge `workflow_dispatch` of `security-scan` on `main`, an operator follow-up alongside the deploy in #174.

## Documentation handoff

None required. The ticket asks for a `docs/threat-model.md` § *Supply chain* update only if a direct module moves. Only the indirect `x/net` moves, and that paragraph does not list it.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings. No Go source changes: frames stay `json.RawMessage`, and the header gates in `ClientHandler` and `ServerHandler` are untouched.
- [Tokens, secrets, credentials] No findings. No token handling in the diff.
- [File operations] No findings. The autocert cache path and permission checks are unchanged, and the Dockerfile's `VOLUME`, `USER nonroot` and distroless runtime stage are untouched.
- [Subprocess] No findings. None is introduced.
- [Cryptographic primitives] No findings. TLS still uses the stdlib `crypto/tls`, now from 1.26.9; `x/crypto` stays at v0.57.0.
- [Network & I/O] No findings. `http.Server` timeouts and frame caps are unchanged. The stdlib and `x/net` fixes reduce exposure on the internet-facing listener.
- [Supply chain] No findings. The builder image stays pinned by OCI index digest from the official `library/golang` repository, with the digest's toolchain version checked against its image config rather than trusted from the tag. Module hashes are in `go.sum` and verified against the Go checksum DB. No new module enters the graph; one existing `golang.org/x` module moves a minor version.
- [Supply chain] SHOULD FIX, done in this plan: per #126's lesson, the resolved set was re-checked against current advisories before choosing a version. It is clean apart from the unfixed GO-2026-5932.
- [Supply chain] OUT OF SCOPE: GO-2026-5932 (`x/crypto/openpgp` unmaintained) has no fix. The relay does not import `openpgp`, and `ignore-unfixed` drops it.
- [Error messages, logs] No findings. No log call or allowlist key changes.
- [Concurrency] No findings. No code changes.
- [Threat model alignment] No findings. No new endpoint, dependency or deploy target, so no re-review trigger trips. Verification gap: the image was not Trivy-scanned locally. The post-merge `security-scan` run is the gate, and the production fix needs the operator deploy in #174.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11
