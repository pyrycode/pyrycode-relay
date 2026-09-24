# Spec #118 — Configure a Fly health check for the relay

Short plan: one config file, no Go code, no new state, no new failure mode in the binary.

## Files read

- `fly.toml` → the two `[[services]]` blocks (`internal_port = 8080` / `8443`) and `[deploy]` — the only file this ticket changes.
- `cmd/pyrycode-relay/main.go` → `run` and `runServers` — every boot check (`CheckEnvConfig`, `CheckSingleInstance`, `CheckRunningAsRoot`, `CheckCapabilities`, `CheckListenerPorts`) and `relay.NewAutocertManager` run before any listener binds and none touches the network, so boot is sub-second; `runServers` drains and exits when any listener fails, so 8080 and 8443 go down together.
- `docs/knowledge/features/fly-deploy.md` § *Single-machine hard cap* — the `[deploy] strategy` row the documentation stage will update.
- `docs/security-followups.md` § *`/healthz` exposure* — the "breaks Fly health probes" caveat that no longer applies once the check is TCP.

No in-flight `feature/*` branch touches `fly.toml`.

## Change

Add a `[[services.tcp_checks]]` block to the `internal_port = 8080` service with `grace_period = "30s"`, `interval = "15s"`, `timeout = "5s"`, and a comment explaining why the check is TCP and on 8080 (HTTP `/healthz` unreachable by a Fly check: 8080 404s everything but ACME per ADR-0002, 8443 requires a PROXY v2 header via `NewProxyProtoListener`; a TCP probe on 8443 fails in the TLS path of a server with no `ErrorLog` and logs noise; a boot refusal exits before either listener binds and `runServers` takes both down together). The 8443 service gets no check. Switch `[deploy] strategy` from `"immediate"` to `"rolling"` and rewrite its comment: rolling waits for each Machine's checks, `immediate` does not, and rolling updates the single Machine in place without creating a second one. `max_unavailable = 1` and every other line stay as they are.

Timings: boot is local-only and sub-second, so 30 s grace is roughly a 30× margin on a shared-cpu-1x / 256 MB machine. A 15 s interval with a 5 s timeout keeps the probe cheap while tolerating a loaded machine: a false failure takes port 80 out of Fly's proxy on a single-Machine fleet, so the timings err generous.

## Testing strategy

No Go logic changes, so no Go test. `go vet ./...` and `go build ./cmd/pyrycode-relay` confirm nothing else moved. `flyctl config validate` would check the schema but needs Fly auth and is operator-side; the operator follow-up after the next `flyctl deploy` is `flyctl checks list -a pyrycode-relay`, confirming the check passes.

## Documentation handoff (pending — documentation stage)

- `docs/knowledge/features/fly-deploy.md`: update the `[deploy] strategy` row in § *Single-machine hard cap* and the operator-direct deploy bullet in § *What it does* to say `rolling`; describe the TCP check on 8080.
- `docs/deploy.md`: in the steady-state deploy steps, replace "`immediate` deploy strategy" with rolling and say the deploy now fails if the Machine never passes its check.
- `docs/security-followups.md` § *`/healthz` exposure*: Fly's check is TCP on 8080, not an HTTP probe of `/healthz`, so the "breaks Fly health probes" caveat no longer applies.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the check opens a TCP connection from Fly's checker and closes it; it sends no request, so no header, envelope or payload reaches any handler. Content-blindness is untouched.
- [Tokens, secrets, credentials] No findings — no credential is configured, read or logged; the check carries none.
- [File operations] No findings — no file other than `fly.toml` changes; the autocert volume mount is untouched.
- [Subprocess execution] No findings — `[processes]` argv is unchanged; the relay still spawns nothing.
- [Cryptographic primitives] No findings — the check never reaches TLS; the 8443 service is deliberately left without a check so no probe hits the PROXY-v2 / TLS path.
- [Network & I/O] No findings — the probe reaches the 8080 `http.Server` in `run`, which keeps its `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`; a connection that closes before sending a request line ends silently in `net/http`. One probe per 15 s is negligible load. No new public endpoint: `/healthz` stays where it is, and nothing is served on a new surface.
- [Network & I/O] Availability risk, accepted — a spurious check failure removes port 80 (ACME HTTP-01 and the ADR-0002 404) from Fly's proxy on a single-Machine fleet. Mitigated by the generous timings above; 443 has no check, so a spurious failure does not take the WebSocket surface offline. The operator confirms with `flyctl checks list` after the next deploy.
- [Network & I/O] OUT OF SCOPE — a TCP check proves only that the 8080 socket accepts; a wedged process whose kernel backlog still completes handshakes would pass. A deeper liveness probe needs an HTTP endpoint Fly can reach, which is a new surface and a threat-model re-review; tracked under `docs/security-followups.md` § *`/healthz` exposure*.
- [Error messages, logs] No findings — no new log key; the check does not trip the TLS handshake error path because it never targets 8443.
- [Concurrency] No findings — no goroutine or lock changes. `rolling` replaces the Machine in place; there is never a window with two Machines and two registries.
- [Threat model alignment] No findings — single-instance holds: `rolling` on a one-Machine fleet with `max_unavailable = 1` never creates a second Machine, unlike blue-green or canary. No `docs/threat-model.md` re-review trigger fires: no dependency, no public endpoint, no deploy-target change.

**Reviewer:** builder (self-review per the security-review checklist)
**Date:** 2026-09-24
