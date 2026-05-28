# Spec #97 — Capture `fly.toml` gotchas surfaced by first deploy bootstrap

Status: ready for implementation
Size: XS (docs + config only; no production code, no tests)

## Files to read first

- `fly.toml` (whole file, ~60 lines) — current manifest shape: one `[processes]` block, two `[[services]]` blocks (ports 80 and 443). The edits land inside this shape; confirm it hasn't drifted since the ticket was filed.
- `docs/deploy.md` (whole file) — current deploy doc. The new gotchas land as a new subsection; pick a placement that reads in narrative order with the existing bootstrap → steady-state → rollback flow.
- `docs/architecture.md:35,48-69` § *Single-instance constraint* and § *The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass* — the canonical explanation of why the self-check exists. The new gotcha-1 prose should be a one-liner that points here, not a re-explanation.
- `internal/relay/single_instance.go:6-26` — the exact `ErrMultiInstanceDeployDetected` message and bypass-env-var constant, so the doc wording matches the code reality.
- `docs/specs/architecture/65-startup-multi-instance-check.md` — full context on the self-check's heuristics, for the architect's own reference if any wording question comes up. The developer does not need to read this; the inline error-message excerpt in the ticket body is sufficient.

## Context

First-deploy bootstrap of `pyrycode-relay` on Fly (2026-05-24) hit two `fly.toml` validation/boot failures that aren't covered by the current `docs/deploy.md` or encoded in the checked-in `fly.toml`:

1. The `PYRYCODE_RELAY_SINGLE_INSTANCE=1` env var is required because Fly's substrate trips the #65 multi-instance heuristic.
2. `processes = ["app"]` is required on each `[[services]]` block whenever `[processes]` is defined — and `[processes]` is always defined here because argv lives there (distroless has no shell for env-var expansion).

Both are non-obvious until you hit the error and trace it back. Capturing them so the next deploy — and the eventual `pyryco.de` re-cutover when DENIC delegation lands — doesn't re-discover them from `flyctl deploy` failures.

Out of scope for this ticket (already documented in the ticket body): the autocert cert-cache subdir workaround (obsoleted by #95) and the custom listen-port flags (tracked under #96; this spec only requires a forward pointer).

## Design

### `fly.toml` edits

Two additions, both inside the existing manifest. No restructuring, no comment rewrites beyond a one-line rationale where it aids future readers.

1. **New top-level `[env]` table.** Place directly under the existing header comment block about the single-machine cap (the header already discusses single-instance enforcement, so the env-var bypass belongs visually adjacent). Add a one-line comment tying the env var to the #65 self-check so a future reader doesn't delete it as dead config.

   ```toml
   [env]
     # Asserts single-instance intent to the #65 self-check, which would
     # otherwise refuse to boot on Fly's multi-instance-capable substrate.
     PYRYCODE_RELAY_SINGLE_INSTANCE = "1"
   ```

2. **`processes = ["app"]` as the first line inside each existing `[[services]]` block.** Required by Fly's schema whenever `[processes]` is defined (which it always is here — argv lives in `[processes]`). No comment needed inline; the rationale is in `docs/deploy.md` per gotcha 2 below.

No other `fly.toml` changes. Do not reorder, reformat, or "tidy" surrounding blocks.

### `docs/deploy.md` edits

Add a new subsection — proposed title **`## fly.toml gotchas`** — placed between *One-time bootstrap* and *Steady-state flow*. Rationale: these are concerns an operator hits when editing `fly.toml` (e.g. on first bootstrap, on a re-cutover, or when porting the manifest to a new app), so they belong adjacent to the bootstrap section but not inside it (they apply beyond first bootstrap).

Subsection structure — three short paragraphs, each ≤ 5 lines of prose, in this order:

1. **`[env] PYRYCODE_RELAY_SINGLE_INSTANCE = "1"` is required.** Explain in one line that Fly's substrate looks multi-instance to the #65 self-check, so the binary refuses to boot without this assertion. Link to `docs/architecture.md` § *The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass* for the full rationale. Do not duplicate the architecture-doc explanation; one sentence + a link.

2. **`processes = ["app"]` on each `[[services]]` block.** Explain in one line that Fly's schema requires every service to name its process when `[processes]` is defined, and that `[processes]` is always defined here because the relay's distroless image needs argv (not env-var expansion) to receive `--domain` / `--cert-cache`. No external link needed.

3. **Forward pointer to #96 — custom listen-port flags.** One sentence noting that `fly.toml`'s current `internal_port = 80` / `internal_port = 443` works only because autocert hardcodes those ports today; once #96 ships, document the high-port pattern (internal 8080/8443 mapped to external 80/443) as the canonical Fly recipe. Link the issue number so the future doc edit isn't forgotten. Do not pre-write the high-port recipe — that belongs in the doc PR that ships alongside #96.

No other `docs/deploy.md` edits. Do not retitle existing sections, do not adjust the bootstrap step list (those steps still describe the first-time path correctly).

## Concurrency model

N/A — docs + config edits only.

## Error handling

N/A — no code paths added.

## Testing strategy

- Run `flyctl config validate` (or `flyctl config validate --config fly.toml` if invoked from outside the repo root) against the updated `fly.toml`. Must exit 0 with no errors and no warnings about service-process mappings or environment configuration.
- If `flyctl` is not available locally, the developer should note that and rely on the CI deploy job catching any validation failure on the next merge. Do not block the PR on local validation.
- Eyeball pass of `docs/deploy.md` for rendering — no broken intra-doc anchor links (e.g. the `architecture.md#...` fragment must match the actual heading slug). No new screenshots or asset additions.

## Open questions

- **Subsection title.** "fly.toml gotchas" is the working title. The developer may pick a near-synonym (e.g. "fly.toml requirements", "Manifest pitfalls") if it reads better in context. Whatever title is chosen, keep it a single short noun phrase — these are reference notes, not a tutorial.
- **Forward-pointer phrasing for #96.** Phrase as "Once [#96](…) ships, this section should be revisited to document the high-port pattern" or equivalent. The exact wording is a developer judgment call; the load-bearing requirement is that the issue number `#96` appears as a clickable GitHub link so search-by-issue finds it.

## Acceptance criteria mapping

For traceability against the ticket body:

- AC1 (`[env]` doc): § *`docs/deploy.md` edits* item 1.
- AC2 (`processes = ["app"]` doc): § *`docs/deploy.md` edits* item 2.
- AC3 (forward pointer to #96): § *`docs/deploy.md` edits* item 3.
- AC4 (`[env]` in `fly.toml`): § *`fly.toml` edits* item 1.
- AC5 (`processes = ["app"]` in `fly.toml`): § *`fly.toml` edits* item 2.
- AC6 (`flyctl config validate` clean): § *Testing strategy*.
