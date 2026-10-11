# Scan the deployed Fly image (#176)

## Files read

- `.github/workflows/security-scan.yml` → `image-scan`, `govulncheck`, `file-issue`: current policies, pinned actions, artifact contract and overly broad title dedup.
- `cmd/vulngate/main.go` and `cmd/vulngate/main_test.go` → `run`, `parse`, `TestRun`: validate complete output before writing body then title; distinguish findings from infrastructure failure.
- `Makefile` → `check`: the user requires this gate before pushing.
- `docs/PROJECT-MEMORY.md` and `docs/knowledge/INDEX.md`: conventions and ownership map.
- `docs/knowledge/features/docker-image.md` → CI image scanning: binary-module matches differ from source reachability.
- `docs/knowledge/features/fly-deploy.md` → Deploy privilege model — operator-direct: CI must acquire no deploy capability.
- `docs/deploy.md` → opening callout and steady-state flow: production can lag main indefinitely.
- `docs/architecture.md`, `docs/threat-model.md`, `docs/security-followups.md`: content blindness, operational credentials and deferred hardening.
- [Protocol Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model): no protocol or payload changes.
- [Machines API](https://docs.fly.io/machines/api/machines-resource/#list-machines), [org read-only tokens](https://docs.fly.io/security/tokens#org-scoped-tokens), [Fly Docker authentication](https://github.com/superfly/flyctl/blob/master/internal/command/auth/docker.go), [Trivy archives](https://trivy.dev/docs/latest/target/container_image/#tar-files) and pinned Trivy action inputs: resolved reference, actual scope, stdin authentication and offline target bytes.

## Context

Repository scans describe main; production is manually deployed and can remain vulnerable while those scans pass. Add one deployed-image result to the daily/manual workflow, preserving artifact-based issue filing. No decision record or new dependency is needed. #175's Go source gate is already present. The #73 analogue changed 199 workflow lines; this design is approximately 550 total written lines across the workflow, one standard-library Python helper and its fixture tests, plus this plan, below all five boundaries (four criteria, zero exported Go types, zero consumer signature changes, fewer than ten failure categories). Documentation edits are handed off. No in-flight branch overlaps the selected paths.

## Design

Add `deployed-image-scan` with job-level `contents: read`, gated on schedule/workflow_dispatch and `refs/heads/main`. Checkout main with the existing pin. A narrow `.github/workflows/security_scan.py` helper has three CLI operations:

- `discover`: GET only `https://api.machines.dev/v1/apps/pyrycode-relay/machines`, with a 30-second timeout and 1 MiB response cap, no redirects. Validate each started Machine's `image_ref` before deduplicating. Require registry `registry.fly.io`, repository `pyrycode-relay`, and `sha256:` plus 64 hexadecimal characters. Ignore stopped Machines. Require exactly one distinct normalized digest reference. Write only reference and UTC observation time to `deployment.json`, and log those validated public values.
- `download`: revalidate discovery metadata; use Docker login username `x` and token via password stdin in a fresh private temporary Docker config. Pull the immutable reference and save those bytes as `deployed-image.tar`. All subprocesses use argument arrays, timeouts and an environment without the Fly token; suppress subprocess output to avoid credential reflection. The temporary config is removed on success and every failure before returning. Missing token is a hard error; never substitute another credential.
- `render`: runs only after the Trivy action completes successfully. Require a usable Trivy v2 JSON report (`Results` array and valid vulnerability structures); validate the whole report before artifact writes. Filter fixable HIGH/CRITICAL findings; sort deterministically by severity then advisory/package/version. No finding returns zero without files. Findings write body before title and return nonzero. The title explicitly names a deployed image plus primary CVE/package, excluding digest, observation and run identity. The body includes all three, run URL, bounded findings and a statement that Go-binary matches are conservative module-version matches, not source call-path evidence.

Reuse pinned Trivy and artifact actions. Trivy scans `input: deployed-image.tar`, with CRITICAL,HIGH / ignore-unfixed / os,library. Its vulnerability exit-code is zero so execution failure is distinguishable from completed findings; the render gate supplies the finding failure. A failed action skips rendering even with partial JSON. Only a completed render's title permits `regression-deployed-image-scan` upload. No token at job scope or in metadata, output, logs or artifacts.

Extend `file-issue.needs`. Keep its shell artifact consumer and body-file creation. Fetch actual titles and compare exactly, including for the generic failure title. Track whether a deployed-title artifact was consumed; when the deployed job fails without one, file/dedup the generic infrastructure issue even if another scanner supplied an artifact. Other scanners' existing fallback policy is unchanged. Existing scanner inputs and permissions are unchanged.

## Concurrency model

No relay goroutines change. Jobs run independently, then file-issue waits for all results. Existing workflow concurrency queues runs. Credential use is sequential in the download step; cleanup precedes the independent Trivy step.

## State transitions and identity reuse

| Repeated event | Coverage |
| --- | --- |
| Multiple started Machines share a digest; stopped machines hold older tags | `test_discovery` |
| Same CVE repeats across observations/runs or input order changes | `test_render_deterministic` |
| Same title repeats with an open issue, versus repository/different-title issue | `test_issue_filing` (stub gh) |
| Download fails during login, pull or save; next operation sees no credential config | `test_download_cleanup` |
| Partial/malformed scan follows valid findings | `test_invalid_scan_has_no_artifact` |
| Another scanner supplies findings while deployed scanner fails | `test_issue_filing` |

These are sequential workflow state, not shared Go identity state; Python fixture/stub tests cover them. The required Go race gate still runs before pushing.

## Error handling

Missing credential, lookup/input validation, zero/ambiguous running images, registry download, Trivy execution, output validation and artifact-write failure fail the deployed job without a finding title. Report fixed error messages; never echo HTTP bodies, subprocess output or exception text containing credentials. Only fully validated findings yield an artifact. The privileged job receives job result through an environment variable, never interpolated shell commands.

## Testing strategy

Write unittest fixtures/stubs first and observe failure before helper/workflow implementation. Exercise discovery shapes including malformed entries beside valid ones, completed clean/nonqualifying/finding scans, deterministic rendering, partial reports and full external-command failure/cleanup paths. Execute the actual file-issue shell with stub gh to verify exact-title dedup, repeat filing and mixed finding/failure. Check workflow restrictions/pins/policy and execution-error render skipping. Run Python tests, `make check` (user-required before push), and `go build ./cmd/pyrycode-relay`. No Fly credential, deployment, scanner installation or production request is needed.

## Open questions

None. Real registry authentication is an operator follow-up, not implementation acceptance.

## Documentation handoff

Pending for the documentation stage:
- `docs/knowledge/features/docker-image.md` § CI image scanning: repository versus deployed results, observation/digest, binary versus source evidence and mixed failure fallback.
- `docs/knowledge/features/fly-deploy.md` § Deploy privilege model — operator-direct: CI-held org-scoped read-only token, event/branch/step restrictions; deploy credential remains operator-only.
- `docs/deploy.md` opening callout and § Verify a deploy: deployed result digest and observation; § One-time bootstrap: `FLY_READONLY_TOKEN`, explicit-expiry `flyctl tokens create readonly --org <owning-org> --name relay-security-scan --expiry <duration>`, rotation and initial main workflow_dispatch.
- `docs/threat-model.md` § Deploy security — VPS compromise: CI-held read-only credential, actual org scope, scanner isolation and preserved operator deploy privileges.

Operator must provision that read-only secret and run workflow_dispatch on main to check live Machines/registry access. Until provisioned, a generic scanner-failure issue is expected.

## Security review

**Verdict:** PASS

**Findings:**
- [Trust boundaries] No findings — `discover` validates every started reference before dedup; `render` validates the complete report before writes. No relay frames or payloads are involved.
- [Tokens] SHOULD FIX — `download` must remove its private temporary config even when login/pull/save fails, exclude the token from child environments and suppress credential-bearing subprocess diagnostics. Fixture tests enforce these choices. Provisioning/expiry/rotation are operator-owned; the credential is org-scoped read-only, never deploy/personal.
- [Files] No findings — runner-only fixed output paths, fresh private temporary credential directory, body written before title. No new relay disk state.
- [Subprocesses] No findings — Docker uses validated fixed-registry digest arguments, token only through stdin, no shell expansion. The issue consumer uses quoted args and --body-file for untrusted rendered content.
- [Cryptography] No findings — no new primitives; HTTPS validates Fly certificates and Docker verifies content-addressed pulls.
- [Network & I/O] No findings — fixed GET endpoint, no redirects, bounded response and timeout; Docker subprocess/job timeouts. No relay listeners change.
- [Errors & logs] No findings — fixed errors, only validated digest/time logged; token/HTTP bodies/full Machines output never persisted or published.
- [Concurrency] No findings — jobs use isolated runners and sequential temporary config lifetime; existing workflow queuing prevents simultaneous issue-filing runs.
- [Threat model] No findings — privileged issue filing stays separate from scanners; documentation handoff records the newly CI-held read-only credential and actual org-wide exposure. Operator retains deploy privileges.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11
