# 0003. Read platform identities through a projected view

- **Status:** Proposed — needs a decision, and the migration needs approval
- **Date:** 2026-10-13
- **Related:** [0002](0002-shuriken-data-ownership.md)

## Context

Shuriken maps a GitHub identity to a platform user on every recorded activity.
It needs exactly three fields: `_id`, `githubProfile.githubId` and
`githubProfile.login`.

`migrations/004` grants it `find` on the whole `users` collection.

**MongoDB has no field-level permissions.** Mongoose's `select: false` on
`password` and `inviteToken` is an application-layer default applied by the
Mongoose query builder; Shuriken talks to MongoDB through the Go driver, which
knows nothing about Mongoose schemas and is unaffected by it.

Verified against the development database — a raw `find` on `users` with an
explicit projection returns:

```
_id           6956d5ae...
email         <redacted>
password      <PRESENT, 60 chars>      # bcrypt hash
```

So today a compromised Shuriken, or a bug in it, can read every platform
user's email address and bcrypt password hash, and `inviteToken` where set.
Nothing in the application layer prevents it. This is the one genuinely
serious consequence of Shuriken sharing the platform database, and it is the
part worth acting on.

Note that this is **not** an argument for splitting the database
([0002](0002-shuriken-data-ownership.md)): the exposure comes from the grant
being collection-shaped, and MongoDB can express a narrower grant.

## Decision

Replace Shuriken's `find` grant on `users` with `find` on a **read-only view**
that projects only the three fields it needs.

```js
db.createView('shuriken_user_identities', 'users', [
  { $match: { 'githubProfile.githubId': { $exists: true, $ne: null } } },
  { $project: {
      _id: 1,
      githubId: '$githubProfile.githubId',
      login: '$githubProfile.login',
  } },
]);
```

Then in the role: grant `find` on `shuriken_user_identities`, and **remove**
`find` on `users`. Querying a view requires the privilege on the view, not on
the source collection, which is exactly the isolation wanted.

`projects` is a separate judgement. It holds no secrets — name, description,
`githubRepo`, star counts — so the same treatment is optional there. Proposed:
leave `find` on `projects` as is, and note that a view would be symmetry rather
than security. Open to doing both if the preference is uniformity.

### Prerequisite

**This depends on `migrations/003`.** A view's `$match` can use the source
collection's indexes, but `githubProfile.githubId` has no index in the
development database. Verified:

```
githubProfile.login        -> IXSCAN     (indexed)
githubProfile.githubId     -> COLLSCAN   (not indexed)
```

Without 003, every identity lookup through the view is a full scan of `users`
— the same cost as today, but now on the hot path of every webhook event. Run
003 first, or boot the NestJS server once and let Mongoose `autoIndex` create
it.

## Consequences

- Shuriken's credential can no longer read any password hash, invite token or
  email address. The exposure is closed at the database layer rather than
  relying on the application layer, which never applied here anyway.
- The blast radius of a compromised Shuriken becomes: its own four collections,
  plus three non-sensitive fields of linked users, plus public project
  metadata.
- **A code change is required** in `internal/store/mongostore`. The view
  flattens the field paths, so `FindPlatformUser` queries `githubId` and
  `login` rather than `githubProfile.githubId` and `githubProfile.login`, and
  reads from the view's collection name. This is a small, contained change to
  one function and its document type.
- The view hides users with no `githubProfile`, which is the correct behaviour:
  Shuriken has no business knowing they exist. It also means "no match" and
  "user exists but has not linked GitHub" become indistinguishable to Shuriken
  — which is already how it behaves, since both record the activity as
  `unlinked`.
- Another object for the platform team to know about. A view on `users` is
  invisible in the Mongoose models, so someone refactoring the user schema
  could break Shuriken without any local signal. Mitigate by naming the view
  with the `shuriken_` prefix and mentioning it in the migration's header.
- Views are read-only by definition, which removes any possibility of Shuriken
  writing to `users` even by mistake. That is a bonus, not the motivation.

## Alternatives considered

**Leave it as is.** The scoped role already excludes `sessions`, `posts`,
`communities` and everything else, and Shuriken has no code path that reads
`password`. True, but it relies on Shuriken never having a bug and never being
compromised, when a tighter grant is available for one migration. Defence in
depth is cheap here.

**Split the database so Shuriken cannot reach `users` at all.** Solves it
completely, and costs everything in [0002](0002-shuriken-data-ownership.md) —
identity resolution becomes a network hop or a copy of user data. Disproportionate
when a view closes the same gap.

**Resolve identity through the platform API instead of the database.** Also
closes the gap, and is the right design if storage ever splits. It puts an HTTP
call on the hot path of every webhook event for a benefit a view already
delivers. Reconsider together with [0002](0002-shuriken-data-ownership.md)'s
revisit triggers.

**Maintain a Shuriken-owned copy of the identity mapping, synced on OAuth
login.** No read access to `users` at all. Introduces a sync to get wrong, and
a stale copy silently mislabels activity as unlinked. Rejected: more moving
parts than the problem justifies.

## Revisit when

- Shuriken needs a user field beyond these three. Widen the view deliberately
  rather than reverting to `find` on `users`.
- Storage splits ([0002](0002-shuriken-data-ownership.md)), at which point
  identity resolution moves to an API and the view disappears.
- MongoDB gains field-level privileges, making the view unnecessary.
