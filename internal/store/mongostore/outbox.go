package mongostore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sos-academy/shuriken/internal/store"
)

type outboxDoc struct {
	ID             bson.ObjectID `bson:"_id,omitempty"`
	Type           string        `bson:"type"`
	IdempotencyKey string        `bson:"idempotencyKey"`
	Payload        bson.Binary   `bson:"payload"`
	OccurredAt     time.Time     `bson:"occurredAt"`

	Attempts      int       `bson:"attempts"`
	NextAttemptAt time.Time `bson:"nextAttemptAt"`
	// LeasedUntil is how concurrent publishers avoid sending the same event:
	// claiming sets it into the future, and ClaimDue only considers rows whose
	// lease has lapsed.
	LeasedUntil    *time.Time `bson:"leasedUntil,omitempty"`
	PublishedAt    *time.Time `bson:"publishedAt,omitempty"`
	LastError      string     `bson:"lastError,omitempty"`
	DeadLetteredAt *time.Time `bson:"deadLetteredAt,omitempty"`
	CreatedAt      time.Time  `bson:"createdAt"`
}

// Enqueue adds events to the outbox, ignoring ones already queued.
//
// The unique index on idempotencyKey is what makes this safe to call from a
// retried job: re-recording an activity re-enqueues nothing.
func (s *Store) Enqueue(ctx context.Context, events []*store.Outbound) error {
	if len(events) == 0 {
		return nil
	}

	now := time.Now().UTC()
	docs := make([]any, 0, len(events))
	for _, e := range events {
		docs = append(docs, outboxDoc{
			Type:           e.Type,
			IdempotencyKey: e.IdempotencyKey,
			Payload:        bson.Binary{Subtype: 0x00, Data: e.Payload},
			OccurredAt:     e.OccurredAt,
			Attempts:       0,
			// Due immediately; the publisher picks it up on its next tick.
			NextAttemptAt: now,
			CreatedAt:     now,
		})
	}

	_, err := s.db.Collection(CollOutbox).InsertMany(ctx, docs,
		options.InsertMany().SetOrdered(false))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("enqueue outbound: %w", err)
	}
	return nil
}

// ClaimDue leases up to limit due events.
//
// FindOneAndUpdate is used in a loop rather than a bulk update because the
// lease and the read have to be one atomic step; a find-then-update would let
// two publishers claim the same row.
func (s *Store) ClaimDue(ctx context.Context, limit int, leaseFor time.Duration) ([]*store.Outbound, error) {
	coll := s.db.Collection(CollOutbox)
	now := time.Now().UTC()
	leaseUntil := now.Add(leaseFor)

	filter := bson.D{
		{Key: "publishedAt", Value: nil},
		{Key: "deadLetteredAt", Value: nil},
		{Key: "nextAttemptAt", Value: bson.D{{Key: "$lte", Value: now}}},
		// Either never leased, or the previous lease has expired because the
		// publisher holding it died.
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "leasedUntil", Value: nil}},
			bson.D{{Key: "leasedUntil", Value: bson.D{{Key: "$lte", Value: now}}}},
		}},
	}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "leasedUntil", Value: leaseUntil}}}}
	opts := options.FindOneAndUpdate().
		SetSort(bson.D{{Key: "occurredAt", Value: 1}}).
		SetReturnDocument(options.After)

	out := make([]*store.Outbound, 0, limit)
	for len(out) < limit {
		var doc outboxDoc
		err := coll.FindOneAndUpdate(ctx, filter, update, opts).Decode(&doc)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				break
			}
			return out, fmt.Errorf("claim outbound: %w", err)
		}
		out = append(out, toOutbound(doc))
	}
	return out, nil
}

func toOutbound(d outboxDoc) *store.Outbound {
	return &store.Outbound{
		ID:             d.ID.Hex(),
		Type:           d.Type,
		IdempotencyKey: d.IdempotencyKey,
		Payload:        d.Payload.Data,
		OccurredAt:     d.OccurredAt,
		Attempts:       d.Attempts,
		NextAttemptAt:  d.NextAttemptAt,
		PublishedAt:    d.PublishedAt,
		LastError:      d.LastError,
		DeadLetteredAt: d.DeadLetteredAt,
		CreatedAt:      d.CreatedAt,
	}
}

// MarkPublished records a successful delivery and releases the lease.
func (s *Store) MarkPublished(ctx context.Context, id string) error {
	oid, err := bson.ObjectIDFromHex(id)
	if err != nil {
		return fmt.Errorf("mark published: bad id %q: %w", id, err)
	}
	now := time.Now().UTC()
	_, err = s.db.Collection(CollOutbox).UpdateByID(ctx, oid, bson.D{
		{Key: "$set", Value: bson.D{{Key: "publishedAt", Value: now}}},
		{Key: "$unset", Value: bson.D{{Key: "leasedUntil", Value: ""}, {Key: "lastError", Value: ""}}},
	})
	if err != nil {
		return fmt.Errorf("mark published: %w", err)
	}
	return nil
}

// MarkFailed records a failed attempt, scheduling a retry or dead-lettering.
//
// A dead-lettered event is kept rather than deleted: it is the only record
// that a consumer never learned about something, and it can be replayed by
// clearing deadLetteredAt.
func (s *Store) MarkFailed(ctx context.Context, id, cause string, retryAfter time.Duration, maxAttempts int) error {
	oid, err := bson.ObjectIDFromHex(id)
	if err != nil {
		return fmt.Errorf("mark failed: bad id %q: %w", id, err)
	}

	now := time.Now().UTC()
	set := bson.D{
		{Key: "lastError", Value: truncate(cause, 500)},
		{Key: "nextAttemptAt", Value: now.Add(retryAfter)},
	}

	// Read the attempt count first so the decision to dead-letter is based on
	// the count after this attempt.
	var doc outboxDoc
	coll := s.db.Collection(CollOutbox)
	if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&doc); err == nil {
		if doc.Attempts+1 >= maxAttempts {
			set = append(set, bson.E{Key: "deadLetteredAt", Value: now})
		}
	}

	_, err = coll.UpdateByID(ctx, oid, bson.D{
		{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}},
		{Key: "$set", Value: set},
		{Key: "$unset", Value: bson.D{{Key: "leasedUntil", Value: ""}}},
	})
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// OutboxDepth reports pending and dead-lettered counts.
func (s *Store) OutboxDepth(ctx context.Context) (pending, dead int64, err error) {
	coll := s.db.Collection(CollOutbox)

	pending, err = coll.CountDocuments(ctx, bson.D{
		{Key: "publishedAt", Value: nil},
		{Key: "deadLetteredAt", Value: nil},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("outbox depth: %w", err)
	}

	dead, err = coll.CountDocuments(ctx, bson.D{
		{Key: "deadLetteredAt", Value: bson.D{{Key: "$ne", Value: nil}}},
	})
	if err != nil {
		return pending, 0, fmt.Errorf("outbox dead-letter depth: %w", err)
	}
	return pending, dead, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
