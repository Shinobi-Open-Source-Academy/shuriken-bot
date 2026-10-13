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

type runDoc struct {
	ID               bson.ObjectID `bson:"_id,omitempty"`
	Repo             store.Repo    `bson:"repo"`
	PR               store.PRRef   `bson:"pr"`
	Trigger          string        `bson:"trigger"`
	TriggeredBy      store.Actor   `bson:"triggeredBy"`
	CommandCommentID int64         `bson:"commandCommentId,omitempty"`
	TriggerKey       string        `bson:"triggerKey"`
	Status           string        `bson:"status"`
	ReviewID         int64         `bson:"reviewId,omitempty"`
	ReviewURL        string        `bson:"reviewUrl,omitempty"`
	FindingsPosted   int           `bson:"findingsPosted"`
	FindingsDropped  int           `bson:"findingsDropped"`
	SkippedFiles     []string      `bson:"skippedFiles,omitempty"`
	Model            string        `bson:"model,omitempty"`
	Usage            store.Usage   `bson:"usage"`
	EstimatedCostUSD float64       `bson:"estimatedCostUsd"`
	StartedAt        time.Time     `bson:"startedAt"`
	FinishedAt       *time.Time    `bson:"finishedAt,omitempty"`
	Error            string        `bson:"error,omitempty"`
}

// triggerKey is the unique key that makes a trigger idempotent.
//
// A mention is keyed by its comment id, so a redelivered comment produces one
// review. A label has no comment, so it is keyed by the head SHA: relabelling
// an unchanged pull request will not re-review it, but a new push will.
func triggerKey(run *store.ReviewRun) string {
	if run.CommandCommentID != 0 {
		return fmt.Sprintf("comment:%d", run.CommandCommentID)
	}
	return fmt.Sprintf("label:%d:%s", run.PR.GithubID, run.PR.HeadSHA)
}

// ClaimReviewRun atomically reserves a run for a trigger.
func (s *Store) ClaimReviewRun(ctx context.Context, run *store.ReviewRun) (bool, error) {
	doc := runDoc{
		Repo:             run.Repo,
		PR:               run.PR,
		Trigger:          run.Trigger,
		TriggeredBy:      run.TriggeredBy,
		CommandCommentID: run.CommandCommentID,
		TriggerKey:       triggerKey(run),
		Status:           store.RunPending,
		Model:            run.Model,
		StartedAt:        run.StartedAt,
	}

	res, err := s.db.Collection(CollRuns).InsertOne(ctx, doc)
	switch {
	case err == nil:
		if id, ok := res.InsertedID.(bson.ObjectID); ok {
			run.ID = id.Hex()
		}
		return true, nil
	case mongo.IsDuplicateKeyError(err):
		// Another delivery of the same trigger already claimed it.
		return false, nil
	default:
		return false, fmt.Errorf("claim review run: %w", err)
	}
}

// CompleteReviewRun writes the terminal state of a run.
func (s *Store) CompleteReviewRun(ctx context.Context, run *store.ReviewRun) error {
	finished := run.FinishedAt
	if finished.IsZero() {
		finished = time.Now().UTC()
	}

	set := bson.D{
		{Key: "status", Value: run.Status},
		{Key: "findingsPosted", Value: run.FindingsPosted},
		{Key: "findingsDropped", Value: run.FindingsDropped},
		{Key: "usage", Value: run.Usage},
		{Key: "estimatedCostUsd", Value: run.EstimatedCostUSD},
		{Key: "finishedAt", Value: finished},
	}
	if run.ReviewID != 0 {
		set = append(set, bson.E{Key: "reviewId", Value: run.ReviewID})
	}
	if run.ReviewURL != "" {
		set = append(set, bson.E{Key: "reviewUrl", Value: run.ReviewURL})
	}
	if len(run.SkippedFiles) > 0 {
		set = append(set, bson.E{Key: "skippedFiles", Value: run.SkippedFiles})
	}
	if run.Error != "" {
		set = append(set, bson.E{Key: "error", Value: run.Error})
	}

	filter := bson.D{{Key: "triggerKey", Value: triggerKey(run)}}
	if run.ID != "" {
		if id, err := bson.ObjectIDFromHex(run.ID); err == nil {
			filter = bson.D{{Key: "_id", Value: id}}
		}
	}

	if _, err := s.db.Collection(CollRuns).UpdateOne(ctx, filter,
		bson.D{{Key: "$set", Value: set}}); err != nil {
		return fmt.Errorf("complete review run: %w", err)
	}
	return nil
}

type findingDoc struct {
	ID           bson.ObjectID  `bson:"_id,omitempty"`
	Actor        store.Actor    `bson:"actor"`
	Repo         store.Repo     `bson:"repo"`
	PR           store.PRRef    `bson:"pr"`
	ReviewID     int64          `bson:"reviewId,omitempty"`
	ReviewURL    string         `bson:"reviewUrl,omitempty"`
	CommentID    int64          `bson:"commentId,omitempty"`
	Concept      string         `bson:"concept"`
	Severity     store.Severity `bson:"severity"`
	Confidence   float64        `bson:"confidence"`
	Path         string         `bson:"path"`
	Line         int            `bson:"line"`
	Side         string         `bson:"side"`
	Title        string         `bson:"title"`
	Explanation  string         `bson:"explanation,omitempty"`
	SuggestedFix string         `bson:"suggestedFix,omitempty"`
	Revealed     bool           `bson:"revealed"`
	PostedAt     time.Time      `bson:"postedAt"`
}

// SaveFindings persists the findings that were posted.
func (s *Store) SaveFindings(ctx context.Context, findings []*store.Finding) error {
	if len(findings) == 0 {
		return nil
	}
	docs := make([]any, 0, len(findings))
	for _, f := range findings {
		docs = append(docs, findingDoc{
			Actor:        f.Actor,
			Repo:         f.Repo,
			PR:           f.PR,
			ReviewID:     f.ReviewID,
			ReviewURL:    f.ReviewURL,
			CommentID:    f.CommentID,
			Concept:      f.Concept,
			Severity:     f.Severity,
			Confidence:   f.Confidence,
			Path:         f.Path,
			Line:         f.Line,
			Side:         f.Side,
			Title:        f.Title,
			Explanation:  f.Explanation,
			SuggestedFix: f.SuggestedFix,
			Revealed:     f.Revealed,
			PostedAt:     f.PostedAt,
		})
	}

	// Unordered so one duplicate does not abandon the rest of the batch.
	_, err := s.db.Collection(CollFindings).InsertMany(ctx, docs,
		options.InsertMany().SetOrdered(false))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("save findings: %w", err)
	}
	return nil
}

// LatestReviewFindings returns the findings from the most recent review on a
// pull request.
func (s *Store) LatestReviewFindings(ctx context.Context, prGithubID int64) ([]*store.Finding, error) {
	coll := s.db.Collection(CollFindings)

	// Find the newest review id for this pull request, then everything in it.
	var newest findingDoc
	err := coll.FindOne(ctx,
		bson.D{{Key: "pr.githubId", Value: prGithubID}},
		options.FindOne().SetSort(bson.D{{Key: "postedAt", Value: -1}}),
	).Decode(&newest)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest review lookup: %w", err)
	}

	filter := bson.D{
		{Key: "pr.githubId", Value: prGithubID},
		{Key: "reviewId", Value: newest.ReviewID},
	}
	cur, err := coll.Find(ctx, filter, options.Find().SetSort(bson.D{
		{Key: "severity", Value: 1},
		{Key: "path", Value: 1},
		{Key: "line", Value: 1},
	}))
	if err != nil {
		return nil, fmt.Errorf("latest review findings: %w", err)
	}
	// Close only reports a failure to release a server-side cursor, which
	// there is no useful recovery from at this point.
	defer func() { _ = cur.Close(ctx) }()

	var out []*store.Finding
	for cur.Next(ctx) {
		var doc findingDoc
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		f := &store.Finding{
			ID:           doc.ID.Hex(),
			Actor:        doc.Actor,
			Repo:         doc.Repo,
			PR:           doc.PR,
			ReviewID:     doc.ReviewID,
			ReviewURL:    doc.ReviewURL,
			CommentID:    doc.CommentID,
			Concept:      doc.Concept,
			Severity:     doc.Severity,
			Confidence:   doc.Confidence,
			Path:         doc.Path,
			Line:         doc.Line,
			Side:         doc.Side,
			Title:        doc.Title,
			Explanation:  doc.Explanation,
			SuggestedFix: doc.SuggestedFix,
			Revealed:     doc.Revealed,
			PostedAt:     doc.PostedAt,
		}
		out = append(out, f)
	}
	return out, cur.Err()
}

// MarkFindingsRevealed flags findings whose full reasoning has been posted.
func (s *Store) MarkFindingsRevealed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	oids := make(bson.A, 0, len(ids))
	for _, id := range ids {
		if oid, err := bson.ObjectIDFromHex(id); err == nil {
			oids = append(oids, oid)
		}
	}
	if len(oids) == 0 {
		return nil
	}

	_, err := s.db.Collection(CollFindings).UpdateMany(ctx,
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: oids}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "revealed", Value: true}}}},
	)
	if err != nil {
		return fmt.Errorf("mark findings revealed: %w", err)
	}
	return nil
}
