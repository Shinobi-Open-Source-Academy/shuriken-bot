// Package activity turns GitHub webhook events into normalized activity rows.
//
// Normalize is a pure function: it performs no I/O and makes no decisions that
// depend on the outside world, which is what makes the event matrix cheap to
// table test.
package activity

import (
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/sos-academy/shuriken/internal/command"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/store"
)

// Input is one webhook delivery to normalize.
type Input struct {
	// Event is the X-GitHub-Event value.
	Event string
	// Payload is the result of github.ParseWebHook.
	Payload any
	// DeliveryID is the X-GitHub-Delivery value, kept for traceability.
	DeliveryID string
	// BotHandle is the bot's login without "@". Comment activities are only
	// recorded when they mention it.
	BotHandle string
}

// Normalize returns the activities a delivery should produce. Most deliveries
// produce none; none ever produce more than one in v0.
//
// Events from bots are dropped, including Shuriken's own, so the bot cannot
// react to itself.
func Normalize(in Input) []*store.Activity {
	switch e := in.Payload.(type) {
	case *gh.IssuesEvent:
		return issues(in, e)
	case *gh.PullRequestEvent:
		return pullRequest(in, e)
	case *gh.PullRequestReviewEvent:
		return pullRequestReview(in, e)
	case *gh.IssueCommentEvent:
		return issueComment(in, e)
	case *gh.PullRequestReviewCommentEvent:
		return reviewComment(in, e)
	default:
		return nil
	}
}

func issues(in Input, e *gh.IssuesEvent) []*store.Activity {
	issue := e.GetIssue()
	if issue == nil {
		return nil
	}
	repo := toRepo(e.GetRepo())
	subj := issueSubject(issue)

	switch e.GetAction() {
	case "opened":
		author := toActor(issue.GetUser())
		if isBot(issue.GetUser()) {
			return nil
		}
		return one(&store.Activity{
			DedupeKey:  key(store.IssueOpened, issue.GetID()),
			Type:       store.IssueOpened,
			Actor:      author,
			Repo:       repo,
			Subject:    subj,
			OccurredAt: at(issue.GetCreatedAt().Time),
			DeliveryID: in.DeliveryID,
		})

	case "assigned":
		// The assignee is the contributor claiming the work, so they are the
		// actor. Who did the assigning is metadata.
		assignee := e.GetAssignee()
		if assignee == nil || isBot(assignee) {
			return nil
		}
		meta := map[string]any{}
		if s := e.GetSender(); s != nil && s.GetID() != assignee.GetID() {
			meta["assignedByLogin"] = s.GetLogin()
		}
		return one(&store.Activity{
			DedupeKey:  fmt.Sprintf("%s:%d:%d", store.IssueAssigned, issue.GetID(), assignee.GetID()),
			Type:       store.IssueAssigned,
			Actor:      toActor(assignee),
			Repo:       repo,
			Subject:    subj,
			OccurredAt: at(time.Time{}),
			Metadata:   meta,
			DeliveryID: in.DeliveryID,
		})
	}
	return nil
}

func pullRequest(in Input, e *gh.PullRequestEvent) []*store.Activity {
	pr := e.GetPullRequest()
	if pr == nil || isBot(pr.GetUser()) {
		return nil
	}
	repo := toRepo(e.GetRepo())
	subj := prSubject(pr)
	// The pull request author is the contributor whose progress we track, even
	// when someone else merged or closed it.
	author := toActor(pr.GetUser())

	switch e.GetAction() {
	case "opened":
		return one(&store.Activity{
			DedupeKey:  key(store.PROpened, pr.GetID()),
			Type:       store.PROpened,
			Actor:      author,
			Repo:       repo,
			Subject:    subj,
			OccurredAt: at(pr.GetCreatedAt().Time),
			Metadata:   map[string]any{"draft": pr.GetDraft()},
			DeliveryID: in.DeliveryID,
		})

	case "closed":
		if pr.GetMerged() {
			meta := map[string]any{"additions": pr.GetAdditions(), "deletions": pr.GetDeletions()}
			if m := pr.GetMergedBy(); m != nil {
				meta["mergedByLogin"] = m.GetLogin()
			}
			return one(&store.Activity{
				DedupeKey:  key(store.PRMerged, pr.GetID()),
				Type:       store.PRMerged,
				Actor:      author,
				Repo:       repo,
				Subject:    subj,
				OccurredAt: at(pr.GetMergedAt().Time),
				Metadata:   meta,
				DeliveryID: in.DeliveryID,
			})
		}
		return one(&store.Activity{
			DedupeKey:  key(store.PRClosedUnmerged, pr.GetID()),
			Type:       store.PRClosedUnmerged,
			Actor:      author,
			Repo:       repo,
			Subject:    subj,
			OccurredAt: at(pr.GetClosedAt().Time),
			DeliveryID: in.DeliveryID,
		})
	}
	return nil
}

func pullRequestReview(in Input, e *gh.PullRequestReviewEvent) []*store.Activity {
	if e.GetAction() != "submitted" {
		return nil
	}
	review, pr := e.GetReview(), e.GetPullRequest()
	if review == nil || pr == nil || isBot(review.GetUser()) {
		return nil
	}
	// Reviewing your own pull request is not peer review.
	if review.GetUser().GetID() == pr.GetUser().GetID() {
		return nil
	}

	return one(&store.Activity{
		DedupeKey:  key(store.PRReviewSubmitted, review.GetID()),
		Type:       store.PRReviewSubmitted,
		Actor:      toActor(review.GetUser()),
		Repo:       toRepo(e.GetRepo()),
		Subject:    prSubject(pr),
		Object:     store.Object{GithubID: review.GetID(), URL: review.GetHTMLURL()},
		OccurredAt: at(review.GetSubmittedAt().Time),
		Metadata: map[string]any{
			"reviewState":   strings.ToLower(review.GetState()),
			"prAuthorLogin": pr.GetUser().GetLogin(),
		},
		DeliveryID: in.DeliveryID,
	})
}

// issueComment covers both issue comments and pull request conversation
// comments: GitHub delivers both as issue_comment, distinguished by whether
// issue.pull_request is set.
func issueComment(in Input, e *gh.IssueCommentEvent) []*store.Activity {
	if e.GetAction() != "created" {
		return nil
	}
	cm, issue := e.GetComment(), e.GetIssue()
	if cm == nil || issue == nil || isBot(cm.GetUser()) {
		return nil
	}
	// Comments are only recorded when they talk to us.
	if !command.Parse(cm.GetBody(), in.BotHandle).Mentioned {
		return nil
	}

	typ := store.IssueComment
	subj := issueSubject(issue)
	if issue.IsPullRequest() {
		typ = store.PRComment
		subj.Kind = "pull_request"
	}

	return one(&store.Activity{
		DedupeKey:  key(typ, cm.GetID()),
		Type:       typ,
		Actor:      toActor(cm.GetUser()),
		Repo:       toRepo(e.GetRepo()),
		Subject:    subj,
		Object:     store.Object{GithubID: cm.GetID(), URL: cm.GetHTMLURL()},
		OccurredAt: at(cm.GetCreatedAt().Time),
		DeliveryID: in.DeliveryID,
	})
}

// reviewComment covers inline code comments on a pull request. They are
// recorded as pr_comment, same as conversation comments.
func reviewComment(in Input, e *gh.PullRequestReviewCommentEvent) []*store.Activity {
	if e.GetAction() != "created" {
		return nil
	}
	cm, pr := e.GetComment(), e.GetPullRequest()
	if cm == nil || pr == nil || isBot(cm.GetUser()) {
		return nil
	}
	if !command.Parse(cm.GetBody(), in.BotHandle).Mentioned {
		return nil
	}

	return one(&store.Activity{
		DedupeKey:  key(store.PRComment, cm.GetID()),
		Type:       store.PRComment,
		Actor:      toActor(cm.GetUser()),
		Repo:       toRepo(e.GetRepo()),
		Subject:    prSubject(pr),
		Object:     store.Object{GithubID: cm.GetID(), URL: cm.GetHTMLURL()},
		OccurredAt: at(cm.GetCreatedAt().Time),
		DeliveryID: in.DeliveryID,
	})
}

func one(a *store.Activity) []*store.Activity { return []*store.Activity{a} }

func key(t store.ActivityType, id int64) string {
	return fmt.Sprintf("%s:%d", t, id)
}

// at prefers the timestamp from the payload so a redelivery days later still
// records when the thing actually happened. Some payloads carry no usable
// timestamp for the action, in which case there is nothing better than now.
func at(payloadTime time.Time) time.Time {
	if !payloadTime.IsZero() {
		return payloadTime.UTC()
	}
	return time.Now().UTC()
}

func isBot(u *gh.User) bool {
	if u == nil {
		return true
	}
	return ghc.IsBotActor(u.GetType(), u.GetLogin())
}

func toActor(u *gh.User) store.Actor {
	if u == nil {
		return store.Actor{}
	}
	return store.Actor{GithubID: u.GetID(), Login: u.GetLogin()}
}

func toRepo(r *gh.Repository) store.Repo {
	if r == nil {
		return store.Repo{}
	}
	owner := r.GetOwner().GetLogin()
	name := r.GetName()
	full := r.GetFullName()
	if full == "" && owner != "" && name != "" {
		full = owner + "/" + name
	}
	return store.Repo{Owner: owner, Name: name, FullName: full, GithubID: r.GetID()}
}

func issueSubject(i *gh.Issue) store.Subject {
	return store.Subject{
		Kind:     "issue",
		Number:   i.GetNumber(),
		GithubID: i.GetID(),
		URL:      i.GetHTMLURL(),
	}
}

func prSubject(pr *gh.PullRequest) store.Subject {
	return store.Subject{
		Kind:     "pull_request",
		Number:   pr.GetNumber(),
		GithubID: pr.GetID(),
		URL:      pr.GetHTMLURL(),
	}
}
