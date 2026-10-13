// Package mongostore implements store.Store against Shuriken's own MongoDB
// database.
//
// Shuriken's database contains only Shuriken's collections. There is no
// connection to, and no credential for, any academy platform database: the
// platform learns about activity through signed outbound webhooks instead.
// That is what makes the same deployment usable for more than one project.
package mongostore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sos-academy/shuriken/internal/store"
)

// Collection names. The shuriken_ prefix keeps ownership obvious and lets the
// database role be scoped per collection.
// Collection names. The shuriken_ prefix is kept even though the database is
// Shuriken's own: it costs nothing and keeps the names unambiguous if these
// documents are ever exported into a shared database.
const (
	CollActivities = "shuriken_activities"
	CollFindings   = "shuriken_review_findings"
	CollRuns       = "shuriken_review_runs"
	CollDeliveries = "shuriken_deliveries"
	CollOutbox     = "shuriken_outbox"
)

// Store is the MongoDB-backed store.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

// Options configure the store.
type Options struct {
	URI      string
	Database string
	// ConnectTimeout bounds the initial connection and ping.
	ConnectTimeout time.Duration
}

// New connects to MongoDB and verifies the connection.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(opts.URI))
	if err != nil {
		// The URI contains credentials, so it is never included in the error.
		return nil, fmt.Errorf("mongo connect failed: %w", scrubURI(err, opts.URI))
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("mongo ping failed: %w", scrubURI(err, opts.URI))
	}

	return &Store{
		client: client,
		db:     client.Database(opts.Database),
	}, nil
}

// Close disconnects the client.
func (s *Store) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

// deliveryDoc is the idempotency ledger entry. TTL is enforced by an index
// created in migrations/002.
type deliveryDoc struct {
	ID         string    `bson:"_id"`
	Event      string    `bson:"event"`
	ReceivedAt time.Time `bson:"receivedAt"`
}

// MarkDelivery records a delivery id, reporting whether it is new.
func (s *Store) MarkDelivery(ctx context.Context, deliveryID, event string) (bool, error) {
	_, err := s.db.Collection(CollDeliveries).InsertOne(ctx, deliveryDoc{
		ID:         deliveryID,
		Event:      event,
		ReceivedAt: time.Now().UTC(),
	})
	switch {
	case err == nil:
		return true, nil
	case mongo.IsDuplicateKeyError(err):
		return false, nil
	default:
		return false, fmt.Errorf("mark delivery: %w", err)
	}
}

// activityDoc is the stored shape. ObjectId references are kept out of the
// domain type, so they are converted here.
type activityDoc struct {
	DedupeKey  string             `bson:"dedupeKey"`
	Type       store.ActivityType `bson:"type"`
	Actor      store.Actor        `bson:"actor"`
	Repo       store.Repo         `bson:"repo"`
	Subject    store.Subject      `bson:"subject"`
	Object     *store.Object      `bson:"object,omitempty"`
	OccurredAt time.Time          `bson:"occurredAt"`
	Metadata   map[string]any     `bson:"metadata,omitempty"`
	DeliveryID string             `bson:"deliveryId,omitempty"`
	CreatedAt  time.Time          `bson:"createdAt"`
	UpdatedAt  time.Time          `bson:"updatedAt"`
}

// RecordActivity inserts an activity, returning store.ErrDuplicate when the
// dedupe key already exists.
func (s *Store) RecordActivity(ctx context.Context, a *store.Activity) error {
	now := time.Now().UTC()
	doc := activityDoc{
		DedupeKey:  a.DedupeKey,
		Type:       a.Type,
		Actor:      a.Actor,
		Repo:       a.Repo,
		Subject:    a.Subject,
		OccurredAt: a.OccurredAt,
		Metadata:   a.Metadata,
		DeliveryID: a.DeliveryID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if a.Object.GithubID != 0 || a.Object.URL != "" {
		obj := a.Object
		doc.Object = &obj
	}

	_, err := s.db.Collection(CollActivities).InsertOne(ctx, doc)
	switch {
	case err == nil:
		return nil
	case mongo.IsDuplicateKeyError(err):
		return store.ErrDuplicate
	default:
		return fmt.Errorf("record activity: %w", err)
	}
}

func scrubURI(err error, uri string) error {
	if err == nil || uri == "" {
		return err
	}
	if msg := err.Error(); strings.Contains(msg, uri) {
		return errors.New(strings.ReplaceAll(msg, uri, "[REDACTED]"))
	}
	return err
}
