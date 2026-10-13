# Architecture decision records

Short documents recording decisions that were expensive to reach and would be
expensive to reverse by accident. They exist so that six months from now
nobody has to reconstruct the reasoning from the code, and so that a decision
can be revisited deliberately rather than drifted away from.

An ADR is immutable once accepted. If a decision changes, write a new ADR that
supersedes the old one and update the status of both. Correcting a typo is
fine; quietly rewriting the reasoning is not.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-shuriken-data-ownership.md) | Shuriken's data stays in the platform database | Accepted |
| [0003](0003-least-privilege-identity-reads.md) | Read platform identities through a projected view | **Proposed** |
| [0004](0004-integration-boundary.md) | Expose contributor data through a read API, not shared collections | **Proposed** |

## Status values

- **Proposed** — written, not yet agreed. Needs a decision.
- **Accepted** — agreed and in force. The code should match it.
- **Superseded by NNNN** — no longer in force; the replacement says why.
- **Rejected** — considered and declined. Kept because the reasoning is useful.

## Format

[Michael Nygard's](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
structure: Context, Decision, Consequences, Alternatives considered. Plus a
**Revisit when** section, because a decision that is right today is usually
right because of conditions that can change, and naming those conditions is
most of the value.
