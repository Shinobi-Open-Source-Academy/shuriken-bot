package publisher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sos-academy/shuriken/internal/store"
	"github.com/sos-academy/shuriken/internal/store/storetest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const secret = "a-consumer-webhook-secret"

// consumer records what it received and answers with a scripted status.
type consumer struct {
	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	statuses []int // consumed in order; the last repeats
}

func (c *consumer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.headers = append(c.headers, r.Header.Clone())
		i := len(c.bodies) - 1
		if i >= len(c.statuses) {
			i = len(c.statuses) - 1
		}
		status := c.statuses[i]
		c.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (c *consumer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func newPublisher(t *testing.T, c *consumer, fake *storetest.Fake, maxAttempts int) *Publisher {
	t.Helper()
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	return New(fake, quiet(), Options{
		URL: srv.URL, Secret: secret, Batch: 10,
		MaxAttempts: maxAttempts, Timeout: 2 * time.Second,
	})
}

func activity() *store.Activity {
	return &store.Activity{
		DedupeKey:  "pr_merged:6001",
		Type:       store.PRMerged,
		Actor:      store.Actor{GithubID: 101, Login: "alice"},
		Repo:       store.Repo{Owner: "sos-academy", Name: "dojo", FullName: "sos-academy/dojo"},
		Subject:    store.Subject{Kind: "pull_request", Number: 2, GithubID: 6001},
		OccurredAt: time.Now().UTC(),
	}
}

func seed(t *testing.T, fake *storetest.Fake, a *store.Activity) {
	t.Helper()
	e, err := ActivityEvent(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Enqueue(context.Background(), []*store.Outbound{e}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishesAndSigns(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusAccepted}}
	fake := storetest.New()
	seed(t, fake, activity())

	n, err := newPublisher(t, c, fake, 5).PublishNow(context.Background())
	if err != nil {
		t.Fatalf("PublishNow() error: %v", err)
	}
	if n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	if fake.PublishedCount() != 1 {
		t.Error("event not marked published")
	}

	// The signature must verify against the exact bytes sent, which is what a
	// consumer will check.
	body, hdr := c.bodies[0], c.headers[0]
	if got, want := hdr.Get(HeaderSignature), Sign(body, []byte(secret)); got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
	if hdr.Get(HeaderEvent) != store.EventActivityRecorded {
		t.Errorf("%s = %q", HeaderEvent, hdr.Get(HeaderEvent))
	}
	if hdr.Get(HeaderIdempotency) != "pr_merged:6001" {
		t.Errorf("%s = %q", HeaderIdempotency, hdr.Get(HeaderIdempotency))
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if env.Type != store.EventActivityRecorded || env.IdempotencyKey != "pr_merged:6001" {
		t.Errorf("envelope = %+v", env)
	}
	if env.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", env.Attempt)
	}

	var payload ActivityPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if payload.Actor.GithubID != 101 || payload.Actor.Login != "alice" {
		t.Errorf("actor = %+v", payload.Actor)
	}
}

// A wrong secret must not verify. This is the property the consumer relies on.
func TestSignatureRejectsWrongSecret(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	if Sign(body, []byte(secret)) == Sign(body, []byte("a-different-secret")) {
		t.Fatal("signatures must differ by secret")
	}
	if Sign(body, []byte(secret)) == Sign([]byte(`{"id":"2"}`), []byte(secret)) {
		t.Fatal("signatures must differ by body")
	}
}

// The payload must not carry any platform identity: that is the whole point of
// the GitHub-identity-only decision.
func TestPayloadCarriesNoPlatformIdentity(t *testing.T) {
	e, err := ActivityEvent(activity())
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(e.Payload, &generic); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"userId", "user", "unlinked", "projectId", "project"} {
		if _, present := generic[forbidden]; present {
			t.Errorf("payload leaked platform field %q: %v", forbidden, generic)
		}
	}
	actor, _ := generic["actor"].(map[string]any)
	if actor["githubId"] == nil {
		t.Error("payload must carry the GitHub id for the consumer to join on")
	}
}

func TestRetriesOnServerError(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusInternalServerError}}
	fake := storetest.New()
	seed(t, fake, activity())
	p := newPublisher(t, c, fake, 5)

	if n, _ := p.PublishNow(context.Background()); n != 0 {
		t.Fatal("a 500 must not count as delivered")
	}
	if fake.PublishedCount() != 0 {
		t.Error("event must not be marked published")
	}

	ev := fake.Outbound[0]
	if ev.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", ev.Attempts)
	}
	if ev.DeadLetteredAt != nil {
		t.Error("one failure must not dead-letter")
	}
	// Backed off into the future, so the next tick does not hammer it.
	if !ev.NextAttemptAt.After(time.Now()) {
		t.Error("NextAttemptAt should be in the future after a failure")
	}
}

func TestDeadLettersAfterMaxAttempts(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusInternalServerError}}
	fake := storetest.New()
	seed(t, fake, activity())
	p := newPublisher(t, c, fake, 3)

	for i := 0; i < 3; i++ {
		// Clear the backoff so the event is due again immediately.
		fake.Outbound[0].NextAttemptAt = time.Now().Add(-time.Second)
		if _, err := p.PublishNow(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	ev := fake.Outbound[0]
	if ev.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", ev.Attempts)
	}
	if ev.DeadLetteredAt == nil {
		t.Fatal("event should be dead-lettered once attempts are exhausted")
	}

	// A dead-lettered event is not retried, and is visible on /healthz.
	before := c.count()
	fake.Outbound[0].NextAttemptAt = time.Now().Add(-time.Second)
	if _, err := p.PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.count() != before {
		t.Error("a dead-lettered event must not be retried")
	}

	_, dead, err := fake.OutboxDepth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dead != 1 {
		t.Errorf("dead-lettered depth = %d, want 1", dead)
	}
}

// A 4xx other than 429 means the consumer rejected the event itself; retrying
// an unchanged body cannot help.
func TestRejectionDeadLettersImmediately(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusBadRequest}}
	fake := storetest.New()
	seed(t, fake, activity())

	if _, err := newPublisher(t, c, fake, 10).PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Outbound[0].DeadLetteredAt == nil {
		t.Fatal("a 400 should dead-letter without burning ten attempts")
	}
	if c.count() != 1 {
		t.Errorf("made %d requests, want 1", c.count())
	}
}

// 429 is a rate limit, not a rejection, so it retries.
func TestRateLimitRetries(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusTooManyRequests}}
	fake := storetest.New()
	seed(t, fake, activity())

	if _, err := newPublisher(t, c, fake, 5).PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Outbound[0].DeadLetteredAt != nil {
		t.Error("429 must not dead-letter on the first attempt")
	}
	if fake.Outbound[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", fake.Outbound[0].Attempts)
	}
}

func TestEventuallySucceedsAfterFailure(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}}
	fake := storetest.New()
	seed(t, fake, activity())
	p := newPublisher(t, c, fake, 5)

	_, _ = p.PublishNow(context.Background())
	fake.Outbound[0].NextAttemptAt = time.Now().Add(-time.Second)
	n, err := p.PublishNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("delivered %d on the retry, want 1", n)
	}

	// The retry must advertise itself as such.
	var env Envelope
	if err := json.Unmarshal(c.bodies[1], &env); err != nil {
		t.Fatal(err)
	}
	if env.Attempt != 2 {
		t.Errorf("Attempt = %d on the retry, want 2", env.Attempt)
	}
	// Same identity across attempts, so the consumer can dedupe.
	if env.IdempotencyKey != "pr_merged:6001" {
		t.Errorf("IdempotencyKey changed between attempts: %q", env.IdempotencyKey)
	}
}

// Enqueue is keyed, so a retried job cannot queue the same event twice.
func TestEnqueueIsIdempotent(t *testing.T) {
	fake := storetest.New()
	for i := 0; i < 3; i++ {
		seed(t, fake, activity())
	}
	if len(fake.Outbound) != 1 {
		t.Fatalf("queued %d events for the same activity, want 1", len(fake.Outbound))
	}
}

// Two findings at different places must not collide on one key.
func TestFindingEventKeysAreDistinct(t *testing.T) {
	base := store.Finding{
		Actor: store.Actor{GithubID: 101, Login: "alice"},
		PR:    store.PRRef{GithubID: 6001},
		Path:  "a.go", Line: 2, Concept: "error handling",
		Severity: store.SeverityHigh, PostedAt: time.Now().UTC(),
	}
	other := base
	other.Line = 40

	a, err := FindingEvent(&base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FindingEvent(&other)
	if err != nil {
		t.Fatal(err)
	}
	if a.IdempotencyKey == b.IdempotencyKey {
		t.Fatalf("distinct findings share a key: %q", a.IdempotencyKey)
	}

	// With a comment id present the key is pinned to it.
	withID := base
	withID.CommentID = 98765
	c, err := FindingEvent(&withID)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdempotencyKey != "finding:98765" {
		t.Errorf("IdempotencyKey = %q, want finding:98765", c.IdempotencyKey)
	}
}

func TestDisabledWithoutURL(t *testing.T) {
	p := New(storetest.New(), quiet(), Options{})
	if p.Enabled() {
		t.Error("a publisher with no URL must report disabled")
	}
	if _, err := p.PublishNow(context.Background()); err == nil {
		t.Error("PublishNow should refuse when disabled")
	}
	// Run must return rather than spin.
	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return for a disabled publisher")
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	c := &consumer{statuses: []int{http.StatusOK}}
	fake := storetest.New()
	seed(t, fake, activity())

	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	p := New(fake, quiet(), Options{
		URL: srv.URL, Secret: secret, Batch: 10, MaxAttempts: 5, DryRun: true,
	})

	if _, err := p.PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.count() != 0 {
		t.Error("dry run must not contact the consumer")
	}
	if fake.PublishedCount() != 0 {
		t.Error("dry run must not mark anything published")
	}
}

func TestBackoffIsBoundedAndIncreasing(t *testing.T) {
	p := New(storetest.New(), quiet(), Options{URL: "http://x", Secret: "s"})
	prev := time.Duration(0)
	for attempts := 0; attempts < 12; attempts++ {
		d := p.backoff(attempts)
		if d <= 0 {
			t.Fatalf("backoff(%d) = %v, must be positive", attempts, d)
		}
		if d > 30*time.Minute {
			t.Fatalf("backoff(%d) = %v, exceeds the ceiling", attempts, d)
		}
		if attempts > 0 && d < prev {
			t.Fatalf("backoff decreased at %d: %v then %v", attempts, prev, d)
		}
		prev = d
	}
}
