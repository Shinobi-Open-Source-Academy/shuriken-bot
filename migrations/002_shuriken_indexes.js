// 002_shuriken_indexes.js
//
// Creates Shuriken's indexes. createIndex is idempotent, so this is safe to
// re-run; it only errors if an index of the same name exists with different
// options, which is the signal you want.
//
//   mongosh "$MONGODB_URI" --file migrations/002_shuriken_indexes.js
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

      // "What has this contributor done?" keyed by GitHub identity, which
      // works whether or not they have linked a platform account.
      [{ 'actor.githubId': 1, occurredAt: -1 }, { name: 'actor_timeline' }],

      // The same question keyed by platform user, for the academy UI.
      [{ user: 1, occurredAt: -1 }, { sparse: true, name: 'user_timeline' }],

      // Finds activity waiting to be attached to a platform account.
      [{ unlinked: 1, 'actor.login': 1 }, { name: 'unlinked_backlog' }],

      [{ 'repo.fullName': 1, occurredAt: -1 }, { name: 'repo_timeline' }],
      [{ type: 1, occurredAt: -1 }, { name: 'type_timeline' }],
    ],
  },
  {
    collection: 'shuriken_review_findings',
    indexes: [
      // The query Kunai will want: which concepts recur for this contributor.
      [{ 'actor.githubId': 1, concept: 1, postedAt: -1 }, { name: 'actor_concepts' }],
      [{ user: 1, concept: 1, postedAt: -1 }, { sparse: true, name: 'user_concepts' }],

      // Loading the latest review on a PR, for the explain command.
      [{ 'pr.githubId': 1, postedAt: -1 }, { name: 'pr_findings' }],

      // Organisation-wide concept trends.
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
    collection: 'shuriken_deliveries',
    indexes: [
      // The cheap short-circuit for literal redeliveries. Entries expire so
      // the ledger does not grow without bound; the dedupeKey index above is
      // what guarantees correctness after they expire.
      [
        { receivedAt: 1 },
        { expireAfterSeconds: DELIVERY_TTL_DAYS * 24 * 60 * 60, name: 'receivedAt_ttl' },
      ],
    ],
  },
];

for (const { collection, indexes } of plan) {
  for (const [keys, options] of indexes) {
    const name = db.getCollection(collection).createIndex(keys, options);
    print(`+ ${collection}.${name}`);
  }
}

print('\n002 done.');
