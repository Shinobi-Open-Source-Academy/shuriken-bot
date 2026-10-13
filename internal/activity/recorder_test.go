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

func TestRecorderLinksKnownUser(t *testing.T) {
	fake := storetest.New()
	fake.AddUser("user-abc", 101, "alice")
	fake.Projects["sos-academy/dojo"] = "project-xyz"

	rec := NewRecorder(fake, quietLogger(), false)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("Record() error: %v", err)
	}

	if fake.ActivityCount() != 1 {
		t.Fatalf("recorded %d activities, want 1", fake.ActivityCount())
	}
	got := fake.Activities[0]
	if got.UserID != "user-abc" {
		t.Errorf("UserID = %q, want user-abc", got.UserID)
	}
	if got.Unlinked {
		t.Error("a matched user must not be flagged unlinked")
	}
	if got.ProjectID != "project-xyz" {
		t.Errorf("ProjectID = %q, want project-xyz", got.ProjectID)
	}
}

// The common case at launch: nobody has linked their GitHub account yet. The
// activity must still be recorded, flagged for later linking.
func TestRecorderFlagsUnlinkedUser(t *testing.T) {
	fake := storetest.New()
	rec := NewRecorder(fake, quietLogger(), false)

	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("Record() error: %v", err)
	}

	got := fake.Activities[0]
	if !got.Unlinked {
		t.Error("an unmatched user must be flagged unlinked")
	}
	if got.UserID != "" {
		t.Errorf("UserID = %q, want empty", got.UserID)
	}
	// The GitHub identity must survive so a later job can link it.
	if got.Actor.GithubID != 101 || got.Actor.Login != "alice" {
		t.Errorf("GitHub identity lost: %+v", got.Actor)
	}
}

func TestRecorderFallsBackToLoginMatch(t *testing.T) {
	fake := storetest.New()
	// A profile linked before the GitHub id was captured.
	fake.UsersByLogin["alice"] = &store.PlatformUser{ID: "user-abc", GithubLogin: "alice"}

	rec := NewRecorder(fake, quietLogger(), false)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatal(err)
	}
	if fake.Activities[0].UserID != "user-abc" {
		t.Errorf("login fallback did not resolve: %+v", fake.Activities[0])
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

// Losing the identity lookup must not lose the activity.
func TestRecorderRecordsWhenEnrichmentFails(t *testing.T) {
	fake := storetest.New()
	failing := &failEnrich{Fake: fake}

	rec := NewRecorder(failing, quietLogger(), false)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("Record() error: %v", err)
	}
	if fake.ActivityCount() != 1 {
		t.Fatal("activity lost when enrichment failed")
	}
	if !fake.Activities[0].Unlinked {
		t.Error("a failed lookup must leave the activity unlinked")
	}
}

func TestRecorderDryRunWritesNothing(t *testing.T) {
	fake := storetest.New()
	fake.AddUser("user-abc", 101, "alice")

	rec := NewRecorder(fake, quietLogger(), true)
	if err := rec.Record(context.Background(), []*store.Activity{sample()}); err != nil {
		t.Fatalf("Record() error: %v", err)
	}
	if fake.ActivityCount() != 0 {
		t.Fatalf("dry run wrote %d activities, want 0", fake.ActivityCount())
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

// failEnrich fails only the lookups, so writes still succeed.
type failEnrich struct {
	*storetest.Fake
}

func (f *failEnrich) FindPlatformUser(context.Context, int64, string) (*store.PlatformUser, error) {
	return nil, errors.New("lookup unavailable")
}
