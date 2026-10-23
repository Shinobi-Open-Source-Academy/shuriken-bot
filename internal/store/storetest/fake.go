// Package storetest provides an in-memory store.Store for tests in other
// packages. It is not used by the running service.
package storetest

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/sos-academy/shuriken/internal/store"
)

// Fake is an in-memory store.Store. It enforces the same uniqueness
// constraints as the real indexes, so idempotency can be tested without a
// database.
type Fake struct {
	mu sync.Mutex

	// Deliveries seen, keyed by delivery id.
	Deliveries map[string]string
	// Activities recorded, in order.
	Activities []*store.Activity
	// Runs claimed, keyed by trigger key.
	Runs map[string]*store.ReviewRun
	// Findings saved, in order.
	Findings []*store.Finding
	// Revealed records ids passed to MarkFindingsRevealed.
	Revealed []string
	// Outbound holds everything enqueued for delivery, in order.
	Outbound []*store.Outbound

	// Err, when set, is returned by every method. For failure-path tests.
	Err error

	dedupe     map[string]bool
	outboxSeen map[string]bool
}

// New builds an empty Fake.
func New() *Fake {
	return &Fake{
		Deliveries: map[string]string{},
		Runs:       map[string]*store.ReviewRun{},
		dedupe:     map[string]bool{},
		outboxSeen: map[string]bool{},
	}
}

func (f *Fake) MarkDelivery(_ context.Context, deliveryID, event string) (bool, error) {
	if f.Err != nil {
		return false, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, seen := f.Deliveries[deliveryID]; seen {
		return false, nil
	}
	f.Deliveries[deliveryID] = event
	return true, nil
}

func (f *Fake) RecordActivity(_ context.Context, a *store.Activity) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dedupe[a.DedupeKey] {
		return store.ErrDuplicate
	}
	f.dedupe[a.DedupeKey] = true
	copied := *a
	f.Activities = append(f.Activities, &copied)
	return nil
}

func (f *Fake) ClaimReviewRun(_ context.Context, run *store.ReviewRun) (bool, error) {
	if f.Err != nil {
		return false, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := TriggerKey(run)
	if _, taken := f.Runs[key]; taken {
		return false, nil
	}
	copied := *run
	copied.ID = "run-" + strconv.Itoa(len(f.Runs)+1)
	f.Runs[key] = &copied
	run.ID = copied.ID
	return true, nil
}

func (f *Fake) CompleteReviewRun(_ context.Context, run *store.ReviewRun) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *run
	f.Runs[TriggerKey(run)] = &copied
	return nil
}

func (f *Fake) SaveFindings(_ context.Context, findings []*store.Finding) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Findings = append(f.Findings, findings...)
	return nil
}

func (f *Fake) LatestReviewFindings(_ context.Context, prGithubID int64) ([]*store.Finding, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var newest int64
	for _, fd := range f.Findings {
		if fd.PR.GithubID == prGithubID && fd.ReviewID > newest {
			newest = fd.ReviewID
		}
	}
	var out []*store.Finding
	for _, fd := range f.Findings {
		if fd.PR.GithubID == prGithubID && fd.ReviewID == newest {
			out = append(out, fd)
		}
	}
	return out, nil
}

func (f *Fake) MarkFindingsRevealed(_ context.Context, ids []string) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Revealed = append(f.Revealed, ids...)
	return nil
}

func (f *Fake) Close(context.Context) error { return nil }

// TriggerKey mirrors the real store's idempotency key for a review trigger.
func TriggerKey(run *store.ReviewRun) string {
	if run.CommandCommentID != 0 {
		return "comment:" + strconv.FormatInt(run.CommandCommentID, 10)
	}
	return "label:" + strconv.FormatInt(run.PR.GithubID, 10) + ":" + run.PR.HeadSHA
}

// ActivityCount returns how many activities were recorded.
func (f *Fake) ActivityCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Activities)
}

// Enqueue records outbound events, ignoring repeated idempotency keys exactly
// as the unique index does.
func (f *Fake) Enqueue(_ context.Context, events []*store.Outbound) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range events {
		if f.outboxSeen[e.IdempotencyKey] {
			continue
		}
		f.outboxSeen[e.IdempotencyKey] = true
		copied := *e
		if copied.ID == "" {
			copied.ID = "out-" + strconv.Itoa(len(f.Outbound)+1)
		}
		f.Outbound = append(f.Outbound, &copied)
	}
	return nil
}

// ClaimDue returns unpublished, non-dead-lettered events that are due.
func (f *Fake) ClaimDue(_ context.Context, limit int, _ time.Duration) ([]*store.Outbound, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	var out []*store.Outbound
	for _, e := range f.Outbound {
		if len(out) >= limit {
			break
		}
		if e.PublishedAt == nil && e.DeadLetteredAt == nil && !e.NextAttemptAt.After(now) {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkPublished stamps an event as delivered.
func (f *Fake) MarkPublished(_ context.Context, id string) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for _, e := range f.Outbound {
		if e.ID == id {
			e.PublishedAt = &now
			return nil
		}
	}
	return nil
}

// MarkFailed increments the attempt count and dead-letters once exhausted.
func (f *Fake) MarkFailed(_ context.Context, id, cause string, retryAfter time.Duration, maxAttempts int) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for _, e := range f.Outbound {
		if e.ID != id {
			continue
		}
		e.Attempts++
		e.LastError = cause
		e.NextAttemptAt = now.Add(retryAfter)
		if e.Attempts >= maxAttempts {
			e.DeadLetteredAt = &now
		}
		return nil
	}
	return nil
}

// OutboxDepth counts pending and dead-lettered events.
func (f *Fake) OutboxDepth(context.Context) (int64, int64, error) {
	if f.Err != nil {
		return 0, 0, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var pending, dead int64
	for _, e := range f.Outbound {
		switch {
		case e.DeadLetteredAt != nil:
			dead++
		case e.PublishedAt == nil:
			pending++
		}
	}
	return pending, dead, nil
}

// PublishedCount reports how many events were delivered, for assertions.
func (f *Fake) PublishedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.Outbound {
		if e.PublishedAt != nil {
			n++
		}
	}
	return n
}
