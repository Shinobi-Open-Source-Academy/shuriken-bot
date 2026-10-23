// Package publisher delivers Shuriken's events to a consumer over HTTP.
//
// Because the webhook is the only path by which anything learns what Shuriken
// recorded, delivery has to be reliable rather than best effort. Events are
// durable in the outbox before this package sees them, so the publisher's job
// is narrow: drain the outbox, sign, POST, and record the outcome.
//
// Delivery is at-least-once. Every event carries an idempotency key and the
// consumer is expected to dedupe on it; see the contract in
// the consumer contract.
package publisher

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sos-academy/shuriken/internal/store"
)

// Header names on every outbound request. The scheme deliberately mirrors
// GitHub's so a consumer can reuse verification code it already has.
const (
	HeaderSignature   = "X-Shuriken-Signature-256"
	HeaderDelivery    = "X-Shuriken-Delivery"
	HeaderEvent       = "X-Shuriken-Event"
	HeaderIdempotency = "X-Shuriken-Idempotency-Key"
)

// Envelope is the wire format. `Data` is held as raw JSON so the payload
// written to the outbox is the exact bytes that get signed and sent: a
// re-marshal could reorder keys and change the signature.
type Envelope struct {
	// ID is the outbox row id, unique per event and stable across retries.
	ID string `json:"id"`
	// Type is one of store.Event*.
	Type string `json:"type"`
	// IdempotencyKey is what the consumer dedupes on.
	IdempotencyKey string `json:"idempotencyKey"`
	// OccurredAt is when the underlying GitHub event happened.
	OccurredAt time.Time `json:"occurredAt"`
	// SentAt is when this attempt was made. It differs between retries, which
	// is why it is outside the signed payload's semantic identity.
	SentAt time.Time `json:"sentAt"`
	// Attempt is 1 on the first try, so a consumer can spot redeliveries.
	Attempt int `json:"attempt"`
	// Data is the event-specific body.
	Data json.RawMessage `json:"data"`
}

// ActivityPayload is the `data` member for store.EventActivityRecorded.
//
// There is no platform user id here by design. The consumer joins on
// actor.githubId against its own records.
type ActivityPayload struct {
	ActivityType string         `json:"activityType"`
	Actor        store.Actor    `json:"actor"`
	Repo         store.Repo     `json:"repo"`
	Subject      store.Subject  `json:"subject"`
	Object       *store.Object  `json:"object,omitempty"`
	OccurredAt   time.Time      `json:"occurredAt"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// FindingPayload is the `data` member for store.EventFindingPosted. Concept and
// severity are the fields a consumer aggregates over.
type FindingPayload struct {
	Actor      store.Actor    `json:"actor"`
	Repo       store.Repo     `json:"repo"`
	PR         store.PRRef    `json:"pullRequest"`
	Concept    string         `json:"concept"`
	Severity   store.Severity `json:"severity"`
	Confidence float64        `json:"confidence"`
	Path       string         `json:"path"`
	Line       int            `json:"line"`
	Title      string         `json:"title"`
	ReviewURL  string         `json:"reviewUrl"`
	PostedAt   time.Time      `json:"postedAt"`
}

// ActivityEvent builds an outbox row for a recorded activity.
func ActivityEvent(a *store.Activity) (*store.Outbound, error) {
	payload := ActivityPayload{
		ActivityType: string(a.Type),
		Actor:        a.Actor,
		Repo:         a.Repo,
		Subject:      a.Subject,
		OccurredAt:   a.OccurredAt,
		Metadata:     a.Metadata,
	}
	if a.Object.GithubID != 0 || a.Object.URL != "" {
		obj := a.Object
		payload.Object = &obj
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal activity payload: %w", err)
	}
	return &store.Outbound{
		Type: store.EventActivityRecorded,
		// The activity's own dedupe key is already a stable natural key, so
		// reuse it rather than inventing a second one.
		IdempotencyKey: a.DedupeKey,
		Payload:        body,
		OccurredAt:     a.OccurredAt,
	}, nil
}

// FindingEvent builds an outbox row for a posted review finding.
func FindingEvent(f *store.Finding) (*store.Outbound, error) {
	payload := FindingPayload{
		Actor:      f.Actor,
		Repo:       f.Repo,
		PR:         f.PR,
		Concept:    f.Concept,
		Severity:   f.Severity,
		Confidence: f.Confidence,
		Path:       f.Path,
		Line:       f.Line,
		Title:      f.Title,
		ReviewURL:  f.ReviewURL,
		PostedAt:   f.PostedAt,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal finding payload: %w", err)
	}

	// A review can raise the same concept at two places, so the key has to
	// include the location to stay unique.
	key := fmt.Sprintf("finding:%d:%s:%d:%s", f.PR.GithubID, f.Path, f.Line, f.Concept)
	if f.CommentID != 0 {
		key = fmt.Sprintf("finding:%d", f.CommentID)
	}
	return &store.Outbound{
		Type:           store.EventFindingPosted,
		IdempotencyKey: key,
		Payload:        body,
		OccurredAt:     f.PostedAt,
	}, nil
}

// Sign returns the value for HeaderSignature over the exact request body.
func Sign(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
