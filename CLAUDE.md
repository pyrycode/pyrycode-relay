# Pyrycode Relay

## Project

`pyrycode-relay` is the stateless, content-blind WebSocket broker between the
`pyry` daemon and its mobile and desktop clients. It routes frames by the
`x-pyrycode-server` header and never reads message payloads; the daemon owns
all canonical state (conversations, sessions, message history). Deployed on
[Fly.io](https://fly.io) at `pyrycode-relay.pyryco.de`, live since 2026-05-29.

- **Language:** Go
- **Binary:** `pyrycode-relay`
- **Repo:** `github.com/pyrycode/pyrycode-relay`
- **License:** MIT

## Reading path

Start with [`docs/PROJECT-MEMORY.md`](docs/PROJECT-MEMORY.md) — it maps where
everything lives. Then:

- [`docs/knowledge/INDEX.md`](docs/knowledge/INDEX.md) — one-line summaries of
  the evergreen knowledge base (features, decisions).
- `docs/knowledge/codebase/<N>.md` — per-ticket implementation notes, frozen
  on 2026-10-03. Read them as history.
- [`docs/architecture.md`](docs/architecture.md) — system-level design.
- [`docs/deploy.md`](docs/deploy.md) — Fly.io bootstrap, steady-state deploy,
  rollback.
- [`docs/threat-model.md`](docs/threat-model.md) and
  [`docs/security-followups.md`](docs/security-followups.md) — security
  posture and the deferred-hardening menu.

## Wire protocol

The single source of truth is the pyrycode repo's `docs/protocol-mobile.md` —
locally at `../pyrycode/docs/protocol-mobile.md` when the sibling checkout
exists. The relay is content-blind: the protocol version lives in each frame's
`v` field, not in the `/v1/*` route paths.

## Use codegraph for symbol lookups

This repo is indexed for codegraph (`.codegraph/`, gitignored). Prefer `mcp__codegraph__codegraph_*` MCP tools over grep for symbol-level questions — where something is defined, what calls it, what breaks if it changes.

- **Before changing or removing an exported function** — run `codegraph_callers` first to find every call site.
- **"Where is X defined" / "what does X call"** — `codegraph_search`, `codegraph_node`, and `codegraph_callees` beat reading files end to end.
- **For a broader "how does this area work"** — `codegraph_context` or `codegraph_impact` before a cross-cutting change.
- Fall back to grep/Read for comments, string literals, and pending edits the index hasn't picked up yet.
- In Claude Code these are deferred tools: load them once with `ToolSearch` (e.g. `select:mcp__codegraph__codegraph_search`) before first use. Codex sees the same `mcp__codegraph__<tool>` names directly.

## Rules

- `docs/PROJECT-MEMORY.md` is **read-only for agents**. Humans maintain it
  directly.
- `docs/lessons.md` is **frozen (2026-05-11)**. Historical reference only.
- Lessons fold into the feature doc for the area under `docs/knowledge/features/`.
  `docs/knowledge/codebase/` is frozen (2026-10-03).
- Run `make check` (`go vet` + `go test -race`) before any push.
