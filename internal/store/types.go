// Package store defines Shuriken's persistence contract and the domain types
// that cross it. Nothing here imports a database driver, so the rest of the
// application can be tested against an in-memory fake.
//
// Shuriken owns its database outright and holds no reference to any academy
// platform record. Contributors are identified by GitHub id and login only;
// resolving one to a platform account is the consumer's job, done at read
// time. See docs/adr/0005-shuriken-owns-its-database.md.
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

// The activity types Shuriken records. These strings are persisted, so
// changing one is a data migration, not a rename.
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

// The severities a finding may carry, highest first.
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
//
// There is no platform user id and no "unlinked" flag. Shuriken does not know
// whether a GitHub account belongs to a platform member and does not need to.
type Activity struct {
	DedupeKey  string         `bson:"dedupeKey"           json:"dedupeKey"`
	Type       ActivityType   `bson:"type"                json:"type"`
	Actor      Actor          `bson:"actor"               json:"actor"`
	Repo       Repo           `bson:"repo"                json:"repo"`
	Subject    Subject        `bson:"subject"             json:"subject"`
	Object     Object         `bson:"object,omitempty"    json:"object,omitempty"`
	OccurredAt time.Time      `bson:"occurredAt"          json:"occurredAt"`
	Metadata   map[string]any `bson:"metadata,omitempty"  json:"metadata,omitempty"`
	DeliveryID string         `bson:"deliveryId,omitempty" json:"deliveryId,omitempty"`
}

// PRRef identifies a pull request under review.
type PRRef struct {
	Number   int    `bson:"number"   json:"number"`
	GithubID int64  `bson:"githubId" json:"githubId"`
	URL      string `bson:"url"      json:"url"`
	HeadSHA  string `bson:"headSha"  json:"headSha"`
}

// Finding is one piece of review feedback that was actually posted. Concept and
// Severity are what consumers aggregate over, so they are always set.
type Finding struct {
	ID           string    `bson:"-"                       json:"id,omitempty"`
	Actor        Actor     `bson:"actor"                   json:"actor"`
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
	Outbox

	// MarkDelivery records a webhook delivery id. It returns false when the id
	// was already present, meaning this is a redelivery and can be dropped.
	MarkDelivery(ctx context.Context, deliveryID, event string) (fresh bool, err error)

	// RecordActivity inserts an activity. It returns ErrDuplicate when
	// DedupeKey already exists.
	RecordActivity(ctx context.Context, a *Activity) error

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

// Outbound event types published to a consumer.
const (
	EventActivityRecorded = "activity.recorded"
	EventFindingPosted    = "review.finding_posted"
)

// Outbound is one event queued for delivery to a consumer.
//
// Because the webhook is the only way data reaches a consumer, delivery cannot
// be best-effort: an event lost here is lost permanently. So events are
// written to the database in the same operation that records them and drained
// by a publisher afterwards — the transactional outbox pattern. A crash
// between recording and publishing costs a retry, not an event.
type Outbound struct {
	ID string `bson:"-" json:"id,omitempty"`
	// Type is one of the Event* constants.
	Type string `bson:"type" json:"type"`
	// IdempotencyKey lets a consumer dedupe. For an activity it is the
	// activity's DedupeKey; for a finding it is the posted comment id.
	IdempotencyKey string `bson:"idempotencyKey" json:"idempotencyKey"`
	// Payload is the already-serialised `data` member of the envelope, stored
	// as bytes so the publisher never re-marshals and risk changing it.
	Payload []byte `bson:"payload" json:"-"`
	// OccurredAt is when the underlying thing happened, not when it was queued.
	OccurredAt time.Time `bson:"occurredAt" json:"occurredAt"`

	Attempts      int        `bson:"attempts"                json:"attempts"`
	NextAttemptAt time.Time  `bson:"nextAttemptAt"           json:"nextAttemptAt"`
	PublishedAt   *time.Time `bson:"publishedAt,omitempty"   json:"publishedAt,omitempty"`
	LastError     string     `bson:"lastError,omitempty"     json:"lastError,omitempty"`
	// DeadLetteredAt is set once attempts are exhausted. The row is kept, not
	// deleted, so it can be inspected and replayed.
	DeadLetteredAt *time.Time `bson:"deadLetteredAt,omitempty" json:"deadLetteredAt,omitempty"`
	CreatedAt      time.Time  `bson:"createdAt"                json:"createdAt"`
}

// Outbox is the delivery queue for outbound events.
type Outbox interface {
	// Enqueue adds events to the outbox. Duplicate idempotency keys are
	// ignored, so a retried job does not queue an event twice.
	Enqueue(ctx context.Context, events []*Outbound) error

	// ClaimDue atomically leases up to limit events whose NextAttemptAt has
	// passed, so concurrent publishers do not send the same event. The lease
	// is released by MarkPublished or MarkFailed.
	ClaimDue(ctx context.Context, limit int, leaseFor time.Duration) ([]*Outbound, error)

	// MarkPublished records a successful delivery.
	MarkPublished(ctx context.Context, id string) error

	// MarkFailed records a failed attempt and schedules the next one. When
	// attempts reaches maxAttempts the event is dead-lettered instead.
	MarkFailed(ctx context.Context, id string, cause string, retryAfter time.Duration, maxAttempts int) error

	// OutboxDepth reports pending and dead-lettered counts for /healthz.
	OutboxDepth(ctx context.Context) (pending, deadLettered int64, err error)
}
