package command

import "testing"

func TestParse(t *testing.T) {
	const handle = "shuriken-bot"

	tests := []struct {
		name     string
		body     string
		wantMent bool
		wantKind Kind
		wantWord string
	}{
		{"no mention", "this looks good to me", false, None, ""},
		{"bare mention", "@shuriken-bot", true, Acknowledge, ""},
		{"bare mention with punctuation", "cc @shuriken-bot, thanks!", true, Acknowledge, ""},
		{"review", "@shuriken-bot review", true, Review, ""},
		{"review with trailing prose", "@shuriken-bot review please when you can", true, Review, ""},
		{"hint", "@shuriken-bot hint", true, Hint, ""},
		{"explain", "@shuriken-bot explain", true, Explain, ""},
		{"explicit help", "@shuriken-bot help", true, Help, ""},
		{"case insensitive handle and command", "@Shuriken-Bot REVIEW", true, Review, ""},
		{"bot login suffix", "@shuriken-bot[bot] review", true, Review, ""},
		{"unknown command", "@shuriken-bot summarise", true, Help, "summarise"},
		{"leading whitespace before command", "@shuriken-bot    hint", true, Hint, ""},
		{"mention mid sentence", "hey @shuriken-bot review this", true, Review, ""},
		{"multiline, mention on second line", "thanks!\n\n@shuriken-bot review", true, Review, ""},

		// A mention inside code must not trigger the bot: people paste
		// snippets and documentation into comments all the time.
		{"inside fenced code", "```\n@shuriken-bot review\n```", false, None, ""},
		{"inside tilde fence", "~~~\n@shuriken-bot review\n~~~", false, None, ""},
		{"inside inline code", "run `@shuriken-bot review` to start", false, None, ""},
		{"inside blockquote", "> @shuriken-bot review", false, None, ""},
		{"real mention after a code block", "```\nfoo\n```\n@shuriken-bot hint", true, Hint, ""},

		// Near misses on the handle.
		{"different handle", "@kunai-bot review", false, None, ""},
		{"handle is a prefix of a longer name", "@shuriken-botanist review", false, None, ""},
		{"no at sign", "shuriken-bot review", false, None, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.body, handle)
			if got.Mentioned != tc.wantMent {
				t.Errorf("Mentioned = %v, want %v", got.Mentioned, tc.wantMent)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Word != tc.wantWord {
				t.Errorf("Word = %q, want %q", got.Word, tc.wantWord)
			}
		})
	}
}

func TestParseEmptyHandleNeverMatches(t *testing.T) {
	if got := Parse("@shuriken-bot review", ""); got.Mentioned {
		t.Fatal("an empty handle must never match")
	}
}

func TestParseCapsUnknownWord(t *testing.T) {
	long := "@shuriken-bot " + "averyveryverylongcommandwordthatkeepsgoing"
	got := Parse(long, "shuriken-bot")
	if got.Kind != Help {
		t.Fatalf("Kind = %q, want %q", got.Kind, Help)
	}
	if len(got.Word) > maxWordLen {
		t.Fatalf("Word length %d exceeds cap %d: %q", len(got.Word), maxWordLen, got.Word)
	}
}

func TestHelpTextNamesCommands(t *testing.T) {
	text := HelpText("shuriken-bot", "frobnicate")
	for _, want := range []string{"frobnicate", "review", "hint", "explain"} {
		if !contains(text, want) {
			t.Errorf("help text missing %q:\n%s", want, text)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
