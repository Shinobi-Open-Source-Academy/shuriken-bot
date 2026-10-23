package mongostore

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// indexSpec is one index, declared rather than built.
//
// The name is a field instead of an option because an unnamed index is a
// latent outage: Mongo derives a name from the key pattern, and creating the
// same keys under a different name fails with IndexOptionsConflict. Running
// EnsureIndexes against a database already migrated by 002 would then stop the
// service from starting. Declaring the name here makes omitting it impossible,
// and TestIndexSpecsMatchMigration002 pins every name to 002.
type indexSpec struct {
	name   string
	keys   bson.D
	unique bool
	sparse bool
	ttl    time.Duration // zero means not a TTL index
}

func (sp indexSpec) model() mongo.IndexModel {
	opts := options.Index().SetName(sp.name)
	if sp.unique {
		opts = opts.SetUnique(true)
	}
	if sp.sparse {
		opts = opts.SetSparse(true)
	}
	if sp.ttl > 0 {
		opts = opts.SetExpireAfterSeconds(int32(sp.ttl.Seconds()))
	}
	return mongo.IndexModel{Keys: sp.keys, Options: opts}
}

// indexSpecs mirrors migrations/002_shuriken_indexes.js exactly. The two are
// kept in step by TestIndexSpecsMatchMigration002, which parses that file.
func indexSpecs(deliveryTTL time.Duration) map[string][]indexSpec {
	return map[string][]indexSpec{
		CollActivities: {
			// The idempotency guard. A redelivery, or the same logical event
			// under a different delivery id, collides here and is dropped.
			{name: "dedupeKey_unique", keys: bson.D{{Key: "dedupeKey", Value: 1}}, unique: true},

			// "What has this contributor done?", keyed by GitHub identity,
			// which is the only identity Shuriken holds.
			{name: "actor_timeline", keys: bson.D{{Key: "actor.githubId", Value: 1}, {Key: "occurredAt", Value: -1}}},
			{name: "actor_login_timeline", keys: bson.D{{Key: "actor.login", Value: 1}, {Key: "occurredAt", Value: -1}}},
			{name: "repo_timeline", keys: bson.D{{Key: "repo.fullName", Value: 1}, {Key: "occurredAt", Value: -1}}},
			{name: "type_timeline", keys: bson.D{{Key: "type", Value: 1}, {Key: "occurredAt", Value: -1}}},
		},
		CollFindings: {
			// Which concepts recur for this contributor.
			{name: "actor_concepts", keys: bson.D{{Key: "actor.githubId", Value: 1}, {Key: "concept", Value: 1}, {Key: "postedAt", Value: -1}}},
			{name: "pr_findings", keys: bson.D{{Key: "pr.githubId", Value: 1}, {Key: "postedAt", Value: -1}}},
			{name: "concept_trends", keys: bson.D{{Key: "concept", Value: 1}, {Key: "postedAt", Value: -1}}},
			{name: "commentId_unique", keys: bson.D{{Key: "commentId", Value: 1}}, unique: true, sparse: true},
		},
		CollRuns: {
			// Answers each trigger at most once.
			{name: "triggerKey_unique", keys: bson.D{{Key: "triggerKey", Value: 1}}, unique: true},
			{name: "pr_runs", keys: bson.D{{Key: "pr.githubId", Value: 1}, {Key: "startedAt", Value: -1}}},
			{name: "repo_runs", keys: bson.D{{Key: "repo.fullName", Value: 1}, {Key: "startedAt", Value: -1}}},
		},
		CollOutbox: {
			// Makes Enqueue idempotent: a retried job cannot queue the same
			// event twice.
			{name: "idempotencyKey_unique", keys: bson.D{{Key: "idempotencyKey", Value: 1}}, unique: true},

			// The publisher's claim query, in its filter order.
			{name: "outbox_due", keys: bson.D{
				{Key: "publishedAt", Value: 1},
				{Key: "deadLetteredAt", Value: 1},
				{Key: "nextAttemptAt", Value: 1},
			}},

			// Finding dead-lettered events to inspect or replay.
			{name: "outbox_dead_letter", keys: bson.D{{Key: "deadLetteredAt", Value: -1}}, sparse: true},
		},
		CollDeliveries: {
			// The cheap short-circuit for literal GitHub redeliveries. Entries
			// expire so the ledger does not grow without bound; dedupeKey_unique
			// is what guarantees correctness after they do.
			{name: "receivedAt_ttl", keys: bson.D{{Key: "receivedAt", Value: 1}}, ttl: deliveryTTL},
		},
	}
}

// EnsureIndexes creates Shuriken's indexes.
//
// This is a convenience for local development only, gated behind
// SHURIKEN_ENSURE_INDEXES. In any shared environment the indexes come from
// migrations/002_shuriken_indexes.js, reviewed and run deliberately. Both
// paths produce byte-identical indexes, so either may follow the other.
func (s *Store) EnsureIndexes(ctx context.Context, deliveryTTL time.Duration) error {
	for coll, specs := range indexSpecs(deliveryTTL) {
		models := make([]mongo.IndexModel, 0, len(specs))
		for _, sp := range specs {
			models = append(models, sp.model())
		}
		if _, err := s.db.Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("ensure indexes on %s: %w", coll, err)
		}
	}
	return nil
}
