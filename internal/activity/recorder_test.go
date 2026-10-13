package activity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sos-academy/shuriken/internal/store"
	"github.com/sos-academy/shuriken/internal/store/storetest"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sample() *store.Activity {
	return &store.Activity{
		DedupeKey:  "pr_opened:6001",
		Type:       store.PROpened,
		Actor:      store.Actor{GithubID: 101, Login: "alice"},
		Repo:       store.Repo{Owner: "sos-academy", Name: "dojo", FullName: "sos-academy/dojo"},
		Subject:    store.Subject{Kind: "pull_request", Number: 2, GithubID: 6001},
		OccurredAt: time.Now().UTC(),
	}
}

// A redelivered event must not create a second row, and must not look like a
// failure to the caller.
func TestRecorderIsIdempotent(t *testing.T) {
	fake := storetest.New()
	rec := NewRecorder(fake, quietLogger(), false)

	for i := 0; i < 3; i++ {
		if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if fake.ActivityCount() != 1 {
		t.Fatalf("recorded %d activities, want 1", fake.ActivityCount())
	}
}

func TestRecorderDryRunWritesNothing(t *testing.T) {
	fake := storetest.New()

	rec := NewRecorder(fake, quietLogger(), true)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("Record() error: %v", err)
	}
	if fake.ActivityCount() != 0 {
		t.Fatalf("dry run wrote %d activities, want 0", fake.ActivityCount())
	}
	if len(fake.Outbound) != 0 {
		t.Fatalf("dry run queued %d events, want 0", len(fake.Outbound))
	}
}

func TestRecorderSurfacesWriteErrors(t *testing.T) {
	fake := storetest.New()
	fake.Err = errors.New("mongo is down")

	rec := NewRecorder(fake, quietLogger(), false)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err == nil {
		t.Fatal("a write failure must be reported so the delivery is retried")
	}
}

func TestRecorderHandlesNilStore(t *testing.T) {
	rec := NewRecorder(nil, quietLogger(), true)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("a nil store in dry run must be safe: %v", err)
	}
}

// Every recorded activity must be queued for delivery: the webhook is the only
// way anything outside Shuriken learns about it.
func TestRecorderEnqueuesForDelivery(t *testing.T) {
	fake := storetest.New()
	rec := NewRecorder(fake, quietLogger(), false)

	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("Record() error: %v", err)
	}

	if len(fake.Outbound) != 1 {
		t.Fatalf("queued %d events, want 1", len(fake.Outbound))
	}
	e := fake.Outbound[0]
	if e.Type != store.EventActivityRecorded {
		t.Errorf("Type = %q, want %q", e.Type, store.EventActivityRecorded)
	}
	// Reusing the activity's dedupe key means a redelivery cannot double-send.
	if e.IdempotencyKey != "pr_opened:6001" {
		t.Errorf("IdempotencyKey = %q, want the activity dedupe key", e.IdempotencyKey)
	}
	if len(e.Payload) == 0 {
		t.Error("event payload is empty")
	}
}

// A redelivery must not queue a second event.
func TestRecorderEnqueueIsIdempotent(t *testing.T) {
	fake := storetest.New()
	rec := NewRecorder(fake, quietLogger(), false)

	for i := 0; i < 3; i++ {
		if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if fake.ActivityCount() != 1 {
		t.Errorf("recorded %d activities, want 1", fake.ActivityCount())
	}
	if len(fake.Outbound) != 1 {
		t.Errorf("queued %d events, want 1", len(fake.Outbound))
	}
}

// An event queued for an activity that was never stored would be a lie, so a
// failed write must not enqueue.
func TestRecorderDoesNotEnqueueWhenWriteFails(t *testing.T) {
	fake := storetest.New()
	fake.Err = errors.New("mongo is down")

	rec := NewRecorder(fake, quietLogger(), false)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err == nil {
		t.Fatal("a write failure must be reported so the delivery is retried")
	}
	if len(fake.Outbound) != 0 {
		t.Errorf("queued %d events despite the write failing", len(fake.Outbound))
	}
}

// Shuriken records the GitHub actor and nothing else: no platform identity is
// resolved or stored.
func TestRecorderStoresGitHubIdentityOnly(t *testing.T) {
	fake := storetest.New()
	rec := NewRecorder(fake, quietLogger(), false)

	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatal(err)
	}
	got := fake.Activities[0]
	if got.Actor.GithubID != 101 || got.Actor.Login != "alice" {
		t.Errorf("GitHub identity not preserved: %+v", got.Actor)
	}
}
