# Patched Go for source scans (#186)

## Files read

- `go.mod` — `go` directive currently permits Go 1.26.2; dependency versions stay fixed.
- `.github/workflows/security-scan.yml` — `govulncheck` selects `1.26.x` independently of the module; scanner v1.1.4 and the symbol-level gate already exist.
- `Makefile` — `check` runs vet and the module race suite; `build` produces the relay binary.
- `docs/PROJECT-MEMORY.md` and `docs/knowledge/INDEX.md` — conventions and the owning feature doc.
- `docs/knowledge/features/docker-image.md` — “Go source scanning” distinguishes text-mode exit status from JSON mode; Docker toolchain selection is independent.
- `docs/architecture.md`, `docs/threat-model.md` (“Supply chain”, “TLS configuration”, “Triggers for re-review”) and `docs/security-followups.md` — operational boundary and deferred hardening.
- [Protocol Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model) — payload opacity and daemon-owned authentication remain the wire contract.
- Relay PR #177 — sizing analogue: eight changed configuration lines across Dockerfile, go.mod and go.sum.

## Change

Raise the module's Go minimum from 1.26.2 to 1.26.9 and replace the `govulncheck` job's `go-version: '1.26.x'` with `go-version-file: go.mod`. This couples source scans to the patched module requirement, clearing the nine stdlib advisories reported by #186. No dependency, scanner, gate, Docker digest or runtime logic changes are needed. One deliverable, two configuration edits, no new exported types, consumer call-site updates or reject branches; total written work including this plan stays below 60 lines. The remote overlap check found no other feature branch editing either configuration file. No new lifecycle or identity state is introduced, and no decision record is needed.

## Testing strategy

Before editing configuration, run a scratch assertion of the required module version and job-local `go-version-file` selection and observe both failures. Repeat after the edit, then run `actionlint` on the workflow. Use exact `GOTOOLCHAIN=go1.26.9` for `go version`, text-mode `govulncheck ./...` v1.1.4 (must exit zero without the named advisories), `make check` and `make build`. Install the ticket-required scanner under `/tmp/builder-relay-186/` if absent; this is validation tooling, not a module dependency. Record the version and scan outcome in the PR. These explicit ticket checks also satisfy the checkout's pre-push rule. The operator confirms the next main-branch govulncheck job after merge; deployed-scanner findings remain #187 and deployment remains #174.

## Documentation handoff

- Pending documentation stage: `docs/knowledge/features/docker-image.md`, “Go source scanning” — scanner Go selection follows `go.mod`, independently of the Docker digest.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — `actions/setup-go` reads the checked-out main branch's `go.mod`; no client input or payload influences toolchain selection.
- [Tokens, secrets and credentials] No findings — the `govulncheck` job retains `contents: read` and introduces no credential or secret-bearing step.
- [File operations] No findings — only reviewed configuration literals change; relay disk state and autocert-cache permissions are unaffected.
- [Subprocesses] No findings — existing fixed scanner commands remain intact, with no new shell interpolation or client-controlled argument.
- [Cryptography] No findings — this is a patch upgrade within Go 1.26; TLS configuration and protocol cryptography do not change.
- [Network and I/O] No findings — no new relay I/O path; the existing pinned setup-go action obtains the toolchain through its established mechanism.
- [Errors, logs and telemetry] No findings — text-mode scan validation checks exit status; workflow JSON scanning still uses the existing symbol-level gate. No relay logs or response bodies change.
- [Concurrency] No findings — no goroutines, locks, lifecycle transitions or persistent identity state are added.
- [Threat model alignment] No findings — patched stdlib addresses the reported supply-chain findings without a new dependency, endpoint or deploy target. OUT OF SCOPE — an operator confirms the post-merge source job; deployed findings and deployment remain relay #187 and #174 respectively.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-13
