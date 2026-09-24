# #136 — `TestUpgradeMetrics_*_TerminalPaths` flaky under full-package `-race`

Short plan: test-only change, no new type, state or failure mode.

## Files read

- `internal/relay/metrics_upgrade_test.go` → `pollUntil`, `assertServerOutcomes`, `assertClientOutcomes`, `assertFailureKinds`, `TestUpgradeMetrics_ServerEndpoint_TerminalPaths`, `TestUpgradeMetrics_ClientEndpoint_TerminalPaths` — the only file touched.
- `internal/relay/metrics_counters_test.go` → `assertCounter` — the one-shot scrape-and-match assert the outcome helpers call; its `t.Errorf` carries the scrape body.

No in-flight feature branch touches the test file.

## Change

`ServerHandler` and `ClientHandler` bump their upgrade counters after the event the test waits on (the registry claim seen through `BinaryFor` / `waitForPhones`, or the 4409/4404/4429 close frame the peer's `Read` returns on), so a one-shot scrape can land before the increment. Add one helper, `awaitUpgradeCounters(t, h, endpoint, outcomes, kinds)`, to `metrics_upgrade_test.go`: it builds the expected line for every cell of the endpoint's outcome set and of `allFailureKinds` (unlisted cells at 0), uses `pollUntil` with a 2-second deadline until one scrape body contains all of them, then runs the existing `assertServerOutcomes`/`assertClientOutcomes` and `assertFailureKinds` unconditionally. On success the asserts pass against the settled counters; on a wrong count they fail after the deadline with the scrape body in the message, exactly as today. The six `httptest.NewServer` subtests (server `accept`, `reject_headers`, `reject_409`; client `accept`, `reject_headers`, `reject_404`, `reject_429`) replace their assert pair with one call. The `reject_rate_limit` subtests call `ServeHTTP` synchronously and keep their one-shot asserts. No production file changes.

## Testing strategy

The subtests are their own proof. Deterministic check that a wrong count still fails in bounded time: temporarily flip one expected count locally, confirm the failure names the missing line with the scrape body after ~2s, revert. Flake check per the AC: `go test -race -count=1 ./internal/relay/` 20 runs in a row, plus `go vet ./...`.

## Revisions

- **2026-09-24, implementation.** The Change paragraph says "six" `httptest.NewServer` subtests; its own list names seven (three server, four client), and all seven moved to `awaitUpgradeCounters`. A miscount, not a design change. `gofmt -w` also dropped a pre-existing trailing blank line at the end of the test file.
