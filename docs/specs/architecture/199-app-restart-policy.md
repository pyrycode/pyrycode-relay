# App restart policy (#199)

## Files read

- `fly.toml` → `[processes].app`, services, mount, VM and deploy settings; the existing manifest has no restart policy.
- `docs/PROJECT-MEMORY.md` → repository conventions and documentation ownership.
- `docs/knowledge/INDEX.md` → deployment knowledge entry.
- `docs/knowledge/features/fly-deploy.md` → process targeting, TCP passthrough, PROXY v2, connection ceilings and single-machine constraints.
- `docs/architecture.md` → Hosting and Single-instance constraint; recovery must retain exactly one active production relay.
- `docs/threat-model.md` → Deploy security; deployment remains operator-owned.
- `docs/deploy.md` → fly.toml gotchas and Verify a deploy; live observations are distinct from manifest validation.
- [Fly configuration reference, restart section](https://docs.fly.io/reference/configuration#the-restart-section) → documented `always` policy and process-group targeting.

## Change

Add one `[[restart]]` table to `fly.toml` with `processes = ["app"]`
and `policy = "always"`, omitting `retries`. The observed production
Machine uses `on-failure` with a ten-retry ceiling; the manifest currently
leaves recovery implicit. This declares recovery regardless of exit code.
All existing configuration values remain unchanged. There are no Go,
payload, credential, logging or application-state changes. No new decision
record is needed. The in-flight branch check found no `fly.toml` overlap.

Size re-count: one deliverable and one acceptance criterion; approximately
60 written lines including this plan and offline validation, one configuration
file, zero new exported types, zero consumer updates and zero new reject branches.

## Testing strategy

Before changing the manifest, run an offline Python standard-library `tomllib`
assertion that requires exactly the intended restart table; observe its failure
because the table is absent. Re-run after the edit and compare the entire parsed
manifest, excluding only the new restart table, with the pre-edit Git version.
This proves every existing value, including services, PROXY v2, volume,
concurrency and rolling deploy, is preserved. Run the required relay race suite,
`go vet ./...` and binary build. No new persistent test harness is needed for
this literal configuration addition. Live recovery is not established offline.

## Documentation handoff

- Pending documentation stage: `docs/deploy.md` § fly.toml gotchas and Verify a deploy must explain restart recovery regardless of exit code, the explicit `app` policy and omitted retry setting.
- Keep operator checks pending: effective-policy verification, isolated recovery after at least eleven consecutive crashes and lifecycle evidence, followed by manual production deployment while preserving exactly one active production relay. Merge alone does not complete these checks.
- Alert installation and delivery remain #198.
