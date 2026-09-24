# ADR-0013: Push wakes dispatch async off the read loop, bounded by two limits, token unverified

**Status:** Accepted (#133)
**Date:** 2026-09-24

## Context

The daemon can ask the relay to wake a backgrounded phone by sending a
`push_wake` routing envelope carrying an FCM device token
([FCM push sender](../features/fcm-push-sender.md), #132). Two properties of
this request shape a decision the relay has to make on every wake:

1. **The relay cannot verify the token.** A binary is authenticated by having
   claimed a server-id, not by any per-device proof — it could ask the relay
   to wake any FCM token, including one belonging to a different install.
2. **`FCMSender.Send` blocks** on an HTTP call (bounded by `fcmSendTimeout`,
   but still synchronous). The caller is `StartBinaryForwarder`'s read loop,
   which also forwards ordinary phone traffic for the same binary — a stalled
   send must not stall unrelated frames.

Two options for (1): refuse to send unless the token is provably tied to the
binary's own phones (not possible without a verification mechanism the
protocol doesn't have), or accept the trust gap and bound its blast radius.
For (2): call `Send` inline and accept the stall, or dispatch off the read
loop.

## Decision

Dispatch every admitted wake on its own goroutine ([`PushWaker.Request`](../features/push-wake-dispatch.md)),
never blocking the binary read loop. Accept that the relay cannot verify a
wake's token, and bound the resulting abuse surface with two independent,
named limits instead of refusing the feature or adding token verification:

- **Per-server-id rate** (`pushWakeRefillEvery` / `pushWakeBurst`, an
  `IPRateLimiter` keyed on server-id) — bounds how fast one binary can push
  wakes at Google.
- **Relay-wide in-flight cap** (`pushWakeMaxInFlight`, a buffered-channel
  semaphore) — bounds concurrent goroutines and outbound sockets across
  every binary regardless of how many server-ids they claim.

A wake over either limit is dropped, never queued — no backpressure state
accumulates under sustained abuse.

## Rationale

- **The wake carries no payload.** `FCMSender.Send` sends a data-only,
  field-free FCM message — a successful send tells the phone only to
  reconnect. That makes the worst case of an unverified token "an app
  reconnects more than it should," not data exposure, which is why bounding
  the rate is an acceptable substitute for verifying the token rather than a
  stopgap. A future protocol change that lets the wake carry any payload
  would need to revisit this trade.
- **Two limits, not one.** A single global cap alone would let one abusive
  binary starve every other binary's wakes; a single per-server-id cap alone
  would let a large-enough set of claimed server-ids collectively exhaust
  relay goroutines. The per-server-id bucket bounds one actor's rate; the
  in-flight cap bounds the relay's own resource consumption regardless of
  how many actors are involved. Neither alone was sufficient.
- **Drop, don't queue.** A bounded queue would still need its own size limit
  and would turn "the relay is under wake pressure" into growing memory
  instead of an immediate, logged refusal. Dropping keeps the failure mode
  visible (one log line per refusal) and memory flat regardless of load.
- **Async dispatch, not a worker pool.** A fixed pool of long-lived worker
  goroutines would need its own queue (same objection as above) or would
  block admission on a worker being free, reintroducing the stall this
  design exists to avoid. A goroutine-per-send bounded by a semaphore gets
  the same concurrency ceiling without a queue: `Request` either gets a slot
  immediately or is refused immediately.
- **`IPRateLimiter` reused, not reimplemented.** The existing limiter
  ([rate-limit middleware](../features/rate-limit-middleware.md), #47) is
  already a generic string-keyed token bucket with bounded memory via
  eviction. Server-id and IP are both just string keys to it — a second
  instance, not a new type.

## Consequences

- Documented as an accepted, bounded residual risk rather than a solved
  problem: see [`docs/threat-model.md` § Outbound network calls](../../threat-model.md#outbound-network-calls--fcm-push-wake).
  A binary can still cause up to `pushWakeBurst` wakes per
  `pushWakeRefillEvery` window indefinitely, and a set of colluding binaries
  can still collectively saturate `pushWakeMaxInFlight`.
- No metric exists yet on refused wakes (out of scope per the ticket) — the
  residual risk is not currently observable without reading logs. A future
  ticket adding one does not change this decision, only its visibility.
- If a future ticket makes the wake payload-bearing, or adds a way to verify
  a token belongs to the claiming binary's own phones, this ADR's
  "acceptable because payload-free" rationale no longer holds and the
  decision should be revisited — the limits alone would no longer be
  sufficient reasoning to skip verification.
- Establishes the "drop under either of two independently-scoped limits,
  never queue" shape as available precedent for any future relay-originated
  outbound call triggered by untrusted-binary input.
