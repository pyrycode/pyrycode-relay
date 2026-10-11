# Independent public listener connection admission (#182)

## Files read

- `cmd/pyrycode-relay/main.go` → `run`, `runServers`, `defaultMaxConnections`: public serving callbacks, startup ordering, drain and memory policy.
- `cmd/pyrycode-relay/main_e2e_test.go` → `dialServer`, `readPump`, `freePort`, signal and cap tests: real-listener harness and 1001 drain assertions.
- `cmd/pyrycode-relay/http_headers_test.go` → `publicHeaderLimits`, `TestPublicServers_HTTP01ResponsesUnchanged`: pin actual timeouts and seed cached challenges.
- `internal/relay/proxyproto.go` → `NewProxyProtoListener`: lazy per-connection parsing and Close delegation.
- `internal/relay/proxyproto_test.go` → `startProxyProtoTLSServer`, `v2Header`: local PROXY/TLS peers.
- `internal/relay/shutdown.go` → `Shutdown`: close listeners before draining; registered hijacked sockets receive 1001 separately.
- `go.mod` and pinned `golang.org/x/net/netutil/listen.go` → `LimitListener`: acquire before Accept, release through connection Close exactly once, wake blocked acquisition on listener Close.
- `docs/PROJECT-MEMORY.md`, `docs/knowledge/INDEX.md`: conventions and reading map.
- `docs/knowledge/features/connection-cap.md`: upgraded cap excludes pre-header connections; keep its independent 20-slot contract.
- `docs/knowledge/features/autocert-tls.md`, `docs/knowledge/features/proxy-protocol-listener.md`: TLS floor, independent HTTP-01 and strict PROXY parsing.
- `docs/architecture.md`, `docs/threat-model.md`, `docs/security-followups.md`: stateless content-blind boundary, supply chain, DoS and residual per-source risk.
- [Wire protocol, Security model and TLS](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md): no wire-shape or close-code change.

## Context

The upgraded-session cap starts too late to bound accepted sockets waiting for PROXY, TLS or HTTP headers. Bound raw acceptance per public listener while retaining the existing session cap and registry. #181's public 16 KiB header limits are present. This is one deliverable: bounded raw admission. No overlapping in-flight branch was found for the planned files.

Sizing: approximately 70 production/module lines, 470 tests/helpers and 105 plan lines, below 800 total; no new exported type/interface, no changed consumer signatures, five criteria, and only one new configuration rejection. The #110 analogue added 450 code/test lines plus module entries; this change replaces parser design with an existing listener wrapper. Re-count before committing and after implementation.

Using the already-pinned `golang.org/x/net` v0.60.0 directly deserves an ADR from the documentation stage. Standard `net` has no equivalent admission wrapper. Reusing the Go project's small `netutil` implementation avoids duplicating semaphore, idempotent release and shutdown logic. This package imports only `net` and `sync`, operates on opaque sockets, and adds no parsing or dependencies; versions and checksums stay fixed. This committed plan records the decision before the module edit.

## Design

Add `defaultMaxListenerConnections = 32` and `--max-listener-connections`. Reject non-positive values in `run` before workers or listeners start, using existing safe `value`/`fix` log keys and exit 2. A positive cap may be below the independent upgraded cap.

In each `runServers` listen callback, leave metrics on its existing `ListenAndServe` path. For public servers, call `net.Listen`, immediately wrap with `netutil.LimitListener`, and pass to `Serve` or `ServeTLS`. HTTPS optionally adds `NewProxyProtoListener` above admission and beneath `ServeTLS`'s TLS listener. Each public server gets its own semaphore. No shared listener abstraction or handler/ConnState release logic.

A slot lasts until socket Close, including keep-alive and WebSocket hijack. At capacity Accept blocks before the underlying Accept, leaving queued TCP peers outside parsing and handler invocation. Dial success does not prove admission. Health checks occupy raw slots; metrics use a separate listener.

Document the combined estimate beside `defaultMaxConnections`: two listeners × 32 slots, budget approximately 128 KiB per raw slot for 16 KiB headers plus 4 KiB Go slack, TLS/PROXY/bufio buffers and grown stacks = 8 MiB. Conservatively charge this for all 64 slots and retain 20 × 4.4 MiB = 88 MiB upgraded state (their sockets are already within those 64). About 96 MiB live connection allocation × 2 for GOGC=100 + 56 MiB runtime/autocert/kernel reserve ≈ 248 MiB, below 256 MiB. This deliberately double-counts some upgraded transport storage, not accepted connections. It is a sizing estimate, pending isolated Fly verification, not an OOM guarantee.

Preserve all public server literals and phase timeouts: PROXY and ReadHeaderTimeout 5s, Read/WriteTimeout 60s, IdleTimeout 120s; TLS floor, 16 KiB header allowance, HTTP-01 challenge/404 behavior and upgraded 503 behavior are unchanged.

## Concurrency model

No new production goroutines. Each serving goroutine acquires its listener semaphore before Accept; net/http owns accepted sockets until hijack, then the existing WebSocket handler owns Close. `LimitListener` releases once even when Close races or repeats. Closing the listener wakes a capacity-blocked Accept. Existing `runServers` and `Shutdown` drain all servers and registered WebSockets on signal (exit 0) or listener failure (exit 1) within `drainDeadline`.

## State transitions and identity reuse

| Event | Race-detector coverage |
|---|---|
| Capacity reached; queued peer remains unparsed until admission | `TestRun_ListenerCapPublicModes`, `TestListenerCap_CloseOnce` |
| Abrupt close, failed/stalled TLS, incomplete HTTP headers, rejected/stalled PROXY → fresh admission | `TestRun_ListenerCapReleasePaths` |
| Keep-alive request finishes → slot retained until socket close | `TestRun_ListenerCapPublicModes` |
| WebSocket hijack → holds slot → normal close releases → fresh admission | `TestRun_ListenerCapWebSocketAndShutdown` |
| Same slot reused repeatedly; concurrent/repeated Close releases only once | `TestListenerCap_CloseOnce` |
| HTTP-01 saturation leaves HTTPS/metrics available; freed slot serves cache and 404 | `TestRun_ListenerCapPublicModes` |
| Signal with saturated listener and queued peer → blocked Accept exits and 1001 drain | `TestRun_ListenerCapWebSocketAndShutdown` |
| Listener failure → same bounded drain, exit 1 | `TestListenerCap_ListenerFailureDrain` |

No routing identity or registry lifecycle changes.

## Error handling

Invalid cap is a startup configuration refusal. Bind, PROXY-wrapper and Serve errors still propagate into `runServers`' listener-error drain. Close the newly opened listener if wrapper setup fails; Serve/ServeTLS own closure otherwise. Capacity is backpressure, without an HTTP rejection or new log per queued peer. Phase failures rely on existing parser/timeouts and Close cleanup.

## Testing strategy

Write tests before production changes and record failure from missing admission/flag. Boot real `run` instances with cap one (below the default upgraded cap), using local cached certificates and challenges to prevent ACME access. Check public plaintext, HTTP-01, HTTPS and PROXY HTTPS wiring. Establish keep-alive admission with a successful request; probe a second connection using a short read deadline and assert no response before freeing the first. TLS failure probes also prove no handshake parsing while queued. Verify metrics remain accessible. Exercise release paths at a small cap, and normal WS close then fresh admission. Direct listener tests observe Accept results and concurrent Close to prove exactly-once release and blocked Accept shutdown. Signal/failure tests verify 1001 and bounded exits.

Run focused new tests under `-race`, then touched packages `go test -race ./internal/relay/... ./cmd/pyrycode-relay/...`, `go vet ./...`, and `go build ./cmd/pyrycode-relay`. User's AGENTS.md additionally requires `make check` before push; run that full check despite the usual builder scoping. Existing header-limit tests pin public timeouts. No Linux-only file changes.

## Open questions

None. Dependency version, admission placement, socket ownership and defaults are fixed above.

## Documentation handoff

Pending for documentation stage:

- `docs/knowledge/features/connection-cap.md` § What is not counted, Wiring, Default derivation, Out of scope: distinguish upgraded versus per-listener raw cap, flag/default/lifetime, combined 256 MB estimate, health capacity and separate metrics.
- `docs/threat-model.md` § DoS resistance: raw bound, one-source saturation risk, phase timeouts versus long-lived upgraded slots; upgrade rate limiting cannot protect pre-header phases.
- `docs/threat-model.md` § Supply chain and Triggers for re-review: direct pinned x/net justification and re-review; write an ADR under `docs/knowledge/decisions/` for reusing netutil, linked from INDEX.
- `docs/knowledge/features/autocert-tls.md` § Concurrency model and `docs/knowledge/features/proxy-protocol-listener.md` § Wiring: admission beneath PROXY/TLS and independent HTTP-01 capacity.
- `docs/deploy.md` § fly.toml gotchas and Verify a deploy: raw default/combined budget and required isolated non-production Fly verification before production deploy. Operator owns checks and deploy.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — `run` caps opaque accepted sockets before `NewProxyProtoListener`/TLS/HTTP. Existing endpoint header gates and `json.RawMessage` forwarding remain intact.
- [Tokens, secrets and credentials] No findings — no credential handling or header logging added; test keys are local fixtures only.
- [File operations] No findings — production autocert cache is unchanged; new persistent state is absent.
- [Subprocesses] No findings — no production subprocesses.
- [Cryptography] No findings — `ServeTLS` retains `relay.TLSConfig` and TLS 1.2 floor; no cryptographic algorithm changes.
- [Network & I/O] SHOULD FIX — every public callback must wrap before parsing, with distinct limiters; assert all actual modes, health slot occupancy, and HTTP-01 independence. Preserve pinned timeout/header tests.
- [Network & I/O] OUT OF SCOPE — one source can fill a listener; per-source allowances and Fly edge ceilings are separate work from #179, catalogued in `docs/security-followups.md` under Per-IP concurrent connection cap and Fly Machines services.concurrency; operator sizing check is pending.
- [Errors, logs and telemetry] No findings — only existing allowlisted `value`/`fix` keys log configuration integers; no per-peer logging, public error body or user-identifiable metric is introduced.
- [Concurrency] SHOULD FIX — ensure repeated Close cannot free a newer owner's slot, admission survives hijack, and Close wakes a saturated Accept; race-test each reuse/shutdown event above.
- [Threat model alignment] No findings — independent per-process limits preserve the single-instance design and content blindness. Direct x/net promotion triggers the explicit supply-chain/ADR documentation handoff.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-10-11

## Revisions

- 2026-10-11: Reading Go's `ServeTLS` confirmed setup errors can return before `Serve` takes listener ownership. Preserve `ListenAndServeTLS`'s deferred listener Close in the HTTPS callback as well as closing on PROXY setup failure. Add an omitted-flag test admitting exactly 32 keep-alive connections. Final recount remains below 800 lines, with no new exported types or changed consumer signatures.
