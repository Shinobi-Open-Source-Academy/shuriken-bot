package review

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/sos-academy/shuriken/internal/diff"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/store"
)

// dryRunService mirrors newService but in dry run, with the sink attached.
func dryRunService(t *testing.T, gh *fakeGitHub, model llm.Client, sink func(DryRunReport)) *Service {
	t.Helper()
	return NewService(
		fixedClients{gh}, model, nil, // nil store: the command records nothing
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{
			BotHandle:         "shuriken-bot",
			MaxDiffBytes:      100_000,
			MaxInlineComments: 10,
			MinConfidence:     0.6,
			MinSeverity:       store.SeverityLow,
			Model:             "claude-opus-5-5",
			Pricing:           llm.Pricing{InputPerMTok: 4, OutputPerMTok: 20},
			DryRun:            true,
			OnDryRun:          sink,
		},
	)
}

// The contract `shuriken review` depends on. Without it the command prints
// nothing, which would look like a model that found no problems.
func TestDryRunReportReachesTheSink(t *testing.T) {
	gh := &fakeGitHub{
		pr:    okPR(),
		files: []diff.File{{Path: "main.go", Patch: servicePatch, Additions: 2, Deletions: 1}},
	}
	model := &fakeModel{out: llm.ReviewOutput{
		Summary: "Nice first contribution.",
		Findings: []llm.Finding{{
			Path: "main.go", Line: 2, Side: "RIGHT", Severity: "high", Confidence: 0.95,
			Title: "Unchecked error", Explanation: "why", Concept: "error handling",
		}},
	}}

	var got *DryRunReport
	svc := dryRunService(t, gh, model, func(r DryRunReport) { got = &r })
	if err := svc.Run(context.Background(), okJob()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if got == nil {
		t.Fatal("OnDryRun was never called, so the review command would print nothing")
	}
	if got.Body == "" {
		t.Error("report carries no body")
	}
	if len(got.Comments) != 1 {
		t.Fatalf("report carries %d inline comments, want 1", len(got.Comments))
	}
	if got.Comments[0].Path != "main.go" {
		t.Errorf("comment path = %q, want main.go", got.Comments[0].Path)
	}

	// Dry run must stay dry: the sink is for reading, not a way to post.
	if len(gh.reviews) != 0 {
		t.Errorf("dry run submitted %d reviews, want 0", len(gh.reviews))
	}
}

// TestDryRunWithoutSinkIsSafe guards the server, which sets no sink.
func TestDryRunWithoutSinkIsSafe(t *testing.T) {
	gh := &fakeGitHub{
		pr:    okPR(),
		files: []diff.File{{Path: "main.go", Patch: servicePatch, Additions: 2, Deletions: 1}},
	}
	model := &fakeModel{out: llm.ReviewOutput{Summary: "ok"}}

	if err := dryRunService(t, gh, model, nil).Run(context.Background(), okJob()); err != nil {
		t.Fatalf("a nil OnDryRun must be safe: %v", err)
	}
	if len(gh.reviews) != 0 {
		t.Errorf("dry run submitted %d reviews, want 0", len(gh.reviews))
	}
}
