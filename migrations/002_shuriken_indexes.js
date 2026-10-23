// 002_shuriken_indexes.js
//
// Creates Shuriken's indexes. createIndex is idempotent, so this is safe to
// re-run; it only errors if an index of the same name exists with different
// options, which is the signal you want.
//
//   mongosh "$SHURIKEN_MONGODB_URI" --file migrations/002_shuriken_indexes.js
//
// Set DELIVERY_TTL_DAYS to match SHURIKEN_DELIVERY_TTL_DAYS if you changed it.

const DELIVERY_TTL_DAYS = 30;

const plan = [
  {
    collection: 'shuriken_activities',
    indexes: [
      // The idempotency guard. A redelivery, or the same logical event
      // arriving under a different delivery id, collides here and is dropped.
      [{ dedupeKey: 1 }, { unique: true, name: 'dedupeKey_unique' }],

      // "What has this contributor done?" keyed by GitHub identity, which is
      // the only identity Shuriken holds. A consumer joins githubId against
      // its own records at read time.
      [{ 'actor.githubId': 1, occurredAt: -1 }, { name: 'actor_timeline' }],
      [{ 'actor.login': 1, occurredAt: -1 }, { name: 'actor_login_timeline' }],

      [{ 'repo.fullName': 1, occurredAt: -1 }, { name: 'repo_timeline' }],
      [{ type: 1, occurredAt: -1 }, { name: 'type_timeline' }],
    ],
  },
  {
    collection: 'shuriken_review_findings',
    indexes: [
      // Which concepts recur for this contributor: the question that makes
      // recurring-feedback tracking possible.
      [{ 'actor.githubId': 1, concept: 1, postedAt: -1 }, { name: 'actor_concepts' }],

      // Loading the latest review on a PR, for the explain command.
      [{ 'pr.githubId': 1, postedAt: -1 }, { name: 'pr_findings' }],

      // Concept trends across all reviewed repositories.
      [{ concept: 1, postedAt: -1 }, { name: 'concept_trends' }],

      [{ commentId: 1 }, { unique: true, sparse: true, name: 'commentId_unique' }],
    ],
  },
  {
    collection: 'shuriken_review_runs',
    indexes: [
      // Answers each trigger at most once: a mention is keyed by its comment
      // id, a label by the pull request's head commit.
      [{ triggerKey: 1 }, { unique: true, name: 'triggerKey_unique' }],

      [{ 'pr.githubId': 1, startedAt: -1 }, { name: 'pr_runs' }],
      [{ 'repo.fullName': 1, startedAt: -1 }, { name: 'repo_runs' }],
    ],
  },
  {
    collection: 'shuriken_outbox',
    indexes: [
      // Makes Enqueue idempotent: a retried job cannot queue the same event
      // twice, which is what lets the recorder enqueue unconditionally.
      [{ idempotencyKey: 1 }, { unique: true, name: 'idempotencyKey_unique' }],

      // The publisher's claim query, in its filter order.
      [
        { publishedAt: 1, deadLetteredAt: 1, nextAttemptAt: 1 },
        { name: 'outbox_due' },
      ],

      // Finding dead-lettered events to inspect or replay.
      [{ deadLetteredAt: -1 }, { sparse: true, name: 'outbox_dead_letter' }],
    ],
  },
  {
    collection: 'shuriken_deliveries',
    indexes: [
      // The cheap short-circuit for literal GitHub redeliveries. Entries
      // expire so the ledger does not grow without bound; the dedupeKey index
      // above is what guarantees correctness after they expire.
      [
        { receivedAt: 1 },
        { expireAfterSeconds: DELIVERY_TTL_DAYS * 24 * 60 * 60, name: 'receivedAt_ttl' },
      ],
    ],
  },
];

print(`database: ${db.getName()}\n`);
for (const { collection, indexes } of plan) {
  for (const [keys, options] of indexes) {
    const name = db.getCollection(collection).createIndex(keys, options);
    print(`+ ${collection}.${name}`);
  }
}

print('\n002 done.');
