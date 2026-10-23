package activity

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/sos-academy/shuriken/internal/store"
)

const botHandle = "shuriken-bot"

var (
	alice = &gh.User{ID: new(int64(101)), Login: new("alice"), Type: new("User")}
	bob   = &gh.User{ID: new(int64(202)), Login: new("bob"), Type: new("User")}
	robot = &gh.User{ID: new(int64(999)), Login: new("shuriken-bot[bot]"), Type: new("Bot")}

	repo = &gh.Repository{
		ID:       new(int64(7001)),
		Name:     new("dojo"),
		FullName: new("sos-academy/dojo"),
		Owner:    &gh.User{Login: new("sos-academy")},
	}

	created = gh.Timestamp{Time: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
)

func issue(id int64, number int, user *gh.User, isPR bool) *gh.Issue {
	i := &gh.Issue{
		ID:        new(id),
		Number:    new(number),
		User:      user,
		HTMLURL:   new("https://github.com/sos-academy/dojo/issues/1"),
		CreatedAt: &created,
	}
	if isPR {
		i.PullRequestLinks = &gh.PullRequestLinks{URL: new("https://api.github.com/pr/1")}
	}
	return i
}

func pull(id int64, number int, user *gh.User) *gh.PullRequest {
	return &gh.PullRequest{
		ID:        new(id),
		Number:    new(number),
		User:      user,
		HTMLURL:   new("https://github.com/sos-academy/dojo/pull/2"),
		CreatedAt: &created,
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name     string
		payload  any
		wantLen  int
		wantType store.ActivityType
		wantKey  string
		wantUser string
	}{
		{
			name: "issue opened",
			payload: &gh.IssuesEvent{
				Action: new("opened"), Repo: repo, Issue: issue(5001, 1, alice, false),
			},
			wantLen: 1, wantType: store.IssueOpened, wantKey: "issue_opened:5001", wantUser: "alice",
		},
		{
			name: "issue assigned records the assignee, not the assigner",
			payload: &gh.IssuesEvent{
				Action: new("assigned"), Repo: repo, Issue: issue(5001, 1, alice, false),
				Assignee: bob, Sender: alice,
			},
			wantLen: 1, wantType: store.IssueAssigned, wantKey: "issue_assigned:5001:202", wantUser: "bob",
		},
		{
			name: "issue closed is not tracked in v0",
			payload: &gh.IssuesEvent{
				Action: new("closed"), Repo: repo, Issue: issue(5001, 1, alice, false),
			},
			wantLen: 0,
		},
		{
			name: "pr opened",
			payload: &gh.PullRequestEvent{
				Action: new("opened"), Repo: repo, PullRequest: pull(6001, 2, alice),
			},
			wantLen: 1, wantType: store.PROpened, wantKey: "pr_opened:6001", wantUser: "alice",
		},
		{
			name: "pr merged credits the author",
			payload: &gh.PullRequestEvent{
				Action: new("closed"), Repo: repo,
				PullRequest: func() *gh.PullRequest {
					pr := pull(6001, 2, alice)
					pr.Merged = new(true)
					pr.MergedAt = &created
					pr.MergedBy = bob
					return pr
				}(),
			},
			wantLen: 1, wantType: store.PRMerged, wantKey: "pr_merged:6001", wantUser: "alice",
		},
		{
			name: "pr closed unmerged",
			payload: &gh.PullRequestEvent{
				Action: new("closed"), Repo: repo,
				PullRequest: func() *gh.PullRequest {
					pr := pull(6001, 2, alice)
					pr.Merged = new(false)
					pr.ClosedAt = &created
					return pr
				}(),
			},
			wantLen: 1, wantType: store.PRClosedUnmerged, wantKey: "pr_closed_unmerged:6001", wantUser: "alice",
		},
		{
			name: "pr labeled produces no activity",
			payload: &gh.PullRequestEvent{
				Action: new("labeled"), Repo: repo, PullRequest: pull(6001, 2, alice),
				Label: &gh.Label{Name: "needs-review"},
			},
			wantLen: 0,
		},
		{
			name: "review submitted by a peer",
			payload: &gh.PullRequestReviewEvent{
				Action: new("submitted"), Repo: repo, PullRequest: pull(6001, 2, alice),
				Review: &gh.PullRequestReview{
					ID: new(int64(7777)), User: bob, State: new("COMMENTED"),
					SubmittedAt: &created, HTMLURL: new("https://github.com/r/1#r7777"),
				},
			},
			wantLen: 1, wantType: store.PRReviewSubmitted, wantKey: "pr_review_submitted:7777", wantUser: "bob",
		},
		{
			// Reviewing your own pull request is not peer review.
			name: "self review is ignored",
			payload: &gh.PullRequestReviewEvent{
				Action: new("submitted"), Repo: repo, PullRequest: pull(6001, 2, alice),
				Review: &gh.PullRequestReview{
					ID: new(int64(7777)), User: alice, State: new("COMMENTED"), SubmittedAt: &created,
				},
			},
			wantLen: 0,
		},
		{
			name: "issue comment mentioning the bot",
			payload: &gh.IssueCommentEvent{
				Action: new("created"), Repo: repo, Issue: issue(5001, 1, alice, false),
				Comment: &gh.IssueComment{
					ID: new(int64(8888)), User: bob, CreatedAt: &created,
					Body: new("@shuriken-bot I claimed this"),
				},
			},
			wantLen: 1, wantType: store.IssueComment, wantKey: "issue_comment:8888", wantUser: "bob",
		},
		{
			// Comments are only recorded when they talk to us.
			name: "issue comment without a mention is ignored",
			payload: &gh.IssueCommentEvent{
				Action: new("created"), Repo: repo, Issue: issue(5001, 1, alice, false),
				Comment: &gh.IssueComment{
					ID: new(int64(8888)), User: bob, CreatedAt: &created,
					Body: new("looks good to me"),
				},
			},
			wantLen: 0,
		},
		{
			// GitHub delivers PR conversation comments as issue_comment.
			name: "pr conversation comment becomes pr_comment",
			payload: &gh.IssueCommentEvent{
				Action: new("created"), Repo: repo, Issue: issue(5001, 2, alice, true),
				Comment: &gh.IssueComment{
					ID: new(int64(8889)), User: bob, CreatedAt: &created,
					Body: new("@shuriken-bot review"),
				},
			},
			wantLen: 1, wantType: store.PRComment, wantKey: "pr_comment:8889", wantUser: "bob",
		},
		{
			name: "inline review comment becomes pr_comment",
			payload: &gh.PullRequestReviewCommentEvent{
				Action: new("created"), Repo: repo, PullRequest: pull(6001, 2, alice),
				Comment: &gh.PullRequestComment{
					ID: new(int64(9001)), User: bob, CreatedAt: &created,
					Body: new("@shuriken-bot is this right?"),
				},
			},
			wantLen: 1, wantType: store.PRComment, wantKey: "pr_comment:9001", wantUser: "bob",
		},
		{
			name: "comment edits are ignored",
			payload: &gh.IssueCommentEvent{
				Action: new("edited"), Repo: repo, Issue: issue(5001, 1, alice, false),
				Comment: &gh.IssueComment{
					ID: new(int64(8888)), User: bob, Body: new("@shuriken-bot review"),
				},
			},
			wantLen: 0,
		},
		{
			name:    "unsupported event",
			payload: &gh.PushEvent{},
			wantLen: 0,
		},
		{
			name:    "nil payload",
			payload: nil,
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(Input{
				Payload: tc.payload, DeliveryID: "delivery-1", BotHandle: botHandle,
			})
			if len(got) != tc.wantLen {
				t.Fatalf("got %d activities, want %d", len(got), tc.wantLen)
			}
			if tc.wantLen == 0 {
				return
			}
			a := got[0]
			if a.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", a.Type, tc.wantType)
			}
			if a.DedupeKey != tc.wantKey {
				t.Errorf("DedupeKey = %q, want %q", a.DedupeKey, tc.wantKey)
			}
			if a.Actor.Login != tc.wantUser {
				t.Errorf("Actor.Login = %q, want %q", a.Actor.Login, tc.wantUser)
			}
			if a.Repo.FullName != "sos-academy/dojo" {
				t.Errorf("Repo.FullName = %q", a.Repo.FullName)
			}
			if a.DeliveryID != "delivery-1" {
				t.Errorf("DeliveryID = %q", a.DeliveryID)
			}
			if a.OccurredAt.IsZero() {
				t.Error("OccurredAt must always be set")
			}
		})
	}
}

// Shuriken must never record or react to a bot, including itself, or two bots
// can talk to each other forever.
func TestNormalizeIgnoresBots(t *testing.T) {
	payloads := []any{
		&gh.IssuesEvent{Action: new("opened"), Repo: repo, Issue: issue(1, 1, robot, false)},
		&gh.PullRequestEvent{Action: new("opened"), Repo: repo, PullRequest: pull(1, 1, robot)},
		&gh.IssueCommentEvent{
			Action: new("created"), Repo: repo, Issue: issue(1, 1, alice, false),
			Comment: &gh.IssueComment{ID: new(int64(2)), User: robot, Body: new("@shuriken-bot review")},
		},
		&gh.PullRequestReviewEvent{
			Action: new("submitted"), Repo: repo, PullRequest: pull(1, 1, alice),
			Review: &gh.PullRequestReview{ID: new(int64(3)), User: robot, State: new("COMMENTED")},
		},
		&gh.IssuesEvent{
			Action: new("assigned"), Repo: repo, Issue: issue(1, 1, alice, false), Assignee: robot,
		},
	}

	for i, p := range payloads {
		if got := Normalize(Input{Payload: p, BotHandle: botHandle}); len(got) != 0 {
			t.Errorf("payload %d: bot activity recorded: %+v", i, got[0])
		}
	}
}

// The dedupe key is what makes redelivery safe, so identical events must
// produce identical keys and distinct events must not collide.
func TestDedupeKeyStability(t *testing.T) {
	ev := &gh.PullRequestEvent{
		Action: new("opened"), Repo: repo, PullRequest: pull(6001, 2, alice),
	}

	first := Normalize(Input{Payload: ev, DeliveryID: "d1", BotHandle: botHandle})
	second := Normalize(Input{Payload: ev, DeliveryID: "d2-redelivery", BotHandle: botHandle})

	if first[0].DedupeKey != second[0].DedupeKey {
		t.Fatalf("redelivery produced a different key: %q vs %q",
			first[0].DedupeKey, second[0].DedupeKey)
	}

	// A merge of the same pull request is a different activity.
	merged := &gh.PullRequestEvent{
		Action: new("closed"), Repo: repo,
		PullRequest: func() *gh.PullRequest {
			pr := pull(6001, 2, alice)
			pr.Merged = new(true)
			pr.MergedAt = &created
			return pr
		}(),
	}
	m := Normalize(Input{Payload: merged, BotHandle: botHandle})
	if m[0].DedupeKey == first[0].DedupeKey {
		t.Fatal("open and merge of the same PR must not share a dedupe key")
	}
}

// Two different people claiming the same issue are two activities.
func TestAssignDedupeKeyIsPerAssignee(t *testing.T) {
	mk := func(u *gh.User) string {
		got := Normalize(Input{Payload: &gh.IssuesEvent{
			Action: new("assigned"), Repo: repo,
			Issue: issue(5001, 1, alice, false), Assignee: u,
		}, BotHandle: botHandle})
		return got[0].DedupeKey
	}
	if mk(alice) == mk(bob) {
		t.Fatal("assignments to different people must have different keys")
	}
}

// Timestamps come from the payload, not from ingest time, so a redelivery days
// later still records when the thing actually happened.
func TestOccurredAtComesFromPayload(t *testing.T) {
	got := Normalize(Input{Payload: &gh.PullRequestEvent{
		Action: new("opened"), Repo: repo, PullRequest: pull(6001, 2, alice),
	}, BotHandle: botHandle})

	if !got[0].OccurredAt.Equal(created.Time) {
		t.Fatalf("OccurredAt = %v, want %v", got[0].OccurredAt, created.Time)
	}
}

// Activities must never carry comment bodies or diffs.
func TestNormalizeStoresNoBodies(t *testing.T) {
	const secret = "please do not store this body text"
	got := Normalize(Input{Payload: &gh.IssueCommentEvent{
		Action: new("created"), Repo: repo, Issue: issue(5001, 1, alice, false),
		Comment: &gh.IssueComment{
			ID: new(int64(8888)), User: bob, CreatedAt: &created,
			Body: new("@shuriken-bot " + secret),
		},
	}, BotHandle: botHandle})

	if len(got) != 1 {
		t.Fatal("expected one activity")
	}
	for k, v := range got[0].Metadata {
		if s, ok := v.(string); ok && s == secret {
			t.Fatalf("metadata key %q leaked the comment body", k)
		}
	}
	if got[0].Object.URL == "" && got[0].Object.GithubID == 0 {
		t.Error("a comment activity should reference the comment by id or URL")
	}
}
