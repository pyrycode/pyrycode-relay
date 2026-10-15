# Architecture

`pyrycode-relay` is a stateless WebSocket router. It sits between mobile clients and pyry binaries; phones and binaries both connect outbound to the relay over WSS.

## Authoritative reference

The wire protocol lives in [`pyrycode/pyrycode/docs/protocol-mobile.md`](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md). That document defines:

- Connection lifecycle (binary connects to `/v1/server`; phones to `/v1/client`)
- Routing identity (`x-pyrycode-server`), the opaque phone-token header (`x-pyrycode-token`) and the daemon admission header (`x-pyrycode-admission-token`)
- The routing-envelope wrapper that the relay prepends/strips when forwarding
- [Identity-bound daemon admission](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#daemon-admission-declared) at Relay, distinct from daemon-owned phone authentication and end-to-end Noise encryption
- Error envelope and WS close codes (`4401`, `4404`, `4409`)
- TLS terminus (relay is the cert holder; autocert via Let's Encrypt)

## What this binary does

1. Check daemon admission before `/v1/server` upgrades using a private startup credential mapping: off (default) bypasses checks, observe counts aggregate decisions, enforce requires an exact identity/token match. Only enforce blocks anonymous claims and daemon-originated wakes. Accept admitted WSS upgrades and the existing `/v1/client` path. See [daemon endpoint](knowledge/features/server-endpoint.md#daemon-admission-before-upgrade).
2. Map server-ids → binary connections (1:1; first-claim-wins with 30s grace on disconnect).
3. Map server-ids → phone connections (1:N).
4. Forward frames between phones and the corresponding binary, prepending/stripping the routing envelope.
5. Surface `4404` (no server) when a phone connects with a server-id no binary holds.
6. Surface `4409` (server-id conflict) when an admitted binary claims a held server-id and its incumbent answers the liveness probe; otherwise reclaim the unresponsive incumbent's slot.
7. Tell the binary when one of its phones' connections ends on its own, via a frameless `{conn_id, close_code}` routing envelope (#152) — skipped when the binary itself requested the close, or no longer holds that server-id. See [`/v1/client` § Close notice to the binary](knowledge/features/client-endpoint.md#close-notice-to-the-binary-152).

## What this binary does NOT do

- Read application payloads or terminate Noise. Phone↔daemon frames remain opaque; application content is Noise ciphertext. Routing metadata and relay-directed control fields remain visible.
- Persist user/connection state (modulo the on-disk autocert cert cache). Provisioned admission credentials are read once at startup; restart discards claims and grace state.
- Authenticate phones, validate their pairing tokens or hold their Noise keys. The daemon does that; Relay only presence-checks and discards the existing opaque phone-token header. Daemon admission does not verify ownership of a push device token.
- Implement APNs push. Only FCM wakes are sent, and only when the binary
  asks for one.
- Operate on multiple users / tenants in v1. One operator, many users' server-ids.

## Public listener admission

Every public path admits at most `--max-listener-connections` accepted
sockets (default 32) before parsing: raw TCP → `netutil.LimitListener` →
optional PROXY v2 → TLS → HTTP on HTTPS; raw TCP → `LimitListener` → HTTP
on HTTP-01 or the alternative insecure listener. HTTPS and HTTP-01 have
independent capacity. At capacity, peers wait outside relay acceptance;
slots last until socket close, including keep-alive and WebSocket hijacking.

After HTTP headers, `/v1/server` and `/v1/client` share admission in this
order: metrics observer → per-source attempt-rate limiter →
`SourceConnCap` (`--max-connections-per-source`, default 8) → relay-wide
`ConnCap` (`--max-connections`, default 20) → daemon admission on
`/v1/server` → endpoint. `/v1/client` retains its existing endpoint validation.
Both source limits
use the exact `ClientIP` result and the same XFF trust setting; PROXY HTTPS
supplies the declared source through `RemoteAddr`. Rate denials return 429
without reservations; either upgraded cap returns 503 before acceptance.
Reservations last through handler cleanup, and the last source release
deletes its count entry. Startup requires `0 < source < global`, including
when the source default was omitted.

Upgraded sockets occupy raw slots too. `/healthz` bypasses upgraded limits
but consumes raw capacity; loopback metrics is outside all connection caps.
Closing listeners wakes blocked acceptance, and the existing shutdown path
drains registered WebSockets with `1001`. One source can still saturate raw
capacity before reaching the source gate; many sources can fill the global
upgraded pool. See [connection caps](knowledge/features/connection-cap.md)
for sizing, configuration and source-trust limits.

Shutdown establishes a permanent registry admission boundary before launching
HTTP shutdown: one mutex operation stops claims, reclaims, takeovers and phone
registration and snapshots admitted handles for concurrent 1001 closes.
Already-upgraded handlers arriving after that boundary close themselves with
1001 without registering. HTTP shutdown cannot drain hijacked WebSockets;
the relay's drain stays context-bounded and does not join handler goroutines.
See [graceful shutdown](knowledge/features/graceful-shutdown.md).

## Outbound calls to Google (FCM push wake)

Unlike everything else in [*What this binary does*](#what-this-binary-does),
this one path makes outbound calls instead of only routing inbound ones. When
a binary sends a `push_wake` routing envelope (`{"push_wake":{"platform":"fcm","token":"…"}}`,
no `conn_id`), the relay uses two outbound HTTPS endpoints at Google: an OAuth
token request to `https://oauth2.googleapis.com/token` when its cached token
is no longer valid (or the key's `token_uri` when supplied;
see [FCM push sender](knowledge/features/fcm-push-sender.md)), and an FCM
send to `https://fcm.googleapis.com/v1/projects/pyrycode-mobile/messages:send`.
The high-priority wake carries no payload — no `notification`, no `data`
field — so a successful send tells the phone only to reconnect, never what
happened in the session. See [Push wake dispatch](knowledge/features/push-wake-dispatch.md)
for the dispatch path (async off the binary read loop) and
[`docs/threat-model.md` § Outbound network calls](threat-model.md#outbound-network-calls--fcm-push-wake)
for the abuse accounting.

Each admitted job reserves one slot through sending and retry waits. Every
sender invocation atomically consumes one token each from initially full
daemon, device and global allowances, attributed to the originating daemon.
Daemon and device bursts are 6, refilling one token per 10s; the global burst
is 32, refilling one per second. Burst capacity is stored credit, distinct from
sustained refill rate. Same complete device tokens coalesce across daemon
identities for a fixed ten seconds from first admission, or until the active
job finishes, including backoff, whichever is later. Accepted duplicates
start no send, spend no allowance and emit no outcome. They extend neither
the window nor the 45s job deadline, reset no attempts and schedule no
follow-on provider work, regardless of success or failure. At the exact boundary a
completed job becomes eligible for ordinary admission.

At most 32 device jobs are retained, including completed jobs still inside
the window and jobs waiting to retry; simultaneous sends also remain capped
at 32. New distinct work at capacity is refused without a queue or charge,
while duplicates still succeed. Expired completed jobs are reclaimed before capacity checks without
erasing allowance debt. Failed attempts remain charged; retry allowance or
accounting-capacity refusal stops the job with one `dropped` outcome and no
partial charge, refund or refill queue. Local push accounting retains at most
1,024 daemon and 1,024 device buckets using full
SHA-256 keys, reclaiming only fully refilled state idle for five minutes.

Jobs use full SHA-256 device keys and store expiry/completion state, a
sanitised outcome and bounded correlation records; completed jobs retain no
raw device token. Each sender invocation records one outcome and log line
attributed to its first admitting
daemon.
Wake delivery is payload-free and best-effort, without durability or an
exactly-once guarantee. Each job permits at most three sender invocations
within one immutable 45s deadline from first admission, covering authentication,
sends and waiting. Transient failures use 10s/20s backoff from call completion
plus independent [0, 1s] jitter, or later receipt-anchored Retry-After guidance.
HTTP 429 imposes at least 60s and therefore ends the job without another call.
A next attempt at or beyond the deadline is never sent early. See
[sender retry policy](knowledge/features/fcm-push-sender.md#retry-classification-and-guidance)
for the status/transport allowlists, permanent-stop precedence and sanitisation.
Outcomes/logs count each invocation once, not each job; ending between calls
adds no synthetic outcome. Duplicates emit none.

An optional valid `push_wake.request_id` opts into the published
[correlated result contract](https://github.com/pyrycode/pyrycode/blob/d400bc3a4ad7e8bba3fffd6be53fc025e7c1f002/docs/protocol-mobile.md#push_wake_result).
Only terminal, uncancelled `ErrFCMUnregistered` permits
`{"push_wake_result":{"request_id":"wake-A-1","code":"invalid_token"}}`.
The ID must be 1–64 ASCII bytes from `[A-Za-z0-9_-]`; an invalid or omitted
ID disables only feedback. Results contain no token, hash, provider text,
`conn_id`, `frame` or Noise sealing and never reach phones. Each distinct
accepted `(original connection, request_id)` gets at most one attempt per
retained job, including after write failure. Equal IDs on separate connections
are independent. New eligible IDs may use a sanitised cached invalid outcome
inside the original ten-second window without a provider call; late completion
opens no new window. Other outcomes/cancellation produce no invalid result.

Pending, queued and seen records share hard caps of 128/original connection,
64/job and 2048/process, expiring no later than 60s after acceptance without
renewal. Overflow declines only new feedback. Queued original delivery keeps
its job/connection charges after cache eviction until completion/cancellation,
without restoring the cache or accepting subscribers. Delivery runs off the
read loop and has five seconds total including scheduling/writer wait, limited
sooner by subscription expiry or cancellation. `StartBinaryForwarder` captures
the origin writer/lifetime; no current-holder lookup routes results. Disconnect
and replacement cancel that origin, including a synchronous registry feedback
fence before publishing a replacement whose old socket may still be open.
Reconnect/eviction/restart grants no replay.

Feedback adds no logs or metric labels and discards a static transport-failure
sentinel; existing sanitised provider-attempt diagnostics remain separate.
It is advisory relay testimony, with device ownership still unverified. The
[Core #3423 family](https://github.com/pyrycode/pyrycode/issues/3423) owns
consumption and atomic removal of only a matching registration snapshot,
preserving pairings and replacements. Keep coordinated activation off until
the [operator release check](deploy.md#coordinated-push-result-release-check)
records compatible revisions and real-daemon evidence.

These single-process in-memory budgets and coalescing apply in off, observe
and enforce modes and reset on relay restart. Reconnects and new daemon
identities do not reset device or global allowances. Only daemon admission
enforce authenticates the requesting identity; device-token ownership remains
unverified. Deployment
and production enforcement activation remain
[manual operator work](deploy.md#daemon-admission-activation); passing
repository checks establishes neither.

Each sender call uses one 10s deadline from entry through token waiting,
authentication and delivery, respecting the earlier job deadline. Concurrent
token waiters share a refresh's success or sanitised failure. The last
departing waiter cancels and joins unfinished authentication, so
`PushWaker.Close` refuses subsequent structurally valid requests, cancels and
joins sends, retry timers, shared authentication, feedback workers/writes and
the existing cleanup sweep, then releases retained jobs and feedback accounting.
It starts no new provider sends or result writes after shutdown begins.
Repeated and concurrent closes join the same work.
Push cleanup runs after `runServers`' separate 10s HTTP/WebSocket drain;
see [push concurrency](knowledge/features/push-wake-dispatch.md#concurrency-model).

## Single-instance constraint (v1)

v1 is single-instance. Operators MUST NOT scale horizontally (`fly scale count > 1`, multiple `docker run` of the same image behind a load balancer, etc.).

### Why

The connection registry (`internal/relay/registry.go`) lives in process memory. Two replicas hold two disjoint registries. When a phone and its binary land on different replicas — a coincidence the load balancer is free to produce — the phone's replica sees a server-id miss in its local table and closes with `4404`, even though the binary is connected to a sibling replica. The failure is silent: nothing in the relay logs identifies "the binary is on the other replica" as the cause, because no replica knows the others exist.

### What multi-instance would require

Documented as future work, not a commitment. Neither path is planned for v1, and adopting either would invalidate the bypass env var below.

1. **Shared registry.** A cross-replica store that every replica consults on every `/v1/server` claim and `/v1/client` lookup — for example Redis pub/sub keyed on server-id, or NATS subjects per server-id. Adds a network hop to every routing decision and a new failure mode (registry down → relay cannot route).
2. **Sticky-session-on-server-id at the LB layer.** The load balancer hashes on the `x-pyrycode-server` header so that all traffic for a given server-id reaches the same replica. Avoids the shared-store cost but requires LB awareness of the header; most L4 load balancers do not route on request headers.

### The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass

A startup self-check (see [single-instance startup check](knowledge/features/single-instance-check.md), #65)
refuses to run on a multi-instance-capable substrate (`FLY_APP_NAME` is set)
unless the operator asserts single-instance intent. Setting

```text
PYRYCODE_RELAY_SINGLE_INSTANCE=1
```

skips that refusal and is required in the production Fly manifest. The check
reads environment variables only; it neither counts Machines nor detects a
later scale-out. Every replica can inherit the same assertion. Operators must
verify actual Machine inventory and preserve exactly one active relay; the
assertion does not make a multi-instance topology safe.

## Hosting

Production deploys to a single **Fly.io** machine in one region. TLS terminates in the relay binary (autocert, #9) — Fly runs the substrate in raw-TCP passthrough mode on `:80` and `:443`, with a dedicated IPv4 so Let's Encrypt's HTTP-01 challenge resolves deterministically. The autocert cache lives on a Fly volume at `/var/lib/relay/autocert`.

Both raw-TCP services (8080/external 80 and 8443/external 443) configure
`services.concurrency` with `type = "connections"` and `hard_limit = 32`
(#183), matching the independent raw listener default. Fly's
[forwarding ceiling](https://docs.fly.io/reference/configuration#servicesconcurrency)
stops new traffic to a Machine at or above the service limit; it does not
guarantee immediate client drops. Relay admission still bounds accepted
sockets before parsing, including traffic that bypasses Fly's public edge.
The operator must [verify forwarding at saturation and recovery on an isolated app](deploy.md#verify-a-deploy)
before production deployment. Port 80 passthrough, 443 PROXY v2 and the 8080
TCP health check retain their existing wiring.

The manifest declares `[[restart]]` with `processes = ["app"]` and
`policy = "always"`, omitting `retries` (#199). Fly attempts restart
recovery regardless of exit code; the effective Machine policy and isolated
recovery/lifecycle checks remain [operator follow-up](deploy.md#app-restart-recovery)
before manual production deployment. Merge alone does not establish live
recovery.

Top-level `kill_signal = "SIGTERM"` and `kill_timeout = 30` reserve a
best-effort stop allowance five seconds above the conservative 25-second
complete-process budget (#200). The ten-second HTTP/WebSocket drain is
followed by deferred upgrade-limiter and push cleanup, including joined
authentication and the push accounting sweep. This is a configured budget,
not a new production deadline or platform guarantee. Effective settings and isolated
shutdown evidence remain [operator follow-up](deploy.md#complete-process-shutdown).

Exactly one active production relay must be preserved through recovery and
deployment. `min_machines_running = 1`, `auto_start_machines = false`, and
`auto_stop_machines = "off"` in `fly.toml` express single-machine intent;
these settings enforce no maximum of one. Disabled autostart prevents
automatic starts, not Machine creation; rolling deployment is not a count
guard. Verify actual inventory before and after rehearsals and production
activation. `PYRYCODE_RELAY_SINGLE_INSTANCE=1` asserts intent to the startup
self-check (#65), without querying inventory. Multi-instance scaling is out
of scope for v1 — see
§ *Single-instance constraint* above.

Bootstrap and rollback procedures: [`docs/deploy.md`](deploy.md). The manifest itself: [`fly.toml`](../fly.toml). Deploys are operator-direct from clean `main` (`flyctl deploy --remote-only`); see [`docs/deploy.md`](deploy.md).

The daily/manual `security-scan` workflow independently observes started Fly
Machines, pulls their single distinct immutable image digest and scans a saved
archive (#176). This CI-to-Fly read boundary uses an org-scoped read-only
credential confined to discovery/download; deploy privileges remain with the
operator. Temporary registry credentials are removed before Trivy runs, and
only a separate issue-filing job has `issues: write`. Repository scans describe
what `main` would ship; the deployed result describes its digest at its UTC
observation time. See [CI image scanning](knowledge/features/docker-image.md#ci-image-scanning)
and [deploy privileges](knowledge/features/fly-deploy.md#deploy-privilege-model--operator-direct).

The repository also supplies an independent pyrybox
[restart incident watcher](deploy.md#restart-incident-monitoring) (#198).
An operator-scheduled process reads one selected Machine's lifecycle through
the fixed Fly Machines API, keeps private app/Machine-scoped incident state
on pyrybox, and invokes the trusted assistant-channel command to notify Pyry.
Pyry investigates read-only and escalates to Juhana. This operational outbound
Fly/notification boundary adds no relay endpoint, payload access or relay
deployment target; relay routing remains stateless and content-blind.
Installation, delivered verification and production activation are
[pending operator work](deploy.md#restart-incident-response).

## Threat model

Wire-protocol threats live in the protocol spec's [Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model). Operational threats specific to the relay binary as a deployed process — deploy, supply chain, DoS, log hygiene, cert handling, TLS config, error-leakage — live in [`docs/threat-model.md`](./threat-model.md).
