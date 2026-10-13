# 0002. Shuriken's data stays in the platform database

- **Status:** Superseded by [0005](0005-shuriken-owns-its-database.md)

> **Superseded.** This ADR's reasoning holds for the premise it was given
> — one bot, one academy, one database. That premise no longer applies:
> Shuriken is now intended for use by other projects, which makes SOS
> Academy a consumer rather than the platform. Kept because the cost
> analysis of a split, and the three-way storage comparison, are what
> 0005 builds on.
- **Date:** 2026-10-13
- **Related:** [0003](0003-least-privilege-identity-reads.md),
[0004](0004-integration-boundary.md)

## Context

The question raised was whether Shuriken should own its own database, with the platform getting at the data through a webhook plus read-only access.

**That question bundles two decisions that are independent, and they have different answers.**

1. **Storage.** Where do Shuriken's records physically live?
2. **Access.** How do the platform and Kunai read them?

Conflating them is the trap. You can keep one database and still have a clean, versioned access boundary. You can split the database and still have the platform querying Shuriken's collections directly, which is coupling with extra operational cost and none of the benefit. **What makes a storage split expensive later is not the move — it is how many consumers depend on Shuriken's schema.** This ADR covers storage; [0004](0004-integration-boundary.md) covers access and is where the decoupling actually happens.

### Today

Shuriken writes four `shuriken_*` collections into the platform's `sos-academy` MongoDB database, and reads two platform collections (`users`, `projects`) for identity mapping. Privileges are scoped by a custom role
(`migrations/004`): read/write on its own collections with no `remove` action, `find` on the two platform collections, nothing else.

### Three storage options, not two

The discussion defaulted to "same database or its own database". There is a middle option that is materially different from both:


|                                        | Ops cost  | Privilege isolation | `$lookup` across data | Identity mapping      |
| -------------------------------------- | --------- | ------------------- | --------------------- | --------------------- |
| **A. Same database** (today)           | none      | per-collection role | yes                   | one indexed `findOne` |
| **B. Separate database, same cluster** | near zero | per-database role   | **no**                | network hop or copy   |
| **C. Separate cluster**                | real      | total               | **no**                | network hop or copy   |


B is the cheap 80%: a database-scoped role is simpler and tighter than a collection-scoped one, and the operational cost on a shared cluster is close to nothing. But it pays the same architectural costs as C — no cross-database joins, and identity mapping stops being a local query — while giving less isolation. If a split ever happens, B is the first step, not C.

### What a split would actually cost

**Identity mapping is on the hot path.** Every recorded activity resolves a GitHub id to a platform user. In one database that is a single indexed `findOne`. Split, it becomes either a call to the platform API — a network dependency on every webhook event, where a platform outage makes `unlinked` the default — or a copy of user identities inside Shuriken, which is the user data we were trying to stop touching.

This matters more than it looks because **no platform user has a linked GitHub profile yet** (`githubProfile` is only populated by GitHub OAuth login, and the development database has zero). Unlinked is already the common case. Making it fail more often is expensive in exactly the dimension that matters.

**MongoDB has no cross-database** `$lookup`**.** The query Kunai is being built toward — a contributor's recurring feedback concepts joined to their platform identity — is one aggregation today. Split, it is two queries and an application-side join, in a bot that currently has no database access at all.

**Operational cost.** Another database to provision, back up, monitor, rotate credentials for and migrate, on a volunteer-run academy platform.

### What a split would genuinely buy

**Privilege isolation — and this is the real finding.** Shuriken's credential currently has `find` on the whole `users` collection. MongoDB has no field-level permissions, and Mongoose's `select: false` is an application-layer default that a raw driver query ignores. Verified against the development database: a raw `find` on `users` returns the 60-character bcrypt password hash and would return `inviteToken`. A compromised Shuriken today can read every user's email and password hash.

That is worth fixing. It does **not** require splitting the database — a projected read-only view fixes it for the cost of one migration, which is [0003](0003-least-privilege-identity-reads.md).

**Independent schema evolution.** Already effectively have it: the `shuriken_` prefix means nothing else writes those collections, and Shuriken runs its own migrations.

**Failure and scaling isolation.** Negligible at academy scale. Shuriken's write volume is a handful of documents per webhook. Claiming this as a benefit would be cargo-culting.

### What staying genuinely costs, stated honestly

- **Shared backup and restore granularity.** This is the most underrated cost. Restoring the platform database to a snapshot also rolls back every activity Shuriken recorded since. There is no way to restore one and not the other.
- Shared connection and resource limits.
- Shuriken's credential lives inside the platform database's blast radius, and the platform's credentials inside Shuriken's.
- A platform-side collection rename or migration can break Shuriken silently, since nothing type-checks across that boundary.



## Decision

**Keep Shuriken's collections in the platform** `sos-academy` **database.** Do not split storage for v0.

Decouple at the **access** layer instead ([0004](0004-integration-boundary.md)), and close the privilege gap with a projected view ([0003](0003-least-privilege-identity-reads.md)).

If a split later becomes right, go to **B** (separate database, same cluster) before considering C.

## Consequences

- Identity mapping stays a single indexed local query. No new network dependency on the event path.
- Kunai's aggregation stays expressible as one MongoDB aggregation.
- No second database to operate.
- **Shuriken's activity records share the platform's restore granularity.** Accepted knowingly. If point-in-time independence becomes a requirement, that is a revisit trigger, not a workaround.
- The privilege gap is **not** closed by this ADR. It stays open until 0003 is accepted and its migration is run. Until then, Shuriken's credential can read password hashes, and should be treated with the same care as a platform credential.
- Because 0004 keeps consumers off Shuriken's schema, a future move to B is an internal refactor of `internal/store/mongostore` plus a migration, not a cross-team change. The door stays open cheaply.



## Alternatives considered

**Separate database, platform reads it directly.** Pays every cost of a split and keeps the coupling, because the platform would still be writing queries against Shuriken's collection shapes. Strictly worse than either staying put or splitting with a real boundary.

**Separate database, Shuriken pushes to a platform webhook.** This was the shape originally proposed. It is coherent, and it is the right answer for a service with a genuinely independent lifecycle. For v0 it means building at-least-once delivery, retry, dead-letter handling and a platform-side projection, plus accepting eventual consistency and two copies of the same records — to solve a problem (isolation) that a view solves for one migration. Revisit if the triggers below fire.

**Shuriken is stateless; the platform owns all storage.** Shuriken POSTs everything to the platform and keeps nothing. Clean ownership, but it puts the platform API on the critical path of webhook processing and of `@shuriken-bot explain`, and it moves Shuriken's idempotency guarantees — the unique `dedupeKey` index — into someone else's schema. The idempotency design is load-bearing and should stay next to the code that depends on it.

## Revisit when

Any one of these holds:

1. **Shuriken needs to store bulk data** — cached file contents, full diffs,
  model transcripts. Today it deliberately stores identifiers and URLs only. Bulk data would bloat platform backups, and that changes the calculus.
2. **Backup or restore cadence needs to differ** between the two, or
  point-in-time restore of one without the other becomes a requirement.
3. **The platform database becomes a compliance or data-residency boundary**
  that Shuriken's records should not sit inside.
4. **A second service starts writing** the `shuriken_`* collections, at which
  point they are shared infrastructure rather than one service's private data.
5. **Measured contention** — Shuriken's writes show up in platform query
  latency. Measure before believing this one.
6. **Shuriken deploys to a different account, region or provider** than the
  platform, making a shared database a network dependency anyway.

Trigger 1 is the most likely. Triggers 2 and 3 are the ones that would force a move regardless of cost.