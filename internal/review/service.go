package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sos-academy/shuriken/internal/diff"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/store"
)

// Trigger values recorded on a run.
const (
	TriggerMention = "mention"
	TriggerLabel   = "label"
)

// Job is one review request.
type Job struct {
	InstallationID int64
	Owner          string
	RepoName       string
	Number         int

	// Trigger is how the review was asked for.
	Trigger string
	// TriggeredBy is the account that asked.
	TriggeredBy store.Actor
	// TriggeredByAssociation is that account's author_association on the repo,
	// which is what authorises a review of someone else's pull request.
	TriggeredByAssociation string
	// CommandCommentID is the comment that asked, used to answer each mention
	// at most once. Zero for a label trigger.
	CommandCommentID int64

	// Mode distinguishes a full review from a hint.
	Mode Mode
}

// FullName is "owner/repo".
func (j Job) FullName() string { return j.Owner + "/" + j.RepoName }

// Options configure the review service.
type Options struct {
	BotHandle         string
	ExcludeGlobs      []string
	MaxDiffBytes      int
	MaxInlineComments int
	MinConfidence     float64
	MinSeverity       store.Severity
	Pricing           llm.Pricing
	Model             string
	DryRun            bool
}

// ClientFactory mints a GitHub client for an installation.
type ClientFactory interface {
	For(installationID int64) (ghc.Client, error)
}

// Service runs the review pipeline.
type Service struct {
	clients ClientFactory
	model   llm.Client
	store   store.Store
	log     *slog.Logger
	opts    Options
}

// NewService builds a review Service.
func NewService(clients ClientFactory, model llm.Client, s store.Store, log *slog.Logger, opts Options) *Service {
	return &Service{clients: clients, model: model, store: s, log: log, opts: opts}
}

// conventionsPath is the optional per-repository tuning file.
const conventionsPath = ".github/shuriken.md"

// Run executes one review job.
func (s *Service) Run(ctx context.Context, job Job) error {
	log := s.log.With(
		slog.String("repo", job.FullName()),
		slog.Int("pr", job.Number),
		slog.String("trigger", job.Trigger),
		slog.String("mode", string(job.Mode)),
	)

	gh, err := s.clients.For(job.InstallationID)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}

	pr, err := gh.GetPullRequest(ctx, job.Owner, job.RepoName, job.Number)
	if err != nil {
		return err
	}

	if reason, ok := s.shouldSkip(pr, job); !ok {
		log.InfoContext(ctx, "review skipped", slog.String("reason", reason))
		if job.CommandCommentID != 0 && !s.opts.DryRun {
			// Tell the requester why, once, rather than silently ignoring them.
			s.replyOnce(ctx, gh, job, "I can't review this one: "+reason+".")
		}
		return nil
	}

	run := &store.ReviewRun{
		Repo:             store.Repo{Owner: job.Owner, Name: job.RepoName, FullName: job.FullName()},
		PR:               store.PRRef{Number: pr.Number, GithubID: pr.GithubID, URL: pr.URL, HeadSHA: pr.HeadSHA},
		Trigger:          job.Trigger,
		TriggeredBy:      job.TriggeredBy,
		CommandCommentID: job.CommandCommentID,
		Status:           store.RunPending,
		Model:            s.opts.Model,
		StartedAt:        time.Now().UTC(),
	}

	// Claiming the run is what stops a redelivered mention producing a second
	// review. A label trigger has no comment id, so it is claimed per head SHA
	// by the store instead.
	if !s.opts.DryRun && s.store != nil {
		claimed, err := s.store.ClaimReviewRun(ctx, run)
		if err != nil {
			return fmt.Errorf("claim review run: %w", err)
		}
		if !claimed {
			log.InfoContext(ctx, "review already handled for this trigger")
			return nil
		}
	}

	if err := s.execute(ctx, gh, pr, job, run, log); err != nil {
		run.Status = store.RunFailed
		run.Error = redactError(err)
		run.FinishedAt = time.Now().UTC()
		if !s.opts.DryRun && s.store != nil {
			if cerr := s.store.CompleteReviewRun(ctx, run); cerr != nil {
				log.ErrorContext(ctx, "failed to record failed run", slog.Any("error", cerr))
			}
		}
		return err
	}
	return nil
}

// shouldSkip applies the gates that decide whether a review happens at all.
func (s *Service) shouldSkip(pr *ghc.PullRequest, job Job) (reason string, ok bool) {
	switch {
	case pr.Draft:
		return "it's still a draft", false
	case pr.AuthorIsBot:
		return "the pull request was opened by a bot", false
	}

	// The author can always ask about their own work. Anyone else needs write
	// access, which we read from the author_association already in the payload.
	if job.Trigger == TriggerMention &&
		job.TriggeredBy.GithubID != pr.AuthorID &&
		!ghc.IsMaintainer(job.TriggeredByAssociation) {
		return "only a maintainer can ask me to review someone else's pull request", false
	}
	return "", true
}

func (s *Service) execute(
	ctx context.Context,
	gh ghc.Client,
	pr *ghc.PullRequest,
	job Job,
	run *store.ReviewRun,
	log *slog.Logger,
) error {
	files, err := gh.ListPullRequestFiles(ctx, job.Owner, job.RepoName, job.Number)
	if err != nil {
		return err
	}

	filtered := diff.Filter(files, s.opts.ExcludeGlobs, s.opts.MaxDiffBytes)
	if len(filtered.Included) == 0 {
		log.InfoContext(ctx, "nothing reviewable after filtering",
			slog.Int("excluded", len(filtered.ExcludedNoise)))
		run.Status = store.RunSkipped
		run.FinishedAt = time.Now().UTC()
		if !s.opts.DryRun && s.store != nil {
			return s.store.CompleteReviewRun(ctx, run)
		}
		return nil
	}
	run.SkippedFiles = filtered.SkippedForSize

	contributing := s.optionalFile(ctx, gh, job, "CONTRIBUTING.md", pr.HeadSHA)
	conventions := s.optionalFile(ctx, gh, job, conventionsPath, pr.HeadSHA)

	tier := TierFor(pr.AuthorAssociation)
	reqCtx := Context{
		PR:           pr,
		Files:        filtered.Included,
		Mode:         job.Mode,
		SkippedNoise: filtered.ExcludedNoise,
		SkippedSize:  filtered.SkippedForSize,
	}

	result, err := s.model.Review(ctx, llm.Request{
		SystemStatic: SystemStatic(),
		Conventions:  ConventionsBlock(contributing, conventions),
		UserContent:  UserContent(reqCtx),
		ReadFile: func(ctx context.Context, path string) (string, error) {
			// Always pinned to the head SHA the review is about, so the model
			// cannot be steered to read a different ref.
			return gh.GetFileContents(ctx, job.Owner, job.RepoName, path, pr.HeadSHA)
		},
	})
	if err != nil {
		return err
	}

	run.Usage = store.Usage{
		InputTokens:              result.Usage.InputTokens,
		OutputTokens:             result.Usage.OutputTokens,
		CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
		ToolCalls:                result.Usage.ToolCalls,
	}
	run.EstimatedCostUSD = s.opts.Pricing.Cost(result.Usage)

	// Cost and token accounting goes to the log, never into a pull request.
	log.InfoContext(ctx, "review model run complete",
		slog.String("model", s.opts.Model),
		slog.Int64("inputTokens", result.Usage.InputTokens),
		slog.Int64("outputTokens", result.Usage.OutputTokens),
		slog.Int64("cacheReadTokens", result.Usage.CacheReadInputTokens),
		slog.Int64("cacheWriteTokens", result.Usage.CacheCreationInputTokens),
		slog.Int("toolCalls", result.Usage.ToolCalls),
		slog.Bool("toolCallsCapped", result.ToolCallsCapped),
		slog.Bool("retried", result.Retried),
		slog.Float64("estimatedCostUsd", run.EstimatedCostUSD),
		slog.Int("rawFindings", len(result.Output.Findings)),
	)

	var existing []ghc.ExistingComment
	if prior, err := gh.ListOwnReviewComments(ctx, job.Owner, job.RepoName, job.Number, s.opts.BotHandle); err != nil {
		// Losing dedupe is better than losing the review; worst case we repeat
		// ourselves once.
		log.WarnContext(ctx, "could not load earlier comments, skipping dedupe", slog.Any("error", err))
	} else {
		existing = prior
	}

	decision := PostProcess(result.Output.Findings, FilterOptions{
		MinConfidence:     s.opts.MinConfidence,
		MinSeverity:       s.opts.MinSeverity,
		MaxInlineComments: s.opts.MaxInlineComments,
		Existing:          existing,
		Index:             diff.BuildLineIndex(filtered.Included),
	})

	for _, d := range decision.Dropped {
		log.DebugContext(ctx, "finding dropped",
			slog.String("reason", string(d.Reason)),
			slog.String("detail", d.Detail),
			slog.String("path", d.Finding.Path),
			slog.Int("line", d.Finding.Line),
			slog.String("concept", d.Finding.Concept))
	}

	comments := make([]ghc.ReviewComment, 0, len(decision.Kept))
	withheld := false
	for _, f := range decision.Kept {
		if !f.Mechanical && strings.TrimSpace(f.SuggestedFix) != "" {
			withheld = true
		}
		comments = append(comments, ghc.ReviewComment{
			Path: f.Path,
			Line: f.Line,
			Side: f.Side,
			Body: RenderComment(f, job.Mode, tier),
		})
	}

	body := RenderReviewBody(BodyInput{
		Summary:        result.Output.Summary,
		Mode:           job.Mode,
		Tier:           tier,
		BotHandle:      s.opts.BotHandle,
		SkippedForSize: filtered.SkippedForSize,
		ToolCapped:     result.ToolCallsCapped,
		HasWithheldFix: withheld,
	})

	run.FindingsPosted = len(decision.Kept)
	run.FindingsDropped = len(decision.Dropped)

	if s.opts.DryRun {
		log.InfoContext(ctx, "DRY RUN would submit review",
			slog.Int("inlineComments", len(comments)),
			slog.Int("dropped", len(decision.Dropped)),
			slog.String("body", body))
		for _, c := range comments {
			log.InfoContext(ctx, "DRY RUN would comment",
				slog.String("path", c.Path), slog.Int("line", c.Line),
				slog.String("side", c.Side), slog.String("body", c.Body))
		}
		return nil
	}

	// Exactly one review, event COMMENT.
	submitted, err := gh.CreateReview(ctx, job.Owner, job.RepoName, job.Number, pr.HeadSHA, body, comments)
	if err != nil {
		return err
	}

	run.Status = store.RunPosted
	run.ReviewID = submitted.ID
	run.ReviewURL = submitted.URL
	run.FinishedAt = time.Now().UTC()

	if s.store != nil {
		findings := s.toStoredFindings(ctx, pr, job, submitted, decision.Kept)
		if err := s.store.SaveFindings(ctx, findings); err != nil {
			log.ErrorContext(ctx, "failed to save findings", slog.Any("error", err))
		}
		if err := s.store.CompleteReviewRun(ctx, run); err != nil {
			log.ErrorContext(ctx, "failed to complete run record", slog.Any("error", err))
		}
	}

	log.InfoContext(ctx, "review submitted",
		slog.Int64("reviewId", submitted.ID),
		slog.Int("posted", len(comments)),
		slog.Int("dropped", len(decision.Dropped)))

	return nil
}

func (s *Service) toStoredFindings(
	ctx context.Context,
	pr *ghc.PullRequest,
	job Job,
	submitted *ghc.SubmittedReview,
	kept []llm.Finding,
) []*store.Finding {
	author := store.Actor{GithubID: pr.AuthorID, Login: pr.Author}

	var userID string
	unlinked := true
	if s.store != nil {
		if u, err := s.store.FindPlatformUser(ctx, author.GithubID, author.Login); err == nil && u != nil {
			userID, unlinked = u.ID, false
		}
	}

	now := time.Now().UTC()
	out := make([]*store.Finding, 0, len(kept))
	for _, f := range kept {
		out = append(out, &store.Finding{
			Actor:    author,
			UserID:   userID,
			Unlinked: unlinked,
			Repo:     store.Repo{Owner: job.Owner, Name: job.RepoName, FullName: job.FullName()},
			PR: store.PRRef{
				Number: pr.Number, GithubID: pr.GithubID, URL: pr.URL, HeadSHA: pr.HeadSHA,
			},
			ReviewID:     submitted.ID,
			ReviewURL:    submitted.URL,
			Concept:      normalizeConcept(f.Concept),
			Severity:     store.Severity(f.Severity),
			Confidence:   f.Confidence,
			Path:         f.Path,
			Line:         f.Line,
			Side:         f.Side,
			Title:        f.Title,
			Explanation:  f.Explanation,
			SuggestedFix: f.SuggestedFix,
			PostedAt:     now,
		})
	}
	return out
}

// optionalFile reads a file that may not exist. A missing file is normal and
// not an error.
func (s *Service) optionalFile(ctx context.Context, gh ghc.Client, job Job, path, ref string) string {
	content, err := gh.GetFileContents(ctx, job.Owner, job.RepoName, path, ref)
	if err != nil {
		if !errors.Is(err, ghc.ErrNotFound) {
			s.log.DebugContext(ctx, "optional file unavailable",
				slog.String("path", path), slog.Any("error", err))
		}
		return ""
	}
	return content
}

// Explain answers an `explain` request from the stored findings of the last
// review, so the reveal matches what was actually said.
func (s *Service) Explain(ctx context.Context, job Job) error {
	gh, err := s.clients.For(job.InstallationID)
	if err != nil {
		return err
	}
	pr, err := gh.GetPullRequest(ctx, job.Owner, job.RepoName, job.Number)
	if err != nil {
		return err
	}

	var findings []*store.Finding
	if s.store != nil {
		findings, err = s.store.LatestReviewFindings(ctx, pr.GithubID)
		if err != nil {
			return fmt.Errorf("load latest findings: %w", err)
		}
	}

	body := RenderExplain(findings)

	if s.opts.DryRun {
		s.log.InfoContext(ctx, "DRY RUN would post explain",
			slog.String("repo", job.FullName()), slog.Int("pr", job.Number),
			slog.Int("findings", len(findings)), slog.String("body", body))
		return nil
	}

	if _, err := gh.CreateIssueComment(ctx, job.Owner, job.RepoName, job.Number, body); err != nil {
		return err
	}

	if len(findings) > 0 && s.store != nil {
		ids := make([]string, 0, len(findings))
		for _, f := range findings {
			ids = append(ids, f.ID)
		}
		if err := s.store.MarkFindingsRevealed(ctx, ids); err != nil {
			s.log.WarnContext(ctx, "could not mark findings revealed", slog.Any("error", err))
		}
	}
	return nil
}

// replyOnce posts a short message, logging rather than failing the job if the
// comment cannot be created.
func (s *Service) replyOnce(ctx context.Context, gh ghc.Client, job Job, body string) {
	if _, err := gh.CreateIssueComment(ctx, job.Owner, job.RepoName, job.Number, body); err != nil {
		s.log.WarnContext(ctx, "could not post reply", slog.Any("error", err))
	}
}

// redactError strips anything that looks like a credential out of an error
// before it is persisted.
func redactError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Bearer tokens and connection strings are the realistic leak paths here.
	for _, marker := range []string{"Bearer ", "bearer ", "mongodb://", "mongodb+srv://", "sk-ant-"} {
		if i := strings.Index(msg, marker); i >= 0 {
			msg = msg[:i] + "[REDACTED]"
			break
		}
	}
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}
