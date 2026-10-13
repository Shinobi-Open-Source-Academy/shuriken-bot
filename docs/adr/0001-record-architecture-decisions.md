# 0001. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-10-13

## Context

Shuriken has already accumulated decisions whose reasoning is not visible in the code, and some of them look arbitrary or wrong without that reasoning:

- The findings schema is enforced with structured outputs rather than a forced tool call. Reading the code, a forced tool call looks simpler and more obvious. It returns HTTP 400 on current models.
- Activity is recorded under a GitHub login with an `unlinked` flag rather than being dropped when no platform account matches. This looks like sloppiness until you know that no user in the platform has a linked GitHub profile yet, so unlinked is the normal state rather than an edge case.
- `author_association` is used as a write-access proxy instead of the collaborator-permission endpoint. This looks like a shortcut. It avoids requesting the `Administration` permission.

Each of those took research to settle. Without a record, the next person — or the same person in six months — either re-derives it or "fixes" it back to the obvious-but-wrong version.

The platform repository has no ADRs today, so this also establishes the practice for Shuriken and is available to adopt there.

## Decision

Record decisions that are **architecturally significant** as numbered ADRs in `docs/adr/`, using Nygard's structure plus an explicit **Revisit when** section.

Architecturally significant means at least one of:

- It is costly to reverse once code depends on it.
- A reasonable engineer would pick differently without the context.
- It constrains what other components may do.
- It trades a known cost for a known benefit, and the trade should be rechecked
as conditions change.

Ordinary implementation choices do not get an ADR; a code comment is the right place for those, and the codebase already uses them for exactly this.

## Consequences

- Significant decisions carry their reasoning, including the options rejected and why. A rejected option with recorded reasoning is as useful as the accepted one.
- **Revisit when** turns "this is right" into "this is right while X holds", which is the honest version and the part that ages well.
- ADRs cost effort to write and can go stale. The mitigation is keeping them few and immutable: supersede rather than edit.
- A **Proposed** ADR is a decision request, not documentation of the status quo. The index marks these in bold so they are visibly awaiting a call.



## Alternatives considered

**Reasoning in code comments only.** Already done, and it works well for local "why this line" context. It cannot hold a cross-cutting decision — the database-ownership question touches the store, the migrations, the deployment and the platform, so there is no single file where it belongs.

**A design section in the README.** The README is written for someone setting Shuriken up. Decision history and rejected options are noise there, and the README has to stay accurate, whereas an ADR is a historical record that is allowed to be superseded.

**Nothing.** The default, and the reason the three examples above were about to be lost.

## Revisit when

The practice stops paying for itself — ADRs are being written for routine choices, or accepted ADRs no longer match the code.