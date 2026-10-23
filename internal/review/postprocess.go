package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sos-academy/shuriken/internal/diff"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/store"
)

// DropReason explains why a finding was not posted. Kept for logging so review
// quality can be tuned without guesswork.
type DropReason string

// The reasons a finding is not posted. Logged at debug level so review
// quality can be tuned by looking at what is being discarded.
const (
	DropInvalidShape  DropReason = "invalid_shape"
	DropLowConfidence DropReason = "low_confidence"
	DropLowSeverity   DropReason = "below_severity_floor"
	DropUnanchored    DropReason = "line_not_in_diff"
	DropDuplicate     DropReason = "already_commented"
	DropOverCap       DropReason = "over_comment_cap"
)

// Dropped is one rejected finding.
type Dropped struct {
	Finding llm.Finding
	Reason  DropReason
	Detail  string
}

// Decision is the outcome of post-processing.
type Decision struct {
	Kept    []llm.Finding
	Dropped []Dropped
}

// FilterOptions tune post-processing.
type FilterOptions struct {
	MinConfidence     float64
	MinSeverity       store.Severity
	MaxInlineComments int
	// Existing are Shuriken's own earlier comments on this pull request.
	Existing []ghc.ExistingComment
	// Index is the set of commentable anchors built from the filtered diff.
	Index *diff.LineIndex
}

// conceptMarker is embedded as an HTML comment in every posted finding. It is
// invisible on GitHub and makes deduplication against our own earlier comments
// exact instead of a guess at text similarity.
func conceptMarker(path string, line int, concept string) string {
	return fmt.Sprintf("<!-- shuriken:%s:%d:%s -->", path, line, normalizeConcept(concept))
}

// normalizeConcept collapses a concept to a stable comparable form, so
// "Error Handling" and "error  handling" are one concept.
func normalizeConcept(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// PostProcess applies every gate between the model's output and a posted
// comment, in the order that drops the most for the least work.
//
// The order matters for the cap: confidence, severity and anchoring are applied
// first so the cap is spent on findings that would actually have been posted.
func PostProcess(findings []llm.Finding, opts FilterOptions) Decision {
	var d Decision

	seenConcepts, seenAnchors := existingKeys(opts.Existing)
	// Guard against the model reporting the same thing twice in one response.
	thisRun := map[string]bool{}

	type scored struct {
		f llm.Finding
		k string
	}
	var candidates []scored

	for _, f := range findings {
		sev := store.Severity(strings.ToLower(strings.TrimSpace(f.Severity)))
		side := strings.ToUpper(strings.TrimSpace(f.Side))
		if side == "" {
			side = diff.SideRight
		}

		switch {
		case strings.TrimSpace(f.Path) == "" || f.Line <= 0 ||
			strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Concept) == "" ||
			!sev.Valid() || (side != diff.SideRight && side != diff.SideLeft):
			d.Dropped = append(d.Dropped, Dropped{f, DropInvalidShape, "missing or invalid required field"})
			continue

		// Confidence is not range checked by the output schema, so a value
		// outside [0,1] means the model produced nonsense here.
		case f.Confidence < 0 || f.Confidence > 1:
			d.Dropped = append(d.Dropped, Dropped{f, DropInvalidShape,
				fmt.Sprintf("confidence %v outside [0,1]", f.Confidence)})
			continue

		case f.Confidence < opts.MinConfidence:
			d.Dropped = append(d.Dropped, Dropped{f, DropLowConfidence,
				fmt.Sprintf("confidence %.2f < %.2f", f.Confidence, opts.MinConfidence)})
			continue

		case sev.Rank() < opts.MinSeverity.Rank():
			d.Dropped = append(d.Dropped, Dropped{f, DropLowSeverity,
				fmt.Sprintf("severity %s below floor %s", sev, opts.MinSeverity)})
			continue

		// A comment on a line GitHub does not consider part of the diff is
		// rejected, and a rejected comment fails the whole review submission.
		case !opts.Index.Valid(f.Path, f.Line, side):
			d.Dropped = append(d.Dropped, Dropped{f, DropUnanchored,
				fmt.Sprintf("%s:%d (%s) is not in the reviewed diff", f.Path, f.Line, side)})
			continue
		}

		f.Severity = string(sev)
		f.Side = side
		k := dedupeKey(f.Path, f.Line, f.Concept)

		// An exact marker match means we raised this very concept here before.
		// An anchor match means we left a comment on this line that predates
		// markers, so we stay quiet rather than risk repeating ourselves.
		if seenConcepts[k] || thisRun[k] || seenAnchors[anchorKey(f.Path, f.Line)] {
			d.Dropped = append(d.Dropped, Dropped{f, DropDuplicate, "same concept already raised here"})
			continue
		}
		thisRun[k] = true
		candidates = append(candidates, scored{f, k})
	}

	// Highest severity first, then highest confidence, so the cap keeps the
	// most useful feedback. Path and line break ties for determinism.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].f, candidates[j].f
		if ra, rb := store.Severity(a.Severity).Rank(), store.Severity(b.Severity).Rank(); ra != rb {
			return ra > rb
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})

	for i, c := range candidates {
		if i >= opts.MaxInlineComments {
			d.Dropped = append(d.Dropped, Dropped{c.f, DropOverCap,
				fmt.Sprintf("cap of %d inline comments reached", opts.MaxInlineComments)})
			continue
		}
		d.Kept = append(d.Kept, c.f)
	}

	return d
}

func dedupeKey(path string, line int, concept string) string {
	return fmt.Sprintf("%s:%d:%s", path, line, normalizeConcept(concept))
}

func anchorKey(path string, line int) string {
	return fmt.Sprintf("%s:%d", path, line)
}

// existingKeys splits Shuriken's earlier comments into exact concept markers
// and bare path/line anchors. A comment we posted before markers existed, or
// one GitHub returned without a parseable body, only yields an anchor.
func existingKeys(existing []ghc.ExistingComment) (concepts, anchors map[string]bool) {
	concepts, anchors = map[string]bool{}, map[string]bool{}
	for _, c := range existing {
		if marker, ok := extractMarker(c.Body); ok {
			concepts[marker] = true
			continue
		}
		anchors[anchorKey(c.Path, c.Line)] = true
	}
	return concepts, anchors
}

const markerPrefix = "<!-- shuriken:"

func extractMarker(body string) (string, bool) {
	i := strings.Index(body, markerPrefix)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(markerPrefix):]
	j := strings.Index(rest, "-->")
	if j < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:j]), true
}
