# Per-Ticket Codebase Notes

One file per ticket: `<ticket-number>.md`. Each file describes what was built for that ticket — the bullets that historically went into `docs/PROJECT-MEMORY.md`'s "What's Built" section.

## Convention

- **Filename**: `<ticket-number>.md` (e.g. `5.md`, `21.md`). Numeric only — no prefix, no description in the filename.
- **One file per ticket.** Never edit a sibling ticket's file.
- **Never modify `docs/PROJECT-MEMORY.md`'s "What's Built" section.** It holds frozen pre-2026-05-10 history; new work lives here.
- **Directory listing IS the index.** `ls docs/knowledge/codebase/` sorted is reverse-chronological-ish (issue numbers monotonic). No separate index file to maintain.

## Why

Parallel docs agents writing to the same line in `PROJECT-MEMORY.md`'s "What's Built" caused recurring merge conflicts in the canonical pyrycode pipeline (2026-05-09 Phase 3 batch — 3 stuck PRs; 2026-05-10 — 2 more stuck PRs). Per-ticket files eliminate the hot line entirely — two concurrent docs runs never touch the same file.

Pyrycode-relay's pipeline is younger but inherits the same convention for consistency, so the docs agent prompt is portable and the failure mode never surfaces here.

See `pyrycode/agent-dispatcher#1` for the rollout history.

## Suggested file shape

```markdown
# Ticket #N — <one-line summary>

<one-paragraph what + why>

## Implementation

- Key bullets describing what was built — types, files, data flows, gotchas
- Cross-link to feature docs / decision docs / spec docs
- Note any out-of-scope items deferred for future work
```

Keep it concise. Per-ticket detail; don't re-document the whole feature.
