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

<!-- CODEGRAPH_START -->
## CodeGraph

Adapted from the block CodeGraph 1.6.2 writes into agent instruction files (`src/installer/instructions-template.ts`, github.com/colbymchenry/codegraph).

This repository is indexed by CodeGraph (`.codegraph/` at the repo root, gitignored). Reach for it BEFORE grep/find or reading files when you need to understand or locate code:

- **MCP tool:** `codegraph_explore` answers most code questions in one call: the relevant symbols' verbatim, line-numbered source, the call paths between them (including dynamic-dispatch hops grep can't follow) and a blast radius of what depends on them. Name a file or symbol in the query to read its current source. If it is listed but deferred, load it by name via tool search (`select:mcp__codegraph__codegraph_explore`).
- **Shell (always works):** `codegraph explore "<symbol names or question>"` prints the same output. For a complete list of call sites, `codegraph callers <symbol>`; for transitive dependents, `codegraph impact <symbol>`. The shell reads the index without updating it.

Trust codegraph's results; don't re-verify them with grep. Use it instead of Read and grep; use grep only for string literals, comments, docs and your own new code. A running codegraph server folds your edits into the index within about a second; if a response starts with a staleness banner or flags a file as changed on disk, Read the files it lists. If there is no `.codegraph/` directory, skip CodeGraph entirely.
<!-- CODEGRAPH_END -->

## Rules

- `docs/PROJECT-MEMORY.md` is **read-only for agents**. Humans maintain it
  directly.
- `docs/lessons.md` is **frozen (2026-05-11)**. Historical reference only.
- Lessons fold into the feature doc for the area under `docs/knowledge/features/`.
  `docs/knowledge/codebase/` is frozen (2026-10-03).
- Run `make check` (`go vet` + `go test -race`) before any push.
