# Security follow-ups — deferral catalog

This document collects hardening work intentionally deferred from v1, in one place, with enough context that a future session can pick from the menu without re-deriving rationale or scope. **Presence here is NOT a commitment to ship — only a commitment to remember.**

The canonical security doc remains [`threat-model.md`](threat-model.md); this file complements it. Items here either expand "Future hardening" lines scattered across `threat-model.md`'s seven threat categories, or address questions (like "drop probes without responses") that span multiple categories and have no single section home.

For each item: **what it buys**, **trigger to revisit**, **rough scope** (S = ~half a day, M = ~1-3 days, L = bigger).

## Application-layer "drop without response" options

The relay currently answers every unsolicited probe at the application layer (`404` on `:80` non-challenge, `421` on `:443` Host mismatch, `429` on rate-limit denial, `400` on `/v1/*` missing required headers). This is deliberate — error responses are useful telemetry for legitimate-client diagnosis. The trade-off is that probers learn what the relay rejects and can tune their tools. The options below convert specific responses to silent TCP close.

- **Convert `429` rate-limit denial to TCP close.** Site: `internal/relay/ratelimit_middleware.go`. Buys: denies abusers the signal that the rate-limit fired. Cost: legitimate clients hitting the limit by mistake see "connection reset" instead of a parseable status — harder to diagnose. Trigger: rate-limit log volume shows organized scrapers tuning their rate against the `429`. Scope: S (~10 LOC + test update).
- **Convert `400` missing-WS-headers to TCP close.** Sites: `internal/relay/server_endpoint.go`, `client_endpoint.go`. Buys: most non-pyrycode clients (browsers, vuln scanners, port-scan tools) hit `/v1/server` or `/v1/client` without `X-Pyrycode-*` headers and get a clear `400` that fingerprints the relay. Closing denies that info. Trigger: scanner traffic dominates rate-limit logs. Scope: S.
- **Convert `421` Host-mismatch to TCP close.** Site: `internal/relay/tls.go` (`EnforceHost`, lines 89-101). Buys: same fingerprinting denial. Note: `autocert.HostWhitelist` already drops the TLS handshake itself for SNIs other than the configured domain — the handshake never completes, no application response is sent. `EnforceHost` 421 only fires when SNI matches but `Host:` header differs, which is rare. Trigger: probably never worth the change. Scope: S, but lowest priority.
- **`/healthz` exposure.** Currently `200 OK` with body `ok`, unauthenticated, no rate limit. Consider: bind to a Fly private-network address only and use Fly health checks; OR keep public but rate-limit aggressively. Do NOT drop without response — that breaks Fly health probes. Trigger: probes of `/healthz` show up as a meaningful share of unsolicited traffic. Scope: S (private-network bind) or M (separate rate-limit policy for `/healthz`).

Recommendation per option: **observe first**. Wait for logs/metrics to show actual probe patterns before committing to silent-close. Until then, the application-layer responses are useful telemetry.

## Deferred from `threat-model.md`

Each item below already exists as a "Future hardening" line in `threat-model.md`. Restated here with a single-sentence trigger + rough scope so the next session can pick from the menu.

- **Per-IP concurrent connection cap.** Rate-limit gates attempt rate (10/min/IP, burst 20), NOT concurrent connection count. A slow attacker can stay under the rate limit and still exhaust FDs/RAM. Trigger: first observed connection flood. Scope: M (~50 LOC + per-IP state in `ratelimit.go`'s bucket map + test matrix).
- **Decouple pong handling from the forwarder's phone sends (issue #140).** A binary's pong is only read while `StartBinaryForwarder` sits in its conn's `Read`; a synchronous `phone.Send` to a stalled phone can hold that `Read`, and pong processing with it, for up to `writeTimeout` (10s) per frame. Since [#112](knowledge/codebase/112.md), this means a *live* incumbent stalled on a slow phone can miss the `/v1/server` conflict probe (also 10s) and be displaced by a concurrent duplicate-binary claim — the heartbeat has the same property with a 30s margin. Buys: a live binary can no longer be mistaken for dead regardless of phone-send stalls. Direction: per-phone bounded send queues, or any `Send` variant that doesn't block the read pump — needs its own design, not a one-line change. Trigger: evidence of a live binary being displaced in production. Scope: M (new send-queue plumbing in `forward.go` + race tests).
- **Slowloris connection-count cap (separate from request timeouts).** Per-connection slowness is covered by `ReadHeaderTimeout: 5s` pre-upgrade, and post-upgrade by the per-message read deadline ([WSConn adapter](knowledge/features/ws-conn-adapter.md), `readMessageTimeout` 30s, #111) plus the write timeout on `Send` (#15). A flood of connections each held for up to ~30s (one message deadline) is not bounded — many slow peers under the per-IP upgrade rate limit can still exhaust FDs/RAM concurrently. Trigger: first observed slow-client flood. Scope: M (overlaps with per-IP conn cap above; might ship together).
- **Multi-instance shared rate-limit state.** Current limiter is in-process; multi-instance deploy would let an attacker rate-limit-bypass via the load balancer. Trigger: first multi-instance deployment (currently forbidden by `#65`). Scope: L (needs Redis/NATS/sticky-LB choice + plumbing).
- **CIDR-aware trusted-proxy chain for `X-Forwarded-For`.** Today `--trust-x-forwarded-for` is binary all-or-nothing. A real reverse-proxy chain needs per-CIDR trust. Trigger: deployment behind a multi-tier proxy. Scope: M.
- **Restrict the PROXY protocol listener to trusted upstreams.** Since [#110](knowledge/codebase/110.md), `--https-proxy-protocol` ([feature doc](knowledge/features/proxy-protocol-listener.md)) honours a PROXY v2 header from *any* TCP peer that reaches the HTTPS listener (`ConnPolicy: REQUIRE`, no source allowlist) — the wrapper validates header *shape*, not *sender*. On Fly, a peer on the app's private 6PN network that reaches internal port 8443 directly, bypassing Fly's public edge, can forge its own source address and pick its rate-limit bucket. Fly does not publish stable proxy IP ranges to allowlist against today, so a source restriction would need either a Fly-provided mechanism or a private-network-only bind for the internal port. Trigger: Fly publishes stable edge-proxy ranges, or evidence of an org-internal actor exploiting this surfaces. Scope: S–M depending on whether Fly provides ranges (S, allowlist check in `ConnPolicy`) or not (M, needs a different mechanism).
- **Geo-blocking.** Not in threat model; requires external IP-geolocation source. Trigger: compliance requirement or abuse pattern from a specific region. Scope: M (vendor dep + middleware).
- **SBOM generation in CI.** Supply-chain hardening. Trigger: first user or audit requirement. Scope: S (add a `cyclonedx-gomod` step to `security-scan.yml`).
- **External pen-test.** No external audit has been performed. Trigger: pre-production deployment with non-trivial user data, or regulatory requirement. Scope: external engagement.
- **TLS handshake fingerprinting defense (JA3/JA4).** Go's TLS is idiomatic, not disguised. A targeted attacker can fingerprint the relay vs other Go services. Trigger: targeted attack evidence. Scope: L; usually requires a non-Go TLS layer in front.
- **Compliance frameworks (GDPR / SOC 2 / ISO 27001).** v1 scope: single operator, minimal user data. Trigger: when users and obligations emerge. Scope: framework-dependent, typically L.
- **`fail2ban` (or equivalent) on the SSH port.** Host-level intrusion detection — out of v1, also out of the binary scope. Trigger: post-launch operational hardening once host-tier abuse is observed. Scope: S (host config, not code).

## Network-layer options NOT in the binary

These would harden the deployment without changing relay source code. Listed so a future operator knows what's on the table.

- **Fly Machines `services.concurrency`.** Tunable concurrency cap per service in `fly.toml`; Fly drops excess connections at the edge before they reach the relay. Fly also provides built-in anycast DDoS absorption (no config needed). Operator-side, no code change. Trigger: any abuse pattern; cheap to tune.
- **Cloudflare Spectrum (or similar TCP-passthrough CDN).** Could front the relay without breaking autocert — TLS still terminates in the binary, Spectrum passes TCP through. Brings WAF, IP reputation, larger DDoS absorption. Trade: operational dependency on Cloudflare. Free tier covers small scale. Trigger: abuse patterns warrant the dependency; not recommended for v1 because it conflicts with the relay's "minimal external surface" design principle.
- **External reverse proxy (nginx, Caddy, etc.).** Out of scope for this codebase — the relay IS the proxy in the conceptual stack. Mentioned only to rule it out.

## What's NOT deferred

For the in-scope mitigations (boot-time checks, TLS config, per-IP rate limiter, log allowlist, cert-cache permission enforcement, opaque-payload routing, error-response policy), see [`threat-model.md`](threat-model.md). This document does not duplicate.
