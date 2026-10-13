package diff

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sos-academy/shuriken/internal/config"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.min.js", "web/static/app.min.js", true},
		{"**/*.min.js", "app.min.js", true},
		{"**/*.min.js", "app.js", false},
		{"**/vendor/**", "vendor/x/y.go", true},
		{"**/vendor/**", "server/vendor/pkg/a.go", true},
		{"**/vendor/**", "vendored/a.go", false},
		{"**/package-lock.json", "package-lock.json", true},
		{"**/package-lock.json", "apps/web/package-lock.json", true},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"**/*.go", "cmd/main.go", true},
		{"src/?.ts", "src/a.ts", true},
		{"src/?.ts", "src/ab.ts", false},
		// A glob must be anchored, so a partial match is not a match.
		{"**/*.json", "config.jsonc", false},
	}

	for _, tc := range tests {
		t.Run(tc.pattern+" vs "+tc.path, func(t *testing.T) {
			if got := Match(tc.pattern, tc.path); got != tc.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// The shipped default globs must actually catch the noise they name.
func TestDefaultGlobsCatchNoise(t *testing.T) {
	noisy := []string{
		"pnpm-lock.yaml", "apps/web/package-lock.json", "go.sum",
		"vendor/github.com/x/y.go", "node_modules/left-pad/index.js",
		"dist/main.js", "apps/web/.next/build-manifest.json",
		"web/app.min.css", "assets/logo.png", "docs/guide.pdf",
		"internal/pb/service.pb.go", "src/__snapshots__/a.snap",
		"types/global.d.ts", "coverage/lcov.info",
	}
	for _, p := range noisy {
		if !MatchAny(config.DefaultExcludeGlobs, p) {
			t.Errorf("default globs did not exclude %q", p)
		}
	}

	reviewable := []string{
		"internal/review/service.go", "apps/server/src/main.ts",
		"README.md", "Dockerfile", "src/components/Button.tsx",
		"migrations/001_init.sql",
	}
	for _, p := range reviewable {
		if MatchAny(config.DefaultExcludeGlobs, p) {
			t.Errorf("default globs wrongly excluded %q", p)
		}
	}
}

const samplePatch = `@@ -1,4 +1,6 @@
 package main
 
-func old() {}
+func replacement() {
+	return
+}
 
`

func TestBuildLineIndex(t *testing.T) {
	idx := BuildLineIndex([]File{{Path: "main.go", Patch: samplePatch}})

	tests := []struct {
		name string
		path string
		line int
		side string
		want bool
	}{
		// Context line 1 exists on both sides.
		{"context on right", "main.go", 1, SideRight, true},
		{"context on left", "main.go", 1, SideLeft, true},
		// Added lines 3-5 exist only on the right.
		{"addition on right", "main.go", 3, SideRight, true},
		{"addition on right 2", "main.go", 4, SideRight, true},
		{"addition on right 3", "main.go", 5, SideRight, true},
		// Left side covers old lines 1-4 (two leading context, one deletion,
		// one trailing context), so 5 is past the end of the old hunk.
		{"addition not on left", "main.go", 5, SideLeft, false},
		{"trailing context on left", "main.go", 4, SideLeft, true},
		{"trailing context on right", "main.go", 6, SideRight, true},
		// Deleted line 3 exists only on the left.
		{"deletion on left", "main.go", 3, SideLeft, true},
		// Out of range.
		{"beyond hunk", "main.go", 99, SideRight, false},
		{"zero line", "main.go", 0, SideRight, false},
		{"negative line", "main.go", -1, SideRight, false},
		// Unknown file and bad side.
		{"unknown file", "other.go", 1, SideRight, false},
		{"bad side", "main.go", 1, "MIDDLE", false},
		// An empty side defaults to RIGHT, matching the GitHub API default.
		{"empty side defaults right", "main.go", 3, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := idx.Valid(tc.path, tc.line, tc.side); got != tc.want {
				t.Fatalf("Valid(%q, %d, %q) = %v, want %v", tc.path, tc.line, tc.side, got, tc.want)
			}
		})
	}
}

func TestBuildLineIndexMultipleHunks(t *testing.T) {
	patch := `@@ -1,2 +1,2 @@
 a
-b
+B
@@ -40,2 +40,3 @@
 x
+y
`
	idx := BuildLineIndex([]File{{Path: "f.txt", Patch: patch}})

	for _, line := range []int{1, 2, 40, 41} {
		if !idx.Valid("f.txt", line, SideRight) {
			t.Errorf("expected right line %d to be valid", line)
		}
	}
	// Line 20 is between hunks and is not part of the diff.
	if idx.Valid("f.txt", 20, SideRight) {
		t.Error("line 20 is not in any hunk and must be invalid")
	}
}

func TestBuildLineIndexHandlesMalformedPatch(t *testing.T) {
	// A nil index and a garbage patch must both be safe, not panic.
	var nilIdx *LineIndex
	if nilIdx.Valid("a.go", 1, SideRight) {
		t.Error("nil index must report nothing valid")
	}

	for _, patch := range []string{"", "not a patch at all", "@@ garbage @@\n+x", "@@ -a,b +c,d @@\n+x"} {
		idx := BuildLineIndex([]File{{Path: "a.go", Patch: patch}})
		if idx.Valid("a.go", 1, SideRight) {
			t.Errorf("malformed patch %q yielded a valid anchor", patch)
		}
	}
}

// "\ No newline at end of file" must not shift the line counters.
func TestBuildLineIndexNoNewlineMarker(t *testing.T) {
	patch := "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n\\ No newline at end of file\n"
	idx := BuildLineIndex([]File{{Path: "f", Patch: patch}})
	if !idx.Valid("f", 2, SideRight) {
		t.Error("expected right line 2 to be valid")
	}
	if idx.Valid("f", 3, SideRight) {
		t.Error("the no-newline marker must not advance the line counter")
	}
}

func TestFilterExcludesNoiseAndEmptyPatches(t *testing.T) {
	files := []File{
		{Path: "main.go", Patch: samplePatch, Additions: 3, Deletions: 1},
		{Path: "pnpm-lock.yaml", Patch: "@@ -1 +1 @@\n+x", Additions: 1},
		{Path: "logo.png", Patch: "", Additions: 0},
		{Path: "notes.md", Patch: "@@ -1 +1 @@\n+hello", Additions: 1},
	}

	res := Filter(files, config.DefaultExcludeGlobs, 1_000_000)

	gotIncluded := paths(res.Included)
	if len(gotIncluded) != 2 {
		t.Fatalf("included = %v, want 2 files", gotIncluded)
	}
	// main.go outranks notes.md: code beats docs.
	if gotIncluded[0] != "main.go" {
		t.Errorf("highest risk file = %q, want main.go", gotIncluded[0])
	}
	if len(res.ExcludedNoise) != 2 {
		t.Errorf("excludedNoise = %v, want the lockfile and the binary", res.ExcludedNoise)
	}
	if res.Truncated {
		t.Error("nothing should be truncated under a huge cap")
	}
}

func TestFilterSizeCapKeepsHighestRisk(t *testing.T) {
	big := "@@ -1,1 +1,1 @@\n" + strings.Repeat("+x\n", 200)

	files := []File{
		{Path: "docs/guide.md", Patch: big, Additions: 200},
		{Path: "internal/auth/session.go", Patch: big, Additions: 200},
		{Path: "internal/util/strings.go", Patch: big, Additions: 200},
	}

	// Room for roughly two of the three patches.
	res := Filter(files, nil, len(big)*2+10)

	if !res.Truncated {
		t.Fatal("expected truncation")
	}
	if len(res.Included) != 2 {
		t.Fatalf("included %d files, want 2: %v", len(res.Included), paths(res.Included))
	}
	// The auth path carries a risk bonus, so it must survive the cap.
	if res.Included[0].Path != "internal/auth/session.go" {
		t.Errorf("first included = %q, want internal/auth/session.go", res.Included[0].Path)
	}
	// Docs are damped hardest, so they are the ones dropped.
	if len(res.SkippedForSize) != 1 || res.SkippedForSize[0] != "docs/guide.md" {
		t.Errorf("skippedForSize = %v, want [docs/guide.md]", res.SkippedForSize)
	}
}

// A single file larger than the whole cap is still reviewed: a review of one
// oversized file beats no review at all.
func TestFilterAlwaysIncludesFirstFile(t *testing.T) {
	huge := "@@ -1,1 +1,1 @@\n" + strings.Repeat("+x\n", 5000)
	res := Filter([]File{{Path: "a.go", Patch: huge, Additions: 5000}}, nil, 10)

	if len(res.Included) != 1 {
		t.Fatalf("included %d files, want 1", len(res.Included))
	}
	if res.Truncated {
		t.Error("a single admitted file is not a truncation")
	}
}

func TestFilterDeterministicOrder(t *testing.T) {
	files := make([]File, 0, 6)
	for i := 0; i < 6; i++ {
		files = append(files, File{
			Path:      fmt.Sprintf("pkg/f%d.go", i),
			Patch:     "@@ -1 +1 @@\n+x",
			Additions: 1,
		})
	}

	first := paths(Filter(files, nil, 1_000_000).Included)
	for i := 0; i < 5; i++ {
		if got := paths(Filter(files, nil, 1_000_000).Included); !equal(got, first) {
			t.Fatalf("ordering is not deterministic: %v != %v", got, first)
		}
	}
}

func TestRiskRanking(t *testing.T) {
	same := 100
	code := File{Path: "internal/svc.go", Additions: same}
	docs := File{Path: "docs/readme.md", Additions: same}
	tests := File{Path: "internal/svc_test.go", Additions: same}
	auth := File{Path: "internal/auth/login.go", Additions: same}
	removed := File{Path: "internal/old.go", Deletions: same, Status: "removed"}

	if Risk(code) <= Risk(docs) {
		t.Error("code should outrank docs")
	}
	if Risk(code) <= Risk(tests) {
		t.Error("implementation should outrank tests")
	}
	if Risk(auth) <= Risk(code) {
		t.Error("security-sensitive paths should outrank ordinary code")
	}
	if Risk(code) <= Risk(removed) {
		t.Error("modified code should outrank a pure deletion")
	}
}

func paths(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
