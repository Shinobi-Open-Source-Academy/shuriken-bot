package mongostore

import (
	"testing"

	"github.com/sos-academy/shuriken/internal/store"
)

func TestTriggerKey(t *testing.T) {
	tests := []struct {
		name string
		run  store.ReviewRun
		want string
	}{
		{
			// A mention is keyed by the comment, so a redelivery is one review.
			name: "mention keyed by comment",
			run:  store.ReviewRun{CommandCommentID: 8888, PR: store.PRRef{GithubID: 1, HeadSHA: "abc"}},
			want: "comment:8888",
		},
		{
			// A label has no comment, so it is keyed by the head commit: a new
			// push re-reviews, relabelling an unchanged PR does not.
			name: "label keyed by head sha",
			run:  store.ReviewRun{PR: store.PRRef{GithubID: 6001, HeadSHA: "deadbeef"}},
			want: "label:6001:deadbeef",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := triggerKey(&tc.run); got != tc.want {
				t.Fatalf("triggerKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTriggerKeyDistinguishesHeadSHAs(t *testing.T) {
	a := triggerKey(&store.ReviewRun{PR: store.PRRef{GithubID: 1, HeadSHA: "aaa"}})
	b := triggerKey(&store.ReviewRun{PR: store.PRRef{GithubID: 1, HeadSHA: "bbb"}})
	if a == b {
		t.Fatal("a new head commit must be a new review opportunity")
	}
}

func TestSeverityRank(t *testing.T) {
	if store.SeverityHigh.Rank() <= store.SeverityMedium.Rank() {
		t.Error("high must outrank medium")
	}
	if store.SeverityMedium.Rank() <= store.SeverityLow.Rank() {
		t.Error("medium must outrank low")
	}
	if store.Severity("bogus").Valid() {
		t.Error("an unknown severity must not validate")
	}
	for _, s := range []store.Severity{store.SeverityHigh, store.SeverityMedium, store.SeverityLow} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
}
