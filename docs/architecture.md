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

## What this binary does NOT do

- Read message payloads. Frames are opaque bytes.
- Persist anything (modulo the on-disk autocert cert cache).
- Validate device tokens. The binary does that.
- Implement push notifications. The binary calls APNs/FCM directly.
- Operate on multiple users / tenants in v1. One operator, many users' server-ids.

## Single-instance constraint (v1)

v1 is single-instance. Operators MUST NOT scale horizontally (`fly scale count > 1`, multiple `docker run` of the same image behind a load balancer, etc.).

### Why

The connection registry (`internal/relay/registry.go`) lives in process memory. Two replicas hold two disjoint registries. When a phone and its binary land on different replicas — a coincidence the load balancer is free to produce — the phone's replica sees a server-id miss in its local table and closes with `4404`, even though the binary is connected to a sibling replica. The failure is silent: nothing in the relay logs identifies "the binary is on the other replica" as the cause, because no replica knows the others exist.

### What multi-instance would require

Documented as future work, not a commitment. Neither path is planned for v1, and adopting either would invalidate the bypass env var below.

1. **Shared registry.** A cross-replica store that every replica consults on every `/v1/server` claim and `/v1/client` lookup — for example Redis pub/sub keyed on server-id, or NATS subjects per server-id. Adds a network hop to every routing decision and a new failure mode (registry down → relay cannot route).
2. **Sticky-session-on-server-id at the LB layer.** The load balancer hashes on the `x-pyrycode-server` header so that all traffic for a given server-id reaches the same replica. Avoids the shared-store cost but requires LB awareness of the header; most L4 load balancers do not route on request headers.

### The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass

A startup self-check (see #65) refuses to run when the relay detects it is one of several instances. Setting

```text
PYRYCODE_RELAY_SINGLE_INSTANCE=1
```

skips that check. The variable name matches what the relay binary checks at startup.

Intended uses:

- **Emergency rollback** — the self-check itself misfires and blocks startup.
- **Migration windows** — the operator is mid-switch to a multi-instance topology and accepts the routing breakage transiently.

**NOT recommended for production.** Setting it permanently silences a check whose entire purpose is to catch the silent-routing-failure mode described above.

## Threat model

Wire-protocol threats live in the protocol spec's [Security model](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md#security-model). Operational threats specific to the relay binary as a deployed process — deploy, supply chain, DoS, log hygiene, cert handling, TLS config, error-leakage — live in [`docs/threat-model.md`](./threat-model.md).
