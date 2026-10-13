// Package mongostore implements store.Store against the academy platform's
// MongoDB database.
//
// Shuriken owns the four shuriken_* collections and has find-only access to
// users and projects. Nothing here writes to a platform collection, and the
// database role in migrations/004 enforces that independently.
package mongostore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sos-academy/shuriken/internal/store"
)

// Collection names. The shuriken_ prefix keeps ownership obvious and lets the
// database role be scoped per collection.
const (
	CollActivities = "shuriken_activities"
	CollFindings   = "shuriken_review_findings"
	CollRuns       = "shuriken_review_runs"
	CollDeliveries = "shuriken_deliveries"

	// Platform collections, read only.
	CollUsers    = "users"
	CollProjects = "projects"
)

// Store is the MongoDB-backed store.
type Store struct {
	client *mongo.Client
	db     *mongo.Database

	// projectCache maps "owner/name" to a platform project id. The projects
	// collection is small and has no index on githubRepo, so it is loaded
	// whole and refreshed periodically rather than queried per event.
	projectMu       sync.RWMutex
	projectCache    map[string]string
	projectLoaded   time.Time
	projectCacheFor time.Duration
}

// Options configure the store.
type Options struct {
	URI      string
	Database string
	// ConnectTimeout bounds the initial connection and ping.
	ConnectTimeout time.Duration
	// ProjectCacheTTL is how long the repo to project map is reused.
	ProjectCacheTTL time.Duration
}

// New connects to MongoDB and verifies the connection.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	if opts.ProjectCacheTTL == 0 {
		opts.ProjectCacheTTL = 5 * time.Minute
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
		client:          client,
		db:              client.Database(opts.Database),
		projectCache:    map[string]string{},
		projectCacheFor: opts.ProjectCacheTTL,
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
	User       *bson.ObjectID     `bson:"user,omitempty"`
	Unlinked   bool               `bson:"unlinked"`
	Repo       store.Repo         `bson:"repo"`
	Project    *bson.ObjectID     `bson:"project,omitempty"`
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
		User:       objectIDPtr(a.UserID),
		Unlinked:   a.Unlinked,
		Repo:       a.Repo,
		Project:    objectIDPtr(a.ProjectID),
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

// userDoc is the projection Shuriken reads from the platform users collection.
type userDoc struct {
	ID            bson.ObjectID `bson:"_id"`
	Email         string        `bson:"email"`
	Name          string        `bson:"name"`
	GithubProfile struct {
		Login    string `bson:"login"`
		GithubID int64  `bson:"githubId"`
	} `bson:"githubProfile"`
}

// FindPlatformUser resolves a GitHub identity to a platform user.
//
// GitHub id is tried first because it is immutable; login is a fallback for
// accounts linked before the id was captured. Email is deliberately not used:
// Shuriken only sees public event data, where an email is unreliable.
func (s *Store) FindPlatformUser(ctx context.Context, githubID int64, login string) (*store.PlatformUser, error) {
	coll := s.db.Collection(CollUsers)
	projection := options.FindOne().SetProjection(bson.D{
		{Key: "email", Value: 1},
		{Key: "name", Value: 1},
		{Key: "githubProfile.login", Value: 1},
		{Key: "githubProfile.githubId", Value: 1},
	})

	if githubID != 0 {
		var doc userDoc
		err := coll.FindOne(ctx, bson.D{{Key: "githubProfile.githubId", Value: githubID}}, projection).Decode(&doc)
		if err == nil {
			return toPlatformUser(doc), nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("find user by github id: %w", err)
		}
	}

	if login != "" {
		// Logins are case insensitive on GitHub, so match them that way.
		var doc userDoc
		filter := bson.D{{Key: "githubProfile.login", Value: bson.D{
			{Key: "$regex", Value: "^" + regexpQuote(login) + "$"},
			{Key: "$options", Value: "i"},
		}}}
		err := coll.FindOne(ctx, filter, projection).Decode(&doc)
		if err == nil {
			return toPlatformUser(doc), nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("find user by login: %w", err)
		}
	}

	// No platform account linked yet. Not an error: the activity is recorded
	// unlinked and can be attached later.
	return nil, nil
}

func toPlatformUser(d userDoc) *store.PlatformUser {
	return &store.PlatformUser{
		ID:          d.ID.Hex(),
		Email:       d.Email,
		Name:        d.Name,
		GithubID:    d.GithubProfile.GithubID,
		GithubLogin: d.GithubProfile.Login,
	}
}

type projectDoc struct {
	ID         bson.ObjectID `bson:"_id"`
	GithubRepo string        `bson:"githubRepo"`
}

// ProjectIDForRepo maps "owner/name" to a platform project id.
func (s *Store) ProjectIDForRepo(ctx context.Context, fullName string) (string, error) {
	if fullName == "" {
		return "", nil
	}
	if err := s.ensureProjects(ctx); err != nil {
		return "", err
	}
	s.projectMu.RLock()
	defer s.projectMu.RUnlock()
	return s.projectCache[strings.ToLower(fullName)], nil
}

func (s *Store) ensureProjects(ctx context.Context) error {
	s.projectMu.RLock()
	fresh := time.Since(s.projectLoaded) < s.projectCacheFor
	s.projectMu.RUnlock()
	if fresh {
		return nil
	}

	cur, err := s.db.Collection(CollProjects).Find(ctx, bson.D{
		{Key: "githubRepo", Value: bson.D{{Key: "$nin", Value: bson.A{nil, ""}}}},
	}, options.Find().SetProjection(bson.D{{Key: "githubRepo", Value: 1}}))
	if err != nil {
		return fmt.Errorf("load projects: %w", err)
	}
	// Close only reports a failure to release a server-side cursor, which
	// there is no useful recovery from at this point.
	defer func() { _ = cur.Close(ctx) }()

	cache := map[string]string{}
	for cur.Next(ctx) {
		var doc projectDoc
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		if key := NormalizeRepoURL(doc.GithubRepo); key != "" {
			cache[key] = doc.ID.Hex()
		}
	}
	if err := cur.Err(); err != nil {
		return fmt.Errorf("iterate projects: %w", err)
	}

	s.projectMu.Lock()
	s.projectCache = cache
	s.projectLoaded = time.Now()
	s.projectMu.Unlock()
	return nil
}

// NormalizeRepoURL reduces a project's githubRepo field to a lowercase
// "owner/name" key. The platform stores full URLs, sometimes with a trailing
// slash or a .git suffix.
func NormalizeRepoURL(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return ""
	}
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/",
		"git@github.com:", "github.com/", "www.github.com/",
	} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git"), "/")

	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// regexpQuote escapes the characters a GitHub login can never contain but a
// regex would treat specially. Logins are [A-Za-z0-9-] only, so this is
// belt-and-braces against a malformed payload.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func objectIDPtr(hex string) *bson.ObjectID {
	if hex == "" {
		return nil
	}
	id, err := bson.ObjectIDFromHex(hex)
	if err != nil {
		return nil
	}
	return &id
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
