# Spec — `docs/threat-model.md`: per-process surface concerns (#11)

This is a documentation deliverable. The architect's job is to produce the structured threat list (categories, severities, mitigations grounded in current code) so the documentation agent can draft the markdown without re-doing the architecture work. The developer agent does no Go coding for this ticket — only markdown.

## Files to read first

- `docs/architecture.md:33-35` — current "Threat model" section that says "to be written"; the AC requires this section to link to the new doc instead.
- `docs/PROJECT-MEMORY.md:6-18` — "What's built" table is the source of truth for v1 mitigations vs deferred work. Anything marked "Not started" is *deferred*, not *missing*; the doc must frame it that way.
- `cmd/pyrycode-relay/main.go:53-95` — the four `http.Server` timeout fields (`ReadHeaderTimeout: 10s`, `ReadTimeout: 60s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`). These are the slow-loris mitigation and the doc must cite them by name.
- `cmd/pyrycode-relay/main.go:111-116` — `defaultCertCache()` returns `~/.pyrycode-relay/certs`; the doc names this path when describing the autocert cache.
- `internal/relay/tls.go:16-55` — `ErrCacheDirInsecure` and the `0o077` permission check; the doc cites both the sentinel name and the file:line so the "cert & key handling" entry has a code anchor.
- `internal/relay/tls.go:80-88` — `TLSConfig()` pins `MinVersion: tls.VersionTLS12`; cite this as the TLS-config mitigation.
- `cmd/pyrycode-relay/main.go:39` — slog handler is `slog.NewTextHandler(os.Stderr, nil)` writing to stderr. The doc's log-hygiene section must note that retention/rotation is operator-owned (systemd journal or similar) because the relay itself does no log file management.
- `go.mod` — current deps are `golang.org/x/crypto v0.50.0` (used for `acme/autocert`) and indirects `golang.org/x/net`, `golang.org/x/text`. The supply-chain entry enumerates *exactly these*; the WS library is "to be added".
- `Makefile` — confirms `govulncheck` is wired into `make lint` (architect verified during prep). The supply-chain entry cites `make lint` as the v1 mitigation.
- [`pyrycode/pyrycode/docs/protocol-mobile.md` § Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model) — structural template (severity / mitigation / residual / deferred per threat) and the doc that this one explicitly differentiates from. The opening paragraph links here.
- `docs/specs/architecture/9-autocert-tls.md:55-130` — established formatting conventions for spec-style markdown in this repo (used as a style reference for the doc agent only — the deliverable lives in `docs/`, not `docs/specs/`).

## Context

The protocol spec already covers wire-protocol-level threats (prompt injection, server-id race, MITM, token leak, replay, DoS-by-malformed-frame). This ticket adds the *operational* layer: threats specific to the relay binary as a deployed Linux process — VPS compromise, dependency compromise, log accidents, cert-cache permissions. These threats don't appear in the protocol because they aren't wire-protocol concerns; they appear once you run the relay on a real machine.

Two invariants drive the doc's framing:

1. **Distinguish from wire-protocol threats.** The opening paragraph names the protocol spec's Security model section and says "this doc covers operational concerns; that doc covers wire-protocol concerns." Without this, the two docs become confusable and the security-review agent can't tell which one to update.
2. **v1-deferred is a first-class status, not a gap.** Per-IP rate limiting, connection-count caps, SBOM, and a chosen WS library are all "Not started" in `PROJECT-MEMORY.md`. The doc frames them as *deferred with a future-hardening trigger*, not as missing controls — otherwise it reads as a remediation list and gets actioned prematurely.

## The threat list (architect's structured output)

The documentation agent renders this as markdown. Each threat is one section with the four fields the AC requires: **severity / v1 mitigation / residual risk / future hardening**. Severities are one of `low` / `medium` / `high`. The list below is exhaustive — the doc must cover every entry and add no others without first questioning the architect.

### 1. Deploy security — VPS compromise

- **Severity:** `high`
- **v1 mitigation:** Operator-owned. SSH key auth only (no passwords); restricted admin set; OS auto-updates (unattended-upgrades or equivalent); host-level intrusion detection out of scope for v1.
- **Residual risk:** If the VPS is rooted, every byte through the relay is exposed (TLS terminates here). This is the operator-side framing of the protocol spec's "relay operator MITM" threat — same plaintext exposure, different actor.
- **Future hardening:** Documented deploy runbook; per-operator SSH-key inventory; `fail2ban` or equivalent on the SSH port; consider host-based IDS once user count justifies the operational cost.

### 2. Supply chain — Go dependencies

- **Severity:** `medium`
- **v1 mitigation:** Minimal dep surface (stdlib + `golang.org/x/crypto v0.50.0` for `acme/autocert`; transitively `golang.org/x/net`, `golang.org/x/text`). `go.sum` provides per-module checksum verification on every build. `govulncheck` runs in `make lint` and gates merge. Dependency additions are deliberate (project-memory convention: "new deps need a justification").
- **Residual risk:** A compromised `golang.org/x/crypto` release would expose TLS private-key handling. A compromised future WS library would see every routed frame in cleartext (the relay is the TLS terminus). `go.sum` defends against tampered downloads, not against a malicious release tagged by an authentic maintainer.
- **Future hardening:** SBOM generation in CI; pinned-version review on every `go.mod` change; consider Go module proxy mirroring once any user data flows. The doc explicitly notes: "the WS library will be added to this list when chosen" (per ticket — `nhooyr.io/websocket` is *not* a current dep).

### 3. DoS resistance — connection floods, slow-loris, fork-bomb retry

- **Severity:** `medium`
- **v1 mitigation:** Slow-loris is mitigated by the `http.Server` timeouts in `cmd/pyrycode-relay/main.go:53-95` (`ReadHeaderTimeout: 10s`, `ReadTimeout: 60s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`). Bandwidth amplification is structurally absent: the relay forwards 1:1, no factor. Connection-count caps and per-IP rate limits are *deferred* — none in v1.
- **Residual risk:** A misbehaving or malicious client can open many concurrent WS connections (once WS is built) and exhaust file descriptors / RAM. A runaway client retry loop is not throttled. A single attacker can saturate the public bandwidth.
- **Future hardening:** Connection-count cap (per-IP and global) on the WS upgrade path; token-bucket rate limit on `/v1/server` and `/v1/client`; reverse-proxy fronting for L4 protections once a chosen WS library is in place. **Trigger:** first observed flood, or first paying user.

### 4. Log hygiene — what must not be logged

- **Severity:** `high` (single accidental `slog.Info("frame", "body", payload)` call leaks user data)
- **v1 mitigation:** The relay logs to stderr via `slog.NewTextHandler` (`cmd/pyrycode-relay/main.go:39`). The doc enumerates two lists explicitly:
  - **MUST NOT be logged:** payload bodies (`json.RawMessage` inner frames), `x-pyrycode-token` values or any header token, full request headers, full URL paths if they ever carry tokens.
  - **MAY be logged:** server-id, `conn-id`, device-name (advertised by the binary, not user-secret), remote host (IP), event types (`upgrade`, `forward`, `close`), close codes (`4401`, `4404`, `4409`).
- **Residual risk:** A future contributor adds a debug log statement that prints a payload body or token. Code review and the AC of any frame-forwarding ticket are the first line of defence; runtime redaction is not built.
- **Future hardening:** A `slog` middleware that redacts known sensitive keys structurally (so the rule is enforced by code, not vigilance). Log rotation/retention is operator-owned (systemd journal, or `logrotate` on a flat file) — the relay itself writes only to stderr and does no rotation.

### 5. Cert & key handling — autocert cache directory

- **Severity:** `high`
- **v1 mitigation:** `internal/relay/tls.go:16-55` enforces `0700` on the cache dir at startup. If permissions are broader than `0700` (any group/world bit set), `NewAutocertManager` returns `ErrCacheDirInsecure` and the process refuses to start (loud-failure pattern, project-memory convention). Default location is `~/.pyrycode-relay/certs` (`cmd/pyrycode-relay/main.go:111-116`). Key rotation is handled by autocert (Let's Encrypt 90-day lifetime; renewal at ~30 days remaining).
- **Residual risk:** If a separate process running as the relay's UID is compromised, it reads the cache and exfiltrates the private key. `0700` defends against other UIDs on the same host, not against same-UID compromise.
- **Future hardening:** Move private keys to a dedicated secret manager once operationally justified; until then, the deploy doc enforces "the relay's UID owns nothing else."

### 6. TLS configuration — cipher suites, version pin

- **Severity:** `low` (Go's defaults are conservative; the doc records the choice rather than flagging risk)
- **v1 mitigation:** `internal/relay/tls.go:80-88` (`TLSConfig`) pins `MinVersion: tls.VersionTLS12`. Cipher-suite selection uses Go's secure defaults; no custom override.
- **Residual risk:** A future Go version weakens defaults — unlikely but worth re-checking on Go upgrade.
- **Future hardening:** Re-read this entry on every Go minor-version bump; consider explicit `CipherSuites` only if a specific compliance reason emerges.

### 7. Error response leakage — public-path error bodies

- **Severity:** `medium`
- **v1 mitigation:** Today's public surface is small (`/healthz` returns "ok"; `:80` returns `404` for non-challenge traffic per ADR-0002; `:443` returns `421` for Host mismatch). None leak internal state. The doc's rule for new endpoints: **public-facing handlers return generic messages; detailed errors go to logs only**.
- **Residual risk:** Future endpoints (WS upgrade error paths in particular) may write `err.Error()` to the response body and leak filesystem paths, dependency names, or stack traces.
- **Future hardening:** A handler-level guideline in `CODING-STYLE.md` once one exists; for now this doc is the canonical reference.

## "Triggers for re-review" section

The doc closes with a bulleted list that names exactly when this document must be revisited:

- A new dependency is added to `go.mod`.
- The deploy target changes (new VPS provider, container platform, managed K8s).
- A new public endpoint is exposed (any new path under `/v1/*` or otherwise).
- A security incident occurs (any unexpected behaviour with security implications, even if no compromise is confirmed).
- The WebSocket library is chosen (re-evaluate Supply chain + DoS entries together).

## File-level deliverable

Two files change. No code, no tests.

1. **New: `docs/threat-model.md`** (~150 lines markdown). Structure:
   - **Title + intro paragraph.** One paragraph explicitly distinguishing this doc from the protocol spec's Security model section, with a link to that section. State the scope: operational threats to the relay-as-deployed-process, not wire-protocol threats.
   - **Threat sections.** Seven `##` headings, one per entry above (Deploy security; Supply chain; DoS resistance; Log hygiene; Cert & key handling; TLS configuration; Error response leakage). Each section uses the four-field structure: **Severity** / **v1 mitigation** / **Residual risk** / **Future hardening**. Use the same prose voice and density as the protocol spec's Security model — short paragraphs, not bullet salad. Cite `file:line` anchors for every code-grounded mitigation.
   - **`## Triggers for re-review`.** The bullet list above, verbatim in spirit.
2. **Modified: `docs/architecture.md`.** Replace the existing "Threat model" section (lines 33-35) with a one-paragraph pointer:

   > ## Threat model
   >
   > Wire-protocol threats live in the protocol spec's [Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model). Operational threats specific to the relay binary as a deployed process — deploy, supply chain, DoS, log hygiene, cert handling, TLS config, error-leakage — live in [`docs/threat-model.md`](./threat-model.md).

   Do not delete the section heading; just replace its body.

## Out of scope (explicit non-deliverables)

- Pen-test write-up. No pen test has been performed.
- Compliance frameworks (GDPR, SOC2, ISO 27001). No applicable obligations in v1; user-data flows are minimal.
- Per-CVE response runbook. Premature; revisit when there are users.
- Code changes. This ticket is markdown-only. Any temptation to "while I'm here, add a redacting log middleware" or "tighten the cache-dir check" is out of scope and gets a separate ticket.

## Open questions

1. **Section ordering.** The threat list above is roughly "infra outward to wire" (deploy → supply chain → DoS → logs → certs → TLS → errors). The protocol spec orders by severity. The doc agent should pick one and be consistent; architect's preference is the order above (operational reading flow), but the agent may reorder if it improves narrative.
2. **`0o077` vs `0700` wording.** The code uses the bitmask check `mode&0o077 != 0`; the human-readable framing is "must be `0700`". The doc should use `0700` in prose (it's what an operator types) and note the code-side bitmask only if a footnote is warranted.
3. **Whether to name the future WS library.** The ticket says don't pre-pick. The doc says "the WebSocket library, when chosen, joins this list" without naming a candidate. Resist the urge to name `nhooyr.io/websocket` or `gorilla/websocket` even informally.
