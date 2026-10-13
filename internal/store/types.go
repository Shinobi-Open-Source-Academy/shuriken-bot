// Package store defines Shuriken's persistence contract and the domain types
// that cross it. Nothing here imports a database driver, so the rest of the
// application can be tested against an in-memory fake.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrDuplicate is returned when a write collided with an existing record on a
// unique key. Callers treat it as success: the work was already done.
var ErrDuplicate = errors.New("store: duplicate record")

// ActivityType enumerates the contributor actions Shuriken records in v0.
type ActivityType string

const (
	IssueOpened       ActivityType = "issue_opened"
	PROpened          ActivityType = "pr_opened"
	PRMerged          ActivityType = "pr_merged"
	PRClosedUnmerged  ActivityType = "pr_closed_unmerged"
	PRReviewSubmitted ActivityType = "pr_review_submitted"
	IssueComment      ActivityType = "issue_comment"
	PRComment         ActivityType = "pr_comment"
	IssueAssigned     ActivityType = "issue_assigned"
)

// Severity ranks a review finding. Order matters: see Rank.
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Rank maps a severity to a sortable weight, highest first.
func (s Severity) Rank() int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// Valid reports whether s is a severity Shuriken recognises.
func (s Severity) Valid() bool { return s.Rank() > 0 }

// Actor is a GitHub account. GithubID is the stable identity; Login can change.
type Actor struct {
	GithubID int64  `bson:"githubId" json:"githubId"`
	Login    string `bson:"login"    json:"login"`
}

// Repo identifies a GitHub repository.
type Repo struct {
	Owner    string `bson:"owner"    json:"owner"`
	Name     string `bson:"name"     json:"name"`
	FullName string `bson:"fullName" json:"fullName"`
	GithubID int64  `bson:"githubId" json:"githubId"`
}

// Subject is the issue or pull request an activity happened on.
type Subject struct {
	Kind     string `bson:"kind"     json:"kind"` // "issue" or "pull_request"
	Number   int    `bson:"number"   json:"number"`
	GithubID int64  `bson:"githubId" json:"githubId"`
	URL      string `bson:"url"      json:"url"`
}

// Object is the specific GitHub object an activity refers to when it is not
// the subject itself, such as a comment or a review.
type Object struct {
	GithubID int64  `bson:"githubId,omitempty" json:"githubId,omitempty"`
	URL      string `bson:"url,omitempty"      json:"url,omitempty"`
}

// Activity is one normalized contributor action. It deliberately holds no
// comment bodies and no diffs: identifiers and URLs only.
type Activity struct {
	DedupeKey  string         `bson:"dedupeKey"           json:"dedupeKey"`
	Type       ActivityType   `bson:"type"                json:"type"`
	Actor      Actor          `bson:"actor"               json:"actor"`
	UserID     string         `bson:"-"                   json:"userId,omitempty"`
	Unlinked   bool           `bson:"unlinked"            json:"unlinked"`
	Repo       Repo           `bson:"repo"                json:"repo"`
	ProjectID  string         `bson:"-"                   json:"projectId,omitempty"`
	Subject    Subject        `bson:"subject"             json:"subject"`
	Object     Object         `bson:"object,omitempty"    json:"object,omitempty"`
	OccurredAt time.Time      `bson:"occurredAt"          json:"occurredAt"`
	Metadata   map[string]any `bson:"metadata,omitempty"  json:"metadata,omitempty"`
	DeliveryID string         `bson:"deliveryId,omitempty" json:"deliveryId,omitempty"`
}

// PlatformUser is the subset of an academy platform user Shuriken reads. It is
// never written: Shuriken has find-only access to the users collection.
type PlatformUser struct {
	ID          string
	Email       string
	Name        string
	GithubID    int64
	GithubLogin string
}

// PRRef identifies a pull request under review.
type PRRef struct {
	Number   int    `bson:"number"   json:"number"`
	GithubID int64  `bson:"githubId" json:"githubId"`
	URL      string `bson:"url"      json:"url"`
	HeadSHA  string `bson:"headSha"  json:"headSha"`
}

// Finding is one piece of review feedback that was actually posted. Concept and
// Severity are the fields Kunai will aggregate over, so they are always set.
type Finding struct {
	ID           string    `bson:"-"                       json:"id,omitempty"`
	Actor        Actor     `bson:"actor"                   json:"actor"`
	UserID       string    `bson:"-"                       json:"userId,omitempty"`
	Unlinked     bool      `bson:"unlinked"                json:"unlinked"`
	Repo         Repo      `bson:"repo"                    json:"repo"`
	PR           PRRef     `bson:"pr"                      json:"pr"`
	ReviewID     int64     `bson:"reviewId,omitempty"      json:"reviewId,omitempty"`
	ReviewURL    string    `bson:"reviewUrl,omitempty"     json:"reviewUrl,omitempty"`
	CommentID    int64     `bson:"commentId,omitempty"     json:"commentId,omitempty"`
	Concept      string    `bson:"concept"                 json:"concept"`
	Severity     Severity  `bson:"severity"                json:"severity"`
	Confidence   float64   `bson:"confidence"              json:"confidence"`
	Path         string    `bson:"path"                    json:"path"`
	Line         int       `bson:"line"                    json:"line"`
	Side         string    `bson:"side"                    json:"side"`
	Title        string    `bson:"title"                   json:"title"`
	Explanation  string    `bson:"explanation,omitempty"   json:"explanation,omitempty"`
	SuggestedFix string    `bson:"suggestedFix,omitempty"  json:"suggestedFix,omitempty"`
	Revealed     bool      `bson:"revealed"                json:"revealed"`
	PostedAt     time.Time `bson:"postedAt"                json:"postedAt"`
}

// Usage records what one review cost in tokens.
type Usage struct {
	InputTokens              int64 `bson:"inputTokens"              json:"inputTokens"`
	OutputTokens             int64 `bson:"outputTokens"             json:"outputTokens"`
	CacheReadInputTokens     int64 `bson:"cacheReadInputTokens"     json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `bson:"cacheCreationInputTokens" json:"cacheCreationInputTokens"`
	ToolCalls                int   `bson:"toolCalls"                json:"toolCalls"`
}

// Review run statuses.
const (
	RunPending = "pending"
	RunPosted  = "posted"
	RunSkipped = "skipped"
	RunFailed  = "failed"
)

// ReviewRun is one review invocation. CommandCommentID gives us
// reply-once-per-mention; the run is also how `explain` finds the last review.
type ReviewRun struct {
	ID               string    `bson:"-"                          json:"id,omitempty"`
	Repo             Repo      `bson:"repo"                       json:"repo"`
	PR               PRRef     `bson:"pr"                         json:"pr"`
	Trigger          string    `bson:"trigger"                    json:"trigger"` // "mention" or "label"
	TriggeredBy      Actor     `bson:"triggeredBy"                json:"triggeredBy"`
	CommandCommentID int64     `bson:"commandCommentId,omitempty" json:"commandCommentId,omitempty"`
	Status           string    `bson:"status"                     json:"status"`
	ReviewID         int64     `bson:"reviewId,omitempty"         json:"reviewId,omitempty"`
	ReviewURL        string    `bson:"reviewUrl,omitempty"        json:"reviewUrl,omitempty"`
	FindingsPosted   int       `bson:"findingsPosted"             json:"findingsPosted"`
	FindingsDropped  int       `bson:"findingsDropped"            json:"findingsDropped"`
	SkippedFiles     []string  `bson:"skippedFiles,omitempty"     json:"skippedFiles,omitempty"`
	Model            string    `bson:"model,omitempty"            json:"model,omitempty"`
	Usage            Usage     `bson:"usage"                      json:"usage"`
	EstimatedCostUSD float64   `bson:"estimatedCostUsd"           json:"estimatedCostUsd"`
	StartedAt        time.Time `bson:"startedAt"                  json:"startedAt"`
	FinishedAt       time.Time `bson:"finishedAt,omitempty"       json:"finishedAt,omitempty"`
	Error            string    `bson:"error,omitempty"            json:"error,omitempty"`
}

// Store is Shuriken's full persistence surface.
type Store interface {
	// MarkDelivery records a webhook delivery id. It returns false when the id
	// was already present, meaning this is a redelivery and can be dropped.
	MarkDelivery(ctx context.Context, deliveryID, event string) (fresh bool, err error)

	// RecordActivity inserts an activity. It returns ErrDuplicate when
	// DedupeKey already exists.
	RecordActivity(ctx context.Context, a *Activity) error

	// FindPlatformUser resolves a GitHub identity to an academy platform user.
	// It matches on github id first, then login. A nil user with a nil error
	// means no platform account is linked yet.
	FindPlatformUser(ctx context.Context, githubID int64, login string) (*PlatformUser, error)

	// ProjectIDForRepo maps "owner/name" to a platform project id, or "" when
	// the repo is not a tracked project.
	ProjectIDForRepo(ctx context.Context, fullName string) (string, error)

	// ClaimReviewRun atomically reserves a run. It returns false when a run for
	// the same command comment already exists, which is how we avoid answering
	// the same mention twice.
	ClaimReviewRun(ctx context.Context, run *ReviewRun) (claimed bool, err error)

	// CompleteReviewRun writes the terminal state of a run.
	CompleteReviewRun(ctx context.Context, run *ReviewRun) error

	// SaveFindings persists the findings that were posted.
	SaveFindings(ctx context.Context, findings []*Finding) error

	// LatestReviewFindings returns the findings from the most recent posted
	// review on a pull request, for the `explain` command.
	LatestReviewFindings(ctx context.Context, prGithubID int64) ([]*Finding, error)

	// MarkFindingsRevealed flags findings whose full reasoning has been posted.
	MarkFindingsRevealed(ctx context.Context, ids []string) error

	// Close releases the underlying connection.
	Close(ctx context.Context) error
}
