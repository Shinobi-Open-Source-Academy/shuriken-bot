# 0004. Expose contributor data through a read API, not shared collections

- **Status:** Superseded by [0005](0005-shuriken-owns-its-database.md)

> **Superseded.** The commitment made here — nothing outside Shuriken
> reads its collections — was kept, and is what made 0005 a contained
> change. The recommendation of a *pull* read API was reversed: under
> multi-tenancy other consumers will not share database credentials, so
> push became the primary direction. The read API may still return later,
> alongside push, for backfill and reconciliation.
- **Date:** 2026-10-13
- **Related:** [0002](0002-shuriken-data-ownership.md)

## Context

[0002](0002-shuriken-data-ownership.md) keeps Shuriken's records in the platform
database. That decision is only cheap to reverse if **nothing outside Shuriken
depends on the shape of the `shuriken_*` collections.** This ADR is where that
property is either preserved or lost.

Kunai is the first consumer. It sends daily messages and will eventually want
"tips based on a contributor's recurring feedback", which means reading
`shuriken_review_findings` grouped by `concept`. Kunai integration is
explicitly out of v0 scope, so nothing consumes this data yet — which is
exactly why the boundary is free to set now and expensive to set later.

One fact shapes the answer: **Kunai has no database access at all.** Its
dependencies are `axios`, `discord.js`, `node-cron` and `dotenv` — no
`mongoose`, no MongoDB driver. It is already an HTTP client. Giving it database
credentials would be adding a capability it does not currently have, in order
to avoid building an endpoint.

### The three options

**A. Consumers query `shuriken_*` directly.**
Cheapest to start. Every consumer becomes coupled to Shuriken's document
shapes, field names and index assumptions. Renaming a field becomes a
cross-service change. A storage split becomes a migration for every consumer.
It also means handing Kunai database credentials it does not have today.

**B. Shuriken exposes a read API.**
Consumers call HTTP endpoints. Shuriken's schema stays private, so it can be
changed freely behind a versioned response shape. A future storage split
becomes an internal refactor of `internal/store/mongostore` with no consumer
change. Costs an HTTP surface, authentication between services, and a second
reason for Shuriken to be available.

**C. Shuriken pushes to a platform webhook.**
The platform stores its own projection and queries it locally. Consumers never
touch Shuriken at all, and Shuriken being down does not block reads. Costs
at-least-once delivery, retry, dead-letter handling, a platform-side schema
and projection, two copies of the same records, and eventual consistency to
reason about.

### Weighing them

B fits what is actually needed. Kunai's query — "which concepts recur for this
contributor" — is a read of aggregated data on a daily schedule. It is not
latency-critical, it does not need transactional consistency with platform
writes, and Kunai is already built to make HTTP calls.

C is the right shape when the producer's availability must not affect the
consumer, or when the consumer needs to join the data locally at scale.
Neither applies to a once-a-day Discord message. The delivery machinery would
be real work to solve a problem nobody has.

A is the one to actively avoid, because its cost is not paid up front. It is
paid later, by whoever tries to change a field name or move the database, and
by then there are consumers.

## Decision

**Two parts, with different timelines.**

### Now — a binding commitment, no code

Nothing outside Shuriken reads the `shuriken_*` collections directly. Not the
platform server, not Kunai, not a script. Shuriken's database role is the only
credential with access to them, which `migrations/004` already enforces: the
role is granted to Shuriken's user alone, and no platform component has it.

This costs nothing today precisely because nothing consumes the data yet. It is
the whole reason [0002](0002-shuriken-data-ownership.md) stays reversible.

### Later — when Kunai integration starts

Shuriken grows a small read API, authenticated service-to-service, shaped
around the questions consumers actually ask rather than around its tables:

```
GET /contributors/{githubId}/summary
    counts by activity type, first and last seen

GET /contributors/{githubId}/concepts?since=
    recurring review concepts with counts and last-seen —
    the query Kunai needs

GET /contributors/{githubId}/activity?since=&type=
    paginated raw activity
```

Keyed by GitHub id, not platform user id, because that is the identity Shuriken
always has — an unlinked contributor has no platform id, and those are the
majority today.

The endpoints are deliberately phrased as questions, not as table dumps. A
`GET /activities?filter=` that mirrors the collection would be option A wearing
an HTTP costume: consumers would still be coupled to the schema, just over the
wire.

## Consequences

- A storage split later ([0002](0002-shuriken-data-ownership.md) revisit
  triggers) touches `internal/store/mongostore` and nothing else. That is the
  main thing bought here.
- Shuriken's collection shapes stay private and can change without
  coordination. The `explanation` and `suggestedFix` retention question, for
  instance, becomes Shuriken's own call.
- Kunai keeps its current dependency set and gains no database credentials.
- **Shuriken becomes a dependency of Kunai's daily job.** If Shuriken is down,
  tips degrade. Acceptable for a daily message, and the endpoint is a read
  against an indexed collection. Kunai should treat an error as "no tip today"
  rather than failing its run.
- Aggregation logic lives in Shuriken rather than being reimplemented per
  consumer. One definition of "recurring concept".
- The API is additional surface to secure and version. It is read-only and
  exposes no secrets, but it does expose per-contributor history and needs
  service authentication, not public access.
- Until the API exists, **the data is write-only in practice.** Accepted: v0's
  job is to start accumulating records correctly, and nothing is waiting to
  read them.

## Alternatives considered

**Direct collection reads (A).** Covered above. The cost is deferred and lands
on whoever changes a field name.

**Push webhook to the platform (C).** The shape originally proposed alongside a
database split. Correct for an independent service lifecycle; disproportionate
for a daily Discord message, and it duplicates the records. Revisit if Kunai
ever needs to join Shuriken data locally at volume, or if Shuriken's
availability becomes unacceptable as a read dependency.

**A platform-side read model Shuriken writes to directly.** Shuriken writes a
denormalised summary into a platform-owned collection that Kunai reads. Avoids
an HTTP surface, but it means Shuriken writing to a platform collection, which
contradicts the find-only posture in
[0003](0003-least-privilege-identity-reads.md) and makes schema ownership
ambiguous in the other direction.

**GraphQL or a shared client library instead of REST.** Over-engineered for
three endpoints with one consumer. Reconsider if consumers multiply.

## Revisit when

- Kunai integration work starts — that is when the **Later** half gets built,
  and the endpoint list should be re-derived from the questions Kunai actually
  asks rather than from this sketch.
- A consumer appears that needs local joins against Shuriken data at volume,
  which is the case for option C.
- More than two or three consumers exist, at which point versioning and a
  shared client become worth their cost.
