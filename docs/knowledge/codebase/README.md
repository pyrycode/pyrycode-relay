# Per-Ticket Codebase Notes

> **Frozen on 2026-10-03 at 53 notes.** Read these as history; never add or edit one. Lessons now fold into the feature doc for the area under `docs/knowledge/features/`, in the section they belong to. The notes were retired because nobody but the documentation stage read them, and the serial documentation stage already prevents the merge conflicts they were introduced to avoid. The daemon and mobile repos froze theirs on 2026-08-19 and 2026-09-05.

One file per ticket: `<ticket-number>.md`. Each file describes what was built for that ticket — **implementation summary, patterns established, AND lessons learned** all live in this per-ticket file.

## Convention

- **Filename**: `<ticket-number>.md` (e.g. `5.md`, `21.md`). Numeric only — no prefix, no description in the filename.
- **One file per ticket.** Never edit a sibling ticket's file.
- **Never modify `docs/PROJECT-MEMORY.md`.** All sections in that file are frozen as of 2026-05-11. New work lives here.
- **Never append to `docs/lessons.md`.** Frozen 2026-05-11. New lessons surface as a "Lessons learned" section inside the relevant `<ticket>.md`.
- **Directory listing IS the index.** `ls docs/knowledge/codebase/` sorted is reverse-chronological-ish (issue numbers monotonic). No separate index file to maintain.

## Why

Shared-append docs (whether `PROJECT-MEMORY.md`'s sections or `lessons.md`) cause recurring merge conflicts via three failure modes:

- **Concurrent docs** — two cycles' documentation agents both append at the same anchor → add/add conflict. Fixed by `serial: true` in v1 dispatcher `2d6b4ee`.
- **Stale-branch + marched-forward main** — feature/B appends based on B's snapshot, while feature/A merges with its own appended section first. When B tries to merge, B has no record of A's entry → conflict. `serial: true` doesn't fix this; only rebase does.
- **Cross-phase bleeding** — developer writes to PROJECT-MEMORY.md in feat commits (not just documentation), bypassing the doc-phase serial protection entirely.

Incidents this convention prevents: 2026-05-09 pyrycode Phase 3 batch (3 PRs stuck), 2026-05-10 pyrycode (#260/PR #266, #255/PR #259), 2026-05-11 v2 pipeline (4 stranded PRs on `docs/PROJECT-MEMORY.md`). Per-ticket files eliminate the hot line entirely — two cycles never touch the same file, stale-branch conflicts impossible by construction.

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
