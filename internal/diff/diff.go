package diff

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Side identifies which version of a file a line belongs to, matching the
// GitHub review comment API.
const (
	SideRight = "RIGHT" // the head commit; additions and context
	SideLeft  = "LEFT"  // the base commit; deletions and context
)

// File is one changed file in a pull request.
type File struct {
	Path      string
	PrevPath  string // set when the file was renamed
	Status    string // added, modified, removed, renamed, copied, changed
	Additions int
	Deletions int
	Patch     string // unified diff hunks for this file, empty for binaries
}

// Changes is the total number of changed lines.
func (f File) Changes() int { return f.Additions + f.Deletions }

// FilterResult is the outcome of preparing a diff for review.
type FilterResult struct {
	// Included are the files handed to the model, highest risk first.
	Included []File
	// ExcludedNoise are files dropped by the exclude globs or because they
	// carried no textual patch.
	ExcludedNoise []string
	// SkippedForSize are files dropped only because the size cap was reached.
	// These are reported in the review body so the author knows.
	SkippedForSize []string
	// Bytes is the total patch size of the included files.
	Bytes int
	// Truncated reports whether anything was dropped for size.
	Truncated bool
}

// Filter drops noise, ranks the remainder by risk and applies the byte cap.
//
// When the cap is reached the highest-risk files are kept, so a large PR still
// gets a useful review of the parts that matter. Every dropped file is named in
// SkippedForSize so the review can say what it did not look at.
func Filter(files []File, excludeGlobs []string, maxBytes int) FilterResult {
	res := FilterResult{}

	candidates := make([]File, 0, len(files))
	for _, f := range files {
		switch {
		case MatchAny(excludeGlobs, f.Path):
			res.ExcludedNoise = append(res.ExcludedNoise, f.Path)
		case strings.TrimSpace(f.Patch) == "":
			// Binary files and files GitHub declined to render a patch for.
			// There is nothing to review and nothing to anchor a comment to.
			res.ExcludedNoise = append(res.ExcludedNoise, f.Path)
		default:
			candidates = append(candidates, f)
		}
	}

	// Highest risk first, with path as a tiebreak so the order is stable.
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := Risk(candidates[i]), Risk(candidates[j])
		if ri != rj {
			return ri > rj
		}
		return candidates[i].Path < candidates[j].Path
	})

	for _, f := range candidates {
		size := len(f.Patch)
		// Always admit the first file even if it alone blows the cap: a review
		// of one oversized file beats a review of nothing.
		if len(res.Included) > 0 && res.Bytes+size > maxBytes {
			res.SkippedForSize = append(res.SkippedForSize, f.Path)
			res.Truncated = true
			continue
		}
		res.Included = append(res.Included, f)
		res.Bytes += size
	}

	return res
}

// riskyPathHints are path fragments where a mistake tends to be expensive.
var riskyPathHints = []string{
	"auth", "security", "crypt", "password", "secret", "token", "session",
	"payment", "billing", "permission", "middleware", "migration",
}

// codeExtensions carry full weight; everything else is damped below.
var codeExtensions = map[string]float64{
	".go": 1.0, ".ts": 1.0, ".tsx": 1.0, ".js": 1.0, ".jsx": 1.0,
	".py": 1.0, ".rb": 1.0, ".java": 1.0, ".kt": 1.0, ".rs": 1.0,
	".c": 1.0, ".h": 1.0, ".cc": 1.0, ".cpp": 1.0, ".cs": 1.0,
	".php": 1.0, ".swift": 1.0, ".scala": 1.0, ".sql": 1.0, ".sh": 1.0,

	".yml": 0.7, ".yaml": 0.7, ".json": 0.6, ".toml": 0.7, ".tf": 0.9,
	".html": 0.6, ".css": 0.5, ".scss": 0.5,

	".md": 0.25, ".txt": 0.25, ".rst": 0.25,
}

// Risk scores a file so the size cap keeps the parts of a PR most likely to
// contain a real bug. It is a heuristic, not a judgement about the code.
func Risk(f File) float64 {
	score := float64(f.Changes())
	if score == 0 {
		score = 1
	}

	weight := 0.8 // unknown extension: between config and code
	lower := strings.ToLower(f.Path)
	if i := strings.LastIndex(lower, "."); i >= 0 {
		if w, ok := codeExtensions[lower[i:]]; ok {
			weight = w
		}
	}

	// A pure deletion rarely needs teaching feedback.
	if f.Status == "removed" {
		weight *= 0.4
	}
	// Tests matter, but a bug in the implementation matters more.
	if strings.Contains(lower, "_test.") || strings.Contains(lower, ".test.") ||
		strings.Contains(lower, ".spec.") || strings.Contains(lower, "/tests/") {
		weight *= 0.6
	}
	for _, hint := range riskyPathHints {
		if strings.Contains(lower, hint) {
			weight *= 1.6
			break
		}
	}

	return score * weight
}

// Anchor is a commentable position in a diff.
type Anchor struct {
	Path string
	Line int
	Side string
}

// LineIndex records, for each file, which (line, side) pairs a review comment
// can legally be attached to.
type LineIndex struct {
	right map[string]map[int]bool
	left  map[string]map[int]bool
}

// BuildLineIndex parses the patches of the given files into a set of
// commentable anchors.
//
// GitHub rejects a review comment whose line is not part of the diff, and a
// rejected comment fails the whole review submission. Validating locally first
// means one bad finding cannot sink an otherwise good review.
func BuildLineIndex(files []File) *LineIndex {
	idx := &LineIndex{
		right: make(map[string]map[int]bool, len(files)),
		left:  make(map[string]map[int]bool, len(files)),
	}
	for _, f := range files {
		r, l := parsePatchLines(f.Patch)
		idx.right[f.Path] = r
		idx.left[f.Path] = l
	}
	return idx
}

// Valid reports whether a comment may be anchored at path/line/side.
func (ix *LineIndex) Valid(path string, line int, side string) bool {
	if ix == nil || line <= 0 {
		return false
	}
	var m map[string]map[int]bool
	switch strings.ToUpper(side) {
	case SideLeft:
		m = ix.left
	case SideRight, "":
		m = ix.right
	default:
		return false
	}
	lines, ok := m[path]
	return ok && lines[line]
}

// Paths returns the files the index knows about.
func (ix *LineIndex) Paths() []string {
	out := make([]string, 0, len(ix.right))
	for p := range ix.right {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// hunkHeader matches "@@ -oldStart,oldLines +newStart,newLines @@".
// The line counts are optional and default to 1.
func parsePatchLines(patch string) (right, left map[int]bool) {
	right, left = map[int]bool{}, map[int]bool{}
	if patch == "" {
		return
	}

	oldLine, newLine := 0, 0
	inHunk := false

	for _, raw := range strings.Split(patch, "\n") {
		if strings.HasPrefix(raw, "@@") {
			o, n, ok := parseHunkHeader(raw)
			if !ok {
				inHunk = false
				continue
			}
			oldLine, newLine = o, n
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		if raw == "" {
			continue
		}

		switch raw[0] {
		case '+':
			right[newLine] = true
			newLine++
		case '-':
			left[oldLine] = true
			oldLine++
		case ' ':
			// Context lines exist on both sides.
			right[newLine] = true
			left[oldLine] = true
			newLine++
			oldLine++
		case '\\':
			// "\ No newline at end of file" advances neither counter.
		default:
			// Anything else ends the hunk body.
			inHunk = false
		}
	}
	return right, left
}

func parseHunkHeader(line string) (oldStart, newStart int, ok bool) {
	// @@ -12,7 +12,9 @@ optional trailing context
	rest := strings.TrimPrefix(line, "@@")
	end := strings.Index(rest, "@@")
	if end < 0 {
		return 0, 0, false
	}
	spec := strings.TrimSpace(rest[:end])

	var minus, plus string
	for _, part := range strings.Fields(spec) {
		switch {
		case strings.HasPrefix(part, "-"):
			minus = part[1:]
		case strings.HasPrefix(part, "+"):
			plus = part[1:]
		}
	}
	if minus == "" || plus == "" {
		return 0, 0, false
	}

	o, err1 := parseStart(minus)
	n, err2 := parseStart(plus)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return o, n, true
}

func parseStart(s string) (int, error) {
	if i := strings.Index(s, ","); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad hunk start %q: %w", s, err)
	}
	return n, nil
}
