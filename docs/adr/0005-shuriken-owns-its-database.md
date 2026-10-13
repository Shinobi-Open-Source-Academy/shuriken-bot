# 0005. Shuriken owns its database; integration is a signed outbound webhook

- **Status:** Accepted
- **Date:** 2026-10-13
- **Supersedes:** [0002](0002-shuriken-data-ownership.md),
  [0003](0003-least-privilege-identity-reads.md),
  [0004](0004-integration-boundary.md)

## Context

[0002](0002-shuriken-data-ownership.md) decided to keep Shuriken's collections
in the academy platform's database. That decision was correct **for the premise
it was given**: one bot, one academy, one database, Kunai integration deferred.

The premise changed. Shuriken is now intended as a tool other open source
projects can use, with multi-tenancy ahead of it. That does not fire one of
0002's revisit triggers — it invalidates the thing 0002 was reasoning about.
SOS Academy stops being *the* platform and becomes *a* consumer. A service
meant to serve several projects cannot keep its records inside one of them, and
cannot hold a database credential for one of them either.

Two sub-decisions followed, and both were settled deliberately rather than
defaulting.

### Identity

The expensive part of splitting, per 0002, was that identity mapping stops
being a local indexed query. Three options:

1. Call the consumer's API to resolve a GitHub id to an account on every event.
2. Keep a synced copy of consumer identities inside Shuriken.
3. Store the GitHub identity only, and let the consumer join at read time.

(3) is the only one that generalises. Under multi-tenancy (1) means one
outbound call per tenant per event with the consumer on the hot path, and (2)
means holding a copy of every tenant's user table — the exact data a separate
service should not have.

(3) also dissolves [0003](0003-least-privilege-identity-reads.md) rather than
implementing it. That ADR proposed a projected view because Shuriken's `find`
grant on `users` exposed bcrypt password hashes, MongoDB having no field-level
privileges. If Shuriken never reads `users` at all, there is no exposure to
mitigate and no view to maintain. **The architecture change fixes the security
finding more completely than the mitigation did.**

The cost is real and accepted: Shuriken cannot answer "which platform member
is this?" and does not try. It records `{githubId, login}`; the consumer joins
`actor.githubId` against `users.githubProfile.githubId` when it wants to. The
`unlinked` flag is gone, because the question it answered is no longer
Shuriken's to ask.

### Integration direction

[0004](0004-integration-boundary.md) recommended a read API, reasoning that
Kunai is already an HTTP client and a daily job needs no delivery
infrastructure. Under multi-tenancy the direction reverses: **other tenants
will not hand over database credentials, and Shuriken should not want them.**
A read API also makes Shuriken's availability a dependency of every consumer's
reads.

Push was chosen: Shuriken POSTs signed events to a consumer-configured
endpoint, and the consumer owns whatever projection it wants. No database
access in either direction.

This makes delivery reliability load-bearing in a way it was not before. When a
read API was the plan, a failed read was retried by the caller. With push as
the *only* path, **an event dropped in flight is lost permanently.** So
delivery is not best-effort:

- Events are written to a `shuriken_outbox` collection in the same operation
  that records the underlying activity — the transactional outbox pattern. A
  crash between recording and sending costs a retry, not an event.
- The publisher leases rows before sending, so concurrent publishers cannot
  double-send and a dead publisher's lease lapses rather than stranding work.
- Delivery is at-least-once with exponential backoff to a 30-minute ceiling.
  Every event carries an idempotency key and consumers must dedupe on it.
- A `5xx` or `429` retries. Any other `4xx` means the consumer rejected the
  event itself, so retrying an unchanged body cannot help and it is
  dead-lettered immediately.
- Dead-lettered events are kept, not deleted. They are the only record that a
  consumer never learned something, and `/healthz` reports the count.

## Decision

1. **Shuriken owns its own MongoDB database** (`shuriken` by default). It holds
   no credential for any consumer's database and no reference to any consumer's
   records.
2. **Contributors are identified by GitHub id and login only.** No platform
   user id, no `unlinked` flag, no identity resolution.
3. **Integration is a signed outbound webhook.** HMAC-SHA256 over the exact
   request body, headers mirroring GitHub's scheme so a consumer can reuse
   verification code it already has. At-least-once, from a durable outbox.
4. **No shared database access in either direction.**
5. A deployment with no consumer configured is valid. Events accumulate in the
   outbox and `/healthz` reports the depth.

## Consequences

- Shuriken is deployable for a project that has nothing to do with SOS Academy.
  That is the point.
- The password-hash exposure is gone outright, not mitigated. Shuriken's
  credential reaches one database containing only its own collections.
- The database role simplifies from collection-level carve-outs across a shared
  database to per-collection grants within its own, with no `drop` actions.
- Consumers hold the projection they want, in their own schema, and are free to
  reshape it without touching Shuriken.
- **Delivery is eventually consistent.** A consumer's view lags by up to one
  publish interval, and longer while a consumer is down. Accepted: nothing
  here is latency-critical.
- **Consumers must be idempotent.** At-least-once means a duplicate will
  arrive eventually. The idempotency key makes that cheap, but it is a real
  requirement on the consumer, documented in
  [`docs/platform-integration.md`](../platform-integration.md).
- **Two copies of the data exist**, Shuriken's records and each consumer's
  projection, and they can diverge if a consumer mishandles an event. The
  dead-letter count is the signal.
- More moving parts than a shared database: an outbox, a publisher, a lease, a
  backoff, a dead-letter queue. This is the cost of the boundary, and it is
  only justified because push is now the sole path.
- Shuriken can no longer answer questions about consumer accounts, by
  construction.

## Alternatives considered

**Keep the shared database and add the projected view ([0003](0003-least-privilege-identity-reads.md)).**
What this project was about to do. Correct under the old premise, and it closes
the password-hash gap for one migration. It cannot survive multi-tenancy: the
view is a platform-side object, so every tenant would have to create one, and
Shuriken would need a credential per tenant database.

**Separate database, consumer reads it directly.** Rejected in 0002 and still
rejected: it pays the cost of a split and keeps the coupling, and it does not
work for a tenant unwilling to share credentials.

**Separate cluster rather than a separate database.** This is a deployment
choice, not a code one — the connection string is the only difference. Start
with a separate database; move to a separate cluster when isolation or
locality demands it.

**Read API instead of push ([0004](0004-integration-boundary.md)).** Still the
better fit for a single consumer that is already an HTTP client, and it avoids
the delivery machinery entirely. Rejected because it makes Shuriken's
availability a dependency of every consumer's reads, and because a pull model
scales badly across tenants who each want their own cadence. A read API may
still be added later *alongside* push, for backfill and reconciliation; that
would be additive and would not change this decision.

**Both push and read-only database access.** Considered and declined: the
database grant is the part that does not generalise, so keeping it would
preserve the coupling this ADR exists to remove.

## Revisit when

- A consumer needs to backfill or reconcile after a long outage, which the
  dead-letter queue makes visible but does not solve. That is the argument for
  adding a read API alongside push, not for replacing it.
- Multi-tenancy work begins in earnest. This ADR makes the data model
  tenant-ready but stops short of tenant isolation: there is no tenant
  identifier on any record, and one deployment currently serves one consumer.
  That is deliberately out of scope here and needs its own ADR.
- Delivery volume outgrows a single polling publisher, at which point the
  outbox is the right seam for a queue.
