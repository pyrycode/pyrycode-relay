# Architecture

`pyrycode-relay` is a stateless WebSocket router. It sits between mobile clients and pyry binaries; phones and binaries both connect outbound to the relay over WSS.

## Authoritative reference

The wire protocol lives in [`pyrycode/pyrycode/docs/protocol-mobile.md`](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md). That document defines:

- Connection lifecycle (binary connects to `/v1/server`; phones to `/v1/client`)
- Headers used for routing (`x-pyrycode-server`, `x-pyrycode-token`)
- The routing-envelope wrapper that the relay prepends/strips when forwarding
- Authentication semantics (relay routes blindly by server-id; binary validates token)
- Error envelope and WS close codes (`4401`, `4404`, `4409`)
- TLS terminus (relay is the cert holder; autocert via Let's Encrypt)

## What this binary does

1. Accept WSS upgrades on `/v1/server` and `/v1/client`.
2. Map server-ids → binary connections (1:1; first-claim-wins with 30s grace on disconnect).
3. Map server-ids → phone connections (1:N).
4. Forward frames between phones and the corresponding binary, prepending/stripping the routing envelope.
5. Surface `4404` (no server) when a phone connects with a server-id no binary holds.
6. Surface `4409` (server-id conflict) when a binary tries to claim a held server-id.
7. Tell the binary when one of its phones' connections ends on its own, via a frameless `{conn_id, close_code}` routing envelope (#152) — skipped when the binary itself requested the close, or no longer holds that server-id. See [`/v1/client` § Close notice to the binary](knowledge/features/client-endpoint.md#close-notice-to-the-binary-152).

## What this binary does NOT do

- Read message payloads. Frames are opaque bytes.
- Persist anything (modulo the on-disk autocert cert cache).
- Validate device tokens. The binary does that.
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
`ConnCap` (`--max-connections`, default 20) → endpoint. Both source limits
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
The wake carries no payload — no `notification`, no `data` field — so a
successful send tells the phone only to reconnect, never what happened in
the session. See [Push wake dispatch](knowledge/features/push-wake-dispatch.md)
for the dispatch path (async off the binary read loop, per-server-id rate
limit, relay-wide in-flight cap) and
[`docs/threat-model.md` § Outbound network calls](threat-model.md#outbound-network-calls--fcm-push-wake)
for the abuse accounting.

Each send uses one 10s deadline from entry through token waiting,
authentication and delivery, respecting earlier caller deadlines. Concurrent
token waiters share a refresh's success or sanitised failure. The last
departing waiter cancels and joins unfinished authentication, so
`PushWaker.Close` waits for refresh cleanup through its send wait group.
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
authentication and the push limiter. This is a configured budget, not a new
production deadline or platform guarantee. Effective settings and isolated
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
