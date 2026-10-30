<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/shuriken-full.png">
    <source media="(prefers-color-scheme: light)" srcset="assets/shuriken-full-light.png">
    <img src="assets/shuriken-full-light.png" alt="Shuriken" width="320">
  </picture>
</p>

<p align="center">
  <em>A teaching-first GitHub App for the Shinobi Open Source Academy.</em>
</p>

---

Shuriken does two things for every repository it is installed on:

1. **Records contributor activity** — issues, pull requests, reviews and
   mentions — into its own database, and pushes each one to a consumer as a
   signed webhook event.
2. **Reviews pull requests with Claude, teaching-first**, when asked. It is a
   mentor, not a linter: it explains the reasoning, names the underlying
   concept, and withholds the patch for conceptual problems so the contributor
   works it out themselves.

**It owns its database and shares none of yours.** It holds no credential for
any consumer's data, and identifies contributors by GitHub id and nothing else,
so a consumer joins that against its own records — which is what makes the same deployment usable by a project
unrelated to SOS Academy. See
[ADR 0005](docs/adr/0005-shuriken-owns-its-database.md).

> **v0 scope.** No backfill of history, no ranks or belts, no Kunai
> integration, no learning from past maintainer reviews, no duplicate-PR
> detection, no dashboard.

| Documentation | |
|---|---|
| [Setup](docs/setup.md) | creating the GitHub App, preparing the database |
| [Configuration](docs/configuration.md) | every environment variable |
| [Data model](docs/data-model.md) | the five collections, and what a consumer receives |
| [Security model](docs/security.md) | webhook auth, prompt injection, privileges, residual risks |
| [Review quality](docs/review-quality.md) | tuning what Shuriken says, and what it costs |
| [Architecture](docs/architecture.md) | the request flow, the review pipeline, the ADRs |
| [Operations](docs/operations.md) | health, backpressure, shutdown, failure behaviour |
| [Development](docs/development.md) | dependencies, the test suite, Docker |

---

## How it behaves

### Activity tracking (automatic)

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

- The **pull request author**, always, on their own PR.
- Anyone else needs **write access**, taken from the `author_association` that
  GitHub already includes in the webhook payload (`OWNER`, `MEMBER` or
  `COLLABORATOR`). That avoids requesting the `Administration` permission a
  real permission check would need; it is slightly coarser, and
  [Security model](docs/security.md) says how.

Drafts and bot-authored pull requests are skipped, and the requester is told
why rather than left waiting.

### Teaching style

The register is chosen from the PR author's `author_association`:

| Association | Register |
|---|---|
| `FIRST_TIME_CONTRIBUTOR`, `FIRST_TIMER` | Warm. More context, explains the why at length, names the concept in prose. Opens by acknowledging something genuinely good — only if there is something genuinely good. |
| `CONTRIBUTOR`, `COLLABORATOR`, `NONE` | Balanced. Explains the reasoning without belabouring it. |
| `MEMBER`, `OWNER` | Terse. Bugs only, one or two sentences each, no teaching. |

A fix is shown as a GitHub `suggestion` block **only** when the model marks it
mechanical and low-risk. For a conceptual problem the comment leads with the
reasoning and says a fix is being held back; `@shuriken-bot explain` reveals it
from the database — no second model call, so it always matches what was
originally said.

Shuriken submits **exactly one review** per trigger, with event `COMMENT`. It
can never approve a pull request or request changes.

---

## Reading a real review

Review quality is the one thing the test suite cannot check: tone, whether the
concept is named well, whether the patch is rightly withheld. So there is a
command that reviews a real pull request and prints what it would have said.

```bash
set -a && . ./.env && set +a
go run ./cmd/shuriken review Shinobi-Open-Source-Academy/shuriken-bot#42
```

A pull request URL works too. It needs an App id, a private key and
`ANTHROPIC_API_KEY` — no webhook, no server, no database, no deployment.

**It never posts and never records.** That is not a flag; the command has no
way to write. The review goes to stdout as Markdown and diagnostics go to
stderr, so a redirect captures only the review — which is how two models get
compared:

```bash
go run ./cmd/shuriken review owner/repo#42 > opus.md
go run ./cmd/shuriken review owner/repo#42 -model claude-sonnet-5-5 > sonnet.md
diff opus.md sonnet.md
```

`-v` turns on the pipeline's own logs, which is how you find out why a pull
request was skipped.

---

## Quick start

Dry run writes nothing to GitHub or the database, so it is safe to run against
real webhook payloads. The webhook secret is the only thing it requires.

```bash
cp .env.example .env
echo 'SHURIKEN_GITHUB_WEBHOOK_SECRET=any-value-for-local-testing' >> .env
echo 'SHURIKEN_DRY_RUN=1' >> .env

set -a && . ./.env && set +a
go run ./cmd/shuriken
```

Then replay a delivery. The signature must be computed over the **exact** bytes
you send:

```bash
BODY='{"action":"opened","pull_request":{"id":6001,"number":2,"user":{"id":101,"login":"alice","type":"User"},"html_url":"https://github.com/sos-academy/dojo/pull/2","created_at":"2026-03-01T12:00:00Z"},"repository":{"id":7001,"name":"dojo","full_name":"sos-academy/dojo","owner":{"login":"sos-academy"}},"installation":{"id":4242}}'
# $NF, not $2: some OpenSSL builds prefix the digest, others print bare hex.
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SHURIKEN_GITHUB_WEBHOOK_SECRET" | awk '{print $NF}')"

curl -sS -X POST http://localhost:8080/webhook \
  -H 'Content-Type: application/json' \
  -H 'X-GitHub-Event: pull_request' \
  -H 'X-GitHub-Delivery: local-1' \
  -H "X-Hub-Signature-256: $SIG" \
  --data "$BODY"
```

`DRY RUN would record activity` appears in the logs. Change the
`X-GitHub-Delivery` value to see it processed again; keep it the same to see
the redelivery dropped.

---

## Deploying

Cloud Run, MongoDB Atlas, Secret Manager and Workload Identity Federation. The
runbook is held by the operator rather than committed, since it carries project
ids and account names. Two things in it are easy to get wrong and expensive to
debug:

- **Cloud Run needs `--no-cpu-throttling --min-instances=1`.** The outbox
  publisher is a background goroutine; with CPU throttled between requests or
  the service scaled to zero it never runs, and the failure is silent —
  webhooks are accepted and `outboxPending` climbs forever.
- **Deploy with `SHURIKEN_DRY_RUN=1` first.** Shuriken then reads GitHub and
  writes nothing, so the review it *would* post can be read in the logs before
  a contributor sees it.

---

## Development

```bash
go build ./... && go vet ./... && go test -race ./... && golangci-lint run
```

**Go 1.26 is the minimum** — `github.com/google/go-github/v92` requires it.
Dependencies, the test matrix, the integration tests and the Docker build are
in [Development](docs/development.md).
