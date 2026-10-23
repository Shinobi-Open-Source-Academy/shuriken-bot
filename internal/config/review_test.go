package config

import (
	"strings"
	"testing"
)

// clearEnv keeps a developer's own environment from deciding these tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SHURIKEN_GITHUB_APP_ID", "SHURIKEN_GITHUB_PRIVATE_KEY",
		"SHURIKEN_GITHUB_PRIVATE_KEY_FILE", "SHURIKEN_GITHUB_WEBHOOK_SECRET",
		"ANTHROPIC_API_KEY", "SHURIKEN_MONGODB_URI", "SHURIKEN_DRY_RUN",
		"SHURIKEN_CONSUMER_WEBHOOK_URL", "SHURIKEN_CONSUMER_WEBHOOK_SECRET",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadForReviewNeedsNoWebhookSecretOrDatabase(t *testing.T) {
	clearEnv(t)
	t.Setenv("SHURIKEN_GITHUB_APP_ID", "123")
	t.Setenv("SHURIKEN_GITHUB_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	cfg, err := LoadForReview()
	if err != nil {
		t.Fatalf("LoadForReview() error: %v", err)
	}
	if cfg.AppID != 123 {
		t.Errorf("AppID = %d, want 123", cfg.AppID)
	}
}

// SHURIKEN_DRY_RUN must not excuse missing credentials here, as it does for
// the server: a stubbed review would prove nothing.
func TestLoadForReviewRequiresRealCredentials(t *testing.T) {
	clearEnv(t)
	t.Setenv("SHURIKEN_DRY_RUN", "1")

	_, err := LoadForReview()
	if err == nil {
		t.Fatal("LoadForReview() succeeded with no credentials; a stubbed review proves nothing")
	}
	for _, want := range []string{"SHURIKEN_GITHUB_APP_ID", "PRIVATE_KEY", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Errorf("review mode asked for a webhook secret it never uses: %v", err)
	}
	if strings.Contains(err.Error(), "MONGODB_URI") {
		t.Errorf("review mode asked for a database it never writes to: %v", err)
	}
}

// The server's own requirements must be unchanged by the split.
func TestLoadStillRequiresWebhookSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("SHURIKEN_DRY_RUN", "1")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SHURIKEN_GITHUB_WEBHOOK_SECRET") {
		t.Fatalf("server mode must still require a webhook secret, got: %v", err)
	}
}
