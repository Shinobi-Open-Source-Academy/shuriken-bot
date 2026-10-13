# Shipping v1 to GCP and testing against real PRs

Deploys to the same project, region and Artifact Registry as the platform
(`sos-academy-483918` / `us-central1` / `sos-academy`), using the same keyless
Workload Identity Federation pattern, so there is one deployment model to
understand rather than two.

**Phases 1–5 are one-time setup.** Phase 6 is the staged rollout, and it is the
part worth reading carefully: the point of a staged rollout is that the first
real pull request you point this at should not be one you mind it being wrong
about.

- [Read this first: the Cloud Run gotcha](#read-this-first-the-cloud-run-gotcha)
- [Phase 1 — MongoDB](#phase-1--mongodb)
- [Phase 2 — The GitHub App](#phase-2--the-github-app)
- [Phase 3 — Secrets](#phase-3--secrets)
- [Phase 4 — Service accounts and WIF](#phase-4--service-accounts-and-wif)
- [Phase 5 — First deploy](#phase-5--first-deploy)
- [Phase 6 — Testing against real PRs](#phase-6--testing-against-real-prs)
- [What CI checks on every PR](#what-ci-checks-on-every-pr)
- [Operating it](#operating-it)
- [Rolling back](#rolling-back)

---



## Read this first: the Cloud Run gotcha

**Shuriken has a background goroutine.** The publisher drains the outbox on a
timer, outside any HTTP request. Cloud Run's defaults break that in two ways:

1. **CPU is throttled to near zero between requests.** A ticker does not fire.
2. `--min-instances=0` **scales to zero.** With no instance, nothing drains.

The symptom is nasty because nothing errors: webhooks are accepted, activity is
recorded, `/healthz` says `ok` — and `outboxPending` climbs forever while the
consumer hears nothing.

So the deploy in Phase 5 sets both of these, and they are not optional:

```
--no-cpu-throttling     # CPU allocated for the life of the instance
--min-instances=1       # at least one instance always exists
```

This is the one place Shuriken costs more than the platform's server, which
runs happily at `--min-instances=0`. One always-on instance of the smallest
size is the price of reliable delivery.

> Running more than one instance is safe. The publisher leases each outbox row
> before sending, so concurrent instances cannot double-send, and a dead
> instance's lease simply lapses.

An alternative exists for later — expose an authenticated drain endpoint and
have Cloud Scheduler hit it, which allows scale-to-zero — but it adds an
endpoint, a token and a scheduler to get wrong. Not worth it for v1.

---



## Phase 1 — MongoDB

Shuriken needs **its own database**. GCP has no managed MongoDB, so use Atlas
(the platform's `MONGODB_URI` likely already points there).

1. In the same Atlas project/cluster as the platform, create a database named
  `shuriken`. A separate database in the same cluster is enough; a separate
   cluster is a connection-string change away if you later want it.
2. Create a user scoped to that database only.
3. Allow Cloud Run's egress. Either allow-list `0.0.0.0/0` for now, or attach
  a VPC connector with a NAT static IP and allow-list that. **If this is
   wrong, Shuriken fails on startup at the ping** rather than limping, which
   is the behaviour you want.
4. Run the migrations from your laptop:

```bash
export SHURIKEN_MONGODB_URI='mongodb+srv://shuriken:<pw>@<cluster>/shuriken?retryWrites=true&w=majority'

mongosh "$SHURIKEN_MONGODB_URI" --file migrations/001_shuriken_collections.js
mongosh "$SHURIKEN_MONGODB_URI" --file migrations/002_shuriken_indexes.js

# Role and user, if you are managing them in Mongo rather than the Atlas UI.
SHURIKEN_DB_PASSWORD='<pw>' mongosh "$ADMIN_MONGODB_URI/shuriken" \
  --file migrations/003_shuriken_db_user.js
```

`001` refuses to run if the database contains anything that is not Shuriken's,
which is the guard against pointing it at the platform database by mistake.

---



## Phase 2 — The GitHub App

Full detail is in the [README](../README.md#github-app-setup). The short
version, plus the one thing you cannot do yet:

1. Create the App. **Permissions:** Metadata read, Contents read, Issues
  read+write, Pull requests read+write. Nothing else.
2. **Events:** Issues, Pull request, Pull request review, Issue comment, Pull
  request review comment. Five, and not `push`.
3. Generate a private key, note the App ID.
4. Leave the **webhook URL blank for now** — you do not have a Cloud Run URL
  until Phase 5. You will come back and set it.
5. **Do not install it on any repository yet.** Installing it is what starts
  real traffic, and that is Phase 6.

---



## Phase 3 — Secrets

Same pattern as the platform: everything sensitive goes into Secret Manager,
read from your local `.env`, so no value is ever typed into a shell.

```bash
export PROJECT_ID=sos-academy-483918
export REGION=us-central1

# Assumes a local .env holding the real values, per .env.example.
set -a && . ./.env && set +a

for name in \
  SHURIKEN_GITHUB_WEBHOOK_SECRET \
  SHURIKEN_GITHUB_PRIVATE_KEY \
  ANTHROPIC_API_KEY \
  SHURIKEN_MONGODB_URI \
  SHURIKEN_CONSUMER_WEBHOOK_SECRET
do
  value="${!name}"
  if [ -z "$value" ]; then echo "✗ $name is not set in .env"; continue; fi
  printf '%s' "$value" \
    | gcloud secrets create "$name" --data-file=- --replication-policy=automatic --project="$PROJECT_ID" \
    || printf '%s' "$value" | gcloud secrets versions add "$name" --data-file=- --project="$PROJECT_ID"
  echo "✓ $name"
done
```

`create` falling through to `versions add` means re-running this rotates rather
than failing on an existing secret.

**The private key is multi-line PEM.** Put it in `.env` as the file's exact
contents, or load it directly instead, which avoids shell quoting entirely:

```bash
gcloud secrets create SHURIKEN_GITHUB_PRIVATE_KEY \
  --data-file=./shuriken.private-key.pem --project="$PROJECT_ID"
```

Two secrets are shared with another system and must match exactly:


| Secret                             | Must match                               |
| ---------------------------------- | ---------------------------------------- |
| `SHURIKEN_GITHUB_WEBHOOK_SECRET`   | the value in the GitHub App's settings   |
| `SHURIKEN_CONSUMER_WEBHOOK_SECRET` | the platform's `SHURIKEN_WEBHOOK_SECRET` |


Generate the consumer secret with `openssl rand -hex 32` and give the same
value to whoever sets up the platform endpoint.

---



## Phase 4 — Service accounts and WIF



### The runtime identity

```bash
gcloud iam service-accounts create shuriken-run \
  --display-name="Shuriken Cloud Run runtime" --project="$PROJECT_ID"

for name in \
  SHURIKEN_GITHUB_WEBHOOK_SECRET SHURIKEN_GITHUB_PRIVATE_KEY \
  ANTHROPIC_API_KEY SHURIKEN_MONGODB_URI SHURIKEN_CONSUMER_WEBHOOK_SECRET
do
  gcloud secrets add-iam-policy-binding "$name" \
    --member="serviceAccount:shuriken-run@${PROJECT_ID}.iam.gserviceaccount.com" \
    --role="roles/secretmanager.secretAccessor" --project="$PROJECT_ID"
done
```

Access to those five secrets and nothing else. In particular **not** the
platform's secrets: Shuriken has no reason to read them and no code path that
would.

### Letting this repo deploy

The platform's WIF provider has an attribute condition pinned to **that**
repository:

```
assertion.repository == 'Shinobi-Open-Source-Academy/sos-academy-platform'
  && assertion.ref == 'refs/heads/main'
```

So this repo cannot use it as-is. Add a second provider in the same pool rather
than broadening the first — least privilege, and either repo can be revoked
independently:

```bash
export PROJECT_NUMBER=194253303838
export REPO=Shinobi-Open-Source-Academy/shuriken-bot

gcloud iam workload-identity-pools providers create-oidc github-actions-shuriken \
  --location=global --workload-identity-pool=github-actions-pool \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
  --attribute-condition="assertion.repository == '${REPO}' && assertion.ref == 'refs/heads/main'" \
  --project="$PROJECT_ID"

# Let this repo's main-branch workflows impersonate the existing deployer.
gcloud iam service-accounts add-iam-policy-binding \
  "github-deployer@${PROJECT_ID}.iam.gserviceaccount.com" \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github-actions-pool/attribute.repository/${REPO}" \
  --project="$PROJECT_ID"

# The deployer must be able to set the runtime identity on the service.
gcloud iam service-accounts add-iam-policy-binding \
  "shuriken-run@${PROJECT_ID}.iam.gserviceaccount.com" \
  --role="roles/iam.serviceAccountUser" \
  --member="serviceAccount:github-deployer@${PROJECT_ID}.iam.gserviceaccount.com" \
  --project="$PROJECT_ID"
```

Then point `WORKLOAD_IDENTITY_PROVIDER` in
`[.github/workflows/deploy.yml](../.github/workflows/deploy.yml)` at the new
provider:

```
projects/194253303838/locations/global/workloadIdentityPools/github-actions-pool/providers/github-actions-shuriken
```

> If you would rather keep one provider, change its attribute condition to
> `assertion.repository_owner == 'Shinobi-Open-Source-Academy' && assertion.ref == 'refs/heads/main'`
> and leave `deploy.yml` alone. That admits *every* repo in the org, which is
> the trade you are making.

---



## Phase 5 — First deploy

The first deploy is by hand, because it is the one that sets the config the
workflow afterwards preserves.

```bash
export PROJECT_ID=sos-academy-483918
export REGION=us-central1

# Build and push once from your laptop. --platform is not optional: Cloud Run
# will not run an arm64 image, and on an Apple Silicon machine that is what
# you get by default.
gcloud artifacts repositories describe sos-academy --location="$REGION" --project="$PROJECT_ID" \
  || gcloud artifacts repositories create sos-academy --repository-format=docker \
       --location="$REGION" --project="$PROJECT_ID"

gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/sos-academy/shuriken:bootstrap"
docker build --platform linux/amd64 -t "$IMAGE" .
docker push "$IMAGE"

gcloud run deploy shuriken \
  --image="$IMAGE" \
  --region="$REGION" \
  --project="$PROJECT_ID" \
  --platform=managed \
  --allow-unauthenticated \
  --service-account="shuriken-run@${PROJECT_ID}.iam.gserviceaccount.com" \
  --port=8080 \
  --cpu=1 --memory=512Mi \
  --min-instances=1 --max-instances=3 \
  --no-cpu-throttling \
  --timeout=300 \
  --set-env-vars="SHURIKEN_GITHUB_APP_ID=<your app id>,SHURIKEN_MONGODB_DATABASE=shuriken,SHURIKEN_BOT_HANDLE=shuriken-bot,SHURIKEN_ANTHROPIC_MODEL=claude-opus-5-5,SHURIKEN_LOG_LEVEL=info,SHURIKEN_DRY_RUN=1" \
  --set-secrets="SHURIKEN_GITHUB_WEBHOOK_SECRET=SHURIKEN_GITHUB_WEBHOOK_SECRET:latest,SHURIKEN_GITHUB_PRIVATE_KEY=SHURIKEN_GITHUB_PRIVATE_KEY:latest,ANTHROPIC_API_KEY=ANTHROPIC_API_KEY:latest,SHURIKEN_MONGODB_URI=SHURIKEN_MONGODB_URI:latest,SHURIKEN_CONSUMER_WEBHOOK_SECRET=SHURIKEN_CONSUMER_WEBHOOK_SECRET:latest"
```

Note what is deliberate here:

- `--allow-unauthenticated`**.** GitHub has to reach `/webhook`, and it cannot present a Google identity. The endpoint authenticates every request itself with HMAC and returns 401 otherwise, which is the real control.
- `SHURIKEN_DRY_RUN=1`**.** The first deploy processes events and writes nothing anywhere. You turn it off in Phase 6, not before.
- `SHURIKEN_CONSUMER_WEBHOOK_URL` **is absent.** Publishing stays off until the platform endpoint exists. Events accumulate in the outbox, which is fine and visible on `/healthz`.

Get the URL and finish the App:

```bash
gcloud run services describe shuriken --region="$REGION" --format='value(status.url)'
```

Set the App's **Webhook URL** to `<that URL>/webhook` and tick **Active**.

Verify:

```bash
URL=$(gcloud run services describe shuriken --region="$REGION" --format='value(status.url)')

curl -s "$URL/healthz"
# {"status":"ok","queueDepth":0,"outboxPending":0,"outboxDeadLettered":0,"dryRun":true}

# An unsigned delivery must be refused.
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$URL/webhook" \
  -H 'Content-Type: application/json' -H 'X-GitHub-Event: ping' --data '{}'
# 401
```

Confirm the Mongo connection actually came up, since that is the step most
likely to be wrong:

```bash
gcloud run services logs read shuriken --region="$REGION" --limit=50 \
  | grep -E "connected to mongodb|mongo (connect|ping) failed|starting shuriken"
```

A `mongo ping failed` here is almost always Atlas network access. The error
never contains the connection string.

---



## Phase 6 — Testing against real PRs

The staged part. Each step is reversible and tells you something the previous
one could not.

### Step 1 — Dry run on one repository

Install the App on **one** repository you own, ideally one with a little real
traffic. `SHURIKEN_DRY_RUN=1` is still on, so Shuriken reads GitHub and calls
nothing else: no comments, no reviews, no database writes.

Open a pull request, comment `@shuriken-bot review`, then read the logs:

```bash
gcloud run services logs read shuriken --region=us-central1 --limit=100 \
  | grep -E "DRY RUN|activity recorded|review"
```

You should see `DRY RUN would record activity`, `DRY RUN would submit review`,
and `DRY RUN would publish event` with the exact payload. **Read the review
body in the log before anyone else reads it on a PR.** This is the cheapest
point at which to discover the tone is wrong.

Also confirm the App's **Advanced → Recent Deliveries** page shows `202`
responses. A `401` there means the webhook secret in Secret Manager and the one
in the App's settings differ.

### Step 2 — Go live on that one repository

```bash
gcloud run services update shuriken --region=us-central1 \
  --update-env-vars=SHURIKEN_DRY_RUN=0
```

Now comment `@shuriken-bot review` on a PR of your own. Check in order:

- [ ] Exactly **one** review appears, as a comment, not an approval
- [ ] Inline comments land on lines that are actually in the diff
- [ ] Each comment names a `concept`
- [ ] A bare `@shuriken-bot` mention gets a 👀 reaction, not a comment
- [ ] `@shuriken-bot frobnicate` gets the short help message
- [ ] `@shuriken-bot explain` reveals the reasoning that was held back
- [ ] Commenting `@shuriken-bot review` twice produces **one** review

Then look at the cost, which only becomes real here:

```bash
gcloud run services logs read shuriken --region=us-central1 --limit=200 \
  | grep "review model run complete"
```

That line carries `estimatedCostUsd`, token counts and `cacheReadTokens`. **If**
`cacheReadTokens` **is 0 across several reviews of the same repository,** prompt
caching is not working and reviews cost several times what they should.

### Step 3 — Tune before widening

Almost every first run is too noisy. The knobs, in the order worth trying, are
in the [README](../README.md#tuning-review-quality). The highest-leverage one
is not a setting: add `.github/shuriken.md` to the repository and put the false
positives you saw in its **do not flag** list.

Raising the floors is a one-liner:

```bash
gcloud run services update shuriken --region=us-central1 \
  --update-env-vars=SHURIKEN_MIN_SEVERITY=medium,SHURIKEN_MIN_CONFIDENCE=0.75
```

To see what is being discarded and why, switch to `SHURIKEN_LOG_LEVEL=debug`
and grep `finding dropped`. Turn it back afterwards; debug is chatty.

### Step 4 — Widen

Add repositories to the installation a few at a time. The things that change
with scale are rate limits and spend, so watch `queueDepth` on `/healthz` and
the per-review cost lines. Nothing else about the behaviour is traffic
dependent.

### Step 5 — Turn on publishing

Only once the platform endpoint exists — hand whoever builds it
`[docs/platform-integration.md](platform-integration.md)` and
`[docs/platform-repo-prompt.md](platform-repo-prompt.md)`.

```bash
gcloud run services update shuriken --region=us-central1 \
  --update-env-vars=SHURIKEN_CONSUMER_WEBHOOK_URL=https://<platform>/api/shuriken/events
```

Everything recorded while publishing was off is still in the outbox and will
drain on the next tick — that is the point of the outbox. Watch it empty:

```bash
watch -n5 "curl -s $URL/healthz"
```

`outboxPending` should fall to 0. If `outboxDeadLettered` rises instead, the
platform is rejecting events; a `4xx` other than `429` dead-letters
immediately, so check what its handler returns.

---



## What CI checks on every PR


| Job                    | Checks                                                                                                                                                                                                                                         |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Lint & Format**      | `gofmt`, `go mod tidy` is stable, `go vet`, migration syntax, `golangci-lint` v2.14.0                                                                                                                                                          |
| **Test**               | `go test -race` with coverage                                                                                                                                                                                                                  |
| **Vulnerability scan** | `govulncheck`, reachable vulnerabilities only                                                                                                                                                                                                  |
| **End-to-end smoke**   | boots the real binary in dry-run; asserts 401 on unsigned, 401 on SHA-1, 202 on signed, correct normalisation, an outbound event is built, **the payload carries** `actor.githubId` **and no consumer identity**, and a clean SIGTERM shutdown |
| **Build image**        | amd64 build, then runs the image and asserts it is healthy and `nonroot`                                                                                                                                                                       |
| **PR title**           | Conventional Commits, matching the platform's commitlint rules                                                                                                                                                                                 |


Two of those jobs exist because a fake cannot prove what they prove.

The **smoke** job covers the wiring: unit tests mock the GitHub and Anthropic
boundaries, so nothing else shows that the HTTP server, signature
verification, event routing, worker dispatch and payload construction work
together.

The **integration** job covers the database semantics the delivery guarantee
rests on. `ClaimDue` leases a row with `FindOneAndUpdate` so two publishers
cannot take the same event; the in-memory fake agrees with itself about that,
which is not evidence. These tests run six concurrent publishers against a
real MongoDB and assert no event is claimed twice, plus that an expired lease
is reclaimed, that `MarkFailed` dead-letters at the attempt ceiling, and that
the replay command in [Operating it](#operating-it) actually works.

`golangci-lint` is **pinned**, not `latest`, for two reasons recorded in the
workflow: the action's major must match the config schema version, and
golangci-lint refuses to run when the Go version it was built with predates
`go.mod`'s directive.

---



## Operating it

```bash
URL=$(gcloud run services describe shuriken --region=us-central1 --format='value(status.url)')
curl -s "$URL/healthz"
```


| Field                | Watch for                                                               |
| -------------------- | ----------------------------------------------------------------------- |
| `queueDepth`         | climbing toward 256 means workers are behind — raise `SHURIKEN_WORKERS` |
| `outboxPending`      | steady above 0 means the consumer is unreachable, or publishing is off  |
| `outboxDeadLettered` | **alert on any non-zero value**                                         |


A dead-lettered event is the only record that a consumer never learned
something. To replay after fixing the consumer:

```js
// Clears the dead-letter mark and makes the events due again.
db.shuriken_outbox.updateMany(
  { deadLetteredAt: { $ne: null } },
  { $set: { nextAttemptAt: new Date(), attempts: 0 },
    $unset: { deadLetteredAt: "", lastError: "" } }
);
```

Log lines worth knowing:


| Grep for                    | Tells you                                  |
| --------------------------- | ------------------------------------------ |
| `starting shuriken`         | which `version` (commit SHA) is running    |
| `rejected webhook delivery` | signature mismatches, with the delivery id |
| `review model run complete` | tokens, cache hits, `estimatedCostUsd`     |
| `finding dropped`           | why feedback was discarded (debug level)   |
| `event delivery failed`     | consumer trouble, with the attempt count   |


---



## Rolling back

```bash
# List revisions, newest first.
gcloud run revisions list --service=shuriken --region=us-central1

# Send all traffic back to a known-good one.
gcloud run services update-traffic shuriken --region=us-central1 \
  --to-revisions=shuriken-00007-abc=100
```

Two faster brakes that need no deploy:

```bash
# Stop writing to GitHub and the database, keep accepting webhooks.
gcloud run services update shuriken --region=us-central1 \
  --update-env-vars=SHURIKEN_DRY_RUN=1

# Stop everything: uninstall the App from the repositories. Nothing is lost —
# GitHub stops delivering, and anything already recorded stays in the outbox.
```

Because every webhook delivery is idempotent on its dedupe key, re-delivering
after a rollback is safe.