# Consuming Shuriken's events

Shuriken records contributor activity and teaching-review findings, then
**pushes them to you** as signed HTTP events. There is no shared database and
no API to poll: see [ADR 0005](adr/0005-shuriken-owns-its-database.md) for why.

This document is the contract. It is what a consumer needs and nothing more.

---

## 1. The endpoint you provide

One route, which must:

- accept `POST` with a JSON body,
- verify `X-Shuriken-Signature-256` **before parsing the body**,
- be idempotent on `idempotencyKey`,
- answer `2xx` quickly and do the real work asynchronously.

Shuriken is configured with its URL and a shared secret:

```bash
SHURIKEN_CONSUMER_WEBHOOK_URL=https://your-host/api/shuriken/events
SHURIKEN_CONSUMER_WEBHOOK_SECRET=<the same long random value you hold>
```

Generate the secret with `openssl rand -hex 32`.

## 2. Verifying the signature

HMAC-SHA256 over the **exact raw request body**, hex encoded, prefixed
`sha256=`. This is the same scheme GitHub uses, so if you already verify GitHub
webhooks you can reuse that code.

```ts
import { createHmac, timingSafeEqual } from 'node:crypto';

export function verifyShurikenSignature(rawBody: Buffer, header: string | undefined, secret: string): boolean {
  if (!header?.startsWith('sha256=')) return false;

  const expected = 'sha256=' + createHmac('sha256', secret).update(rawBody).digest('hex');

  // Both are fixed length, so a length mismatch means the header is malformed.
  const a = Buffer.from(header);
  const b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}
```

**The raw body matters.** If your framework parses JSON before you see it, the
bytes you re-serialise will not match what was signed and every request will
fail. In NestJS, capture it with the `rawBody` option or a body-parser `verify`
hook on that route only.

Reject with **401** when verification fails, and do not say why.

## 3. Headers

| Header | Meaning |
|---|---|
| `X-Shuriken-Signature-256` | `sha256=<hex>` over the raw body |
| `X-Shuriken-Event` | `activity.recorded` or `review.finding_posted` |
| `X-Shuriken-Delivery` | unique per event, stable across retries of that event |
| `X-Shuriken-Idempotency-Key` | what you dedupe on; also in the body |

## 4. The envelope

```json
{
  "id": "6956d5ae37027883a6ccb3ab",
  "type": "activity.recorded",
  "idempotencyKey": "pr_merged:6001",
  "occurredAt": "2026-03-01T12:00:00Z",
  "sentAt": "2026-03-01T12:00:04Z",
  "attempt": 1,
  "data": { }
}
```

`occurredAt` is when the thing happened on GitHub. `sentAt` and `attempt`
describe this delivery attempt — `attempt > 1` means you have seen this event
before, or Shuriken could not confirm that you had.

### `activity.recorded`

```json
{
  "activityType": "pr_merged",
  "actor":   { "githubId": 101, "login": "alice" },
  "repo":    { "owner": "sos-academy", "name": "dojo",
               "fullName": "sos-academy/dojo", "githubId": 7001 },
  "subject": { "kind": "pull_request", "number": 2,
               "githubId": 6001,
               "url": "https://github.com/sos-academy/dojo/pull/2" },
  "object":  { "githubId": 8888, "url": "https://github.com/..." },
  "occurredAt": "2026-03-01T12:00:00Z",
  "metadata": { "additions": 42, "deletions": 7, "mergedByLogin": "maintainer" }
}
```

`activityType` is one of:

| Value | Credited to |
|---|---|
| `issue_opened` | the opener |
| `pr_opened` | the author |
| `pr_merged` | the **author**, not whoever merged |
| `pr_closed_unmerged` | the author |
| `pr_review_submitted` | the reviewer (never a self-review) |
| `issue_assigned` | the **assignee** who claimed it |
| `issue_comment` | the commenter (only when it mentions the bot) |
| `pr_comment` | the commenter (only when it mentions the bot) |

`object` is present only when the event refers to something other than the
subject, such as a comment or a review. `metadata` is a small, open map: treat
unknown keys as additive and do not validate strictly.

### `review.finding_posted`

One event per review comment actually posted. Dropped findings are never sent,
because a contributor never saw them.

```json
{
  "actor":       { "githubId": 101, "login": "alice" },
  "repo":        { "owner": "sos-academy", "name": "dojo",
                   "fullName": "sos-academy/dojo", "githubId": 7001 },
  "pullRequest": { "number": 2, "githubId": 6001,
                   "url": "https://github.com/sos-academy/dojo/pull/2",
                   "headSha": "deadbeef" },
  "concept":     "error handling",
  "severity":    "high",
  "confidence":  0.95,
  "path":        "internal/svc.go",
  "line":        42,
  "title":       "Unchecked error from the database call",
  "reviewUrl":   "https://github.com/sos-academy/dojo/pull/2#pullrequestreview-555",
  "postedAt":    "2026-03-01T12:05:00Z"
}
```

**`concept` is the field to build on.** It is a short lowercase phrase naming
the underlying idea — `error handling`, `input validation`, `race condition`,
`off-by-one` — and Shuriken is prompted to reuse the same wording for the same
idea. Grouping by `concept` per contributor is what makes "this person keeps
getting told the same thing" answerable, which is the point of recording it.

The finding's explanation and any withheld fix are **not** sent. They stay with
Shuriken, which serves them when a contributor asks `@shuriken-bot explain`.

## 5. Identity: you do the join

**No event carries a platform user id.** Shuriken stores GitHub identity only
and does not know whether a GitHub account belongs to one of your members.
That is what lets one deployment serve several projects.

Join on the GitHub id, which is immutable, rather than the login, which is not:

```ts
const user = await this.userModel
  .findOne({ 'githubProfile.githubId': event.data.actor.githubId })
  .select('_id')
  .lean();
```

A miss is normal and must not be an error. Nobody has linked a GitHub account
until they sign in with GitHub, so **unmatched is the common case early on.**
Store the event with its GitHub identity regardless and resolve later; a
contributor who links their account months from now should get their history
attributed retroactively, which works precisely because you kept the
`githubId`.

> The `users.githubProfile.githubId` index is declared in
> `user.schema.ts` but was absent from at least one environment. Confirm it
> exists before this join runs on every event, or it is a collection scan.

## 6. Idempotency is required, not optional

Delivery is **at-least-once**. Duplicates will arrive: on retry after a
timeout, or when Shuriken delivered successfully but could not record that it
had. Dedupe on `idempotencyKey` with a unique index, and treat a duplicate as
success — answer `2xx`, because a `4xx` will dead-letter the event.

## 7. Status codes, and what each one makes Shuriken do

| You return | Shuriken does |
|---|---|
| `2xx` | marks delivered, moves on |
| `429` | retries with backoff |
| `5xx` | retries with backoff |
| any other `4xx` | **dead-letters immediately** — no retry |

That last row is the one to get right. A `4xx` means "this event is
unacceptable and resending it will not help", so a validation bug that returns
`400` will silently discard events. **If you are unsure, return `5xx`** and let
it retry.

Retries are exponential from 15s to a 30-minute ceiling, up to 8 attempts by
default, then dead-lettered. Dead-lettered events are kept and counted on
Shuriken's `/healthz`:

```json
{"status":"ok","queueDepth":0,"outboxPending":0,"outboxDeadLettered":0,"dryRun":false}
```

**Alert on `outboxDeadLettered > 0`.** It means something happened that you
will never otherwise learn about.

## 8. A worked endpoint shape

```
POST /api/shuriken/events
  ├─ read the RAW body
  ├─ verify X-Shuriken-Signature-256 ─────── 401, no detail
  ├─ parse
  ├─ upsert on idempotencyKey ─────────────── 200 on a duplicate
  ├─ enqueue projection work (do not block)
  └─ 202 Accepted
```

Resolve the platform user in the asynchronous step, not inline: a slow lookup
turns into a Shuriken retry and a duplicate.

## 9. Testing it without Shuriken

```bash
SECRET='your-consumer-webhook-secret'
BODY='{"id":"test-1","type":"activity.recorded","idempotencyKey":"pr_merged:6001","occurredAt":"2026-03-01T12:00:00Z","sentAt":"2026-03-01T12:00:04Z","attempt":1,"data":{"activityType":"pr_merged","actor":{"githubId":101,"login":"alice"},"repo":{"owner":"sos-academy","name":"dojo","fullName":"sos-academy/dojo","githubId":7001},"subject":{"kind":"pull_request","number":2,"githubId":6001,"url":"https://github.com/sos-academy/dojo/pull/2"},"occurredAt":"2026-03-01T12:00:00Z"}}'

# $NF, not $2: some OpenSSL builds print a "SHA2-256(stdin)= " prefix and some
# print the bare hex. Taking the last field works for both.
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')"

curl -sS -X POST http://localhost:4200/api/shuriken/events \
  -H 'Content-Type: application/json' \
  -H "X-Shuriken-Signature-256: $SIG" \
  -H 'X-Shuriken-Event: activity.recorded' \
  -H 'X-Shuriken-Delivery: test-1' \
  -H 'X-Shuriken-Idempotency-Key: pr_merged:6001' \
  --data "$BODY"
```

Send it twice with the same `idempotencyKey`: both must return `2xx` and only
one record should exist. Then corrupt one byte of `$BODY` without
re-signing — that must return `401`.

On the Shuriken side, `SHURIKEN_DRY_RUN=1` logs the exact payload it would
send without sending it, which is the cheapest way to see a real event shape.

## 10. Checklist

- [ ] Raw body captured before JSON parsing, on this route
- [ ] Signature verified in constant time, `401` on failure
- [ ] Unique index on `idempotencyKey`
- [ ] Duplicates answer `2xx`
- [ ] Unknown `activityType` and unknown `metadata` keys tolerated
- [ ] `5xx` on internal errors, never `4xx`
- [ ] Platform user resolved asynchronously, miss is not an error
- [ ] `githubId` retained even when unmatched, for retroactive attribution
- [ ] `users.githubProfile.githubId` index confirmed to exist
- [ ] Alert on Shuriken's `outboxDeadLettered`
