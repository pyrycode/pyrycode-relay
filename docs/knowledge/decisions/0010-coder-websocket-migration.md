# ADR-0010: Migrate to `github.com/coder/websocket`

**Status:** Accepted (#98)
**Supersedes:** [ADR-0004](0004-ws-library-and-adapter-context-strategy.md)
**Date:** 2026-05-28

## Context

`nhooyr.io/websocket` (v1.8.x), chosen in ADR-0004, was deprecated by its author; canonical development moved to `github.com/coder/websocket`. Coder forked for continued maintenance under a non-vanity import path with the same public API — not an API redesign.

Two existing artefacts forbade an opportunistic switch and reserved judgement for the deprecation case:

- `docs/lessons.md:63` — "do not opportunistically switch to the coder fork without an ADR." Upstream deprecation is the trigger the lesson reserved judgement for.
- ADR-0004's last *Consequences* bullet — "do not switch to `github.com/coder/websocket` opportunistically." Same reservation.

This ADR is the ADR those artefacts asked for.

Trigger today: the QA agent stage on `pyrycode-relay-agents` runs `make check` as its mechanical gate, and the follow-up plan adds `staticcheck` to `make check`. The pre-existing SA1019 deprecation errors against `nhooyr.io/websocket` would otherwise flood every QA run with infra-classified failures.

## Decision

The relay imports `github.com/coder/websocket` (v1.8.14, latest released). The `WSConn` adapter, upgrade handlers, heartbeat goroutine, and shutdown drain are otherwise unchanged. The context strategy from ADR-0004 (`closeCtx`-cancelled-by-`Close`, per-call `WithTimeout`) carries forward verbatim because the library API is unchanged.

## Rationale

Coder forked `nhooyr.io/websocket` for continued maintenance, not API redesign. The pre-swap audit confirmed that every package symbol the relay touches (`Accept`, `Dial`, `StatusCode`, the four named close-code constants — `StatusNormalClosure` 1000, `StatusGoingAway` 1001, `StatusInternalError` 1011, `StatusMessageTooBig` 1009 — `MessageBinary`, `AcceptOptions{OriginPatterns}`, `DialOptions{HTTPHeader}`, `CloseError{Code, Reason}`) and every `Conn` method (`Read`, `Write`, `Close`, `Ping`, `SetReadLimit`, `CloseRead`) is present with identical signatures.

The migration is mechanical: import-path swap in 13 `.go` files plus `go.mod`/`go.sum`, plus doc-comment library-name updates. No call-site rewrites, no new types, no behavioural change in frame handling or close codes.

Adding `staticcheck` to `make check` (separate downstream ticket) becomes unblocked.

## Consequences

- The relay has exactly one WS library and one adapter, unchanged from ADR-0004. Future routing-layer code hands the registry a `*WSConn`, never a hand-rolled adapter.
- `govulncheck`'s scan surface shifts from `nhooyr.io/websocket` to `github.com/coder/websocket`. The supply-chain framing in `docs/threat-model.md` § *Supply chain — Go dependencies* already names "any direct WS library"; this changes *which* module sits there, not the threat surface.
- Any future migration trigger (a meaningful API redesign in coder, or a new fork) requires another ADR; opportunistic library swaps remain forbidden.

Three behavioural-property verdicts against the relay's load-bearing assumptions (`docs/lessons.md:59,77,85`):

- **Close-safe-with-in-flight-Write (lesson 59) — preserved.** Godoc for `(*Conn).Close` documents that all methods may be called concurrently except `Reader`/`Read`, so `Close` is safe alongside an in-flight `Write`. The non-deadlocking adapter design (`closeCtx`-cancelled-by-`Close` without taking `writeMu`) continues to hold. Runtime cross-check: `TestWSConn_ConcurrentSend_ProducesIntactFrames` and `TestShutdown_CloseIdempotentOnRealWSConn` pass under `-race`.
- **5s close-handshake grace (lesson 77) — preserved.** Godoc for `(*Conn).Close`: "It will write a WebSocket close frame with a timeout of 5s and then wait 5s for the peer to send a close frame." Numerically identical to nhooyr's documented behaviour. `drainDeadline = 10 * time.Second` in `cmd/pyrycode-relay/main.go` remains correctly sized, and `TestHeartbeat_UnresponsivePeer_TriggersClose`'s 7s wait still has appropriate margin.
- **Default per-frame read cap — preserved.** Godoc for `(*Conn).SetReadLimit`: "By default, the connection has a message read limit of 32768 bytes." That is 32 KiB, not the 32 MiB stated by the pre-existing lesson-85 headline (`nhooyr.io/websocket` v1.8.x also defaulted to 32 KiB; the lesson's number is a frozen factual error documented here for the next reader). Production wires 256 KiB via `SetReadLimit` in `NewWSConn` before any frame is read, so the default is not exercised on the routed-frame path; test fixtures (`startEcho`, `startHeartbeatPair`, …) use small frames that fit comfortably under 32 KiB. The relevant property — "production code controls the cap; the constructor closes the window before any reader runs" — holds unchanged across the fork.
