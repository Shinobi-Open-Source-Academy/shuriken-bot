package review

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sos-academy/shuriken/internal/diff"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/store"
)

// A two-hunk patch giving right-side lines 1,2,3 and 10,11.
const testPatch = `@@ -1,2 +1,3 @@
 context
-removed
+added one
+added two
@@ -20,1 +10,2 @@
 more context
+added three
`

func testIndex() *diff.LineIndex {
	return diff.BuildLineIndex([]diff.File{{Path: "a.go", Patch: testPatch}})
}

func finding(mut func(*llm.Finding)) llm.Finding {
	f := llm.Finding{
		Path:       "a.go",
		Line:       2,
		Side:       diff.SideRight,
		Severity:   "high",
		Confidence: 0.9,
		Title:      "Unchecked error",
		Concept:    "error handling",
	}
	if mut != nil {
		mut(&f)
	}
	return f
}

func baseOpts() FilterOptions {
	return FilterOptions{
		MinConfidence:     0.6,
		MinSeverity:       store.SeverityLow,
		MaxInlineComments: 10,
		Index:             testIndex(),
	}
}

func TestPostProcessGates(t *testing.T) {
	tests := []struct {
		name     string
		finding  llm.Finding
		opts     func(*FilterOptions)
		wantKept bool
		wantWhy  DropReason
	}{
		{"valid high confidence finding", finding(nil), nil, true, ""},

		// Confidence floor.
		{
			"below confidence floor",
			finding(func(f *llm.Finding) { f.Confidence = 0.5 }),
			nil, false, DropLowConfidence,
		},
		{
			"exactly at confidence floor is kept",
			finding(func(f *llm.Finding) { f.Confidence = 0.6 }),
			nil, true, "",
		},
		{
			"confidence above 1 is nonsense",
			finding(func(f *llm.Finding) { f.Confidence = 1.5 }),
			nil, false, DropInvalidShape,
		},
		{
			"negative confidence is nonsense",
			finding(func(f *llm.Finding) { f.Confidence = -0.1 }),
			nil, false, DropInvalidShape,
		},

		// Severity floor.
		{
			"low severity below a medium floor",
			finding(func(f *llm.Finding) { f.Severity = "low" }),
			func(o *FilterOptions) { o.MinSeverity = store.SeverityMedium },
			false, DropLowSeverity,
		},
		{
			"medium severity at a medium floor",
			finding(func(f *llm.Finding) { f.Severity = "medium" }),
			func(o *FilterOptions) { o.MinSeverity = store.SeverityMedium },
			true, "",
		},
		{
			"unknown severity",
			finding(func(f *llm.Finding) { f.Severity = "catastrophic" }),
			nil, false, DropInvalidShape,
		},
		{
			"severity is normalised for case",
			finding(func(f *llm.Finding) { f.Severity = "HIGH" }),
			nil, true, "",
		},

		// Anchoring. A comment on a line GitHub does not consider part of the
		// diff makes the whole review submission fail.
		{
			"line not in the diff",
			finding(func(f *llm.Finding) { f.Line = 7 }),
			nil, false, DropUnanchored,
		},
		{
			"line in the second hunk",
			finding(func(f *llm.Finding) { f.Line = 11 }),
			nil, true, "",
		},
		{
			"unknown path",
			finding(func(f *llm.Finding) { f.Path = "nope.go" }),
			nil, false, DropUnanchored,
		},
		{
			"empty side defaults to RIGHT",
			finding(func(f *llm.Finding) { f.Side = "" }),
			nil, true, "",
		},
		{
			"invalid side",
			finding(func(f *llm.Finding) { f.Side = "SIDEWAYS" }),
			nil, false, DropInvalidShape,
		},

		// Required fields.
		{"empty path", finding(func(f *llm.Finding) { f.Path = "" }), nil, false, DropInvalidShape},
		{"zero line", finding(func(f *llm.Finding) { f.Line = 0 }), nil, false, DropInvalidShape},
		{"empty title", finding(func(f *llm.Finding) { f.Title = "" }), nil, false, DropInvalidShape},
		{"empty concept", finding(func(f *llm.Finding) { f.Concept = "   " }), nil, false, DropInvalidShape},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOpts()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			got := PostProcess([]llm.Finding{tc.finding}, opts)

			if tc.wantKept {
				if len(got.Kept) != 1 {
					t.Fatalf("kept %d findings, want 1 (dropped: %+v)", len(got.Kept), got.Dropped)
				}
				return
			}
			if len(got.Kept) != 0 {
				t.Fatalf("kept a finding that should have been dropped")
			}
			if len(got.Dropped) != 1 {
				t.Fatalf("dropped %d findings, want 1", len(got.Dropped))
			}
			if got.Dropped[0].Reason != tc.wantWhy {
				t.Errorf("reason = %q, want %q (detail: %s)",
					got.Dropped[0].Reason, tc.wantWhy, got.Dropped[0].Detail)
			}
		})
	}
}

func TestPostProcessCapKeepsHighestSeverityFirst(t *testing.T) {
	in := []llm.Finding{
		finding(func(f *llm.Finding) { f.Severity = "low"; f.Line = 1; f.Concept = "naming" }),
		finding(func(f *llm.Finding) { f.Severity = "high"; f.Line = 2; f.Concept = "error handling" }),
		finding(func(f *llm.Finding) { f.Severity = "medium"; f.Line = 3; f.Concept = "input validation" }),
		finding(func(f *llm.Finding) { f.Severity = "high"; f.Line = 10; f.Concept = "race condition" }),
	}

	opts := baseOpts()
	opts.MaxInlineComments = 2
	got := PostProcess(in, opts)

	if len(got.Kept) != 2 {
		t.Fatalf("kept %d, want 2", len(got.Kept))
	}
	for _, f := range got.Kept {
		if f.Severity != "high" {
			t.Errorf("cap kept a %s finding over a high one", f.Severity)
		}
	}
	// The two dropped ones must be dropped for the cap, not misclassified.
	for _, d := range got.Dropped {
		if d.Reason != DropOverCap {
			t.Errorf("reason = %q, want %q", d.Reason, DropOverCap)
		}
	}
}

func TestPostProcessCapBreaksTiesOnConfidence(t *testing.T) {
	in := []llm.Finding{
		finding(func(f *llm.Finding) { f.Line = 1; f.Confidence = 0.7; f.Concept = "a thing" }),
		finding(func(f *llm.Finding) { f.Line = 2; f.Confidence = 0.99; f.Concept = "b thing" }),
	}
	opts := baseOpts()
	opts.MaxInlineComments = 1

	got := PostProcess(in, opts)
	if len(got.Kept) != 1 {
		t.Fatalf("kept %d, want 1", len(got.Kept))
	}
	if got.Kept[0].Confidence != 0.99 {
		t.Errorf("kept confidence %v, want the more confident 0.99", got.Kept[0].Confidence)
	}
}

func TestPostProcessDedupe(t *testing.T) {
	f := finding(nil)

	t.Run("against our own earlier marker", func(t *testing.T) {
		opts := baseOpts()
		opts.Existing = []ghc.ExistingComment{{
			Path: "a.go", Line: 2,
			Body: "**Unchecked error**\n\nsome text\n" + conceptMarker("a.go", 2, "error handling"),
		}}

		got := PostProcess([]llm.Finding{f}, opts)
		if len(got.Kept) != 0 {
			t.Fatal("repeated the same concept at the same place")
		}
		if got.Dropped[0].Reason != DropDuplicate {
			t.Errorf("reason = %q, want %q", got.Dropped[0].Reason, DropDuplicate)
		}
	})

	t.Run("a different concept at the same line is allowed", func(t *testing.T) {
		opts := baseOpts()
		opts.Existing = []ghc.ExistingComment{{
			Path: "a.go", Line: 2,
			Body: "text\n" + conceptMarker("a.go", 2, "naming"),
		}}
		if got := PostProcess([]llm.Finding{f}, opts); len(got.Kept) != 1 {
			t.Fatalf("kept %d, want 1: %+v", len(got.Kept), got.Dropped)
		}
	})

	t.Run("concept comparison ignores case and spacing", func(t *testing.T) {
		opts := baseOpts()
		opts.Existing = []ghc.ExistingComment{{
			Path: "a.go", Line: 2, Body: conceptMarker("a.go", 2, "Error   Handling"),
		}}
		if got := PostProcess([]llm.Finding{f}, opts); len(got.Kept) != 0 {
			t.Fatal("concept matching should be case and whitespace insensitive")
		}
	})

	t.Run("an unmarked earlier comment blocks the whole line", func(t *testing.T) {
		// Comments from before markers existed give us no concept, so we stay
		// quiet on that line rather than risk repeating ourselves.
		opts := baseOpts()
		opts.Existing = []ghc.ExistingComment{{Path: "a.go", Line: 2, Body: "an old comment"}}
		if got := PostProcess([]llm.Finding{f}, opts); len(got.Kept) != 0 {
			t.Fatal("expected an unmarked comment to suppress that line")
		}
	})

	t.Run("duplicates within one response", func(t *testing.T) {
		got := PostProcess([]llm.Finding{f, f}, baseOpts())
		if len(got.Kept) != 1 {
			t.Fatalf("kept %d, want 1", len(got.Kept))
		}
	})
}

func TestPostProcessIsDeterministic(t *testing.T) {
	in := make([]llm.Finding, 0, 8)
	for i := 1; i <= 3; i++ {
		in = append(in, finding(func(f *llm.Finding) {
			f.Line = i
			f.Concept = fmt.Sprintf("concept %d", i)
		}))
	}

	first := PostProcess(in, baseOpts())
	for i := 0; i < 5; i++ {
		next := PostProcess(in, baseOpts())
		if len(next.Kept) != len(first.Kept) {
			t.Fatal("kept count varies between runs")
		}
		for j := range next.Kept {
			if next.Kept[j].Concept != first.Kept[j].Concept {
				t.Fatalf("order varies: %q vs %q", next.Kept[j].Concept, first.Kept[j].Concept)
			}
		}
	}
}

func TestPostProcessEmptyInput(t *testing.T) {
	got := PostProcess(nil, baseOpts())
	if len(got.Kept) != 0 || len(got.Dropped) != 0 {
		t.Fatal("no findings in, nothing out")
	}
}

func TestTierFor(t *testing.T) {
	tests := []struct {
		association string
		want        Tier
	}{
		{"FIRST_TIME_CONTRIBUTOR", TierNewcomer},
		{"FIRST_TIMER", TierNewcomer},
		{"first_time_contributor", TierNewcomer},
		{"CONTRIBUTOR", TierContributor},
		{"COLLABORATOR", TierContributor},
		{"NONE", TierContributor},
		{"", TierContributor},
		{"SOMETHING_NEW", TierContributor},
		{"MEMBER", TierInsider},
		{"OWNER", TierInsider},
	}
	for _, tc := range tests {
		t.Run(tc.association, func(t *testing.T) {
			if got := TierFor(tc.association); got != tc.want {
				t.Fatalf("TierFor(%q) = %q, want %q", tc.association, got, tc.want)
			}
		})
	}
}

func TestRenderCommentWithholdsFixUnlessMechanical(t *testing.T) {
	withFix := finding(func(f *llm.Finding) {
		f.SuggestedFix = "if err != nil { return err }"
		f.Explanation = "The error is dropped."
	})

	t.Run("mechanical fix is shown as a suggestion", func(t *testing.T) {
		f := withFix
		f.Mechanical = true
		body := RenderComment(f, ModeReview, TierNewcomer)
		if !strings.Contains(body, "```suggestion") {
			t.Errorf("expected a suggestion block:\n%s", body)
		}
		if !strings.Contains(body, f.SuggestedFix) {
			t.Error("suggestion block missing the fix")
		}
	})

	t.Run("conceptual fix is held back", func(t *testing.T) {
		f := withFix
		f.Mechanical = false
		body := RenderComment(f, ModeReview, TierNewcomer)
		if strings.Contains(body, "```suggestion") {
			t.Errorf("a conceptual fix must not be handed over:\n%s", body)
		}
		if strings.Contains(body, f.SuggestedFix) {
			t.Error("the withheld fix leaked into the comment")
		}
	})

	t.Run("hint mode never shows a fix", func(t *testing.T) {
		f := withFix
		f.Mechanical = true
		body := RenderComment(f, ModeHint, TierNewcomer)
		if strings.Contains(body, "```suggestion") {
			t.Errorf("hint mode must not include a suggestion:\n%s", body)
		}
	})

	t.Run("every comment carries a dedupe marker", func(t *testing.T) {
		body := RenderComment(withFix, ModeReview, TierContributor)
		if got, ok := extractMarker(body); !ok {
			t.Error("no marker found")
		} else if got != "a.go:2:error handling" {
			t.Errorf("marker = %q", got)
		}
	})
}

func TestRenderReviewBodyFallsBackWhenSummaryEmpty(t *testing.T) {
	body := RenderReviewBody(ReviewBodyInput{BotHandle: "shuriken-bot"})
	if strings.TrimSpace(body) == "" {
		t.Fatal("an empty summary must still produce a body")
	}
}

func TestRenderReviewBodyNamesSkippedFiles(t *testing.T) {
	body := RenderReviewBody(ReviewBodyInput{
		Summary:        "Looks reasonable.",
		BotHandle:      "shuriken-bot",
		SkippedForSize: []string{"big/one.go", "big/two.go"},
	})
	for _, want := range []string{"big/one.go", "big/two.go", "not reviewed"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// explain is only advertised when something was actually held back, and never
// to insiders who asked for terse output.
func TestRenderReviewBodyAdvertisesExplainOnlyWhenUseful(t *testing.T) {
	with := RenderReviewBody(ReviewBodyInput{
		Summary: "x", BotHandle: "shuriken-bot", HasWithheldFix: true, Tier: TierNewcomer,
	})
	if !strings.Contains(with, "explain") {
		t.Error("expected explain to be offered when a fix was withheld")
	}

	without := RenderReviewBody(ReviewBodyInput{
		Summary: "x", BotHandle: "shuriken-bot", HasWithheldFix: false, Tier: TierNewcomer,
	})
	if strings.Contains(without, "explain") {
		t.Error("explain offered when nothing was withheld")
	}

	insider := RenderReviewBody(ReviewBodyInput{
		Summary: "x", BotHandle: "shuriken-bot", HasWithheldFix: true, Tier: TierInsider,
	})
	if strings.Contains(insider, "explain") {
		t.Error("explain should not be offered to insiders")
	}
}

func TestRenderExplainWithoutFindings(t *testing.T) {
	body := RenderExplain(nil)
	if !strings.Contains(body, "review") {
		t.Errorf("expected a pointer to running a review first:\n%s", body)
	}
}

func TestRenderExplainRevealsWithheldFix(t *testing.T) {
	body := RenderExplain([]*store.Finding{{
		Title: "Unchecked error", Concept: "error handling", Severity: store.SeverityHigh,
		Path: "a.go", Line: 2,
		Explanation:  "The returned error is discarded.",
		SuggestedFix: "if err != nil { return err }",
	}})

	for _, want := range []string{"Unchecked error", "error handling", "discarded", "return err"} {
		if !strings.Contains(body, want) {
			t.Errorf("explain body missing %q:\n%s", want, body)
		}
	}
}
