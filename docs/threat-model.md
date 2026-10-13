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

Production runs on Fly.io with operator-only deploy privileges. The deploy
credential stays in the operator's local environment; GitHub Actions holds only
the separate `FLY_READONLY_TOKEN` repository secret for deployed-image discovery
and download (#176). Fly's standard read-only token has
[org-scoped read access](https://docs.fly.io/security/tokens/#org-scoped-tokens),
including the owning organization's other apps and resources, even though the
helper queries only `pyrycode-relay`. It cannot deploy or mutate Fly resources;
a deploy or personal token must never be substituted for it.

The `deployed-image-scan` job is restricted to `schedule` or
`workflow_dispatch` on `refs/heads/main`, with a checkout of `main`. Only its
discovery/download steps receive the secret. Discovery uses a fixed Machines
API GET without redirects and publishes only the validated digest and UTC
observation time. Download uses password stdin for Docker login, excludes the
token from subprocess environments and suppresses subprocess output. Its private
temporary registry config is removed on success and failure before Trivy scans
the saved archive. The token is never printed or put in outputs/artifacts;
Trivy, rendering and the separate issue-filing job receive no Fly credential.
Scanner jobs retain `contents: read`; `issues: write` belongs only to
`file-issue`. Explicit expiry, rotation and the initial live access check are
operator-owned in [the bootstrap runbook](deploy.md#daily-security-scan-credential).

The standalone [restart watcher](deploy.md#restart-incident-monitoring) (#198)
adds an operational outbound boundary on pyrybox: one read-only GET to the
selected Machine at fixed `api.machines.dev`, then a trusted assistant-channel
notification command. The dedicated local token is expiring and org-scoped,
including read access to other apps; it is separate from CI and deploy credentials.
Requests use TLS with no redirects/retries, 10s socket/20s request deadlines
and a 1 MiB cap. Operator argv receives a generated `0600` message file,
never Fly-derived command text, and runs without a shell for at most 15s with
an allowlisted environment excluding the Fly credential and discarded output.

Private configuration/token/state files require current-UID ownership and
`0600`; the state directory requires `0700`. Final-component symlinks and
insecure existing storage are rejected. A nonblocking lock serializes polls,
and state replacement uses file/directory fsync. API input is projected to
validated lifecycle evidence; duplicates must agree on full flags/codes and
exit time across polls, and history overlap must also satisfy platform-time
coverage. Conflicting or incomplete evidence fails monitoring rather than
fabricating an incident or recovery. Version 2 state validates this evidence;
incomplete version 1 is rejected unchanged. The relay has no new payload
access, public endpoint or deployment target, and its content blindness and
runtime statelessness are unchanged.

**Daemon admission:** Only [enforce](knowledge/features/server-endpoint.md#daemon-admission-before-upgrade)
blocks anonymous identity reservation and daemon-originated wakes. Off (default)
and observe retain anonymous admission. Provisioning and explicit production
activation are [operator work](deploy.md#daemon-admission-activation), not a result
of merging or passing offline gates. A stolen admission token permits claiming
its exact bound identity and abusing push; it supplies neither the daemon's
Noise private key nor phone authorization. Removal plus Relay restart revokes
future admission and drops existing connections, claims and grace state.

**Residual risk:** A rooted host or compromised Relay can capture admission
headers and the configured credential map, the transient opaque phone-token
header, device tokens in relay-directed push requests and routing/traffic metadata.
TLS terminates here, but application content remains end-to-end Noise ciphertext:
Relay holds no Noise keys and a memory dump does not yield application plaintext.
Admission does not protect against a malicious Relay, which can deny service,
misroute, drop or delay frames and abuse push. See the protocol's
[relay operator MITM boundary](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model).
The existing `x-pyrycode-token` phone header is still visible in memory at upgrade;
Relay presence-checks and discards it without parsing, comparing or forwarding it.
Daemon phone authorization occurs inside the encrypted Noise handshake.

A compromised CI discovery/download step could expose the read-only credential
and its organization-wide read access. Event, branch and step restrictions limit
where the workflow supplies it; they do not narrow the token's Fly scope.
Production can lag `main` until the operator deploys: repository scan results
cannot establish that a patch shipped. The
[deployed-image result](knowledge/features/docker-image.md#deployed-image-scanning)
describes the digest at its observation time, and may be stale after a later
deploy.

Compromise of pyrybox or its scheduler UID could expose the watcher's org-wide
read credential, alter operational state or control trusted notification argv;
owner-only storage does not protect against that UID. Token expiry, API history
truncation, missed polls or channel failure can leave monitoring/delivery
unavailable. A send followed by interruption before the delivery-state save
can repeat a notice. The operator owns credential rotation and scheduler
failure visibility; [installation and delivered verification](deploy.md#restart-incident-response)
remain pending. The watcher cannot mutate Fly resources or authorize remediation.

**Future hardening:** A documented deploy runbook; a per-operator SSH-key inventory; `fail2ban` (or equivalent) on the SSH port; a host-based IDS once user count justifies the operational cost.

## Supply chain — Go dependencies

**Severity:** `medium`.

**v1 mitigation:** A small, justified dependency surface (`go.mod`): `github.com/coder/websocket v1.8.14` (the WebSocket implementation, chosen 2026-05-28 in [ADR-0010](knowledge/decisions/0010-coder-websocket-migration.md)), `github.com/prometheus/client_golang v1.23.2` with its `client_model v0.6.2` and `common v0.66.1` companions (metrics, [ADR-0008](knowledge/decisions/0008-prometheus-client-adoption.md)), `golang.org/x/crypto v0.57.0` for `acme/autocert`, `golang.org/x/oauth2 v0.37.0` (the `jwt` subpackage only) to mint FCM access tokens from the operator's service-account key ([FCM push sender](knowledge/features/fcm-push-sender.md), [ADR-0011](knowledge/decisions/0011-oauth2-jwt-not-google-for-fcm.md)) — this path pulls in no packages outside the `x/oauth2` module itself; `cloud.google.com/go/compute/metadata` is named in `x/oauth2`'s own `go.mod` (via the unused `oauth2/google` subpackage) but is not compiled into the binary — and `github.com/pires/go-proxyproto v0.15.0` to parse Fly's PROXY protocol v2 header on the pre-TLS HTTPS listener path ([PROXY protocol listener](knowledge/features/proxy-protocol-listener.md), [ADR-0012](knowledge/decisions/0012-go-proxyproto-for-proxy-header-parsing.md)) — its root package imports the standard library only, no transitive modules compiled in. `golang.org/x/net v0.60.0` is also a direct dependency for `netutil.LimitListener` ([ADR-0014](knowledge/decisions/0014-netutil-limitlistener-for-public-admission.md)): it was already pinned indirectly; promotion changes neither versions nor checksums. The package imports only `net` and `sync`, adds no parsing or transitive packages, and wraps opaque sockets for admission, release and shutdown. Standard `net` has no equivalent wrapper. `go.sum` provides per-module checksum verification on every build. `govulncheck` runs in `make lint` (#41 pins it to a semver tag) and daily against `main` in the `govulncheck` job of `.github/workflows/security-scan.yml` (#72) — the repo's only workflow since the convenience `ci.yml` was removed 2026-05-24. The runtime image is additionally scanned by Trivy in the same workflow's `image-scan` job (#68/#72), covering OS-package + Go-binary content, so CVEs disclosed against deps that have not changed since the last PR surface as a red Actions run within ≤24h rather than staying invisible until the next dep bump. A regression also opens a `security-sensitive`-labelled GitHub issue via the workflow's `file-issue` job (#73), lifting the red Actions row to a tracked work-item in the triage queue rather than leaving it as a passive signal. New dependencies require a justification (`docs/PROJECT-MEMORY.md`).

**Residual risk:** A compromised release of `golang.org/x/crypto` would expose TLS private-key handling. A compromised release of `github.com/coder/websocket` could see admission and opaque phone-token headers, routing metadata and routed Noise ciphertext after TLS termination, but Relay has no Noise keys to decrypt application content. A compromised release of `golang.org/x/oauth2` would see the FCM service-account private key (held in memory only — see [FCM push sender](knowledge/features/fcm-push-sender.md)) and every device token the relay sends to wake a phone. A compromised release of `github.com/pires/go-proxyproto` sits on the least-trusted, pre-TLS path of the HTTPS listener when `--https-proxy-protocol` is enabled — it sees only PROXY header bytes and TLS ciphertext, never a decrypted frame, but a malicious release could falsify the address every downstream rate-limit decision keys on. A compromised `golang.org/x/net/netutil` release could undermine admission or socket cleanup on every public listener; its current implementation does not read bytes. `go.sum` defends against tampered downloads, not against a malicious release tagged by an authentic maintainer.

Daemon admission adds only standard-library parsing and constant-time equality plus the existing private metrics registry; it introduces no credential service or dependency. A malicious dependency with process access can capture the startup credential map or undermine admission, regardless of its intended API boundary.

**Future hardening:** SBOM generation in CI; pinned-version review on every `go.mod` change; consider Go module proxy mirroring if the supply-chain risk warrants it.

## Outbound network calls — FCM push wake

**Severity:** `medium`. Trips the "new outbound network call" re-review trigger below (#133).

**v1 mitigation:** A `push_wake` routing envelope from a connected binary (`{"push_wake":{"platform":"fcm","token":"…"}}`, no `conn_id` — see [Push wake dispatch](knowledge/features/push-wake-dispatch.md)) is the trigger for an outbound push call. In enforce, only a daemon admitted for its exact identity can reach this path; rejected requests never upgrade or gain push access. Off/observe still allow anonymous daemon admission. `parsePushWake` (`internal/relay/envelope.go`) validates the object structurally before anything is dispatched: platform must be exactly `fcm` (`apns` and anything else is dropped), token must be non-empty. `PushWaker.Request` (`internal/relay/push_wake.go`) then checks closure, coalesces existing device jobs, and checks retained capacity and three initially full attempt allowances before dispatch:

- **Per-daemon allowance** — burst 6, refill one token per 10s, across all devices requested by the same server-id.
- **Per-device allowance** — burst 6, refill one token per 10s, shared across all daemons requesting the complete same device token.
- **Global allowance** — burst 32, refill one token per second, shared across all daemons and devices in the relay process.
- **Relay-wide retained-job cap** — `pushWakeMaxInFlight` (32), counting active sends and completed jobs still inside the coalescing window. A buffered channel also bounds active send goroutines and outbound sockets across every binary regardless of how many server-ids they claim.

The complete device token coalesces across daemon identities for a fixed ten
seconds from first admission, regardless of success or failure; an active job
suppresses duplicates even beyond expiry. At the exact boundary a completed
job is eligible for ordinary admission. After structural validation and the
closed check, duplicates return `nil` before capacity or allowance checks,
extend neither window nor deadline, spend nothing, create no daemon bucket
and emit no outcome. Distinct complete tokens remain independent. Full
SHA-256 job keys retain only fixed expiry and active metadata, no requester
list; completed jobs retain no raw token. Expired completed jobs are reclaimed
before capacity checks without erasing accounting debt. A new distinct device
at retained capacity is refused without charge or queue; an existing
coalesced device still succeeds.

Burst capacity is maximum stored credit; sustained refill is one per 10s for
each daemon/device and one per second globally, never six per 10s indefinitely.
Admission charges all three allowances and reserves a slot atomically under
one mutex. Rate, state-capacity and retained-capacity refusals spend nothing and
queue nothing; dispatched failures remain charged. Completing sends frees
slots without restoring tokens or freeing jobs still inside their window.
Changing daemon identity or reconnecting cannot reset the device or global
budget.

Daemon and device maps each retain at most 1,024 buckets with full 32-byte
SHA-256 keys independent of input length, rather than raw tokens or truncated
logging fingerprints. Five-minute idle expiry counts requests reaching
allowance checks, including refusals, as activity on existing keys; duplicates
and retained-capacity refusals do not touch them. Periodic sweeps or new-key
admission reclaim only fully refilled idle buckets; capacity pressure never
deletes debt. If a new
key still cannot fit, it is refused, while existing keys remain usable within
budget. All modes receive these single-process in-memory budgets and
coalescing; restart resets both. Multiple replicas would need shared accounting
and coalescing. Delivery remains payload-free and best-effort: there is no
retry, daemon reply, durable or exactly-once guarantee. Each actual send keeps
one whole-send ten-second deadline through authentication and delivery.
`Close` refuses subsequent structurally valid requests, cancels and joins
sends/shared authentication and the cleanup sweep, then releases retained
jobs. Repeated and concurrent closes join the same work.

A refused wake's log line carries only `server_id` / `binary_conn_id` / `err`, and every actual dispatched job logs exactly one outcome line attributed to its first admitting daemon — `push_wake_sent` or `push_wake_send_failed`, including a send cancelled by relay shutdown — carrying `server_id`, a one-way `token_fp` fingerprint of the device token, and the send's `duration`; a successful send additionally carries FCM's message id (`fcm_message_id`) when the reply carried a valid one, and a failed send carries `err` (#160). Raw device/admission tokens and service-account credentials remain absent from logs and errors; accounting digests are not logged. `FCMSender`'s errors carry at most an HTTP status plus FCM's enum-shaped `error.status` and `errorCode` (#150), never free text or the token (see [FCM push sender](knowledge/features/fcm-push-sender.md) § Error handling). The wake itself carries no payload, so even a successful send tells the phone only to reconnect.

**Residual risk:** Device-token ownership remains unverified. Enforce alone
authenticates daemon identity, including a holder of a stolen credential;
off/observe retain anonymous push abuse. Admission grants no Noise keys or
phone authorization. A holder can still cause arbitrary devices to reconnect
within the shared device/global budgets, and colluding holders can exhaust
those budgets or the 32 retained jobs, denying legitimate new wakes. Because
wakes carry no payload, this exposes no session data. Restart grants fresh
credit, and the budgets are not shared across processes. Every rate, state-capacity or
retained-capacity refusal increments
`pyrycode_relay_push_wakes_total{outcome="dropped"}` exactly once; its five fixed
labels carry no identities, tokens or fingerprints — see
[push metrics](knowledge/features/push-wake-dispatch.md#metrics).
Accepted duplicates count no outcome and produce no send log line; each
actual send is attributed only to its first admitting daemon.

**Future hardening:** Telling the daemon a token is dead remains out of scope
(no dead-token callback, despite `ErrFCMUnregistered` distinguishing it — see
[FCM push sender](knowledge/features/fcm-push-sender.md)). Bounded coalescing
is implemented by #211, split from #173; later retries must consume the same
attempt budgets.
Manual deployment and production admission activation remain
[operator follow-ups](deploy.md#daemon-admission-activation); repository checks
establish neither deployment nor live enforcement.

## DoS resistance — connection floods, slow-loris, fork-bomb retry

**Severity:** `medium`.

**HTTP parsing:** All three public servers in `run` — insecure, autocert
HTTPS and HTTP-01 — set `MaxHeaderBytes: 16 * 1024` (16 KiB), replacing Go's
1 MiB default (#181). The allowance covers the request line and header keys
and values before any handler runs; it does not limit request bodies or
WebSocket frames. Go's HTTP/1.1 parser permits buffer slack, so this is not
an exact 16 KiB wire-size cutoff. A clearly oversized 64 KiB header value
receives `431 Request Header Fields Too Large` before a handler is invoked.
The [HTTP-limit regression coverage](knowledge/features/autocert-tls.md#testing-http-limits)
checks this on all three public servers, including HTTPS/TLS. This reduces
per-request pre-upgrade memory exposure; the separate raw listener cap below
bounds concurrent accepted sockets, including those parsing headers.
Routing-header semantics remain defined by the [protocol's Authentication section](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#authentication).

**v1 mitigation:** Both raw-TCP services in `fly.toml` configure Fly
forwarding ceilings (`type = "connections"`, `hard_limit = 32`), independently
for 8080/external 80 and 8443/external 443 (#183). These match the public
listener default below. Fly [stops forwarding new traffic](https://docs.fly.io/reference/configuration#servicesconcurrency)
to a Machine at or above the service limit; immediate client drops are not
guaranteed. Saturation and recovery must be verified by the operator on an
[isolated Fly app before production deployment](deploy.md#verify-a-deploy).

Raw admission uses `netutil.LimitListener` in `run`
([connection caps](knowledge/features/connection-cap.md)), independently on
HTTPS, HTTP-01 and the alternative insecure listener.
`--max-listener-connections` defaults to 32 per public listener; non-positive
values refuse startup with exit 2 before listeners or workers start. Admission
precedes PROXY, TLS and HTTP parsing. At capacity, acceptance waits, without
parsing or handler invocation for queued peers. TCP dial success alone is not
admission. A slot persists through keep-alive and WebSocket hijacking until
socket close. `/healthz` consumes raw capacity and can wait at saturation;
loopback metrics stays on its separate listener outside these limits.
The combined [256 MB sizing estimate](knowledge/features/connection-cap.md#default-derivation)
is about 248 MiB with both public listeners full and 20 upgraded sessions
within their raw slots; isolated operator validation is still required.

Stalled phases retain their respective deadlines: PROXY header parsing 5s
(`proxyHeaderTimeout`), TLS handshake 5s (Go derives this from the public
server timeouts), and HTTP headers 5s (`ReadHeaderTimeout`). All public
servers retain `ReadTimeout: 60s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`;
the loopback metrics server uses the same HTTP timeouts.

Shutdown's ten-second HTTP/WebSocket drain is separate from complete process
exit. Deferred cleanup then closes the upgrade limiter and cancels/joins push
sends, shared authentication and the push accounting sweep. The
[complete-process budget](knowledge/features/graceful-shutdown.md#contract)
reserves 25s; Fly's explicit SIGTERM/30s allowance adds five seconds but is
best-effort, not a guarantee. Local process regression proves cleanup joining
under held synthetic authentication; [isolated Fly validation](deploy.md#complete-process-shutdown)
remains pending.

After `websocket.Accept` hijacks the connection, post-upgrade slow peers are bounded by three complementary mechanisms: a per-message read deadline armed the moment a message's first data-frame header arrives (`internal/relay/ws_conn.go`'s `readMessageTimeout`, 30s — [WSConn adapter](knowledge/features/ws-conn-adapter.md), #111), the write timeout on `Send` (`writeTimeout`, 10s, #15), and the heartbeat ping/pong for idle liveness (#7). The read deadline never bounds the idle gap before a message starts — a peer may sit silent indefinitely between messages — only the time to receive one once it begins, so a peer that dribbles a started message one byte at a time while still answering pings is cut off within the deadline instead of pinning the connection forever. A per-IP token-bucket rate limit fronts the `/v1/server` and `/v1/client` upgrade paths (`cmd/pyrycode-relay/main.go` policy block, `internal/relay/ratelimit_middleware.go`): ~10 attempts/IP/minute steady-state, burst 20, eviction sweep every 5 minutes. Over-cap attempts receive `429 Too Many Requests` before `websocket.Accept` runs; the wrapped registry is never touched on a deny. The middleware extracts the source IP via `relay.ClientIP`, which reads `r.RemoteAddr`; `X-Forwarded-For` is honoured only when the operator opts in with `--trust-x-forwarded-for` (default `false`). In production, `RemoteAddr` on the HTTPS listener is no longer the raw socket peer: Fly's raw-TCP services hand the relay Fly's own edge-proxy address, so since [#110](knowledge/codebase/110.md) the `443` port runs `--https-proxy-protocol` ([PROXY protocol listener](knowledge/features/proxy-protocol-listener.md)), which requires and parses a Fly `proxy_proto` v2 header before the TLS handshake and rewrites `RemoteAddr` to the header's source — the per-IP key the rate limiter reads is the client's real address, not Fly's. `/healthz` bypasses upgrade-rate and upgraded-session limits, but still needs a raw listener slot. Bandwidth amplification is structurally absent: the relay forwards 1:1, so there is no amplification factor an attacker can lever. A conflicting `/v1/server` claim now probes the incumbent binary for liveness before displacing it ([`/v1/server` § conflict probe](knowledge/features/server-endpoint.md), #112); the probe costs one ping frame per upgrade attempt and holds one handler goroutine for at most `conflictProbeTimeout` (10s) — no worse than the pre-#112 upgraded-then-closed conflict, and no amplification (one claim, one ping). Since [#113](knowledge/codebase/113.md), delivering a routed frame to a phone no longer runs on the binary forwarder's read loop at all: each phone is registered behind a bounded [per-phone delivery queue](knowledge/features/phone-outbox.md), and the forwarder only enqueues — an operation that never blocks. A stalled phone can therefore no longer hold up the binary's `Read`, its sibling phones, or the binary's own pong processing; it is bounded first by the queue filling and then by `WSConn.Send`'s 10s write deadline, and is closed with `1011` either way. The queue was originally bound by frame count (16 deep); since [#154](knowledge/codebase/154.md) it is bound by bytes instead (`phoneOutboxBudget` = 4 MiB, charging each item's frame length plus a fixed per-item overhead, including the frame currently being written), because a daemon holding a dozen or more sessions can legitimately send well over 16 small frames back to back right after the handshake — a burst the frame-count bound mistook for a stalled phone and closed with `1011`. The 4 MiB-per-phone / 64 MiB-per-server-id worst case is unchanged.

The relay-wide [connection cap](knowledge/features/connection-cap.md)
(`internal/relay/conncap.go`'s `ConnCap`, `--max-connections`, default 20)
bounds live upgraded handlers across both routes (#114). The per-source
allowance (`SourceConnCap`, `--max-connections-per-source`, default 8) prevents
one source from occupying that entire pool (#180). Eight accommodates a
daemon, six phones and a desktop behind a NAT, leaving twelve global slots
for other sources. Both endpoints share one source count using the exact
`ClientIP` result and the same `--trust-x-forwarded-for` setting as rate
limiting; PROXY HTTPS supplies the declared source through `RemoteAddr`.

Admission order is HTTP-429 metrics observer → rate limiter → source
allowance → global cap → daemon admission on `/v1/server` → endpoint.
Empty-source and rate-limited attempts
reserve neither capacity. Source saturation returns HTTP 503 with only a
newline and one `source_conn_cap_reached` Warn carrying
`strconv.Quote(source)` under `remote`, without taking a global slot. Global
saturation returns the same public response with a `conn_cap_reached` Warn
and releases its temporary source reservation. Both refuse before
`websocket.Accept` and before endpoint routing-header checks; neither 503
increments the 429 counters. Reservations last until handler cleanup returns,
including rejected handshakes, close handshakes, heartbeat timeout,
grace-expiry teardown and shutdown. The last source release deletes its
entry. Startup requires positive raw/global caps and `0 < source < global`,
including omitted defaults, or exits 2 before listeners or workers start.

**Residual risk:** One source can still fill a raw listener with pre-header
connections without ever reaching the upgraded source gate. Phase timeouts
limit stalled PROXY/TLS/HTTP occupancy, but continuously replaced peers can
keep the listener full. Accepted upgraded sessions that answer heartbeats
can hold raw slots much longer than those timeouts. HTTPS and HTTP-01
capacity is independent, but a source can target both. Many sources can
still exhaust the global upgraded pool, including one operator's daemons
and phones on different addresses. New admission can be locked out while
established connections continue.

The caps bound accepted sockets and upgraded state; they do not guarantee
memory use under the estimated machine budget or cap Fly's edge resources
and the kernel backlog. Fly's configured forwarding ceilings are a separate
boundary and do not cover direct private-network connections to the
listeners. Per-source raw allowances remain deferred in
[`security-followups.md`](security-followups.md).

Source limits are per-process; a future multi-instance deployment behind
L4 load balancing would need shared state. XFF trust is all-or-nothing:
when enabled without a trusted proxy setting it correctly, clients can spoof
their source and bypass both source limits. A CIDR-aware trusted-proxy chain is not
implemented. The PROXY header is trusted from any upstream reaching the
HTTPS listener (`ConnPolicy: REQUIRE`, no source allowlist). A peer on Fly's
private 6PN network reaching internal port 8443 directly can forge its source
and choose both its rate bucket and upgraded allowance; see
[`security-followups.md`](security-followups.md). The per-phone queue budget
remains bounded by the global cap: 16 phones on one server-id use 16 of the
20 global slots, and require multiple source addresses under the default
source allowance. Several daemons on different server-ids still compete for
the same global pool, regardless of operator ownership.

**Future hardening:** Per-source allowances on raw listener sockets, with
source attribution before HTTP and trusted PROXY handling; a CIDR-allowlisted
trusted-proxy chain for XFF; reverse-proxy fronting for L4 protections; shared
source-limit state for multi-instance deployments. Trigger: observed
single-source raw listener lockout, first multi-instance deployment, or first
paying user.

## Log hygiene — what must not be logged

**Severity:** `high`. A single accidental `slog.Info("frame", "body", payload)` call leaks user data.

**v1 mitigation:** The relay logs to stderr via `slog.NewTextHandler` (`cmd/pyrycode-relay/main.go:39`). The contributor-facing rule is enumerated explicitly:

- **MUST NOT be logged:** opaque inner-frame bodies (including Noise ciphertext), `x-pyrycode-token` or `x-pyrycode-admission-token` values, the daemon credential map, any secret-bearing header, full request headers, parser excerpts or URL paths carrying tokens. TLS termination exposes headers and routing metadata to Relay; content blindness is not header secrecy.
- **MAY be logged:** server-id, `conn-id`, device-name (advertised by the binary, not user-secret), remote host (IP), event types (`upgrade`, `forward`, `close`), close codes (`4401`, `4404`, `4409`), a push device token's fingerprint (`token_fp` — the first 8 hex characters of the token's SHA-256, a truncated one-way hash; the raw device token itself is never logged, #160), and an FCM push wake's message id (`fcm_message_id` — Firebase's `name` field from a successful send, kept only when it matches `[A-Za-z0-9/:%_-]{1,256}`, #160).

Admission has no runtime logs. Startup validation discards parser causes and
returns static, value-free errors. Credentials/maps/raw headers must also stay
out of returned error chains, rejection bodies, status and metrics; admission
credentials enter no URL, routing envelope or application frame. Admission
telemetry has exactly five fixed aggregate `outcome` labels (`valid`, `missing`,
`malformed`, `unknown`, `mismatch`), never claimed identities or tokens. Existing
allowlisted endpoint metadata logs remain; they do not prove per-identity
admission outcomes. Repository credentials must be explicitly synthetic.

**Enforcement:** The canonical set of permitted keys lives in `internal/relay/log_allowlist.go`. `TestLogKeysAreAllowlisted` in `internal/relay/log_keys_test.go` AST-walks every non-test `.go` file in the package on each `make test` run and fails if any `logger.{Info,Warn,Error,Debug}` call carries a key absent from that set or uses a dynamic (non-string-literal) key. Adding a logged key requires editing the allowlist file in the same commit.

Log retention and rotation are operator-owned. The relay writes only to stderr; on a typical deployment the systemd journal or a `logrotate`-managed flat file owns rotation.

The operational watcher has a separate output contract: notifications contain
only validated app/Machine identity, UTC observation/window, reason/count and
Pyry's response ownership. Failures exit `2` with the fixed diagnostic
`relay monitoring failure; incident delivery or monitoring coverage unavailable`.
No credentials, relay payloads, raw API configuration or remote/subprocess
error text enter messages or diagnostics; child stdout/stderr is discarded.
Private state retains normalized lifecycle evidence, not API configuration.
Operators must keep scheduler diagnostics visible and distinguish monitoring
failures from relay incidents without publishing raw responses or secret files.
No new relay logger keys are introduced.

**Residual risk:** A future contributor adds a debug log statement that prints a payload body or token. Code review and the acceptance criteria of any frame-forwarding ticket are the first line of defence; runtime redaction is not built.

**Future hardening:** A `slog` middleware that redacts known sensitive keys structurally, so the rule is enforced by code rather than vigilance.

## Cert & key handling — autocert cache directory

**Severity:** `high`.

**v1 mitigation:** `NewAutocertManager` enforces `0700` on the cache directory
at startup. A broader existing mode is tightened only when the owner is the
relay's own effective UID; a foreign or undiscoverable owner causes
`ErrCacheDirInsecure` and startup refusal. The default cache is
`~/.pyrycode-relay/certs`; Fly explicitly mounts it at `/var/lib/relay/autocert`.
Key rotation is handled by autocert: Let's Encrypt issues 90-day certificates
and autocert renews at roughly 30 days remaining.

[Certificate snapshot recovery](deploy.md#certificate-snapshot-recovery)
requires operator-local Fly access and a current snapshot restored to a new
encrypted isolated volume. Preserve cache ownership/permissions, disclose no
private keys and never mount over the live volume. Snapshots also contain
keys; successful access, isolated recovery and elapsed-time evidence remain
pending operator follow-up.

**Residual risk:** If a separate process running as the relay's UID is compromised, it can read the cache and exfiltrate the private key. The `0700` check defends against other UIDs on the same host, not against same-UID compromise.

**Future hardening:** Move private keys to a dedicated secret manager once operationally justified. Until then, the deploy doc enforces "the relay's UID owns nothing else."

## TLS configuration — cipher suites, version pin

**Severity:** `low`. Go's defaults are conservative; this entry records the choice rather than flagging risk.

**v1 mitigation:** `internal/relay/tls.go:80-88` pins `MinVersion: tls.VersionTLS12`. Cipher-suite selection uses Go's secure defaults; no custom override.

**Residual risk:** A future Go version weakens defaults — unlikely but worth re-checking on Go upgrade.

**Future hardening:** Re-read this entry on every Go minor-version bump. Consider an explicit `CipherSuites` list only if a specific compliance reason emerges.

## Error response leakage — public-path error bodies

**Severity:** `medium`.

**v1 mitigation:** The current public surface is small. `/healthz` returns bounded status/version/connection-count/uptime JSON with `Cache-Control: no-store` and no credential information. Port 80 returns `404` for non-challenge traffic (ADR-0002). Port 443 returns `421 Misdirected Request` with no body when the Host header does not match the configured domain (`internal/relay/tls.go:66-78`). These responses expose no credentials. The rule for new endpoints: public-facing handlers return generic messages;
only secret-safe diagnostics may go to logs.

Every enforce rejection at the daemon gate is HTTP 401, `Cache-Control: no-store`,
and exactly `daemon admission rejected\n`, independent of missing/malformed,
unknown/revoked or mismatched credentials and occupied identities. There is no
credential echo, identity echo, parser cause, close code or application error
frame. Rejection precedes upgrades, claims, probes, grace changes and push access.
Existing outer limits can return 429/503 before reaching the gate. Observe counts
outcomes without adding a rejection; off skips checking. Supplied invalid startup
configuration fails before listeners in every mode with static value-free errors.

**Residual risk:** Future endpoints — WebSocket upgrade error paths in particular — may write `err.Error()` to the response body and leak filesystem paths, dependency names, or stack traces.

**Future hardening:** A handler-level guideline in a future `CODING-STYLE.md`. For now, this document is the canonical reference.

## Triggers for re-review

This document must be revisited when any of the following occurs:

- A new dependency is added to `go.mod`, or an indirect dependency becomes
  directly imported. The pinned `x/net` promotion for raw admission (#182)
  is reviewed in [Supply chain](#supply-chain--go-dependencies) and
  [ADR-0014](knowledge/decisions/0014-netutil-limitlistener-for-public-admission.md).
- The deploy target changes (new VPS provider, container platform, managed Kubernetes).
- A new public endpoint is exposed (any new path under `/v1/*` or otherwise).
- A new outbound network call is added — the relay itself originates a request, rather than only serving one. First tripped by [Outbound network calls — FCM push wake](#outbound-network-calls--fcm-push-wake) (#133).
- An operational tool gains a network/notification boundary, broader credential
  scope, new storage or lifecycle-evidence semantics. The #198 pyrybox Fly GET
  and assistant-channel boundary is reviewed in [Deploy security](#deploy-security--vps-compromise)
  and [Log hygiene](#log-hygiene--what-must-not-be-logged); changing its fixed
  host, trusted argv, environment isolation or private-state contract requires re-review.
- A security incident occurs — any unexpected behaviour with security implications, even if no compromise is confirmed.

## Out of scope

- Pen-test write-up. No pen test has been performed.
- Compliance frameworks (GDPR, SOC 2, ISO 27001). No applicable obligations in v1; user-data flows are minimal.
- Per-CVE response runbook. Premature; revisit when there are users.
