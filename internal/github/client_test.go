package github

import "testing"

// Shuriken must never act on a bot's event, or two bots can talk to each other
// forever. Both the type field and the login suffix are checked because an
// App's own comments arrive with a "[bot]" login.
func TestIsBotActor(t *testing.T) {
	tests := []struct {
		userType string
		login    string
		want     bool
	}{
		{"Bot", "dependabot[bot]", true},
		{"bot", "renovate[bot]", true},
		{"BOT", "anything", true},
		{"User", "shuriken-bot[bot]", true},
		{"User", "SHURIKEN-BOT[BOT]", true},
		{"User", "alice", false},
		{"User", "bot-enthusiast", false},
		{"", "alice", false},
		{"", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.userType+"/"+tc.login, func(t *testing.T) {
			if got := IsBotActor(tc.userType, tc.login); got != tc.want {
				t.Fatalf("IsBotActor(%q, %q) = %v, want %v", tc.userType, tc.login, got, tc.want)
			}
		})
	}
}

// author_association stands in for write access, which avoids asking for the
// Administration permission. Only these three values grant it.
func TestIsMaintainer(t *testing.T) {
	tests := []struct {
		association string
		want        bool
	}{
		{"OWNER", true},
		{"MEMBER", true},
		{"COLLABORATOR", true},
		{"owner", true},
		{"CONTRIBUTOR", false},
		{"FIRST_TIME_CONTRIBUTOR", false},
		{"FIRST_TIMER", false},
		{"NONE", false},
		{"MANNEQUIN", false},
		{"", false},
		{"ADMIN", false}, // not a real author_association value
	}

	for _, tc := range tests {
		t.Run(tc.association, func(t *testing.T) {
			if got := IsMaintainer(tc.association); got != tc.want {
				t.Fatalf("IsMaintainer(%q) = %v, want %v", tc.association, got, tc.want)
			}
		})
	}
}

func TestSameActorToleratesBotSuffix(t *testing.T) {
	tests := []struct {
		login  string
		handle string
		want   bool
	}{
		{"shuriken-bot[bot]", "shuriken-bot", true},
		{"shuriken-bot", "shuriken-bot", true},
		{"Shuriken-Bot[bot]", "shuriken-bot", true},
		{"alice", "shuriken-bot", false},
		{"kunai-bot[bot]", "shuriken-bot", false},
	}

	for _, tc := range tests {
		t.Run(tc.login, func(t *testing.T) {
			if got := sameActor(tc.login, tc.handle); got != tc.want {
				t.Fatalf("sameActor(%q, %q) = %v, want %v", tc.login, tc.handle, got, tc.want)
			}
		})
	}
}

// A bad private key must fail at boot, not on the first webhook.
func TestNewAppClientFactoryRejectsBadKey(t *testing.T) {
	if _, err := NewAppClientFactory(123, []byte("not a pem key")); err == nil {
		t.Fatal("expected an error for an invalid private key")
	}
}
