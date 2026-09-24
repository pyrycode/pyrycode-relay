# #140 — A live binary with a stalled phone survives the conflict probe

Test-only. No production file changes.

## Files read

- `internal/relay/server_endpoint.go` → `serverHandler`, `takeoverUnresponsive`, `conflictProbeTimeout`: the probe under test; `serverHandler` takes the probe timeout so the test can shorten it.
- `internal/relay/phone_outbox.go` → `phoneOutbox`, `phoneOutboxDepth`, `deliver`: the per-phone queue #113 added; `onWritten` fires after each frame actually written to the phone.
- `internal/relay/registry.go` → `SetForwarderHooks`: installs the `onBinaryForwarded` hook `ClientHandler` threads into each outbox as `onWritten`; the test's observable for "a phone write is in flight and not finishing".
- `internal/relay/ws_conn.go` → `writeTimeout`: 10s; the probe timeout must sit well under it.
- `internal/relay/client_endpoint.go` → `ClientHandler`: wraps the phone in a `phoneOutbox`; sends nothing to the binary on connect, so the incumbent's `CloseRead` stays safe.
- `internal/relay/server_endpoint_test.go` → `startServerProbe`, `awaitBinary`, `TestServerEndpoint_DuplicateClaim_4409`, `TestServerEndpoint_UnresponsiveIncumbent_TakenOver`: precedent for the probe tests and the 4409 assertions.
- `internal/relay/metrics_upgrade_test.go` → `pollUntil`; `internal/relay/client_endpoint_test.go` → `validClientHeaders`; `internal/relay/envelope.go` → `Marshal` (the inner frame must be valid JSON).

## Change

Add `TestServerEndpoint_StalledPhone_LiveIncumbentKeepsSlot` to `server_endpoint_test.go`. One `httptest` server muxes `/v1/server` to `serverHandler` (probe timeout 200ms, well under `writeTimeout`, frame cap 1 MiB) and `/v1/client` to `ClientHandler`, both on one registry whose `SetForwarderHooks` binary hook counts phone writes atomically. The incumbent binary dials, calls `CloseRead` so it answers pings only by reading; a real phone dials `/v1/client` and never reads.

The binary then sends ~256 KiB JSON-string frames addressed to the phone **one at a time**: after each, it waits briefly for the write counter to catch up. The first frame whose write does not complete within that window is the blocked write — past the phone's socket buffers, with at most one frame in the outbox, so never near `phoneOutboxDepth` and the 1011 overflow. A cap on the number of frames fails the test loudly rather than looping forever if the kernel buffers absorb everything.

While that write is blocked, a second `/v1/server` claim for the same server-id must read close code 4409; the registry must still hold the incumbent; the write counter must not have advanced (the write stayed blocked across the whole probe); and the phone must still be registered. Passing under `-race` proves the pong path runs while a phone write is in flight. Cleanup closes the phone first, which fails the blocked write, then the servers.

If this fails on `main`, it is a finding reported on the ticket, not something fixed here.

## Testing strategy

The test is the deliverable. Run it with `go test -race -run StalledPhone -count=5 ./internal/relay/` for stability, then the touched package under `-race`. A sanity check that it can fail: temporarily raising the probe timeout is not a RED; instead confirm the blocked-write detection triggers (written == sent − 1 at probe time), which is what distinguishes it from `TestServerEndpoint_DuplicateClaim_4409`.

## Documentation handoff

None. #113 already updated `conflictProbeTimeout`'s comment, `docs/threat-model.md` § DoS resistance and `docs/security-followups.md`.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the test adds no production path; frames stay opaque `json.RawMessage` through `StartBinaryForwarder` and `phoneOutbox`, and the test never inspects what the phone would receive.
- [Tokens, secrets, credentials] No findings — the phone presents the fixed test token from `validClientHeaders`; nothing is logged or stored.
- [File operations] No findings — no files touched at runtime.
- [Subprocess] No findings — none spawned.
- [Cryptographic primitives] No findings — none used beyond `randHex8` in the handler under test.
- [Network & I/O] No findings — the test raises `serverHandler`'s frame cap to 1 MiB in its own server only; production caps are untouched. The test exercises exactly the DoS property the threat model claims: a stalled phone cannot starve the incumbent's pong path.
- [Error messages, logs] No findings — no new log calls, so `TestLogKeysAreAllowlisted` is unaffected.
- [Concurrency] No findings — the write counter is an `atomic.Int64`; the hook is set before the server serves, per `SetForwarderHooks`' contract. Every connection is closed by the test's defers, which unblocks the in-flight phone write so the outbox goroutine exits.
- [Threat model alignment] No findings — pins the #112/#113 claim in `docs/threat-model.md` § DoS resistance; no re-review trigger tripped.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
