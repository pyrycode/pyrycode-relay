# Public HTTP header limit

## Files read

- `cmd/pyrycode-relay/main.go` → `run`: three public `http.Server` literals and their existing timeouts; `runServers` retains listener shutdown ownership.
- `cmd/pyrycode-relay/main_e2e_test.go` → `TestRun_SigtermClosesWSConnsWithGoingAway`, `TestRun_HTTPSProxyProtocolWiring`: existing real upgrade and listener compatibility coverage.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`, `docs/knowledge/features/autocert-tls.md` → policy values at wiring sites and explicit HTTP-01 fallback 404.
- `docs/architecture.md`, `docs/threat-model.md` (DoS resistance and Log hygiene), `docs/security-followups.md` → content-blind routing, pre-upgrade resource limits and deferred connection-count hardening.
- [Protocol Authentication](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#authentication) and Security model → routing-header semantics remain unchanged.
- `golang.org/x/crypto/acme/autocert` → `Manager.HTTPHandler`, `httpToken`, `httpTokenCacheKey`: cache-backed HTTP-01 test fixture avoids external ACME calls.
- Relay #35 (`2bad1bf`) → timeout-only analogue: 108 added production/test lines.

## Change

Set `MaxHeaderBytes: 16 * 1024` directly in the insecure, autocert HTTPS and HTTP-01 server literals in `run`. Go's HTTP parser bounds the request line and headers before invoking a handler, allowing its buffer slack; use a clearly oversized 64 KiB header value to prove HTTP/1.1 returns 431. Keep handlers, TLS configuration, routing-header semantics and all four timeouts unchanged. No new type, lifecycle state, dependency, log or application rejection branch. No overlapping in-flight branch touched the planned files after fetching origin.

Size re-count: one deliverable, approximately 250 total written lines including this plan and tests, two code files, zero exported types/interfaces, zero consumer call-site changes, two acceptance criteria, zero new state-machine rejection branches. Within all five limits; no decision record needed.

## Testing strategy

Write `cmd/pyrycode-relay/http_headers_test.go` first. Parse the public `http.Server` literals in `run` to pin all three header limits and the unchanged 5s/60s/60s/120s timeouts. Exercise those header-limit values on in-process HTTP/1.1 servers (TLS for HTTPS) with a handler invocation counter: a 64 KiB header returns 431 and never enters the handler, while a small request does. An HTTP-01 handler with a locally cached fixture must still serve the exact challenge body and return 404 for non-challenge traffic under the limit. Existing command-package end-to-end tests prove real binary and phone upgrades remain functional. Run command and relay package race tests, vet and build. Run `make check` before pushing as required by the checkout's AGENTS.md.

## Documentation handoff

- Pending documentation stage: `docs/threat-model.md` § DoS resistance — describe the 16 KiB setting, request-line/header scope and Go's buffer slack.
- Pending documentation stage: `docs/deploy.md` § fly.toml gotchas — record the public header limit for the 256 MB machine.
- Operator follow-up: deploy from clean `main` after merge to make the limit effective in production.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — `run` configures Go's pre-handler HTTP parsing limit on every public listener. No payload is inspected; existing routing-header gates retain protocol semantics.
- [Tokens, secrets and credentials] No findings — no new credential handling; oversized values are not echoed or logged by added code.
- [File operations] No findings — production adds no disk operation. Tests use a temporary autocert cache with synthetic challenge data.
- [Subprocesses] No findings — production introduces no subprocess.
- [Cryptography] No findings — existing `TLSConfig` is unchanged; HTTPS test uses httptest's local certificate.
- [Network & I/O] No findings — all three public parsers receive the 16 KiB limit and retain 5s header, 60s read/write and 120s idle timeouts. Tests use HTTP/1.1 and a 64 KiB value rather than assuming an exact cutoff despite Go's slack. The loopback metrics listener is outside this public-listener ticket.
- [Errors, logs and telemetry] No findings — rejection is net/http's fixed 431 response; no logger or metric changes.
- [Concurrency] No findings — production adds no goroutine, lock or lifecycle state. Test servers close through cleanup and handler counters use atomics.
- [Threat model alignment] No findings — reduces per-request pre-upgrade memory exposure without adding endpoints, dependencies or deploy targets. Existing per-IP concurrent cap deferral in `docs/security-followups.md` remains separate; this does not bound total pre-upgrade connections.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11
