# Project memory — pyrycode-relay

Stateless WebSocket router between mobile clients and pyry binaries. Internet-exposed; adversarial input is the default assumption. Authoritative wire spec lives in `pyrycode/pyrycode/docs/protocol-mobile.md`.

> **READ-ONLY FOR AGENTS (as of 2026-05-11).** All five pipeline agents have explicit "Never Update docs/PROJECT-MEMORY.md" rules. Lessons fold into the feature doc for the area under [`docs/knowledge/features/`](knowledge/features/). The per-ticket notes under [`docs/knowledge/codebase/`](knowledge/codebase/) were frozen on 2026-10-03. Humans maintain this file directly.

## Where things live

- `docs/knowledge/codebase/<N>.md` — **frozen 2026-10-03.** Per-ticket notes up to relay #154, kept as history.
- `docs/knowledge/features/` — evergreen feature docs. Lessons fold into the section of the doc they belong to.
- `docs/knowledge/decisions/` — ADRs, numbered sequentially.
- `docs/knowledge/INDEX.md` — one-line summaries. **Documentation phase is the sole writer.**
- `docs/lessons.md` — **frozen 2026-05-11.** Historical reference. New lessons go in the feature doc for the area.
- `docs/specs/architecture/<ticket>-<slug>.md` — architect specs.
- `docs/architecture.md` — system-level design overview.
- `docs/threat-model.md` — operational threat model (deploy, supply chain, DoS, log hygiene, TLS).
- `docs/archive/PROJECT-MEMORY-history-2026-05-11.md` — pre-2026-05-11 "What's built" table + "Patterns established" content store, archived in place so existing cross-references still resolve.

## Project-level conventions (human-maintained)

Stable rules that span tickets. New entries land here only when genuinely cross-cutting; per-ticket detail goes in the feature doc for the area.

- **Sentinel errors + `errors.Is` branching at protocol boundaries.** New routing-layer code follows the `Err...` naming and wraps with `fmt.Errorf("…: %w", err, sentinel)`.
- **Opacity by type for inner-frame payloads.** Carried as `json.RawMessage`. The relay never deserialises payloads; the type makes that hard to violate accidentally.
- **Validate at the envelope boundary, not deeper.** Structural checks (presence, non-empty, JSON well-formedness) belong here; semantic checks (token validity, message kind) belong to the binary.
- **Credentials the relay does not validate are presence-checked then discarded** — never logged, never put in error strings. `X-Pyrycode-Token` is opaque to the relay (binary owns verification).
- **Loud failure over silent correction.** `--domain` and `--insecure-listen` are mutually exclusive and explicit. `ErrCacheDirInsecure` refuses to start on a world/group-readable cert cache rather than re-chmoding it.
- **Tests live in the same package** (`package relay`, not `relay_test`) so they can `errors.Is` against unexported sentinels.
- **Per-conn goroutines exit via LIFO defers, not by closing the conn themselves.** Handler owns cleanup; goroutines own only the failure-path close.

## Conventions

- Single ticket = single commit on `feature/<n>` named `feat(relay): <summary> (#<n>)`.
- `make vet`, `make test` (`-race`), and `make build` must all be clean before merge.

## Open follow-ups

*Human-maintained. Agents: do not edit. File new follow-ups as GitHub issues.*

- (none currently — file new entries as GitHub issues)
