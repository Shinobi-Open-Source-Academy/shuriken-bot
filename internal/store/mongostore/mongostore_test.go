package mongostore

import (
	"testing"

	"github.com/sos-academy/shuriken/internal/store"
)

// The platform stores githubRepo as a full URL, in whatever shape whoever
// created the project pasted.
func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://github.com/calcom/cal.com", "calcom/cal.com"},
		{"https://github.com/sos-academy/dojo", "sos-academy/dojo"},
		{"http://github.com/sos-academy/dojo", "sos-academy/dojo"},
		{"https://github.com/SOS-Academy/Dojo", "sos-academy/dojo"},
		{"https://github.com/sos-academy/dojo/", "sos-academy/dojo"},
		{"https://github.com/sos-academy/dojo.git", "sos-academy/dojo"},
		{"git@github.com:sos-academy/dojo.git", "sos-academy/dojo"},
		{"github.com/sos-academy/dojo", "sos-academy/dojo"},
		{"www.github.com/sos-academy/dojo", "sos-academy/dojo"},
		{"  https://github.com/sos-academy/dojo  ", "sos-academy/dojo"},
		// Extra path segments are ignored: a tree or blob URL still identifies
		// the repository.
		{"https://github.com/sos-academy/dojo/tree/main", "sos-academy/dojo"},

		// Unusable values must yield an empty key, never a partial one.
		{"", ""},
		{"not a url", ""},
		{"https://github.com/", ""},
		{"https://github.com/owner-only", ""},
		{"https://gitlab.com/x/y", ""},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeRepoURL(tc.in); got != tc.want {
				t.Fatalf("NormalizeRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A GitLab URL must not be mistaken for a GitHub repo just because it has two
// path segments.
func TestNormalizeRepoURLRejectsOtherHosts(t *testing.T) {
	for _, in := range []string{
		"https://gitlab.com/group/project",
		"https://bitbucket.org/team/repo",
	} {
		if got := NormalizeRepoURL(in); got != "" {
			t.Errorf("NormalizeRepoURL(%q) = %q, want empty", in, got)
		}
	}
}

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

func TestRegexpQuote(t *testing.T) {
	// A payload carrying regex metacharacters in a login must not become a
	// pattern that matches other users.
	for _, in := range []string{".*", "a|b", "x$", "[a-z]", "a.b"} {
		got := regexpQuote(in)
		if got == in {
			t.Errorf("regexpQuote(%q) did not escape anything: %q", in, got)
		}
	}
	if got := regexpQuote("alice-dev"); got != "alice-dev" {
		t.Errorf("an ordinary login should pass through unchanged, got %q", got)
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
