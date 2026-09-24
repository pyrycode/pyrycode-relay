# #126 — Patch image-scan CVEs: Go 1.26.8 builder, x/net v0.59.0

Short plan: two pinned-version edits, no Go source change, no new type, state or failure mode.

## Files read

- `Dockerfile` → the build-stage `FROM golang:1.26-bookworm@sha256:…` pin and its `Tracks:` comment: the digest decides the stdlib Trivy reports.
- `go.mod` / `go.sum` → `golang.org/x/net` (indirect), `golang.org/x/crypto` (direct), `golang.org/x/sys`, `golang.org/x/text`.
- `.github/workflows/security-scan.yml` → `image-scan` job: the Trivy policy (`CRITICAL,HIGH`, `ignore-unfixed`, `os,library`) the last AC mirrors.
- Commit `ac3cdca` (#122) → precedent: module bumps plus builder digest refresh in one change.

## Change

**Dockerfile.** Replace the build-stage digest (`sha256:1ecb7edf…`, Go 1.26.5) with the current `golang:1.26-bookworm` OCI index digest `sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d`. Resolved from Docker Hub's registry API on 2026-09-24; its linux/amd64 and linux/arm64 image configs carry `GOLANG_VERSION=1.26.8` and `GOTOOLCHAIN=local`, created 2026-09-19. 1.26.8 ≥ 1.26.6 clears all eight stdlib findings. The `Tracks: golang:1.26-bookworm` comment is still accurate and stays. The distroless runtime digest is untouched.

**go.mod.** `go get golang.org/x/net@v0.59.0` (current release, fixes CVE-2026-46600 at ≥ v0.56.0), then `go mod tidy`. By MVS this also moves `golang.org/x/crypto` v0.52.0 → v0.57.0, `x/sys` v0.45.0 → v0.48.0 and `x/text` v0.39.0 → v0.42.0. The `go 1.26.2` directive does not move.

Why v0.59.0 rather than the minimum v0.56.0: the minimum still moves `x/crypto` (to v0.53.0) and `x/sys`, so the ticket's "if nothing else moves" condition fails either way. And v0.53.0 still carries CVE-2026-56854 (GO-2026-6303, GHSA-gjhq-gjfw-99mq, **high**, fixed in x/crypto v0.55.0), published 2026-08-28. The last red scan (run 31931018392, 2026-08-16) predates it, so the next post-merge scan would go red again on x/crypto. x/crypto ≥ v0.56.0 also clears GO-2026-6354/6355 (CVE-2026-78662, CVE-2026-56855). Taking v0.59.0 lands `x/crypto` v0.57.0, the current release, which satisfies the last AC. No other module moves.

## Testing strategy

No new logic, so no new test. The existing suite (`go test -race ./...`, `go vet ./...`, `go build`) covers the dependency bump: `x/crypto/acme/autocert` and `x/net/idna` are the only packages the binary links from these modules.

The last AC asks for a local `docker build` plus a Trivy scan. **This builder host has no Docker or Trivy, and the builder role does not install scanners.** The substitute proof has four parts:
1. Registry API: the new digest's image configs report `GOLANG_VERSION=1.26.8`.
2. `GOTOOLCHAIN=go1.26.8 CGO_ENABLED=0 GOOS=linux go build -trimpath …` then `go version -m`. This gives the exact stdlib and module set the image's binary embeds, which is what Trivy's Go-binary analyser reads.
3. An OSV `querybatch` over that set. As a control, the same query for `stdlib@1.26.5` and `x/net@0.55.0` returns 8 and 1 vulns, matching the red scan exactly.
4. For the new set, the query returns nothing except GO-2026-5932 (x/crypto/openpgp, no fixed version, so dropped by `ignore-unfixed`).

The distroless runtime layer is unchanged and was clean on the last scan. The definitive Trivy check is the post-merge `workflow_dispatch` of `security-scan` on `main`. That is an operator follow-up, per the ticket's Technical Notes.

## Documentation handoff

None required by the ticket. Pending for the documentation stage, optional: `docs/threat-model.md` § *Supply chain*, if it records pinned module versions, should reflect x/crypto v0.57.0 / x/net v0.59.0. No new dependency is added, so no re-review trigger fires.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings. No Go source changes: frames stay `json.RawMessage`, and the header gates in `ClientHandler`/`ServerHandler` are untouched.
- [Tokens, secrets, credentials] No findings. There is no token handling in the diff.
- [File operations] No findings. The autocert cache path and permissions checks are unchanged. The Dockerfile's `VOLUME`, `USER nonroot` and distroless runtime stage are untouched.
- [Subprocess] No findings. None is introduced.
- [Cryptographic primitives] No findings. TLS still uses `crypto/tls` from the stdlib, now 1.26.8, and ACME uses `x/crypto/acme/autocert` v0.57.0. Both are upstream patch/minor releases with no API change to call sites.
- [Network & I/O] No findings. `http.Server` timeouts and frame caps are unchanged. The stdlib `net/http` fixes in 1.26.6+ reduce exposure.
- [Supply chain] The builder image stays pinned by digest (an OCI index digest, as before), resolved from the official `library/golang` repository. Module hashes are in `go.sum` and verified against the Go checksum DB on download. No new module enters the graph; four existing `golang.org/x` modules move forward.
- [Supply chain] SHOULD FIX, done in this plan: the ticket's minimum `x/net` v0.56.0 would leave `x/crypto` at v0.53.0 with HIGH CVE-2026-56854. Resolved by taking v0.59.0 / x/crypto v0.57.0.
- [Supply chain] OUT OF SCOPE: GO-2026-5932 (`x/crypto/openpgp` unmaintained) has no fix. The relay does not import `openpgp`, and `ignore-unfixed` drops it. It needs no ticket unless a fix version appears.
- [Error messages, logs] No findings. No log call or allowlist key changes.
- [Concurrency] No findings. No code changes.
- [Threat model alignment] No findings. No new endpoint, dependency or deploy target, so no "Triggers for re-review" item trips. Verification gap: the image was not Trivy-scanned locally (no Docker on the builder host). The post-merge `security-scan` `workflow_dispatch` is the gate, and this issue closes on that green run.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
