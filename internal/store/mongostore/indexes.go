package mongostore

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// EnsureIndexes creates Shuriken's indexes.
//
// This is a convenience for local development only, gated behind
// SHURIKEN_ENSURE_INDEXES. In any shared environment the indexes come from
// migrations/002_shuriken_indexes.js, reviewed and run deliberately.
func (s *Store) EnsureIndexes(ctx context.Context, deliveryTTL time.Duration) error {
	specs := map[string][]mongo.IndexModel{
		CollActivities: {
			{
				Keys:    bson.D{{Key: "dedupeKey", Value: 1}},
				Options: options.Index().SetUnique(true).SetName("dedupeKey_unique"),
			},
			{Keys: bson.D{{Key: "actor.githubId", Value: 1}, {Key: "occurredAt", Value: -1}}},
			{Keys: bson.D{{Key: "actor.login", Value: 1}, {Key: "occurredAt", Value: -1}}},
			{Keys: bson.D{{Key: "repo.fullName", Value: 1}, {Key: "occurredAt", Value: -1}}},
			{Keys: bson.D{{Key: "type", Value: 1}, {Key: "occurredAt", Value: -1}}},
		},
		CollFindings: {
			{Keys: bson.D{{Key: "actor.githubId", Value: 1}, {Key: "concept", Value: 1}, {Key: "postedAt", Value: -1}}},
			{Keys: bson.D{{Key: "pr.githubId", Value: 1}, {Key: "postedAt", Value: -1}}},
			{Keys: bson.D{{Key: "concept", Value: 1}, {Key: "postedAt", Value: -1}}},
			{
				Keys:    bson.D{{Key: "commentId", Value: 1}},
				Options: options.Index().SetUnique(true).SetSparse(true).SetName("commentId_unique"),
			},
		},
		CollRuns: {
			{
				Keys:    bson.D{{Key: "triggerKey", Value: 1}},
				Options: options.Index().SetUnique(true).SetName("triggerKey_unique"),
			},
			{Keys: bson.D{{Key: "pr.githubId", Value: 1}, {Key: "startedAt", Value: -1}}},
			{Keys: bson.D{{Key: "repo.fullName", Value: 1}, {Key: "startedAt", Value: -1}}},
		},
		CollOutbox: {
			{
				// Makes Enqueue idempotent: a retried job cannot queue the
				// same event twice.
				Keys:    bson.D{{Key: "idempotencyKey", Value: 1}},
				Options: options.Index().SetUnique(true).SetName("idempotencyKey_unique"),
			},
			{
				// The publisher's claim query, in its filter order.
				Keys: bson.D{
					{Key: "publishedAt", Value: 1},
					{Key: "deadLetteredAt", Value: 1},
					{Key: "nextAttemptAt", Value: 1},
				},
				Options: options.Index().SetName("outbox_due"),
			},
			{Keys: bson.D{{Key: "deadLetteredAt", Value: -1}}, Options: options.Index().SetSparse(true)},
		},
		CollDeliveries: {
			{
				Keys: bson.D{{Key: "receivedAt", Value: 1}},
				Options: options.Index().
					SetExpireAfterSeconds(int32(deliveryTTL.Seconds())).
					SetName("receivedAt_ttl"),
			},
		},
	}

	for coll, models := range specs {
		if _, err := s.db.Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("ensure indexes on %s: %w", coll, err)
		}
	}
	return nil
}
