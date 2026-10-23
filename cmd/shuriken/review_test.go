package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/review"
)

func TestParseTarget(t *testing.T) {
	want := reviewTarget{Owner: "Shinobi-Open-Source-Academy", Repo: "shuriken-bot", Number: 42}

	for _, in := range []string{
		"Shinobi-Open-Source-Academy/shuriken-bot#42",
		"https://github.com/Shinobi-Open-Source-Academy/shuriken-bot/pull/42",
		"http://github.com/Shinobi-Open-Source-Academy/shuriken-bot/pull/42",
		// trailing slash, as copied from the address bar
		"https://github.com/Shinobi-Open-Source-Academy/shuriken-bot/pull/42/",
		"  Shinobi-Open-Source-Academy/shuriken-bot#42  ",
	} {
		got, err := parseTarget(in)
		if err != nil {
			t.Errorf("parseTarget(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseTarget(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestParseTargetRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"owner/repo",                    // no number
		"owner/repo#",                   // empty number
		"owner/repo#0",                  // not a pull request
		"owner/repo#-1",                 // negative
		"owner#42",                      // no repo
		"owner/repo#abc",                // not a number
		"https://gitlab.com/o/r/pull/1", // not GitHub
	} {
		if got, err := parseTarget(in); err == nil {
			t.Errorf("parseTarget(%q) = %+v, want an error", in, got)
		}
	}
}

func TestWriteReport(t *testing.T) {
	var buf bytes.Buffer
	if err := writeReport(&buf, reviewTarget{Owner: "o", Repo: "r", Number: 7}, "claude-opus-5-5",
		review.DryRunReport{
			Body:     "## Summary\n\nGood work.",
			Comments: []ghc.ReviewComment{{Path: "main.go", Line: 12, Side: "RIGHT", Body: "Unchecked error."}},
			Dropped:  3,
			Usage:    llm.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 200},
			CostUSD:  0.014,
		}); err != nil {
		t.Fatalf("writeReport() error: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"# o/r#7",
		"Not posted",
		"claude-opus-5-5",
		"Good work.",
		"main.go:12",
		"Unchecked error.",
		"3 finding(s) dropped",
		"$0.0140",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// A review with no inline comments must not print an empty section.
func TestWriteReportWithoutComments(t *testing.T) {
	var buf bytes.Buffer
	if err := writeReport(&buf, reviewTarget{Owner: "o", Repo: "r", Number: 1}, "m",
		review.DryRunReport{Body: "Nothing to flag."}); err != nil {
		t.Fatalf("writeReport() error: %v", err)
	}
	if strings.Contains(buf.String(), "Inline comments") {
		t.Errorf("printed an inline-comment heading with no comments:\n%s", buf.String())
	}
}

// failWriter fails after n successful writes, standing in for a closed pipe.
type failWriter struct {
	ok int
	n  int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n >= f.ok {
		return 0, errors.New("pipe closed")
	}
	f.n++
	return len(p), nil
}

// A closed pipe must surface, which is the whole reason errWriter exists.
func TestWriteReportSurfacesWriteErrors(t *testing.T) {
	for _, ok := range []int{0, 1, 3} {
		err := writeReport(&failWriter{ok: ok}, reviewTarget{Owner: "o", Repo: "r", Number: 1}, "m",
			review.DryRunReport{
				Body:     "body",
				Comments: []ghc.ReviewComment{{Path: "a.go", Line: 1, Side: "RIGHT", Body: "x"}},
			})
		if err == nil {
			t.Errorf("writeReport() with a writer failing after %d writes returned nil", ok)
		}
	}
}
