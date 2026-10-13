package mongostore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sos-academy/shuriken/internal/store"
)

// These tests exercise the real MongoDB behaviour that the in-memory fake in
// storetest can only imitate: unique-index collisions, and the atomicity of
// the outbox lease. The lease is what stops two Cloud Run instances delivering
// the same event, so "the fake agrees with itself" is not evidence that it
// works.
//
// They are skipped unless SHURIKEN_TEST_MONGODB_URI is set, so the normal
// `go test ./...` needs no database. CI supplies a service container.
//
//	SHURIKEN_TEST_MONGODB_URI=mongodb://localhost:27017 go test ./internal/store/mongostore/
//
// Each test gets its own uniquely named database, dropped on cleanup, so runs
// never collide and nothing survives a failure.

// newTestStore connects and returns a Store backed by a throwaway database.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	uri := os.Getenv("SHURIKEN_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("SHURIKEN_TEST_MONGODB_URI is not set; skipping MongoDB integration tests")
	}

	// Unique per test, so a parallel or repeated run cannot see another's data.
	dbName := fmt.Sprintf("shuriken_itest_%d_%d", os.Getpid(), time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := New(ctx, Options{URI: uri, Database: dbName})
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}

	// The unique indexes are the thing under test, so they must exist.
	if err := s.EnsureIndexes(ctx, 24*time.Hour); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.db.Drop(cleanupCtx); err != nil {
			t.Errorf("drop test database %s: %v", dbName, err)
		}
		if err := s.Close(cleanupCtx); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	return s
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func outboundEvent(key string) *store.Outbound {
	return &store.Outbound{
		Type:           store.EventActivityRecorded,
		IdempotencyKey: key,
		Payload:        []byte(`{"activityType":"pr_merged","actor":{"githubId":101,"login":"alice"}}`),
		OccurredAt:     time.Now().UTC(),
	}
}

func TestIntegrationEnqueueIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	// Three separate calls, as a retried job would make.
	for i := 0; i < 3; i++ {
		if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("pr_merged:6001")}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	pending, dead, err := s.OutboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("pending = %d, want 1: the unique index should collapse repeats", pending)
	}
	if dead != 0 {
		t.Errorf("deadLettered = %d, want 0", dead)
	}
}

// A batch containing a duplicate must still insert the new events. Ordered
// inserts would abandon the batch at the collision.
func TestIntegrationEnqueuePartialBatch(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("a")}); err != nil {
		t.Fatal(err)
	}
	err := s.Enqueue(ctx, []*store.Outbound{
		outboundEvent("a"), // already present
		outboundEvent("b"),
		outboundEvent("c"),
	})
	if err != nil {
		t.Fatalf("a duplicate in the batch must not fail the call: %v", err)
	}

	pending, _, err := s.OutboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 3 {
		t.Fatalf("pending = %d, want 3 (a, b, c)", pending)
	}
}

// The property the whole lease exists for: one row is never handed to two
// callers.
func TestIntegrationClaimDueLeasesExclusively(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("only-one")}); err != nil {
		t.Fatal(err)
	}

	first, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim got %d events, want 1", len(first))
	}

	// A second publisher, while the lease is still held.
	second, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second claim got %d events, want 0: the lease was not exclusive", len(second))
	}
}

// The same property under real concurrency, which is the Cloud Run case with
// more than one instance.
func TestIntegrationConcurrentClaimsDoNotOverlap(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	const events = 40
	batch := make([]*store.Outbound, 0, events)
	for i := 0; i < events; i++ {
		batch = append(batch, outboundEvent(fmt.Sprintf("event-%02d", i)))
	}
	if err := s.Enqueue(ctx, batch); err != nil {
		t.Fatal(err)
	}

	const publishers = 6
	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
	)
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimDue(ctx, events, time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, e := range got {
				claimed[e.IdempotencyKey]++
			}
		}()
	}
	wg.Wait()

	var doubled []string
	for key, n := range claimed {
		if n > 1 {
			doubled = append(doubled, fmt.Sprintf("%s claimed %d times", key, n))
		}
	}
	if len(doubled) > 0 {
		t.Fatalf("events claimed more than once, so they would be delivered twice: %v", doubled)
	}
	if len(claimed) != events {
		t.Errorf("claimed %d distinct events, want all %d", len(claimed), events)
	}
}

// A publisher that dies mid-batch must not strand its events forever.
func TestIntegrationExpiredLeaseIsReclaimed(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("stranded")}); err != nil {
		t.Fatal(err)
	}

	// Lease it, then let the lease lapse as a crashed instance's would.
	if got, err := s.ClaimDue(ctx, 10, 50*time.Millisecond); err != nil || len(got) != 1 {
		t.Fatalf("initial claim: got %d, err %v", len(got), err)
	}
	if got, err := s.ClaimDue(ctx, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("claim during the lease: got %d, err %v", len(got), err)
	}

	time.Sleep(300 * time.Millisecond)

	got, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after the lease lapsed got %d events, want 1", len(got))
	}
}

func TestIntegrationClaimDueHonoursBackoff(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("backed-off")}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: got %d, err %v", len(claimed), err)
	}

	// Fail it with a long retry delay.
	if err := s.MarkFailed(ctx, claimed[0].ID, "consumer returned 503", time.Hour, 8); err != nil {
		t.Fatal(err)
	}

	got, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("claimed %d events that are not due yet", len(got))
	}
}

func TestIntegrationMarkPublishedIsTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("delivered")}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: got %d, err %v", len(claimed), err)
	}
	if err := s.MarkPublished(ctx, claimed[0].ID); err != nil {
		t.Fatal(err)
	}

	pending, dead, err := s.OutboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 || dead != 0 {
		t.Errorf("pending = %d, dead = %d, want 0 and 0", pending, dead)
	}

	// A published event must never be handed out again.
	if got, err := s.ClaimDue(ctx, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("a published event was re-claimed: got %d, err %v", len(got), err)
	}
}

func TestIntegrationDeadLettersAtMaxAttempts(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	const maxAttempts = 3
	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("doomed")}); err != nil {
		t.Fatal(err)
	}

	var id string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		claimed, err := s.ClaimDue(ctx, 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 1 {
			t.Fatalf("attempt %d: claimed %d events, want 1", attempt, len(claimed))
		}
		id = claimed[0].ID
		// retryAfter 0 so the event is due again immediately.
		if err := s.MarkFailed(ctx, id, "consumer unreachable", 0, maxAttempts); err != nil {
			t.Fatal(err)
		}
	}

	pending, dead, err := s.OutboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dead != 1 {
		t.Errorf("deadLettered = %d, want 1 after %d attempts", dead, maxAttempts)
	}
	if pending != 0 {
		t.Errorf("pending = %d, want 0: a dead-lettered event is not pending", pending)
	}

	// And it is never retried again.
	if got, err := s.ClaimDue(ctx, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("a dead-lettered event was re-claimed: got %d, err %v", len(got), err)
	}

	// The failure cause is kept, because it is the only record of what went
	// wrong for an event nobody received.
	var doc outboxDoc
	oid, err := bson.ObjectIDFromHex(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Collection(CollOutbox).
		FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.LastError == "" {
		t.Error("lastError should record why delivery failed")
	}
	if doc.Attempts != maxAttempts {
		t.Errorf("attempts = %d, want %d", doc.Attempts, maxAttempts)
	}
}

// Replaying a dead-lettered event is a documented operation, so the shape the
// runbook uses must actually work.
func TestIntegrationDeadLetterCanBeReplayed(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.Enqueue(ctx, []*store.Outbound{outboundEvent("replay-me")}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: got %d, err %v", len(claimed), err)
	}
	if err := s.MarkFailed(ctx, claimed[0].ID, "rejected", 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, dead, _ := s.OutboxDepth(ctx); dead != 1 {
		t.Fatalf("expected the event to be dead-lettered first")
	}

	// The update documented in docs/shipping.md under "Operating it".
	if _, err := s.db.Collection(CollOutbox).UpdateMany(ctx,
		bson.D{{Key: "deadLetteredAt", Value: bson.D{{Key: "$ne", Value: nil}}}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "nextAttemptAt", Value: time.Now().UTC()},
				{Key: "attempts", Value: 0},
			}},
			{Key: "$unset", Value: bson.D{
				{Key: "deadLetteredAt", Value: ""}, {Key: "lastError", Value: ""},
			}},
		}); err != nil {
		t.Fatalf("replay update: %v", err)
	}

	pending, dead, err := s.OutboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dead != 0 || pending != 1 {
		t.Fatalf("after replay: pending = %d, dead = %d, want 1 and 0", pending, dead)
	}
	if got, err := s.ClaimDue(ctx, 10, time.Minute); err != nil || len(got) != 1 {
		t.Fatalf("a replayed event must be claimable: got %d, err %v", len(got), err)
	}
}

// Oldest first, so a backlog drains in the order things happened.
func TestIntegrationClaimDueIsOldestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	base := time.Now().UTC().Add(-time.Hour)
	var batch []*store.Outbound
	for i, offset := range []time.Duration{30 * time.Minute, 0, 15 * time.Minute} {
		e := outboundEvent(fmt.Sprintf("ordered-%d", i))
		e.OccurredAt = base.Add(offset)
		batch = append(batch, e)
	}
	if err := s.Enqueue(ctx, batch); err != nil {
		t.Fatal(err)
	}

	got, err := s.ClaimDue(ctx, 3, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("claimed %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].OccurredAt.Before(got[i-1].OccurredAt) {
			t.Fatalf("not oldest-first: %v came after %v", got[i].OccurredAt, got[i-1].OccurredAt)
		}
	}
	// ordered-1 has the earliest timestamp.
	if got[0].IdempotencyKey != "ordered-1" {
		t.Errorf("first claimed = %q, want ordered-1", got[0].IdempotencyKey)
	}
}

func TestIntegrationClaimDueRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	var batch []*store.Outbound
	for i := 0; i < 10; i++ {
		batch = append(batch, outboundEvent(fmt.Sprintf("limited-%d", i)))
	}
	if err := s.Enqueue(ctx, batch); err != nil {
		t.Fatal(err)
	}

	got, err := s.ClaimDue(ctx, 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("claimed %d, want the limit of 4", len(got))
	}
}

// The payload must survive the round trip byte for byte: it is what gets
// signed, so a mutation would break the consumer's verification.
func TestIntegrationPayloadRoundTripsExactly(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	original := []byte(`{"activityType":"pr_merged","actor":{"githubId":101,"login":"alice"},"metadata":{"additions":42}}`)
	e := outboundEvent("round-trip")
	e.Payload = original

	if err := s.Enqueue(ctx, []*store.Outbound{e}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimDue(ctx, 1, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim: got %d, err %v", len(got), err)
	}
	if string(got[0].Payload) != string(original) {
		t.Fatalf("payload changed in storage:\n got %s\nwant %s", got[0].Payload, original)
	}
}

func TestIntegrationRecordActivityRejectsDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	activity := func() *store.Activity {
		return &store.Activity{
			DedupeKey:  "pr_merged:6001",
			Type:       store.PRMerged,
			Actor:      store.Actor{GithubID: 101, Login: "alice"},
			Repo:       store.Repo{Owner: "o", Name: "r", FullName: "o/r", GithubID: 7001},
			Subject:    store.Subject{Kind: "pull_request", Number: 2, GithubID: 6001},
			OccurredAt: time.Now().UTC(),
		}
	}

	if err := s.RecordActivity(ctx, activity()); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// This is what makes a GitHub redelivery safe.
	err := s.RecordActivity(ctx, activity())
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second insert = %v, want store.ErrDuplicate", err)
	}
}

func TestIntegrationMarkDeliveryDetectsRedelivery(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	fresh, err := s.MarkDelivery(ctx, "delivery-abc", "pull_request")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Error("a new delivery id should report fresh")
	}

	fresh, err = s.MarkDelivery(ctx, "delivery-abc", "pull_request")
	if err != nil {
		t.Fatalf("a repeated delivery id must not error: %v", err)
	}
	if fresh {
		t.Error("a repeated delivery id must report not fresh")
	}
}

// Answering one mention twice would post two reviews, so the claim has to be
// atomic against the real unique index.
func TestIntegrationClaimReviewRunIsExclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	run := func() *store.ReviewRun {
		return &store.ReviewRun{
			Repo:             store.Repo{Owner: "o", Name: "r", FullName: "o/r"},
			PR:               store.PRRef{Number: 2, GithubID: 6001, HeadSHA: "deadbeef"},
			Trigger:          "mention",
			CommandCommentID: 8888,
			Status:           store.RunPending,
			StartedAt:        time.Now().UTC(),
		}
	}

	claimed, err := s.ClaimReviewRun(ctx, run())
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("the first claim should succeed")
	}

	claimed, err = s.ClaimReviewRun(ctx, run())
	if err != nil {
		t.Fatalf("a repeated claim must not error: %v", err)
	}
	if claimed {
		t.Fatal("the same mention was claimed twice, so it would be reviewed twice")
	}
}

// A new head commit is a new review opportunity; relabelling an unchanged PR
// is not.
func TestIntegrationLabelClaimIsPerHeadSHA(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	labelRun := func(sha string) *store.ReviewRun {
		return &store.ReviewRun{
			Repo:      store.Repo{FullName: "o/r"},
			PR:        store.PRRef{Number: 2, GithubID: 6001, HeadSHA: sha},
			Trigger:   "label",
			Status:    store.RunPending,
			StartedAt: time.Now().UTC(),
		}
	}

	if ok, err := s.ClaimReviewRun(ctx, labelRun("sha-one")); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimReviewRun(ctx, labelRun("sha-one")); err != nil || ok {
		t.Fatalf("relabelling an unchanged PR should not re-review: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimReviewRun(ctx, labelRun("sha-two")); err != nil || !ok {
		t.Fatalf("a new head commit should be claimable: ok=%v err=%v", ok, err)
	}
}

func TestIntegrationCompleteReviewRunPersistsUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	run := &store.ReviewRun{
		Repo:             store.Repo{FullName: "o/r"},
		PR:               store.PRRef{Number: 2, GithubID: 6001, HeadSHA: "deadbeef"},
		Trigger:          "mention",
		CommandCommentID: 8888,
		Status:           store.RunPending,
		StartedAt:        time.Now().UTC(),
	}
	if ok, err := s.ClaimReviewRun(ctx, run); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	run.Status = store.RunPosted
	run.ReviewID = 555
	run.FindingsPosted = 2
	run.FindingsDropped = 5
	run.Usage = store.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 800, ToolCalls: 2}
	run.EstimatedCostUSD = 0.0142
	run.FinishedAt = time.Now().UTC()

	if err := s.CompleteReviewRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	var doc runDoc
	if err := s.db.Collection(CollRuns).
		FindOne(ctx, bson.D{{Key: "triggerKey", Value: "comment:8888"}}).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != store.RunPosted {
		t.Errorf("status = %q, want %q", doc.Status, store.RunPosted)
	}
	if doc.Usage.InputTokens != 1000 || doc.Usage.CacheReadInputTokens != 800 {
		t.Errorf("usage not persisted: %+v", doc.Usage)
	}
	if doc.EstimatedCostUSD != 0.0142 {
		t.Errorf("estimatedCostUsd = %v", doc.EstimatedCostUSD)
	}
}

// `explain` must reveal the latest review, not a mixture of every review on
// the pull request.
func TestIntegrationLatestReviewFindingsPicksNewestReview(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	finding := func(reviewID int64, concept string, at time.Time) *store.Finding {
		return &store.Finding{
			Actor:    store.Actor{GithubID: 101, Login: "alice"},
			Repo:     store.Repo{FullName: "o/r"},
			PR:       store.PRRef{Number: 2, GithubID: 6001},
			ReviewID: reviewID,
			Concept:  concept,
			Severity: store.SeverityHigh,
			Path:     "a.go", Line: 2, Side: "RIGHT",
			Title: "t", Explanation: "why", SuggestedFix: "fix",
			PostedAt: at,
		}
	}

	now := time.Now().UTC()
	if err := s.SaveFindings(ctx, []*store.Finding{
		finding(100, "old concept", now.Add(-2*time.Hour)),
		finding(200, "new concept", now),
		finding(200, "also new", now),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestReviewFindings(ctx, 6001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d findings, want the 2 from review 200", len(got))
	}
	for _, f := range got {
		if f.ReviewID != 200 {
			t.Errorf("finding from review %d leaked into the latest set", f.ReviewID)
		}
		if f.ID == "" {
			t.Error("finding id is empty, so it could not be marked revealed")
		}
		// The withheld text is what explain reveals, so it must survive.
		if f.Explanation == "" || f.SuggestedFix == "" {
			t.Errorf("withheld text lost in storage: %+v", f)
		}
	}

	// A pull request with no reviews is not an error.
	none, err := s.LatestReviewFindings(ctx, 999999)
	if err != nil {
		t.Fatalf("unknown pull request: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d findings for an unknown PR", len(none))
	}
}

func TestIntegrationMarkFindingsRevealed(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.SaveFindings(ctx, []*store.Finding{{
		Actor: store.Actor{GithubID: 101, Login: "alice"},
		Repo:  store.Repo{FullName: "o/r"},
		PR:    store.PRRef{GithubID: 6001},
		// A comment id makes the row unique, matching a posted comment.
		CommentID: 98765,
		ReviewID:  200,
		Concept:   "error handling", Severity: store.SeverityHigh,
		Path: "a.go", Line: 2, PostedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}

	found, err := s.LatestReviewFindings(ctx, 6001)
	if err != nil || len(found) != 1 {
		t.Fatalf("load: got %d, err %v", len(found), err)
	}
	if found[0].Revealed {
		t.Error("a new finding should not be marked revealed")
	}

	if err := s.MarkFindingsRevealed(ctx, []string{found[0].ID}); err != nil {
		t.Fatal(err)
	}

	after, err := s.LatestReviewFindings(ctx, 6001)
	if err != nil {
		t.Fatal(err)
	}
	if !after[0].Revealed {
		t.Error("the finding should be marked revealed")
	}

	// A malformed id must be ignored rather than fail the reveal.
	if err := s.MarkFindingsRevealed(ctx, []string{"not-an-object-id"}); err != nil {
		t.Errorf("a bad id should be skipped, not returned as an error: %v", err)
	}
}

// Posting the same comment twice must not create two findings.
func TestIntegrationSaveFindingsDedupesOnCommentID(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	finding := func() *store.Finding {
		return &store.Finding{
			Actor:    store.Actor{GithubID: 101, Login: "alice"},
			Repo:     store.Repo{FullName: "o/r"},
			PR:       store.PRRef{GithubID: 6001},
			ReviewID: 200, CommentID: 98765,
			Concept: "error handling", Severity: store.SeverityHigh,
			Path: "a.go", Line: 2, PostedAt: time.Now().UTC(),
		}
	}

	if err := s.SaveFindings(ctx, []*store.Finding{finding()}); err != nil {
		t.Fatal(err)
	}
	// Unordered insert plus a unique index: the duplicate is ignored rather
	// than failing the batch.
	if err := s.SaveFindings(ctx, []*store.Finding{finding()}); err != nil {
		t.Fatalf("a duplicate comment id must not fail: %v", err)
	}

	got, err := s.LatestReviewFindings(ctx, 6001)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
}

func TestIntegrationSaveFindingsEmptyIsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	if err := s.SaveFindings(ctx, nil); err != nil {
		t.Errorf("saving no findings should be a no-op: %v", err)
	}
	if err := s.Enqueue(ctx, nil); err != nil {
		t.Errorf("enqueuing no events should be a no-op: %v", err)
	}
	if err := s.MarkFindingsRevealed(ctx, nil); err != nil {
		t.Errorf("revealing no findings should be a no-op: %v", err)
	}
}

// EnsureIndexes runs on every boot when enabled, so it has to be idempotent.
func TestIntegrationEnsureIndexesIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	for i := 0; i < 3; i++ {
		if err := s.EnsureIndexes(ctx, 24*time.Hour); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	// The unique indexes the correctness of everything else depends on.
	for coll, want := range map[string]string{
		CollActivities: "dedupeKey_unique",
		CollRuns:       "triggerKey_unique",
		CollOutbox:     "idempotencyKey_unique",
		CollFindings:   "commentId_unique",
	} {
		cur, err := s.db.Collection(coll).Indexes().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for cur.Next(ctx) {
			var idx struct {
				Name string `bson:"name"`
			}
			if err := cur.Decode(&idx); err == nil {
				names = append(names, idx.Name)
			}
		}
		_ = cur.Close(ctx)

		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing index %q; have %v", coll, want, names)
		}
	}
}
