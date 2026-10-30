// Package command parses @mentions of the bot out of issue and pull request
// comments.
//
// Comment bodies are untrusted input written by anyone on the internet. This
// package only ever extracts a short enum from them; it never returns free
// text for downstream code to act on.
package command

import (
	"regexp"
	"strings"
)

// Kind is the command a comment asked for.
type Kind string

const (
	// None means the bot was not mentioned at all.
	None Kind = ""
	// Acknowledge means the bot was mentioned with no recognised command.
	// The activity is recorded and the mention gets a short acknowledgement.
	Acknowledge Kind = "acknowledge"
	// Review asks for a full teaching review of the pull request.
	Review Kind = "review"
	// Hint asks for a nudge without the fix.
	Hint Kind = "hint"
	// Explain asks for the full reasoning behind the latest review.
	Explain Kind = "explain"
	// Help is returned for an unrecognised command word.
	Help Kind = "help"
)

// Parsed is the result of scanning a comment body.
type Parsed struct {
	// Mentioned reports whether the bot handle appeared outside of code.
	Mentioned bool
	// Kind is the requested command.
	Kind Kind
	// Word is the raw command word, lowercased and capped in length. It is
	// only set for Help, so the reply can name what was not understood.
	Word string
}

var (
	// Fenced code blocks, then inline code spans. Stripped before scanning so
	// that a pasted snippet containing the handle does not trigger the bot.
	fencedCode = regexp.MustCompile("(?s)```.*?```|(?s)~~~.*?~~~")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	// Markdown blockquote lines: quoting someone else's mention is not a
	// mention of us.
	quoteLine = regexp.MustCompile(`(?m)^\s*>.*$`)
)

// maxWordLen caps how much of an unknown command word we echo back.
const maxWordLen = 24

// Parse scans a comment body for a mention of handle and returns the command.
//
// handle is the bot's GitHub login without the leading "@" (for example
// "shuriken-bot"). The App's real login has a "[bot]" suffix, so both
// "@shuriken-bot" and "@shuriken-bot[bot]" are recognised.
func Parse(body, handle string) Parsed {
	if handle == "" {
		return Parsed{}
	}

	clean := quoteLine.ReplaceAllString(body, "")
	clean = fencedCode.ReplaceAllString(clean, " ")
	clean = inlineCode.ReplaceAllString(clean, " ")

	// A GitHub App's real login carries a "[bot]" suffix. Normalising it away
	// first keeps the mention pattern a single \b-anchored expression: \b does
	// not match between "]" and a space, so folding the suffix into the
	// pattern would silently fail to see "@shuriken-bot[bot] review".
	botSuffix := regexp.MustCompile(`(?i)(@` + regexp.QuoteMeta(handle) + `)\[bot\]`)
	clean = botSuffix.ReplaceAllString(clean, "$1")

	// \b after the handle is what stops "@shuriken-botanist" counting as a
	// mention of "@shuriken-bot".
	mention := regexp.MustCompile(
		`(?i)@` + regexp.QuoteMeta(handle) + `\b[ \t]*([a-z0-9_-]{1,40})?`,
	)

	m := mention.FindStringSubmatch(clean)
	if m == nil {
		return Parsed{}
	}

	word := strings.ToLower(strings.TrimSpace(m[1]))
	p := Parsed{Mentioned: true}

	switch word {
	case "":
		p.Kind = Acknowledge
	case "review":
		p.Kind = Review
	case "hint":
		p.Kind = Hint
	case "explain":
		p.Kind = Explain
	case "help":
		p.Kind = Help
	default:
		p.Kind = Help
		if len(word) > maxWordLen {
			word = word[:maxWordLen]
		}
		p.Word = word
	}
	return p
}

// HelpText is the reply for an unrecognised command. It is intentionally one
// short block: the bot should not be chatty.
func HelpText(handle, word string) string {
	var b strings.Builder
	if word != "" {
		b.WriteString("I don't know `")
		b.WriteString(word)
		b.WriteString("`. ")
	}
	b.WriteString("I understand:\n\n")
	b.WriteString("- `@")
	b.WriteString(handle)
	b.WriteString(" review` — a teaching review of this PR\n")
	b.WriteString("- `@")
	b.WriteString(handle)
	b.WriteString(" hint` — a nudge, without the fix\n")
	b.WriteString("- `@")
	b.WriteString(handle)
	b.WriteString(" explain` — the full reasoning behind my last review\n")
	return b.String()
}
