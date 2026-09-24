# Threat model — operational surface

This document catalogues the operational threats that apply to `pyrycode-relay` as a deployed Linux process: VPS compromise, dependency compromise, log accidents, cert-cache permissions, TLS configuration, and error-response leakage. Wire-protocol-level threats — prompt injection, server-id race, MITM, token leak, replay, malformed-frame DoS — live in the protocol spec's [Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model). Both documents are required reading for any security review; neither subsumes the other.

Each threat below records four fields:

- **Severity** — `low` / `medium` / `high`.
- **v1 mitigation** — what is already in place, with a `file:line` anchor when grounded in code.
- **Residual risk** — what the v1 mitigation does not cover.
- **Future hardening** — what we would add if the residual risk became material; "deferred" status is first-class, not a gap. The "Future hardening" lines below are also collected, with triggers and rough scope, in [`security-followups.md`](security-followups.md) — that file is the menu when "what about hardening X?" comes up; this file is the threat narrative.

## Deploy security — VPS compromise

**Severity:** `high`.

**v1 mitigation:** Operator-owned. SSH key authentication only (no passwords); restricted admin set; OS auto-updates via unattended-upgrades or equivalent. Host-level intrusion detection is out of scope for v1.

**Residual risk:** If the VPS is rooted, every byte through the relay is exposed: TLS terminates here, so plaintext frames are recoverable from process memory. This is the operator-side framing of the protocol spec's "relay operator MITM" threat — same plaintext exposure, different actor.

**Future hardening:** A documented deploy runbook; a per-operator SSH-key inventory; `fail2ban` (or equivalent) on the SSH port; a host-based IDS once user count justifies the operational cost.

## Supply chain — Go dependencies

**Severity:** `medium`.

**v1 mitigation:** A small, justified dependency surface (`go.mod`): `github.com/coder/websocket v1.8.14` (the WebSocket implementation, chosen 2026-05-28 in [ADR-0010](knowledge/decisions/0010-coder-websocket-migration.md)), `github.com/prometheus/client_golang v1.23.2` with its `client_model v0.6.2` and `common v0.66.1` companions (metrics, [ADR-0008](knowledge/decisions/0008-prometheus-client-adoption.md)), and `golang.org/x/crypto v0.57.0` for `acme/autocert`. `go.sum` provides per-module checksum verification on every build. `govulncheck` runs in `make lint` (#41 pins it to a semver tag) and daily against `main` in the `govulncheck` job of `.github/workflows/security-scan.yml` (#72) — the repo's only workflow since the convenience `ci.yml` was removed 2026-05-24. The runtime image is additionally scanned by Trivy in the same workflow's `image-scan` job (#68/#72), covering OS-package + Go-binary content, so CVEs disclosed against deps that have not changed since the last PR surface as a red Actions run within ≤24h rather than staying invisible until the next dep bump. A regression also opens a `security-sensitive`-labelled GitHub issue via the workflow's `file-issue` job (#73), lifting the red Actions row to a tracked work-item in the triage queue rather than leaving it as a passive signal. New dependencies require a justification (`docs/PROJECT-MEMORY.md`).

**Residual risk:** A compromised release of `golang.org/x/crypto` would expose TLS private-key handling. A compromised release of `github.com/coder/websocket` would see every routed frame in cleartext, since the relay is the TLS terminus. `go.sum` defends against tampered downloads, not against a malicious release tagged by an authentic maintainer.

**Future hardening:** SBOM generation in CI; pinned-version review on every `go.mod` change; consider Go module proxy mirroring once any user data flows through the relay.

## DoS resistance — connection floods, slow-loris, fork-bomb retry

**Severity:** `medium`.

**v1 mitigation:** Slow-loris is mitigated by the `http.Server` timeouts in `cmd/pyrycode-relay/main.go` (`ReadHeaderTimeout: 5s`, `ReadTimeout: 60s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`), applied identically to the insecure listener, the autocert TLS listener, the autocert HTTP-01 listener, and the loopback metrics listener (`internal/relay/metrics_listen.go`). `ReadHeaderTimeout` bounds the pre-upgrade window — the gap between TCP accept and full HTTP header receipt — at 5s; after `websocket.Accept` hijacks the connection, post-upgrade slow peers are bounded by per-frame deadlines (#15) and the heartbeat ping/pong (#7). A per-IP token-bucket rate limit fronts the `/v1/server` and `/v1/client` upgrade paths (`cmd/pyrycode-relay/main.go` policy block, `internal/relay/ratelimit_middleware.go`): ~10 attempts/IP/minute steady-state, burst 20, eviction sweep every 5 minutes. Over-cap attempts receive `429 Too Many Requests` before `websocket.Accept` runs; the wrapped registry is never touched on a deny. The middleware extracts the source IP via `relay.ClientIP`; `X-Forwarded-For` is honoured only when the operator opts in with `--trust-x-forwarded-for` (default `false`). `/healthz` is intentionally unwrapped — it must remain pollable from monitoring without throttling. Bandwidth amplification is structurally absent: the relay forwards 1:1, so there is no amplification factor an attacker can lever. Connection-count caps (per-IP and global, distinct from the attempt-rate cap) are deferred.

**Residual risk:** The rate limit caps the *attempt rate* per source IP but not the *concurrent connection count*: a slow attacker that opens connections at or below the bucket refill rate can still pin file descriptors and RAM. The single-instance limiter is per-process — a future multi-instance deployment behind L4 load balancing would need shared state to converge on a global limit. `X-Forwarded-For` is binary trust (all-or-nothing) — a CIDR-aware trusted-proxy chain is not implemented.

**Future hardening:** A connection-count cap (per-IP and global) on the WebSocket upgrade path; a CIDR-allowlisted trusted-proxy chain for `X-Forwarded-For`; reverse-proxy fronting for L4 protections; shared rate-limit state for multi-instance deployments. Trigger: first observed flood, first multi-instance deployment, or first paying user.

## Log hygiene — what must not be logged

**Severity:** `high`. A single accidental `slog.Info("frame", "body", payload)` call leaks user data.

**v1 mitigation:** The relay logs to stderr via `slog.NewTextHandler` (`cmd/pyrycode-relay/main.go:39`). The contributor-facing rule is enumerated explicitly:

- **MUST NOT be logged:** payload bodies (the inner `json.RawMessage` of an envelope), `x-pyrycode-token` values or any header that carries a token, full request headers, full URL paths if they ever carry tokens.
- **MAY be logged:** server-id, `conn-id`, device-name (advertised by the binary, not user-secret), remote host (IP), event types (`upgrade`, `forward`, `close`), and close codes (`4401`, `4404`, `4409`).

**Enforcement:** The canonical set of permitted keys lives in `internal/relay/log_allowlist.go`. `TestLogKeysAreAllowlisted` in `internal/relay/log_keys_test.go` AST-walks every non-test `.go` file in the package on each `make test` run and fails if any `logger.{Info,Warn,Error,Debug}` call carries a key absent from that set or uses a dynamic (non-string-literal) key. Adding a logged key requires editing the allowlist file in the same commit.

Log retention and rotation are operator-owned. The relay writes only to stderr; on a typical deployment the systemd journal or a `logrotate`-managed flat file owns rotation.

**Residual risk:** A future contributor adds a debug log statement that prints a payload body or token. Code review and the acceptance criteria of any frame-forwarding ticket are the first line of defence; runtime redaction is not built.

**Future hardening:** A `slog` middleware that redacts known sensitive keys structurally, so the rule is enforced by code rather than vigilance.

## Cert & key handling — autocert cache directory

**Severity:** `high`.

**v1 mitigation:** `internal/relay/tls.go:16-55` enforces `0700` on the cache directory at startup. If the directory exists with permissions broader than `0700` — any group or world bit set — `NewAutocertManager` returns `ErrCacheDirInsecure` and the process refuses to start. This is the project's loud-failure pattern (`docs/PROJECT-MEMORY.md`): refuse rather than re-chmod and continue. The default location is `~/.pyrycode-relay/certs` (`cmd/pyrycode-relay/main.go:111-116`). Key rotation is handled by autocert: Let's Encrypt issues 90-day certificates and autocert renews at roughly 30 days remaining.

**Residual risk:** If a separate process running as the relay's UID is compromised, it can read the cache and exfiltrate the private key. The `0700` check defends against other UIDs on the same host, not against same-UID compromise.

**Future hardening:** Move private keys to a dedicated secret manager once operationally justified. Until then, the deploy doc enforces "the relay's UID owns nothing else."

## TLS configuration — cipher suites, version pin

**Severity:** `low`. Go's defaults are conservative; this entry records the choice rather than flagging risk.

**v1 mitigation:** `internal/relay/tls.go:80-88` pins `MinVersion: tls.VersionTLS12`. Cipher-suite selection uses Go's secure defaults; no custom override.

**Residual risk:** A future Go version weakens defaults — unlikely but worth re-checking on Go upgrade.

**Future hardening:** Re-read this entry on every Go minor-version bump. Consider an explicit `CipherSuites` list only if a specific compliance reason emerges.

## Error response leakage — public-path error bodies

**Severity:** `medium`.

**v1 mitigation:** The current public surface is small. `/healthz` returns a literal `ok`. Port 80 returns `404` for non-challenge traffic (ADR-0002). Port 443 returns `421 Misdirected Request` with no body when the Host header does not match the configured domain (`internal/relay/tls.go:66-78`). None leak internal state. The rule for new endpoints: public-facing handlers return generic messages; detailed errors go to logs only.

**Residual risk:** Future endpoints — WebSocket upgrade error paths in particular — may write `err.Error()` to the response body and leak filesystem paths, dependency names, or stack traces.

**Future hardening:** A handler-level guideline in a future `CODING-STYLE.md`. For now, this document is the canonical reference.

## Triggers for re-review

This document must be revisited when any of the following occurs:

- A new dependency is added to `go.mod`.
- The deploy target changes (new VPS provider, container platform, managed Kubernetes).
- A new public endpoint is exposed (any new path under `/v1/*` or otherwise).
- A security incident occurs — any unexpected behaviour with security implications, even if no compromise is confirmed.

## Out of scope

- Pen-test write-up. No pen test has been performed.
- Compliance frameworks (GDPR, SOC 2, ISO 27001). No applicable obligations in v1; user-data flows are minimal.
- Per-CVE response runbook. Premature; revisit when there are users.
