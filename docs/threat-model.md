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

**v1 mitigation:** A small, justified dependency surface (`go.mod`): `github.com/coder/websocket v1.8.14` (the WebSocket implementation, chosen 2026-05-28 in [ADR-0010](knowledge/decisions/0010-coder-websocket-migration.md)), `github.com/prometheus/client_golang v1.23.2` with its `client_model v0.6.2` and `common v0.66.1` companions (metrics, [ADR-0008](knowledge/decisions/0008-prometheus-client-adoption.md)), `golang.org/x/crypto v0.57.0` for `acme/autocert`, `golang.org/x/oauth2 v0.37.0` (the `jwt` subpackage only) to mint FCM access tokens from the operator's service-account key ([FCM push sender](knowledge/features/fcm-push-sender.md), [ADR-0011](knowledge/decisions/0011-oauth2-jwt-not-google-for-fcm.md)) — this path pulls in no packages outside the `x/oauth2` module itself; `cloud.google.com/go/compute/metadata` is named in `x/oauth2`'s own `go.mod` (via the unused `oauth2/google` subpackage) but is not compiled into the binary — and `github.com/pires/go-proxyproto v0.15.0` to parse Fly's PROXY protocol v2 header on the pre-TLS HTTPS listener path ([PROXY protocol listener](knowledge/features/proxy-protocol-listener.md), [ADR-0012](knowledge/decisions/0012-go-proxyproto-for-proxy-header-parsing.md)) — its root package imports the standard library only, no transitive modules compiled in. `go.sum` provides per-module checksum verification on every build. `govulncheck` runs in `make lint` (#41 pins it to a semver tag) and daily against `main` in the `govulncheck` job of `.github/workflows/security-scan.yml` (#72) — the repo's only workflow since the convenience `ci.yml` was removed 2026-05-24. The runtime image is additionally scanned by Trivy in the same workflow's `image-scan` job (#68/#72), covering OS-package + Go-binary content, so CVEs disclosed against deps that have not changed since the last PR surface as a red Actions run within ≤24h rather than staying invisible until the next dep bump. A regression also opens a `security-sensitive`-labelled GitHub issue via the workflow's `file-issue` job (#73), lifting the red Actions row to a tracked work-item in the triage queue rather than leaving it as a passive signal. New dependencies require a justification (`docs/PROJECT-MEMORY.md`).

**Residual risk:** A compromised release of `golang.org/x/crypto` would expose TLS private-key handling. A compromised release of `github.com/coder/websocket` would see every routed frame in cleartext, since the relay is the TLS terminus. A compromised release of `golang.org/x/oauth2` would see the FCM service-account private key (held in memory only — see [FCM push sender](knowledge/features/fcm-push-sender.md)) and every device token the relay sends to wake a phone. A compromised release of `github.com/pires/go-proxyproto` sits on the least-trusted, pre-TLS path of the HTTPS listener when `--https-proxy-protocol` is enabled — it sees only PROXY header bytes and TLS ciphertext, never a decrypted frame, but a malicious release could falsify the address every downstream rate-limit decision keys on. `go.sum` defends against tampered downloads, not against a malicious release tagged by an authentic maintainer.

**Future hardening:** SBOM generation in CI; pinned-version review on every `go.mod` change; consider Go module proxy mirroring once any user data flows through the relay.

## Outbound network calls — FCM push wake

**Severity:** `medium`. Trips the "new outbound network call" re-review trigger below (#133).

**v1 mitigation:** A `push_wake` routing envelope from a connected binary (`{"push_wake":{"platform":"fcm","token":"…"}}`, no `conn_id` — see [Push wake dispatch](knowledge/features/push-wake-dispatch.md)) is the only trigger for the relay originating an outbound call. `parsePushWake` (`internal/relay/envelope.go`) validates the object structurally before anything is dispatched: platform must be exactly `fcm` (`apns` and anything else is dropped), token must be non-empty. `PushWaker.Request` (`internal/relay/push_wake.go`) then admits or refuses the wake under two independent, named limits before a single outbound call is made:

- **Per-server-id rate** — `pushWakeRefillEvery` (10s) / `pushWakeBurst` (6), an `IPRateLimiter` keyed on server-id. Bounds how fast one binary can push wakes at Google.
- **Relay-wide in-flight cap** — `pushWakeMaxInFlight` (32), a buffered channel. Bounds concurrent goroutines and outbound sockets across every binary regardless of how many server-ids they claim.

A wake over either limit is dropped with a log line, never queued — no backpressure state accumulates. The credential (`PYRYCODE_RELAY_FCM_CREDENTIALS`, a whole Google service-account JSON key) and the device token are never logged: every drop and send-failure log line carries only `server_id` / `binary_conn_id` / `err`, and `FCMSender`'s errors carry at most an HTTP status (see [FCM push sender](knowledge/features/fcm-push-sender.md) § Error handling). The wake itself carries no payload, so even a successful send tells the phone only to reconnect.

**Residual risk:** The relay cannot verify a binary's claimed device token belongs to one of that binary's own phones — any binary that has claimed a server-id can ask the relay to wake an arbitrary FCM token. The two limits bound the *rate* of abuse, not eliminate it: a single binary can still cause up to `pushWakeBurst` wakes per `pushWakeRefillEvery` window indefinitely. Because the wake carries no payload, the worst case is an app repeatedly reconnecting, not data exposure. The in-flight cap bounds concurrency relay-wide, not total throughput when many server-ids are claimed simultaneously — a set of colluding binaries, each under its own per-server-id bucket, can still saturate the 32-slot cap. No metric exists yet to see this happening (out of scope per the ticket).

**Future hardening:** A global wakes-per-second ceiling (distinct from the in-flight cap) if abuse across many server-ids is observed; a metric on dropped/refused wakes to make the residual risk observable; telling the daemon a token is dead (currently out of scope — no retry, no dead-token callback).

## DoS resistance — connection floods, slow-loris, fork-bomb retry

**Severity:** `medium`.

**v1 mitigation:** Slow-loris is mitigated by the `http.Server` timeouts in `cmd/pyrycode-relay/main.go` (`ReadHeaderTimeout: 5s`, `ReadTimeout: 60s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`), applied identically to the insecure listener, the autocert TLS listener, the autocert HTTP-01 listener, and the loopback metrics listener (`internal/relay/metrics_listen.go`). `ReadHeaderTimeout` bounds the pre-upgrade window — the gap between TCP accept and full HTTP header receipt — at 5s; after `websocket.Accept` hijacks the connection, post-upgrade slow peers are bounded by three complementary mechanisms: a per-message read deadline armed the moment a message's first data-frame header arrives (`internal/relay/ws_conn.go`'s `readMessageTimeout`, 30s — [WSConn adapter](knowledge/features/ws-conn-adapter.md), #111), the write timeout on `Send` (`writeTimeout`, 10s, #15), and the heartbeat ping/pong for idle liveness (#7). The read deadline never bounds the idle gap before a message starts — a peer may sit silent indefinitely between messages — only the time to receive one once it begins, so a peer that dribbles a started message one byte at a time while still answering pings is cut off within the deadline instead of pinning the connection forever. A per-IP token-bucket rate limit fronts the `/v1/server` and `/v1/client` upgrade paths (`cmd/pyrycode-relay/main.go` policy block, `internal/relay/ratelimit_middleware.go`): ~10 attempts/IP/minute steady-state, burst 20, eviction sweep every 5 minutes. Over-cap attempts receive `429 Too Many Requests` before `websocket.Accept` runs; the wrapped registry is never touched on a deny. The middleware extracts the source IP via `relay.ClientIP`, which reads `r.RemoteAddr`; `X-Forwarded-For` is honoured only when the operator opts in with `--trust-x-forwarded-for` (default `false`). In production, `RemoteAddr` on the HTTPS listener is no longer the raw socket peer: Fly's raw-TCP services hand the relay Fly's own edge-proxy address, so since [#110](knowledge/codebase/110.md) the `443` port runs `--https-proxy-protocol` ([PROXY protocol listener](knowledge/features/proxy-protocol-listener.md)), which requires and parses a Fly `proxy_proto` v2 header before the TLS handshake and rewrites `RemoteAddr` to the header's source — the per-IP key the rate limiter reads is the client's real address, not Fly's. `/healthz` is intentionally unwrapped — it must remain pollable from monitoring without throttling. Bandwidth amplification is structurally absent: the relay forwards 1:1, so there is no amplification factor an attacker can lever. Connection-count caps (per-IP and global, distinct from the attempt-rate cap) are deferred. A conflicting `/v1/server` claim now probes the incumbent binary for liveness before displacing it ([`/v1/server` § conflict probe](knowledge/features/server-endpoint.md), #112); the probe costs one ping frame per upgrade attempt and holds one handler goroutine for at most `conflictProbeTimeout` (10s) — no worse than the pre-#112 upgraded-then-closed conflict, and no amplification (one claim, one ping). Since [#113](knowledge/codebase/113.md), delivering a routed frame to a phone no longer runs on the binary forwarder's read loop at all: each phone is registered behind a bounded [per-phone delivery queue](knowledge/features/phone-outbox.md) (16 frames deep), and the forwarder only enqueues — an operation that never blocks. A stalled phone can therefore no longer hold up the binary's `Read`, its sibling phones, or the binary's own pong processing; it is bounded first by the queue filling (4 MiB worst case per phone, 64 MiB for a full 16-phone server-id) and then by `WSConn.Send`'s 10s write deadline, and is closed with `1011` either way. Since [#114](knowledge/codebase/114.md), a relay-wide [connection cap](knowledge/features/connection-cap.md) (`internal/relay/conncap.go`'s `ConnCap`, `--max-connections`, default 20) bounds the number of *live* connections across `/v1/server` and `/v1/client` combined — closing the OOM path a slow attacker could previously reach by staying under the rate limiter's refill rate. One `ConnCap` instance wraps both upgrade handlers, nested inside the rate limiter and the HTTP-429-only observers (`WrapServerRateLimitDeny(rateLimit(connCap.Wrap(...)))`), so a rate-limited attempt never takes a slot. A slot is taken right before `websocket.Accept` and released by `defer` when the handler returns; because both handlers stay inside `ServeHTTP` for the connection's whole life and return only after their own cleanup, that single acquire/release pair covers every close path. Over cap: an empty-body `503` and one `conn_cap_reached` Warn line, before any header is read. `--max-connections` must be positive; the relay refuses to start otherwise.

**Residual risk:** The global cap is relay-wide, not per source: the rate limiter's burst (20) equals the default global cap (20), so a single IP can still fill the whole cap in one burst and lock out new upgrades relay-wide while its own connections keep working. Established connections are unaffected, and the relay no longer risks OOM from this path — but that lockout is now the residual, and the remedy is the still-deferred per-IP concurrent connection cap (see [`security-followups.md`](security-followups.md)). Connections still in the TLS or HTTP-header phase are not counted against the global cap; they remain bounded only by `ReadHeaderTimeout` (5s) and the per-IP rate limiter. The single-instance limiter is per-process — a future multi-instance deployment behind L4 load balancing would need shared state to converge on a global limit. `X-Forwarded-For` is binary trust (all-or-nothing) — a CIDR-aware trusted-proxy chain is not implemented. The PROXY header is trusted from *any* upstream that reaches the HTTPS listener (`ConnPolicy: REQUIRE`, not scoped to a source allowlist) — a peer on the Fly app's private 6PN network that reaches internal port 8443 directly, bypassing Fly's public edge, can forge its own source address and choose its rate-limit bucket. That is an org-internal attacker, not an internet one; see [`security-followups.md`](security-followups.md). The per-phone delivery queue's 64 MiB-per-server-id worst case is now also bounded by the global connection cap: 16 phones on one server-id already consume 16 of the default 20 global slots, so a hostile operator running several non-reading-phone-heavy daemons is throttled by the same cap as any other connection flood, not exempt from it. What remains is the same per-source gap as above — nothing stops one operator's several daemons, each on a different server-id, from being the ones that fill those slots.

**Future hardening:** A per-IP concurrent connection cap layered on top of the global cap shipped in #114; a CIDR-allowlisted trusted-proxy chain for `X-Forwarded-For`; reverse-proxy fronting for L4 protections; shared rate-limit state for multi-instance deployments. Trigger: first observed single-IP connection flood, first multi-instance deployment, or first paying user.

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
- A new outbound network call is added — the relay itself originates a request, rather than only serving one. First tripped by [Outbound network calls — FCM push wake](#outbound-network-calls--fcm-push-wake) (#133).
- A security incident occurs — any unexpected behaviour with security implications, even if no compromise is confirmed.

## Out of scope

- Pen-test write-up. No pen test has been performed.
- Compliance frameworks (GDPR, SOC 2, ISO 27001). No applicable obligations in v1; user-data flows are minimal.
- Per-CVE response runbook. Premature; revisit when there are users.
