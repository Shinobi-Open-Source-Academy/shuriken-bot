# Shuriken

A GitHub App for **SOS Academy** (Shinobi Open Source Academy) that does two
things:

1. **Records contributor activity** — issues, pull requests, reviews and
   mentions — into the academy platform's database, so each contributor's
   progress can be tracked.
2. **Reviews pull requests with Claude, teaching-first**, when asked. It is a
   mentor, not a linter: it explains the reasoning, names the underlying
   concept, and withholds the patch for conceptual problems so the contributor
   works it out themselves.

Shuriken is separate from **Kunai**, the existing Discord bot. It does not call
Kunai and Kunai does not call it. The data is stored so Kunai can read it later
(see [Data model](#data-model)).

> **v0 scope.** No backfill of history, no ranks or belts, no Kunai
> integration, no learning from past maintainer reviews, no duplicate-PR
> detection, no dashboard.

---

## Contents

- [How it behaves](#how-it-behaves)
- [Quick start](#quick-start)
- [GitHub App setup](#github-app-setup)
- [Database setup](#database-setup)
- [Configuration](#configuration)
- [Data model](#data-model)
- [Security model](#security-model)
- [Tuning review quality](#tuning-review-quality)
- [Architecture](#architecture)
- [Architecture decisions](#architecture-decisions)
- [Development](#development)
- [Operations](#operations)

---

## How it behaves

### Activity tracking (automatic)

For every repository the App is installed on, Shuriken records:

| Activity | Trigger | Who it credits |
|---|---|---|
| `issue_opened` | an issue is opened | the opener |
| `pr_opened` | a pull request is opened | the author |
| `pr_merged` | a pull request is merged | the **author**, not the merger |
| `pr_closed_unmerged` | a pull request is closed unmerged | the author |
| `pr_review_submitted` | someone reviews **another person's** PR | the reviewer |
| `issue_assigned` | an assignee is added | the **assignee** who claimed it |
| `issue_comment` | a comment mentioning `@shuriken-bot` on an issue | the commenter |
| `pr_comment` | a comment mentioning `@shuriken-bot` on a PR | the commenter |

Comments are recorded **only** when they mention the bot. Self-reviews are not
recorded. Events from bots, including Shuriken's own, are ignored.

A bare mention (`@shuriken-bot` with no command) is acknowledged with an 👀
reaction rather than a comment, so threads do not fill with bot chatter.

### Commands

| Command | What it does |
|---|---|
| `@shuriken-bot review` | a full teaching review of the pull request |
| `@shuriken-bot hint` | names what to look for, without the answer |
| `@shuriken-bot explain` | the full reasoning and the fixes withheld from the last review |
| anything else | a three-line help message |

A review also runs when the **`needs-review` label** is added. It never runs on
a push.

Mentions inside fenced code blocks, inline code spans, or blockquotes are
ignored — people paste snippets into comments constantly.

### Who may trigger a review

- The **pull request author** can always ask about their own PR.
- Anyone else needs **write access**, determined from the `author_association`
  that GitHub already includes in the webhook payload (`OWNER`, `MEMBER` or
  `COLLABORATOR`).

Using `author_association` avoids requesting the `Administration` permission
that the collaborator-permission endpoint would need. It is slightly coarser
than a real permission check; see [Security model](#security-model).

Shuriken skips **drafts** and **bot-authored** pull requests, and tells the
requester why rather than going quiet.

### Teaching style

The register is chosen from the PR author's `author_association`:

| Association | Register |
|---|---|
| `FIRST_TIME_CONTRIBUTOR`, `FIRST_TIMER` | Warm. More context, explains the why at length, names the concept in prose. Opens by acknowledging something genuinely good — only if there is something genuinely good. |
| `CONTRIBUTOR`, `COLLABORATOR`, `NONE` | Balanced. Explains the reasoning without belabouring it. |
| `MEMBER`, `OWNER` | Terse. Bugs only, one or two sentences each, no teaching. |

A fix is shown as a GitHub `suggestion` block **only** when the model marks it
mechanical and low-risk. For a conceptual problem the comment leads with the
reasoning, says a fix is being held back, and the contributor can ask
`@shuriken-bot explain` to see it. That reveal is served from the database — no
second model call, and it always matches what was originally said.

Shuriken submits **exactly one review** per trigger, with event `COMMENT`. It
can never approve a pull request or request changes.

---

## Quick start

Nothing is written to GitHub or the database in dry-run mode, so this is safe
to run against real webhook payloads.

```bash
git clone <this repo> && cd shuriken-bot
cp .env.example .env

# The webhook secret is the only thing dry run requires.
echo 'SHURIKEN_GITHUB_WEBHOOK_SECRET=any-value-for-local-testing' >> .env
echo 'SHURIKEN_DRY_RUN=1' >> .env

set -a && . ./.env && set +a
go run ./cmd/shuriken
```

Then replay a delivery. The signature must be computed over the **exact** bytes
you send:

```bash
BODY='{"action":"opened","pull_request":{"id":6001,"number":2,"user":{"id":101,"login":"alice","type":"User"},"html_url":"https://github.com/sos-academy/dojo/pull/2","created_at":"2026-03-01T12:00:00Z"},"repository":{"id":7001,"name":"dojo","full_name":"sos-academy/dojo","owner":{"login":"sos-academy"}},"installation":{"id":4242}}'
# $NF, not $2: some OpenSSL builds print a "SHA2-256(stdin)= " prefix and
# some print the bare hex. Taking the last field works for both.
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SHURIKEN_GITHUB_WEBHOOK_SECRET" | awk '{print $NF}')"

curl -sS -X POST http://localhost:8080/webhook \
  -H 'Content-Type: application/json' \
  -H 'X-GitHub-Event: pull_request' \
  -H 'X-GitHub-Delivery: local-1' \
  -H "X-Hub-Signature-256: $SIG" \
  --data "$BODY"
```

You should see `DRY RUN would record activity` in the logs. Change the
`X-GitHub-Delivery` value to see it processed again; keep it the same to see
the redelivery dropped.

---

## GitHub App setup

### 1. Create the App

**Settings → Developer settings → GitHub Apps → New GitHub App**

- **Webhook URL**: `https://<your-host>/webhook`
- **Webhook secret**: generate a long random value, e.g.
  `openssl rand -hex 32`. You need this in both places.

### 2. Permissions — exactly these, nothing more

| Permission | Access | Why |
|---|---|---|
| **Metadata** | Read-only | mandatory for all Apps |
| **Contents** | Read-only | the diff, `CONTRIBUTING.md`, `.github/shuriken.md`, and file contents at the head SHA for the `read_file` tool |
| **Issues** | **Read and write** | read issue events; write acknowledgement reactions and help replies |
| **Pull requests** | **Read and write** | read PR metadata and files; submit the `COMMENT` review and its inline comments |

Do **not** grant Administration, Members, Actions, Checks, Workflows, or
Contents write. Shuriken has no code path that merges, pushes, approves,
requests changes, or runs anything.

### 3. Events — subscribe to exactly these five

- `Issues`
- `Pull request`
- `Pull request review`
- `Issue comment`
- `Pull request review comment`

Nothing else is handled, and `push` is deliberately not subscribed.

### 4. Private key and installation

1. **Generate a private key** and download the `.pem`.
2. Note the **App ID**.
3. **Install the App** on the organisation, choosing the repositories.
4. Set `SHURIKEN_GITHUB_APP_ID`, `SHURIKEN_GITHUB_PRIVATE_KEY_FILE` (or
   `SHURIKEN_GITHUB_PRIVATE_KEY`) and `SHURIKEN_GITHUB_WEBHOOK_SECRET`.

Installation access tokens are minted and refreshed automatically per
installation; there is no long-lived token to rotate beyond the private key.

### 5. The bot handle

`SHURIKEN_BOT_HANDLE` must be the App's **slug**, without `@`. The real login
GitHub assigns is that slug plus `[bot]`, and both forms are recognised, so
`@shuriken-bot` and `@shuriken-bot[bot]` both work.

---

## Database setup

Shuriken uses the academy platform's **MongoDB** database. It owns four new
`shuriken_*` collections and reads two existing ones.

> **Nothing auto-migrates.** The service performs no DDL at startup. Review
> each migration before you run it.

```bash
# 1. Collections (safe, idempotent)
mongosh "$SHURIKEN_MONGODB_URI" --file migrations/001_shuriken_collections.js

# 2. Indexes (safe, idempotent)
mongosh "$SHURIKEN_MONGODB_URI" --file migrations/002_shuriken_indexes.js

# 3. OPTIONAL — touches the platform `users` collection. Read it first.
mongosh "$ADMIN_MONGODB_URI" --file migrations/003_users_githubid_index.js

# 4. Scoped role and database user. Set the password first.
SHURIKEN_DB_PASSWORD='...' mongosh "$ADMIN_MONGODB_URI" \
  --file migrations/004_shuriken_role_and_user.js
```

### About migration 003

`apps/server/src/modules/user/schemas/user.schema.ts` declares a unique sparse
index on `githubProfile.githubId`, but **it was missing from the development
database** when Shuriken was built — only the `githubProfile.login` index
existed. Shuriken resolves a GitHub identity to a platform user on
`githubProfile.githubId` for every recorded activity, so without it each lookup
is a collection scan.

Two ways to fix it, pick either:

- Run `003`, which first reports any duplicate github ids and refuses to create
  the index if there are any.
- **Or** boot the NestJS server once. Mongoose `autoIndex` is not disabled, so
  it will create the index itself. If you prefer the application to own its own
  schema, do this and skip `003`.

For local development only, `SHURIKEN_ENSURE_INDEXES=1` makes Shuriken create
**its own** indexes at boot. It never creates the `users` index — its database
role is find-only there.

---

## Configuration

Everything comes from the environment. See [`.env.example`](.env.example).

### Required

| Variable | Notes |
|---|---|
| `SHURIKEN_GITHUB_WEBHOOK_SECRET` | **Always required**, including in dry run — without it nothing can be authenticated. |
| `SHURIKEN_GITHUB_APP_ID` | Not required when `SHURIKEN_DRY_RUN=1`. |
| `SHURIKEN_GITHUB_PRIVATE_KEY` / `_FILE` | Not required when `SHURIKEN_DRY_RUN=1`. |
| `ANTHROPIC_API_KEY` | Not required when `SHURIKEN_DRY_RUN=1`. |
| `SHURIKEN_MONGODB_URI` | Not required when `SHURIKEN_DRY_RUN=1`. |

Startup validation reports **every** missing or invalid value at once, so one
run tells you everything that is wrong.

### Everything else

| Variable | Default | Purpose |
|---|---|---|
| `SHURIKEN_ADDR` | `:8080` | listen address |
| `SHURIKEN_LOG_LEVEL` | `info` | `debug` logs every dropped finding and why |
| `SHURIKEN_BOT_HANDLE` | `shuriken-bot` | mention handle, without `@` |
| `SHURIKEN_REVIEW_LABEL` | `needs-review` | label that triggers a review |
| `SHURIKEN_ANTHROPIC_MODEL` | `claude-opus-5-5` | never hardcoded |
| `SHURIKEN_EFFORT` | `high` | `low`–`max` |
| `SHURIKEN_MAX_OUTPUT_TOKENS` | `8000` | per review |
| `SHURIKEN_MAX_TOOL_CALLS` | `8` | hard cap on `read_file` calls |
| `SHURIKEN_MAX_DIFF_BYTES` | `200000` | size cap on the reviewed diff |
| `SHURIKEN_MAX_INLINE_COMMENTS` | `10` | highest severity first |
| `SHURIKEN_MIN_CONFIDENCE` | `0.6` | findings below this are dropped |
| `SHURIKEN_MIN_SEVERITY` | `low` | `low`, `medium` or `high` |
| `SHURIKEN_EXCLUDE_GLOBS` | built-in list | comma-separated; replaces the default |
| `SHURIKEN_PRICE_*_PER_MTOK` | Opus 5.5 rates | for cost logging only |
| `SHURIKEN_MONGODB_DATABASE` | `sos-academy` | |
| `SHURIKEN_DELIVERY_TTL_DAYS` | `30` | delivery-ledger retention |
| `SHURIKEN_ENSURE_INDEXES` | `0` | local development only |
| `SHURIKEN_WORKERS` | `4` | worker goroutines |
| `SHURIKEN_QUEUE_SIZE` | `256` | bounded queue depth |
| `SHURIKEN_JOB_TIMEOUT` | `5m` | per-job `context` deadline |
| `SHURIKEN_SHUTDOWN_TIMEOUT` | `20s` | grace period for in-flight jobs |
| `SHURIKEN_DRY_RUN` | `0` | `1` writes nothing anywhere |

---

## Data model

Four new collections in the **same** `sos-academy` database. Same database
because MongoDB cannot join across databases and Kunai will want to correlate
with `users`; the `shuriken_` prefix makes ownership obvious and lets the
database role be scoped per collection.

### What already existed, and is **not** duplicated

| Existing field | Shuriken's relationship to it |
|---|---|
| `users.githubProfile.{githubId,login}` | **read only** — the identity mapping |
| `projects.githubRepo` | **read only** — a full URL, normalised to `owner/name` |
| `users.experiencePoints` | **never written** (ranks are out of v0 scope) |
| `communitymemberships.contributionScore` | **never written** |

There is no link table. Mapping is a read-only query, and an `unlinked` flag
lets a later job attach activity to an account.

### `shuriken_activities`

One normalized row per contributor action. No comment bodies, no diffs —
identifiers and URLs only.

```js
{
  dedupeKey, type, occurredAt,
  actor:   { githubId, login },   // always present
  user:    ObjectId | null,       // → users
  unlinked: Boolean,              // true when no platform account matched
  repo:    { owner, name, fullName, githubId },
  project: ObjectId | null,       // → projects
  subject: { kind, number, githubId, url },
  object:  { githubId, url },     // the comment or review, when distinct
  metadata, deliveryId, createdAt, updatedAt
}
```

### Idempotency is two-layered

GitHub retries, and the same logical event can arrive under a different
delivery id.

1. **`shuriken_deliveries`** — `_id` is the `X-GitHub-Delivery` value, TTL
   expired. The cheap short-circuit for literal redeliveries.
2. **`dedupeKey`, unique index** — the real guarantee, a deterministic natural
   key that survives the TTL:

| Activity | `dedupeKey` |
|---|---|
| `issue_opened` | `issue_opened:{issue.id}` |
| `pr_opened` / `pr_merged` / `pr_closed_unmerged` | `{type}:{pr.id}` |
| `pr_review_submitted` | `pr_review_submitted:{review.id}` |
| `issue_comment` / `pr_comment` | `{type}:{comment.id}` |
| `issue_assigned` | `issue_assigned:{issue.id}:{assignee.id}` |

If a delivery fails after the ledger write, the `dedupeKey` index still makes
GitHub's retry safe.

### `shuriken_review_findings` — the Kunai feed

One document per **posted** finding. `concept` and `severity` are the fields to
aggregate over.

```js
{
  actor: { githubId, login },     // the PR author being taught
  user: ObjectId | null, unlinked,
  repo, pr: { number, githubId, url, headSha },
  reviewId, reviewUrl, commentId,
  concept,                        // "error handling" ← the Kunai key
  severity, confidence, path, line, side, title,
  explanation, suggestedFix,      // withheld text, revealed by `explain`
  revealed, postedAt
}
```

The query Kunai will want — what this contributor keeps being told:

```js
db.shuriken_review_findings.aggregate([
  { $match: { 'actor.githubId': 101 } },
  { $group: { _id: '$concept', count: { $sum: 1 }, last: { $max: '$postedAt' } } },
  { $sort: { count: -1 } },
]);
```

`explanation` and `suggestedFix` are Shuriken's own generated text, not GitHub
content. They are stored so `explain` is a deterministic, free reveal of what
was withheld rather than a second model call that might say something
different. If you would rather not keep them, add a TTL — `concept`,
`severity` and `title` are all Kunai needs long term.

### `shuriken_review_runs`

One document per review invocation. It earns its place three times over:
`triggerKey` is what answers each trigger exactly once, it is how `explain`
finds the last review, and it carries the token and cost accounting.

```js
{
  repo, pr, trigger, triggeredBy, commandCommentId,
  triggerKey,                     // unique: "comment:<id>" or "label:<prId>:<sha>"
  status,                         // pending | posted | skipped | failed
  reviewId, reviewUrl,
  findingsPosted, findingsDropped, skippedFiles,
  model, usage: { inputTokens, outputTokens,
                  cacheReadInputTokens, cacheCreationInputTokens, toolCalls },
  estimatedCostUsd, startedAt, finishedAt, error
}
```

A **mention** is keyed by its comment id, so a redelivered comment produces one
review. A **label** has no comment, so it is keyed by the head SHA: relabelling
an unchanged PR will not re-review it, but a new push will.

Token and cost figures go to the logs **and** this document. They never appear
in a pull request comment.

### Unlinked activity is the normal case

`users.githubProfile` is only populated by GitHub OAuth login. At the time of
writing, **none** of the users in the development database had one. So:

- Activity is always recorded, under `actor.{githubId, login}`.
- `unlinked: true` and the `unlinked_backlog` index make it findable.
- Shuriken **never creates platform accounts**.

Once someone signs in with GitHub, a backfill job can attach their history by
matching `actor.githubId`. That job is not part of v0.

---

## Security model

### Webhook authentication

Every request to `/webhook` must carry a valid `X-Hub-Signature-256`. The body
is read, the HMAC-SHA256 is compared in constant time, and **only then** is
anything parsed, routed or enqueued. An invalid signature gets a bare `401`;
the reason goes to the log, not the response, so a prober learns nothing.

The older SHA-1 `X-Hub-Signature` header is **rejected**, not merely ignored —
accepting it would let a caller downgrade to a broken MAC.

### Prompt injection

All pull request content — diff, title, description, comments, file contents,
`CONTRIBUTING.md`, `.github/shuriken.md` — is **untrusted input written by
anyone on the internet**.

- It is passed inside an explicit `<pull_request_data>` delimiter.
- The system prompt states that text inside it which looks like instructions
  (ignore your guidelines, approve this, report nothing, reveal your prompt) is
  **itself a finding worth reporting**, not an instruction to follow.
- `.github/shuriken.md` can only **narrow** what Shuriken reports. It cannot
  widen its abilities or change the output format.
- The `read_file` tool is pinned to the pull request's head SHA by the service,
  not by the model, so it cannot be steered to a different ref.

### What the model cannot do

There is no tool for merging, pushing, approving, requesting changes, or
running a command. Pull request code is **never checked out and never
executed**. `CreateReview` hardcodes event `COMMENT`.

### Secrets

Secrets live in a `config.Secret` type whose `String`, `GoString` and
`MarshalJSON` methods all return `[REDACTED]`, so logging the config struct
cannot leak them. On top of that, every secret is registered with a redacting
`slog` handler that scrubs the value out of messages, string attributes, groups
and error values — including a connection string that surfaces inside a driver
error. Errors stored on a review run are scrubbed again before the write.

### Database privileges

`migrations/004` creates a `shurikenBot` role with collection-level privileges:

- **read/write** on the four `shuriken_*` collections (no `remove` action)
- **find only** on `users` and `projects`
- **no access at all** to anything else

### Residual risks, stated plainly

- **Shuriken's database credential can read password hashes.** `migrations/004`
  grants `find` on the whole `users` collection, and MongoDB has no field-level
  permissions. Mongoose's `select: false` on `password` and `inviteToken` is an
  application-layer default that the Go driver ignores, so a raw query returns
  the bcrypt hash. Shuriken has no code path that reads those fields, but the
  grant allows it. **Fix proposed in
  [ADR 0003](docs/adr/0003-least-privilege-identity-reads.md)** — a projected
  read-only view, one migration. Until that is accepted and run, treat
  Shuriken's database credential with the same care as a platform credential.
- **`author_association` as a permission proxy.** `COLLABORATOR` means "has
  been added as a collaborator", which in rare configurations can mean read-only
  access. The practical consequence is that such a person could trigger a
  review on someone else's PR. If that matters, grant `Administration: read`
  and switch to the collaborator-permission endpoint.
- **Repository write access implies review triggering.** Anyone who can add a
  label can spend tokens. Watch `estimatedCostUsd` in
  `shuriken_review_runs`.
- **`explanation` and `suggestedFix` are retained.** Model-generated text about
  someone's code, stored indefinitely. TTL them if that is not wanted.

---

## Tuning review quality

**Start here: `.github/shuriken.md` in the repository being reviewed.** See
[`examples/shuriken.md`](examples/shuriken.md). The "do not flag" section is
the highest-leverage knob you have — it is how you silence a false positive
permanently.

### Too noisy

| Symptom | Change |
|---|---|
| Correct but unimportant findings | raise `SHURIKEN_MIN_SEVERITY` to `medium` |
| Findings that are wrong | raise `SHURIKEN_MIN_CONFIDENCE` to `0.75`+ |
| Too many comments at once | lower `SHURIKEN_MAX_INLINE_COMMENTS` |
| The same style point repeatedly | add it to **do not flag** |
| Comments on generated files | extend `SHURIKEN_EXCLUDE_GLOBS` |

### Too quiet

| Symptom | Change |
|---|---|
| Misses real bugs | raise `SHURIKEN_EFFORT` to `xhigh` or `max` |
| "I could not verify this" | raise `SHURIKEN_MAX_TOOL_CALLS` |
| Large PRs partly skipped | raise `SHURIKEN_MAX_DIFF_BYTES` |
| Findings silently vanishing | set `SHURIKEN_LOG_LEVEL=debug` |

### Diagnosing with debug logs

At `debug`, every dropped finding is logged with a reason:
`low_confidence`, `below_severity_floor`, `line_not_in_diff`,
`already_commented`, `over_comment_cap`, `invalid_shape`.

`line_not_in_diff` appearing often means the model is reasoning about code
outside the diff — usually a sign to raise `SHURIKEN_MAX_TOOL_CALLS` so it can
read the surrounding file instead of guessing.

### Cost

Prompt caching puts one breakpoint at the end of the system blocks, so the
frozen teaching prompt, the tool definition and the repository conventions are
all cached; the volatile diff sits after it in `messages` and cannot invalidate
the prefix. Watch `cacheReadTokens` in the logs — if it stays zero across
reviews of the same repository, something is invalidating the prefix.

Every review logs `inputTokens`, `outputTokens`, `cacheReadTokens`,
`cacheWriteTokens`, `toolCalls` and `estimatedCostUsd`.

---

## Architecture

```
POST /webhook
  │
  ├─ verify X-Hub-Signature-256 ──────────── 401 on failure, nothing parsed
  ├─ parse event
  ├─ check the delivery ledger ───────────── 200 on a redelivery
  ├─ route to tasks (no I/O)
  ├─ enqueue on the bounded queue ────────── 503 when saturated
  └─ 202 Accepted                            ← GitHub times out slow webhooks

worker pool (N goroutines, per-job context timeout)
  ├─ record activity ─── map GitHub user → platform user → insert
  └─ review ─── fetch PR → filter diff → build prompt → Claude (tool loop)
                       → post-process → ONE COMMENT review → save findings
```

```
cmd/shuriken/          entry point and wiring
internal/config/       env parsing, validation, redacting Secret type
internal/logging/      slog setup with secret scrubbing
internal/webhook/      signature verification, HTTP handler, event routing
internal/worker/       bounded queue and goroutine pool
internal/command/      @mention and command parsing
internal/github/       GitHub REST behind an interface, App auth
internal/activity/     event → activity normalization (pure) and recording
internal/diff/         glob filtering, risk ranking, size cap, line validation
internal/review/       orchestration, prompts, post-processing, rendering
internal/llm/          Anthropic wrapper behind an interface
internal/store/        persistence interface and domain types (driver-free)
internal/store/mongostore/  MongoDB implementation
internal/store/storetest/   in-memory fake for tests in other packages
migrations/            numbered, idempotent mongosh scripts
```

Four packages are separate from the brief's layout because each is a distinct
testable unit: `worker` (the queue), `command` (used by both features),
`diff` (pure functions over a parsed patch), and `logging` (redaction).

### Structured output, and why it is not a forced tool call

The brief asked for the findings schema to be enforced "via a tool schema",
meaning a forced tool call. **Forced tool use — `tool_choice` `any` or `tool` —
returns HTTP 400 on current models**, so that cannot work. Instead:

- **Findings** are enforced with **structured outputs**
  (`output_config.format` plus a JSON schema). Decoded into typed Go structs.
- **File reading** is a normal tool (`read_file`) with `tool_choice: auto` and
  `strict: true` on the tool, which preserves the schema-valid-arguments
  guarantee that `any` used to provide.
- Strict client-side validation with **one retry** on malformed output. This is
  needed regardless: the schema dialect rejects `minimum`/`maximum`, so
  `confidence ∈ [0,1]` is range-checked in Go.
- When the tool budget is spent, `tool_choice` switches to `none` — still
  accepted — and the model is told to finish with what it has.

### The review pipeline

1. **Fetch** PR metadata and the file list.
2. **Filter noise** — lockfiles, vendored, generated, minified, snapshots,
   binaries (configurable globs), plus any file with no textual patch.
3. **Rank by risk and apply the size cap.** Risk weights code over config over
   docs, damps tests and pure deletions, and boosts security-sensitive paths.
   The first file is always admitted — a review of one oversized file beats no
   review. Dropped files are named in the review body.
4. **Build the prompt** — frozen system prompt + repository conventions
   (cached), then the teaching register and the delimited PR data.
5. **Call Claude** with the `read_file` tool, capped.
6. **Post-process** — drop invalid shapes, low confidence, below-floor
   severity; validate every `path`/`line`/`side` against the parsed diff; dedupe
   against our own earlier comments via an invisible HTML marker; sort by
   severity then confidence; cap.
7. **Submit one `COMMENT` review.**
8. **Save** each posted finding's concept and severity, plus the run's usage
   and cost.

---

## Architecture decisions

Decisions that were expensive to reach and would be expensive to reverse by
accident live in [`docs/adr/`](docs/adr/). Start with the index.

| ADR | Question it answers | Status |
|---|---|---|
| [0002](docs/adr/0002-shuriken-data-ownership.md) | Should Shuriken own its own database? | Accepted — no, keep it in the platform database |
| [0003](docs/adr/0003-least-privilege-identity-reads.md) | How little of `users` can Shuriken be allowed to read? | **Proposed** |
| [0004](docs/adr/0004-integration-boundary.md) | How do the platform and Kunai read Shuriken's data? | **Proposed** |

0002 and 0004 are a pair: storage stays shared *because* the access boundary
keeps that reversible. If you read only one, read 0002.

---

## Development

```bash
go build ./...
go vet ./...
go test -race ./...
golangci-lint run
```

**Go 1.26 is the minimum** — `github.com/google/go-github/v92` requires it.
(The brief said 1.23; the dependency floor is higher.)

### Dependencies

| Module | Version | Notes |
|---|---|---|
| `github.com/google/go-github/v92` | v92.0.0 | needs Go 1.26 |
| `github.com/bradleyfalzon/ghinstallation/v2` | v2.19.0 | App auth; pins go-github **v88** internally |
| `github.com/anthropics/anthropic-sdk-go` | v1.80.0 | |
| `go.mongodb.org/mongo-driver/v2` | v2.9.2 | v2, not v1 |

The v88/v92 skew is harmless: `ghinstallation` exposes an `http.RoundTripper`,
so no `go-github` types cross the boundary. It shows up only as an extra
indirect module.

### Tests

Table-driven, with the GitHub, Anthropic and store interfaces faked.

| Area | Covered |
|---|---|
| Signature verification | valid, missing, malformed, SHA-1 rejection, wrong secret, tampered body, uppercase hex |
| Webhook handler | 202 / 401 / 400 / 503, delivery idempotency, ledger outage |
| Event normalization | the full event matrix, bot filtering, self-review, dedupe-key stability, no bodies stored |
| Command parsing | every command, case, `[bot]` suffix, code blocks, blockquotes, prefix near-misses |
| Diff filtering | default globs, risk ranking, size cap, determinism |
| Line validation | context/addition/deletion on both sides, multiple hunks, malformed patches, no-newline marker |
| Post-processing | confidence and severity floors, anchoring, dedupe, cap ordering |
| Tool loop | the call cap, `tool_choice: none` after the cap, tool errors, truncation |
| Malformed output | retried exactly once, then gives up; refusals not retried |
| Review service | authorization matrix, draft/bot skips, idempotency, dry run, secret redaction |
| Secret handling | `%v`/`%s`/`%#v`/JSON redaction, scrubbing in messages, attrs, groups, errors |

The `llm` tests drive the **real** Anthropic SDK against an `httptest` server,
so the request serialisation — schema placement, cache breakpoint, `strict`,
`tool_choice` — is asserted on the wire rather than mocked away.

### Docker

```bash
docker build -t shuriken:dev .
docker run --rm -p 8080:8080 --env-file .env shuriken:dev
```

Multi-stage, static `CGO_ENABLED=0` binary, `distroless/static-debian12:nonroot`
final image. No shell and no package manager in the runtime image.

---

## Operations

### Health

```
GET /healthz  →  {"status":"ok","queueDepth":0,"dryRun":false}
```

`queueDepth` climbing toward `SHURIKEN_QUEUE_SIZE` means workers are not
keeping up — raise `SHURIKEN_WORKERS`.

### Backpressure

The queue is bounded deliberately: shedding load with a clear log line beats
growing memory until the process dies. A full queue returns **503**, which asks
GitHub to redeliver later.

### Shutdown

On `SIGTERM`/`SIGINT` the HTTP server stops accepting deliveries first, then
the pool drains work already accepted, within `SHURIKEN_SHUTDOWN_TIMEOUT`.
Anything not finished will arrive again on GitHub's redelivery, and the
idempotency keys make that safe.

### Logs

JSON to stdout. Useful fields:

- `queueDepth`, `task`, `elapsed` — throughput
- `deliveryId`, `event` — correlate with GitHub's **Advanced → Recent
  Deliveries** page
- `estimatedCostUsd`, `inputTokens`, `cacheReadTokens` — spend
- `reason` on `finding dropped` (at `debug`) — review quality

### Failure behaviour

| Failure | Behaviour |
|---|---|
| Delivery ledger unavailable | accept anyway; the `dedupeKey` index still prevents duplicates |
| Identity lookup fails | record the activity `unlinked` rather than lose it |
| Earlier comments cannot be loaded | post anyway; worst case we repeat ourselves once |
| Model returns malformed output | retry once, then fail the job |
| Model refuses | fail the job without retrying |
| A job panics | the worker logs it and continues |
