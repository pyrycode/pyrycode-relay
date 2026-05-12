# Spec: document v1 single-instance constraint in `docs/architecture.md`

Ticket: [#64](https://github.com/pyrycode/pyrycode-relay/issues/64). Doc-only, XS. Split from #39.

## Files to read first

- `docs/architecture.md` (whole file, 36 lines) — the canonical structure to extend. Note the existing two-list pattern ("What this binary does" / "does NOT do") and the trailing **Threat model** section that cross-links to `docs/threat-model.md`. The new section slots between those.
- `internal/relay/registry.go:1-60` — confirm the registry's in-memory, per-process nature. Skim only; the doc summarises the constraint, it does not re-document the type.
- `docs/threat-model.md:1-20` — the cross-link target if you choose to add one. Note the format: `[Section name](./threat-model.md#anchor)` works because both files live in `docs/`.
- `docs/knowledge/features/docker-image.md:70-75` — the existing prose pattern for "single-instance enforcement is the host's problem" — reuse the same tone, do not duplicate the content.
- `docs/specs/architecture/50-ip-rate-limiter.md:260` and `docs/knowledge/codebase/50.md:43` — prior in-tree uses of the phrase "v1 is single-instance"; mirror that phrasing for consistency.

## Context

The ticket is the **doc (stochastic) half** of the belt-and-suspenders pair noted in `docs/PROJECT-MEMORY.md`. The deterministic backstop — a startup self-check that refuses to run when the relay detects it is one of several instances — ships in sibling #65.

The risk being mitigated: Docker + Fly.io makes `fly scale count 3` a one-line command. The connection registry (`internal/relay/registry.go`) is in-memory per process, so two replicas hold two disjoint registries: a phone connected to replica A cannot reach a binary connected to replica B because replica A's server-id table never sees replica B's claim. An operator (or AI agent) who scales out without reading the code will silently break server-id routing for any phone↔binary pair that load-balancer-coincidence places on different replicas. Today `docs/architecture.md` does not call this out; it lists what the relay does and does not do, but the single-instance constraint is implicit. This spec makes it explicit.

No code changes. Doc-only.

## Design

Add a new top-level section to `docs/architecture.md`. The heading **must contain the phrase "single-instance"** (AC-1). Place it after **"What this binary does NOT do"** and before **"Threat model"** — the section is a constraint on deployment topology, which sits naturally between the binary's behavioural contract and the operational threat surface.

### Section structure

The section is short — three subsections plus an optional cross-link line. Total target: 25–40 lines of prose, no code blocks needed except for the env var name and a single example deployment command.

#### Heading

```markdown
## Single-instance constraint (v1)
```

The exact wording is not load-bearing as long as the heading text contains "single-instance" (the AC-1 anchor). Match the title-case style of existing H2s in the file ("What this binary does", "Threat model").

#### Subsection 1 — Why v1 is single-instance

State the structural reason directly. Required content (paraphrase, do not copy verbatim):

- The connection registry (`internal/relay/registry.go`) lives in process memory.
- Two replicas → two disjoint registries.
- Routing fails when a phone and its binary land on different replicas: the phone's replica sees a server-id miss and returns `4404`, even though the binary is connected to a sibling replica.
- v1 ships single-instance for this reason. Operators MUST NOT scale horizontally (`fly scale count > 1`, multiple `docker run` of the same image behind a load balancer, etc.).

Anchor the registry claim with an inline reference to `internal/relay/registry.go` (no line numbers — the file is small and they will rot).

#### Subsection 2 — What multi-instance would require

Enumerate the two known paths (AC-3), documented as **future work, not a commitment**. Phrase as "would need", not "we will add":

1. **Shared registry.** A cross-replica store that every replica consults on every `/v1/server` claim and `/v1/client` lookup. Examples: Redis pub/sub keyed on server-id, NATS subjects per server-id. Imposes a network hop on every routing decision and a new failure mode (registry down → relay can't route).
2. **Sticky-session-on-server-id at the LB layer.** The load balancer hashes on the `x-pyrycode-server` header so that all traffic for a given server-id reaches the same replica. Avoids the shared-store cost but requires LB awareness of the header and breaks if the LB does not support request-routing on headers (most L4 LBs do not).

Make explicit that **neither is planned for v1** and that adopting either would invalidate the bypass env var contract below.

#### Subsection 3 — The `PYRYCODE_RELAY_SINGLE_INSTANCE` bypass

Document the env var (AC-4). Required content:

- The env var name is exactly `PYRYCODE_RELAY_SINGLE_INSTANCE`. Setting it to `1` skips the startup self-check shipped in #65.
- Intended use cases: **emergency rollback** (the self-check itself misfires and blocks startup), and **migration windows** (operator is in the middle of switching to a multi-instance topology and accepts the routing breakage transiently).
- **NOT recommended for production.** Setting it permanently silences a check whose entire purpose is to catch the silent-routing-failure mode this section documents.
- The variable name is the shared contract between this document and #65. If #65 lands with a different name, this section is updated in the same change set. (This sentence may be omitted if it reads as agent-internal scaffolding — the operator-facing version is "the variable name matches what the relay binary checks at startup".)

A one-line code block showing the variable form is fine:

```text
PYRYCODE_RELAY_SINGLE_INSTANCE=1
```

#### Optional cross-link

The ticket notes a cross-link to `docs/threat-model.md` is allowed only if natural. Recommendation: **skip it.** The threat model catalogues adversarial threats (DoS, supply chain, TLS, log hygiene). Operator misconfiguration that silently degrades routing is a different category — it is not an attack, it is a foot-gun. Adding a "see also" link conflates them. If a future ticket adds an "operator foot-guns" section to the threat model, link it then.

### Constraints on prose style

- Match the existing tone of `docs/architecture.md`: declarative, present-tense, no marketing voice. The existing file says "Read message payloads. Frames are opaque bytes." That is the target register.
- Do not duplicate the per-feature detail that already lives in `docs/knowledge/features/docker-image.md`. The architecture doc states the constraint; the features doc states host-wiring detail. Refer by name (`#65`) for the enforcement detail rather than restating it.
- Do not name a target release for multi-instance support. The constraint is "v1 is single-instance"; v2 is not a commitment.

## Concurrency model

N/A — doc-only.

## Error handling

N/A — doc-only.

## Testing strategy

No automated tests. Verification is by reading the diff against the acceptance criteria:

1. The new section's heading text contains the substring "single-instance" (case-insensitive grep against the rendered heading).
2. The section names the in-memory registry, the two-disjoint-registries failure mode, and the resulting `4404` symptom.
3. The section enumerates at least the two multi-instance paths (shared registry, sticky-on-header) and labels them as future work, not commitments.
4. The section contains the exact string `PYRYCODE_RELAY_SINGLE_INSTANCE` and explains: what setting it skips, when it is acceptable, and that it is not recommended for production.
5. No file outside `docs/architecture.md` is changed.

A developer who finishes the edit should run `grep -n single-instance docs/architecture.md` and `grep -n PYRYCODE_RELAY_SINGLE_INSTANCE docs/architecture.md` to confirm AC-1 and AC-4 are literally satisfied before committing.

## Open questions

1. **Env var name.** The ticket nominates `PYRYCODE_RELAY_SINGLE_INSTANCE`. The architect for #65 may pick a different name. Resolution at merge time, not at spec time: whichever of #64 and #65 lands second updates the other to match. Both this doc PR and the #65 implementation PR carry the same env var name at merge.
2. **Future "operator foot-guns" threat-model section.** Out of scope for this ticket. Filed implicitly as something to consider when a sibling ticket touches `docs/threat-model.md`.

## Why not split further

The work is one heading + ~30 lines of prose in one file. There is no fan-out, no cross-package coordination, no test surface. Splitting would produce two tickets that each rewrite the same paragraph.
