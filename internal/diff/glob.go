// Package diff filters pull request file lists, enforces the review size cap
// and validates that a finding points at a line that is actually in the diff.
package diff

import (
	"regexp"
	"strings"
	"sync"
)

var (
	globCacheMu sync.RWMutex
	globCache   = map[string]*regexp.Regexp{}
)

// Match reports whether path matches a glob pattern.
//
// Supported syntax, which is the subset .gitignore-style config actually uses:
//
//	**/   zero or more leading path segments
//	**    anything, including separators
//	*     anything except a separator
//	?     a single character except a separator
//
// Matching is anchored at both ends and case sensitive.
func Match(pattern, path string) bool {
	re, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// MatchAny reports whether path matches any of the patterns.
func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	globCacheMu.RLock()
	re, ok := globCache[pattern]
	globCacheMu.RUnlock()
	if ok {
		return re, nil
	}

	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		// "**/" collapses to an optional run of leading segments, so
		// "**/vendor/**" also matches "vendor/x".
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:[^/]*/)*")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}

	globCacheMu.Lock()
	globCache[pattern] = re
	globCacheMu.Unlock()
	return re, nil
}
