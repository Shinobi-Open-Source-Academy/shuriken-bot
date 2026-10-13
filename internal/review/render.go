package review

import (
	"fmt"
	"strings"

	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/store"
)

// severityLabel is the short marker shown on each inline comment.
func severityLabel(s string) string {
	switch store.Severity(s) {
	case store.SeverityHigh:
		return "high"
	case store.SeverityMedium:
		return "medium"
	default:
		return "low"
	}
}

// RenderComment builds the body of one inline review comment.
//
// The suggestion block is withheld unless the fix is mechanical, which is what
// makes `explain` meaningful: for a conceptual finding the reasoning is the
// lesson, and handing over the patch skips it.
func RenderComment(f llm.Finding, mode Mode, tier Tier) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("**%s** · `%s` · `%s`\n\n", strings.TrimSpace(f.Title),
		severityLabel(f.Severity), normalizeConcept(f.Concept)))
	b.WriteString(strings.TrimSpace(f.Explanation))
	b.WriteString("\n")

	showFix := mode == ModeReview && f.Mechanical && strings.TrimSpace(f.SuggestedFix) != ""
	if showFix {
		b.WriteString("\n```suggestion\n")
		b.WriteString(strings.TrimRight(f.SuggestedFix, "\n"))
		b.WriteString("\n```\n")
	} else if mode == ModeReview && strings.TrimSpace(f.SuggestedFix) != "" && tier != TierInsider {
		b.WriteString("\nI have a concrete fix in mind, but this one is worth working out yourself first.\n")
	}

	b.WriteString("\n")
	b.WriteString(conceptMarker(f.Path, f.Line, f.Concept))
	return b.String()
}

// ReviewBodyInput is what the summary comment needs.
type ReviewBodyInput struct {
	Summary        string
	Mode           Mode
	Tier           Tier
	BotHandle      string
	SkippedForSize []string
	ToolCapped     bool
	HasWithheldFix bool
}

// RenderReviewBody builds the single review summary comment.
func RenderReviewBody(in ReviewBodyInput) string {
	var b strings.Builder

	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = "I went through this and did not find anything important to raise."
	}
	b.WriteString(summary)
	b.WriteString("\n")

	if len(in.SkippedForSize) > 0 {
		b.WriteString(fmt.Sprintf("\n<details><summary>%d file(s) were not reviewed (size limit)</summary>\n\n",
			len(in.SkippedForSize)))
		for _, p := range clipList(in.SkippedForSize, 25) {
			b.WriteString("- `" + p + "`\n")
		}
		b.WriteString("\n</details>\n")
	}

	if in.ToolCapped {
		b.WriteString("\n_I hit my file reading budget on this one, so I left out anything I could not verify._\n")
	}

	// Only advertise explain when there is actually something held back.
	if in.HasWithheldFix && in.Tier != TierInsider {
		b.WriteString(fmt.Sprintf("\n---\n\nAsk `@%s explain` if you want the full reasoning and the fixes I held back.\n",
			in.BotHandle))
	}

	return b.String()
}

// RenderExplain builds the reply to an `explain` request: the full reasoning
// and the fixes that were withheld from the original review.
func RenderExplain(findings []*store.Finding) string {
	if len(findings) == 0 {
		return "I don't have a recent review on this pull request to explain. Ask me to `review` first."
	}

	var b strings.Builder
	b.WriteString("Here is the full reasoning behind my last review.\n")

	for _, f := range findings {
		b.WriteString(fmt.Sprintf("\n---\n\n### %s\n\n`%s` · `%s` · `%s:%d`\n\n",
			strings.TrimSpace(f.Title), severityLabel(string(f.Severity)),
			normalizeConcept(f.Concept), f.Path, f.Line))

		if e := strings.TrimSpace(f.Explanation); e != "" {
			b.WriteString(e)
			b.WriteString("\n")
		}
		if fix := strings.TrimSpace(f.SuggestedFix); fix != "" {
			b.WriteString("\nThe fix I had in mind:\n\n```\n")
			b.WriteString(strings.TrimRight(fix, "\n"))
			b.WriteString("\n```\n")
		}
	}
	return b.String()
}
