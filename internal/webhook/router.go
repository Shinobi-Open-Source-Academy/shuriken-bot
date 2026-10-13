package webhook

import (
	"context"
	"fmt"
	"log/slog"

	gh "github.com/google/go-github/v92/github"

	"github.com/sos-academy/shuriken/internal/activity"
	"github.com/sos-academy/shuriken/internal/command"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/review"
	"github.com/sos-academy/shuriken/internal/store"
	"github.com/sos-academy/shuriken/internal/worker"
)

// ackReaction is the acknowledgement for a bare mention. A reaction says "logged"
// without adding a comment to the thread.
const ackReaction = "eyes"

// AppRouter turns verified deliveries into worker tasks.
type AppRouter struct {
	recorder    *activity.Recorder
	reviews     *review.Service
	clients     review.ClientFactory
	botHandle   string
	reviewLabel string
	log         *slog.Logger
	dryRun      bool
}

// NewRouter builds the application router.
func NewRouter(
	recorder *activity.Recorder,
	reviews *review.Service,
	clients review.ClientFactory,
	botHandle, reviewLabel string,
	log *slog.Logger,
	dryRun bool,
) *AppRouter {
	return &AppRouter{
		recorder:    recorder,
		reviews:     reviews,
		clients:     clients,
		botHandle:   botHandle,
		reviewLabel: reviewLabel,
		log:         log,
		dryRun:      dryRun,
	}
}

// Route returns the tasks for a delivery. It performs no I/O.
func (r *AppRouter) Route(d Delivery) []worker.Task {
	var tasks []worker.Task

	// Feature 1: every supported event may produce an activity row.
	activities := activity.Normalize(activity.Input{
		Event:      d.Event,
		Payload:    d.Payload,
		DeliveryID: d.ID,
		BotHandle:  r.botHandle,
	})
	if len(activities) > 0 {
		tasks = append(tasks, worker.Task{
			Name: "record-activity:" + d.Event,
			Run: func(ctx context.Context) error {
				return r.recorder.Record(ctx, activities)
			},
		})
	}

	// Feature 2: commands and the review label.
	switch e := d.Payload.(type) {
	case *gh.IssueCommentEvent:
		tasks = append(tasks, r.fromIssueComment(d, e)...)
	case *gh.PullRequestReviewCommentEvent:
		tasks = append(tasks, r.fromReviewComment(d, e)...)
	case *gh.PullRequestEvent:
		tasks = append(tasks, r.fromPullRequest(d, e)...)
	}

	return tasks
}

func (r *AppRouter) fromIssueComment(d Delivery, e *gh.IssueCommentEvent) []worker.Task {
	if e.GetAction() != "created" {
		return nil
	}
	cm, issue, repo := e.GetComment(), e.GetIssue(), e.GetRepo()
	if cm == nil || issue == nil || repo == nil {
		return nil
	}
	// Never react to a bot, including ourselves.
	if ghc.IsBotActor(cm.GetUser().GetType(), cm.GetUser().GetLogin()) {
		return nil
	}

	parsed := command.Parse(cm.GetBody(), r.botHandle)
	if !parsed.Mentioned {
		return nil
	}

	owner, name := repo.GetOwner().GetLogin(), repo.GetName()
	job := review.Job{
		InstallationID:         d.InstallationID,
		Owner:                  owner,
		RepoName:               name,
		Number:                 issue.GetNumber(),
		Trigger:                review.TriggerMention,
		TriggeredBy:            store.Actor{GithubID: cm.GetUser().GetID(), Login: cm.GetUser().GetLogin()},
		TriggeredByAssociation: cm.GetAuthorAssociation(),
		CommandCommentID:       cm.GetID(),
	}

	// review, hint and explain only make sense on a pull request.
	isPR := issue.IsPullRequest()

	switch parsed.Kind {
	case command.Review, command.Hint:
		if !isPR {
			return []worker.Task{r.replyTask(d, job, cm.GetID(),
				"I can only review pull requests. Open one and mention me there.")}
		}
		job.Mode = review.ModeReview
		if parsed.Kind == command.Hint {
			job.Mode = review.ModeHint
		}
		return []worker.Task{{
			Name: fmt.Sprintf("review:%s#%d", job.FullName(), job.Number),
			Run:  func(ctx context.Context) error { return r.reviews.Run(ctx, job) },
		}}

	case command.Explain:
		if !isPR {
			return []worker.Task{r.replyTask(d, job, cm.GetID(),
				"I only have reviews to explain on pull requests.")}
		}
		return []worker.Task{{
			Name: fmt.Sprintf("explain:%s#%d", job.FullName(), job.Number),
			Run:  func(ctx context.Context) error { return r.reviews.Explain(ctx, job) },
		}}

	case command.Help:
		return []worker.Task{r.replyTask(d, job, cm.GetID(),
			command.HelpText(r.botHandle, parsed.Word))}

	case command.Acknowledge:
		return []worker.Task{r.ackTask(d, job, cm.GetID(), false)}
	}
	return nil
}

func (r *AppRouter) fromReviewComment(d Delivery, e *gh.PullRequestReviewCommentEvent) []worker.Task {
	if e.GetAction() != "created" {
		return nil
	}
	cm, pr, repo := e.GetComment(), e.GetPullRequest(), e.GetRepo()
	if cm == nil || pr == nil || repo == nil {
		return nil
	}
	if ghc.IsBotActor(cm.GetUser().GetType(), cm.GetUser().GetLogin()) {
		return nil
	}
	if !command.Parse(cm.GetBody(), r.botHandle).Mentioned {
		return nil
	}

	// Inline comments acknowledge only. Running a whole review from a thread
	// buried in the diff is more surprising than useful.
	job := review.Job{
		InstallationID: d.InstallationID,
		Owner:          repo.GetOwner().GetLogin(),
		RepoName:       repo.GetName(),
		Number:         pr.GetNumber(),
	}
	return []worker.Task{r.ackTask(d, job, cm.GetID(), true)}
}

func (r *AppRouter) fromPullRequest(d Delivery, e *gh.PullRequestEvent) []worker.Task {
	if e.GetAction() != "labeled" {
		return nil
	}
	if e.GetLabel().GetName() != r.reviewLabel {
		return nil
	}
	pr, repo := e.GetPullRequest(), e.GetRepo()
	if pr == nil || repo == nil {
		return nil
	}

	// A label is applied by someone with write access by definition, so no
	// further authorisation check is needed.
	job := review.Job{
		InstallationID: d.InstallationID,
		Owner:          repo.GetOwner().GetLogin(),
		RepoName:       repo.GetName(),
		Number:         pr.GetNumber(),
		Trigger:        review.TriggerLabel,
		TriggeredBy:    store.Actor{GithubID: e.GetSender().GetID(), Login: e.GetSender().GetLogin()},
		Mode:           review.ModeReview,
	}
	return []worker.Task{{
		Name: fmt.Sprintf("review-label:%s#%d", job.FullName(), job.Number),
		Run:  func(ctx context.Context) error { return r.reviews.Run(ctx, job) },
	}}
}

// ackTask reacts to a mention. inlineComment selects the review comment
// endpoint rather than the issue comment one.
func (r *AppRouter) ackTask(d Delivery, job review.Job, commentID int64, inlineComment bool) worker.Task {
	return worker.Task{
		Name: fmt.Sprintf("ack:%s#%d", job.FullName(), job.Number),
		Run: func(ctx context.Context) error {
			if r.dryRun {
				r.log.InfoContext(ctx, "DRY RUN would acknowledge mention",
					slog.String("repo", job.FullName()),
					slog.Int("pr", job.Number),
					slog.Int64("commentId", commentID),
					slog.String("reaction", ackReaction))
				return nil
			}
			client, err := r.clients.For(d.InstallationID)
			if err != nil {
				return err
			}
			if inlineComment {
				return client.AddReviewCommentReaction(ctx, job.Owner, job.RepoName, commentID, ackReaction)
			}
			return client.AddIssueCommentReaction(ctx, job.Owner, job.RepoName, commentID, ackReaction)
		},
	}
}

func (r *AppRouter) replyTask(d Delivery, job review.Job, commentID int64, body string) worker.Task {
	return worker.Task{
		Name: fmt.Sprintf("reply:%s#%d", job.FullName(), job.Number),
		Run: func(ctx context.Context) error {
			if r.dryRun {
				r.log.InfoContext(ctx, "DRY RUN would reply",
					slog.String("repo", job.FullName()),
					slog.Int("number", job.Number),
					slog.String("body", body))
				return nil
			}
			client, err := r.clients.For(d.InstallationID)
			if err != nil {
				return err
			}
			if _, err := client.CreateIssueComment(ctx, job.Owner, job.RepoName, job.Number, body); err != nil {
				return err
			}
			// Mark the mention as seen so a redelivery is visibly a no-op.
			return client.AddIssueCommentReaction(ctx, job.Owner, job.RepoName, commentID, ackReaction)
		},
	}
}
