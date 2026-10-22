package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sos-academy/shuriken/internal/config"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/logging"
	"github.com/sos-academy/shuriken/internal/review"
	"github.com/sos-academy/shuriken/internal/store"
)

const reviewUsage = `shuriken review — run one review against a real pull request and print it.

Usage:
  shuriken review <owner>/<repo>#<number> [flags]
  shuriken review https://github.com/<owner>/<repo>/pull/<number> [flags]

Nothing is ever posted and nothing is recorded: no comment, no review, no
database. The review goes to stdout so it can be piped or saved; diagnostics
go to stderr.

Needs SHURIKEN_GITHUB_APP_ID, a private key, and ANTHROPIC_API_KEY. A local
.env is not read automatically — source it first:

  set -a && . ./.env && set +a

Flags:
`

// Accepts "owner/repo#123" and pull request URLs.
var targetRe = regexp.MustCompile(`^(?:https?://github\.com/)?([^/\s]+)/([^/\s#]+?)(?:/pull/|#)(\d+)/?$`)

type reviewTarget struct {
	Owner  string
	Repo   string
	Number int
}

func parseTarget(s string) (reviewTarget, error) {
	m := targetRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return reviewTarget{}, fmt.Errorf("cannot parse %q: want owner/repo#123 or a pull request URL", s)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return reviewTarget{}, fmt.Errorf("pull request number must be a positive integer, got %q", m[3])
	}
	return reviewTarget{Owner: m[1], Repo: m[2], Number: n}, nil
}

// runReview drives the review pipeline directly: no webhook, no server, no
// database.
func runReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		model   = fs.String("model", "", "override SHURIKEN_ANTHROPIC_MODEL for this run")
		verbose = fs.Bool("v", false, "log the pipeline's own output to stderr")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, reviewUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("exactly one pull request is required")
	}
	target, err := parseTarget(fs.Arg(0))
	if err != nil {
		return err
	}

	cfg, err := config.LoadForReview()
	if err != nil {
		return err
	}
	if *model != "" {
		cfg.Model = *model
	}

	level := "error"
	if *verbose {
		level = cfg.LogLevel
	}
	log := logging.NewTo(os.Stderr, level, []string{
		cfg.AnthropicAPIKey.Reveal(),
		string(cfg.PrivateKeyPEM),
	})

	clients, err := ghc.NewAppClientFactory(cfg.AppID, []byte(cfg.PrivateKeyPEM.Reveal()))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "resolving installation for %s/%s...\n", target.Owner, target.Repo)
	installationID, err := clients.InstallationFor(ctx, target.Owner, target.Repo)
	if err != nil {
		return err
	}

	var report *review.DryRunReport
	svc := review.NewService(clients, llm.New(llm.Options{
		APIKey:          cfg.AnthropicAPIKey.Reveal(),
		Model:           cfg.Model,
		Effort:          cfg.Effort,
		MaxOutputTokens: cfg.MaxOutputTokens,
		MaxToolCalls:    cfg.MaxToolCalls,
	}), nil /* no store: nothing is recorded */, log, review.Options{
		BotHandle:         cfg.BotHandle,
		ExcludeGlobs:      cfg.ExcludeGlobs,
		MaxDiffBytes:      cfg.MaxDiffBytes,
		MaxInlineComments: cfg.MaxInlineComments,
		MinConfidence:     cfg.MinConfidence,
		MinSeverity:       store.Severity(cfg.MinSeverity),
		Model:             cfg.Model,
		// Deliberately not a flag: one that could post from a laptop is a foot-gun.
		DryRun:   true,
		OnDryRun: func(r review.DryRunReport) { report = &r },
		Pricing: llm.Pricing{
			InputPerMTok:      cfg.PriceInputPerMTok,
			OutputPerMTok:     cfg.PriceOutputPerMTok,
			CacheReadPerMTok:  cfg.PriceCacheReadPerMTok,
			CacheWritePerMTok: cfg.PriceCacheWritePerMTok,
		},
	})

	fmt.Fprintf(os.Stderr, "reviewing %s/%s#%d with %s...\n",
		target.Owner, target.Repo, target.Number, cfg.Model)

	if err := svc.Run(ctx, review.Job{
		InstallationID: installationID,
		Owner:          target.Owner,
		RepoName:       target.Repo,
		Number:         target.Number,
		Trigger:        review.TriggerLabel, // no comment to answer, so no acknowledgement
		Mode:           review.ModeReview,
	}); err != nil {
		return err
	}

	if report == nil {
		fmt.Fprintln(os.Stderr, "no review produced — run again with -v to see why")
		return nil
	}

	return writeReport(os.Stdout, target, cfg.Model, *report)
}

// errWriter latches the first write error, so a closed pipe stops the render
// instead of being ignored a dozen times.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, a ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, a...)
}

func writeReport(w io.Writer, t reviewTarget, model string, r review.DryRunReport) error {
	e := &errWriter{w: w}
	e.printf("# %s/%s#%d\n\n", t.Owner, t.Repo, t.Number)
	e.printf("_Not posted. Model: %s._\n\n---\n\n", model)
	e.printf("%s\n", strings.TrimSpace(r.Body))

	if len(r.Comments) > 0 {
		e.printf("\n---\n\n## Inline comments (%d)\n", len(r.Comments))
		for _, c := range r.Comments {
			e.printf("\n### `%s:%d` (%s)\n\n%s\n", c.Path, c.Line, c.Side, strings.TrimSpace(c.Body))
		}
	}

	e.printf("\n---\n\n")
	if r.Dropped > 0 {
		e.printf("%d finding(s) dropped below the confidence or severity floor.\n", r.Dropped)
	}
	e.printf("Tokens: %d in, %d out, %d cache read. Estimated cost: $%.4f\n",
		r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadInputTokens, r.CostUSD)
	return e.err
}
