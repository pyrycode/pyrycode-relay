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

## Threat model

To be written. Will live at `docs/threat-model.md`. Wire-protocol-level threats are already in the protocol spec's Security model section.
