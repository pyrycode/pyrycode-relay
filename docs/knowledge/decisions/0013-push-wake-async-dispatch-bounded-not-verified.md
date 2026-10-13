# ADR-0013: Push wakes dispatch async off the read loop, bounded attempts, token unverified

**Status:** Accepted (#133), extended with shared allowances (#172)
**Date:** 2026-09-24
**Updated:** 2026-10-13

## Context

The daemon can ask the relay to wake a backgrounded phone by sending a
`push_wake` routing envelope carrying an FCM device token
([FCM push sender](../features/fcm-push-sender.md), #132). Two properties of
this request shape a decision the relay has to make on every wake:

1. **The relay cannot verify device-token ownership.** In
   [daemon admission enforce](../features/server-endpoint.md#daemon-admission-before-upgrade)
   (#167), a binary needs a credential bound to its exact server-id before
   upgrade or claim. That proof does not bind a wake's device token to the
   binary: an admitted holder could ask to wake any FCM token, including one
   belonging to a different install. Off/observe retain anonymous admission.
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
wake's token, and bound the resulting abuse surface with three initially full
attempt allowances and a separate concurrency cap:

- **Per-daemon allowance** (`pushWakeBurst` / `pushWakeRefillEvery`) — burst
  6, refill one token per 10s, across all devices requested by that server-id.
- **Per-device allowance** (`pushWakeDeviceBurst` / `pushWakeDeviceRefillEvery`)
  — burst 6, refill one token per 10s, shared by all daemons requesting the
  complete same device token.
- **Global allowance** (`pushWakeGlobalBurst` / `pushWakeGlobalRefillEvery`)
  — burst 32, refill one token per second, across all daemons and devices.
- **Relay-wide in-flight cap** (`pushWakeMaxInFlight`, a buffered-channel
  semaphore) — bounds concurrent goroutines and outbound sockets across
  every binary regardless of how many server-ids they claim.

Burst capacity is stored credit, distinct from sustained refill rate; refills
stop at capacity. Under one `PushWaker` mutex, admission charges one token
from each allowance and reserves a slot atomically. A rate, keyed-capacity or
concurrency refusal spends nothing and queues nothing. A dispatched failure
remains charged. Each daemon/device map is capped at 1,024 full SHA-256 keys;
only fully refilled state idle for five minutes, counting refusals as
activity, may be reclaimed by periodic sweep or new-key admission.

These budgets apply in every admission mode, live in the single relay
process's memory and reset on restart. Reconnects and identity churn cannot
reset the shared device/global allowances. Only enforce authenticates daemon
identity; device-token ownership remains unverified and wakes carry no payload.

## Rationale

- **The wake carries no payload.** `FCMSender.Send` sends a data-only,
  field-free FCM message — a successful send tells the phone only to
  reconnect. That makes the worst case of an unverified token "an app
  reconnects more than it should," not data exposure, which is why bounding
  the rate is an acceptable substitute for verifying the token rather than a
  stopgap. A future protocol change that lets the wake carry any payload
  would need to revisit this trade.
- **Separate attempt and concurrency bounds.** Completed sends free slots,
  so concurrency alone cannot bound outgoing rate. Fresh identities receive
  fresh daemon credit, so a daemon-only allowance cannot bound shared-device
  pressure or aggregate throughput. Shared device/global allowances address
  those paths, while the semaphore still bounds simultaneous send work.
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
- **Local atomic accounting.** Successive `IPRateLimiter.Allow` calls cannot
  reserve three budgets and a slot atomically; an early charge would be lost
  on a later refusal. Push uses local buckets, leaving the
  [upgrade-path limiter](../features/rate-limit-middleware.md) unchanged.
  Fixed-size complete-input digests bound key storage without conflating
  tokens through their truncated log fingerprint. Full-refill-only eviction
  prevents identity reuse or capacity pressure from deleting allowance debt.

## Consequences

- Documented as an accepted, bounded residual risk rather than a solved
  problem: see [`docs/threat-model.md` § Outbound network calls](../../threat-model.md#outbound-network-calls--fcm-push-wake).
  Enforce excludes anonymous daemon-originated wakes, but admitted holders,
  including stolen-token holders, retain push abuse within their bound identity.
  Admission grants neither Noise keys nor phone authorization. A binary can
  still request arbitrary-device wakes within its daemon allowance and the
  shared device/global budgets. Colluding holders can exhaust those budgets
  or saturate the 32 slots, denying legitimate wakes. Restart grants fresh
  credit; multiple replicas would need shared accounting.
- Since #161, `pyrycode_relay_push_wakes_total{outcome="dropped"}` records
  refusals with fixed aggregate labels. Each rate, state-capacity or
  concurrency refusal increments it exactly once; malformed and push-off
  requests still count nothing. Labels contain no identities, device tokens
  or fingerprints; errors remain static and token-free. See
  [push metrics](../features/push-wake-dispatch.md#metrics).
- Deployment from clean `main` and production admission activation remain
  [manual operator follow-ups](../../deploy.md#daemon-admission-activation).
  Repository checks establish neither deployment nor live enforcement.
  Later retries (#173) must consume these same attempt budgets.
- If a future ticket makes the wake payload-bearing, or adds a way to verify
  a token belongs to the claiming binary's own phones, this ADR's
  "acceptable because payload-free" rationale no longer holds and the
  decision should be revisited — the limits alone would no longer be
  sufficient reasoning to skip verification.
- Establishes atomic admission across shared budgets and concurrency,
  with immediate refusal and no queue, as precedent for future relay-originated
  outbound call triggered by untrusted-binary input.
