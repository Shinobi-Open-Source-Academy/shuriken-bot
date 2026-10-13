package webhook

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	gh "github.com/google/go-github/v92/github"

	"github.com/sos-academy/shuriken/internal/activity"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/review"
	"github.com/sos-academy/shuriken/internal/store/storetest"
)

type noClients struct{}

func (noClients) For(int64) (ghc.Client, error) { return nil, errors.New("not used") }

func testRouter() *AppRouter {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := storetest.New()
	reviews := review.NewService(noClients{}, nil, fake, log, review.Options{BotHandle: "shuriken-bot"})
	return NewRouter(
		activity.NewRecorder(fake, log, true),
		reviews, noClients{}, "shuriken-bot", "needs-review", log, true,
	)
}

var (
	user  = &gh.User{ID: gh.Ptr(int64(101)), Login: gh.Ptr("alice"), Type: gh.Ptr("User")}
	bot   = &gh.User{ID: gh.Ptr(int64(9)), Login: gh.Ptr("shuriken-bot[bot]"), Type: gh.Ptr("Bot")}
	rrepo = &gh.Repository{
		ID: gh.Ptr(int64(7001)), Name: gh.Ptr("dojo"), FullName: gh.Ptr("sos-academy/dojo"),
		Owner: &gh.User{Login: gh.Ptr("sos-academy")},
	}
)

func comment(id int64, u *gh.User, body string) *gh.IssueComment {
	return &gh.IssueComment{ID: gh.Ptr(id), User: u, Body: gh.Ptr(body)}
}

func rissue(number int, isPR bool) *gh.Issue {
	i := &gh.Issue{ID: gh.Ptr(int64(5001)), Number: gh.Ptr(number), User: user}
	if isPR {
		i.PullRequestLinks = &gh.PullRequestLinks{URL: gh.Ptr("https://api.github.com/pr")}
	}
	return i
}

// kinds reduces the routed tasks to their name prefix, which identifies the
// kind of work without depending on repo names or numbers.
func kinds(r *AppRouter, d Delivery) []string {
	tasks := r.Route(d)
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if i := strings.IndexByte(t.Name, ':'); i >= 0 {
			out = append(out, t.Name[:i])
			continue
		}
		out = append(out, t.Name)
	}
	return out
}

func TestRouteCommands(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    []string
	}{
		{
			name: "review on a pull request",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(2, true),
				Comment: comment(8888, user, "@shuriken-bot review"),
			},
			// The comment is also recorded as activity because it mentions us.
			want: []string{"record-activity", "review"},
		},
		{
			name: "hint on a pull request",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(2, true),
				Comment: comment(8888, user, "@shuriken-bot hint"),
			},
			want: []string{"record-activity", "review"},
		},
		{
			name: "explain on a pull request",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(2, true),
				Comment: comment(8888, user, "@shuriken-bot explain"),
			},
			want: []string{"record-activity", "explain"},
		},
		{
			// A bare mention is acknowledged with a reaction, not a comment,
			// so the thread does not fill up with bot chatter.
			name: "bare mention gets a reaction",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(1, false),
				Comment: comment(8888, user, "cc @shuriken-bot"),
			},
			want: []string{"record-activity", "ack"},
		},
		{
			// A trailing word we do not recognise is an unknown command, which
			// gets the short help reply.
			name: "unrecognised trailing word gets help",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(1, false),
				Comment: comment(8888, user, "@shuriken-bot thanks"),
			},
			want: []string{"record-activity", "reply"},
		},
		{
			name: "unknown command gets help",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(1, false),
				Comment: comment(8888, user, "@shuriken-bot frobnicate"),
			},
			want: []string{"record-activity", "reply"},
		},
		{
			// review only makes sense on a pull request.
			name: "review asked for on an issue",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(1, false),
				Comment: comment(8888, user, "@shuriken-bot review"),
			},
			want: []string{"record-activity", "reply"},
		},
		{
			name: "no mention does nothing",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(2, true),
				Comment: comment(8888, user, "looks good"),
			},
			want: nil,
		},
		{
			// Never respond to a bot, including ourselves.
			name: "our own comment is ignored",
			payload: &gh.IssueCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo, Issue: rissue(2, true),
				Comment: comment(8888, bot, "@shuriken-bot review"),
			},
			want: nil,
		},
		{
			name: "the review label triggers a review",
			payload: &gh.PullRequestEvent{
				Action: gh.Ptr("labeled"), Repo: rrepo,
				PullRequest: &gh.PullRequest{ID: gh.Ptr(int64(6001)), Number: gh.Ptr(2), User: user},
				Label:       &gh.Label{Name: "needs-review"},
				Sender:      user,
			},
			want: []string{"review-label"},
		},
		{
			name: "another label does nothing",
			payload: &gh.PullRequestEvent{
				Action: gh.Ptr("labeled"), Repo: rrepo,
				PullRequest: &gh.PullRequest{ID: gh.Ptr(int64(6001)), Number: gh.Ptr(2), User: user},
				Label:       &gh.Label{Name: "documentation"},
				Sender:      user,
			},
			want: nil,
		},
		{
			// A push must never trigger a review.
			name:    "a push triggers nothing",
			payload: &gh.PushEvent{},
			want:    nil,
		},
		{
			name: "an inline comment mentioning us only acknowledges",
			payload: &gh.PullRequestReviewCommentEvent{
				Action: gh.Ptr("created"), Repo: rrepo,
				PullRequest: &gh.PullRequest{ID: gh.Ptr(int64(6001)), Number: gh.Ptr(2), User: user},
				Comment: &gh.PullRequestComment{
					ID: gh.Ptr(int64(9001)), User: user, Body: gh.Ptr("@shuriken-bot review"),
				},
			},
			want: []string{"record-activity", "ack"},
		},
		{
			name: "pr opened records activity only",
			payload: &gh.PullRequestEvent{
				Action: gh.Ptr("opened"), Repo: rrepo,
				PullRequest: &gh.PullRequest{ID: gh.Ptr(int64(6001)), Number: gh.Ptr(2), User: user},
			},
			want: []string{"record-activity"},
		},
	}

	r := testRouter()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := kinds(r, Delivery{ID: "d-1", Payload: tc.payload, InstallationID: 4242})

			if len(got) != len(tc.want) {
				t.Fatalf("tasks = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("task[%d] = %q, want %q (all: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}
