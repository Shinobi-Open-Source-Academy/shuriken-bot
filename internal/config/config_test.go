package config

import (
	"fmt"
	"strings"
	"testing"
)

// A secret must not render itself, whatever formatting verb is used, because
// the whole config struct is logged at startup.
func TestSecretNeverRenders(t *testing.T) {
	const raw = "super-secret-token-value"
	s := Secret(raw)

	for _, got := range []string{
		s.String(),
		fmt.Sprintf("%v", s),
		// Calling String() here would defeat the test: the point is that the
		// %s verb itself redacts, because that is how a stray log call
		// formats a secret.
		fmt.Sprintf("%s", s), //nolint:staticcheck // S1025: verb under test
		fmt.Sprintf("%q", s.String()),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%+v", struct{ Token Secret }{s}),
	} {
		if strings.Contains(got, raw) {
			t.Errorf("secret leaked: %s", got)
		}
	}

	j, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), raw) {
		t.Errorf("secret leaked through JSON: %s", j)
	}

	if s.Reveal() != raw {
		t.Error("Reveal must return the real value")
	}
	if s.Empty() {
		t.Error("a set secret must not report empty")
	}
	if !Secret("").Empty() {
		t.Error("an unset secret must report empty")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", "a-webhook-secret")
	t.Setenv("SHURIKEN_DRY_RUN", "1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"addr", cfg.Addr, ":8080"},
		{"model", cfg.Model, "claude-opus-5-5"},
		{"effort", cfg.Effort, "high"},
		{"bot handle", cfg.BotHandle, "shuriken-bot"},
		{"review label", cfg.ReviewLabel, "needs-review"},
		{"database", cfg.MongoDatabase, "sos-academy"},
		{"min severity", cfg.MinSeverity, "low"},
		{"min confidence", cfg.MinConfidence, 0.6},
		{"max inline comments", cfg.MaxInlineComments, 10},
		{"max tool calls", cfg.MaxToolCalls, 8},
		{"max diff bytes", cfg.MaxDiffBytes, 200_000},
		{"workers", cfg.Workers, 4},
		{"queue size", cfg.QueueSize, 256},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}

	if len(cfg.ExcludeGlobs) == 0 {
		t.Error("exclude globs should default to the built-in noise list")
	}
}

// The webhook secret is the one thing required even in dry run: without it
// nothing can be authenticated at all.
func TestLoadRequiresWebhookSecret(t *testing.T) {
	t.Setenv("SHURIKEN_DRY_RUN", "1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error without a webhook secret")
	}
	if !strings.Contains(err.Error(), "SHURIKEN_GITHUB_WEBHOOK_SECRET") {
		t.Errorf("error should name the missing variable: %v", err)
	}
}

func TestLoadRequiresCredentialsWhenNotDryRun(t *testing.T) {
	t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", "a-webhook-secret")
	t.Setenv("SHURIKEN_DRY_RUN", "0")

	err := func() error { _, e := Load(); return e }()
	if err == nil {
		t.Fatal("expected errors for the missing live credentials")
	}
	// errors.Join reports every problem at once, so one run names them all.
	for _, want := range []string{
		"SHURIKEN_GITHUB_APP_ID",
		"SHURIKEN_GITHUB_PRIVATE_KEY",
		"ANTHROPIC_API_KEY",
		"SHURIKEN_MONGODB_URI",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s:\n%v", want, err)
		}
	}
}

// An error from Load is printed to stderr before the redacting logger exists,
// so it must never carry a secret.
func TestLoadErrorsCarryNoSecrets(t *testing.T) {
	const webhook = "the-webhook-secret-value"
	t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", webhook)
	t.Setenv("SHURIKEN_MONGODB_URI", "mongodb://user:password123@localhost:27017/db")
	t.Setenv("SHURIKEN_MIN_SEVERITY", "nonsense")
	t.Setenv("SHURIKEN_DRY_RUN", "1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	for _, secret := range []string{webhook, "password123"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked %q:\n%v", secret, err)
		}
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"bad severity", map[string]string{"SHURIKEN_MIN_SEVERITY": "critical"}, "SHURIKEN_MIN_SEVERITY"},
		{"confidence above one", map[string]string{"SHURIKEN_MIN_CONFIDENCE": "1.5"}, "SHURIKEN_MIN_CONFIDENCE"},
		{"negative confidence", map[string]string{"SHURIKEN_MIN_CONFIDENCE": "-0.2"}, "SHURIKEN_MIN_CONFIDENCE"},
		{"zero workers", map[string]string{"SHURIKEN_WORKERS": "0"}, "SHURIKEN_WORKERS"},
		{"negative queue", map[string]string{"SHURIKEN_QUEUE_SIZE": "-1"}, "SHURIKEN_QUEUE_SIZE"},
		{"zero tool calls", map[string]string{"SHURIKEN_MAX_TOOL_CALLS": "0"}, "SHURIKEN_MAX_TOOL_CALLS"},
		{"zero output tokens", map[string]string{"SHURIKEN_MAX_OUTPUT_TOKENS": "0"}, "SHURIKEN_MAX_OUTPUT_TOKENS"},
		{"bad app id", map[string]string{"SHURIKEN_GITHUB_APP_ID": "not-a-number"}, "SHURIKEN_GITHUB_APP_ID"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", "a-webhook-secret")
			t.Setenv("SHURIKEN_DRY_RUN", "1")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("expected an error mentioning %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %s", err, tc.wantErr)
			}
		})
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", "a-webhook-secret")
	t.Setenv("SHURIKEN_DRY_RUN", "yes")
	t.Setenv("SHURIKEN_BOT_HANDLE", "@custom-bot")
	t.Setenv("SHURIKEN_ANTHROPIC_MODEL", "claude-sonnet-5-5")
	t.Setenv("SHURIKEN_MIN_SEVERITY", "MEDIUM")
	t.Setenv("SHURIKEN_MIN_CONFIDENCE", "0.8")
	t.Setenv("SHURIKEN_EXCLUDE_GLOBS", "**/*.generated.go, **/testdata/** ,")
	t.Setenv("SHURIKEN_JOB_TIMEOUT", "90s")
	t.Setenv("SHURIKEN_DELIVERY_TTL_DAYS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.DryRun {
		t.Error(`SHURIKEN_DRY_RUN="yes" should enable dry run`)
	}
	// A leading @ in the handle is tolerated and stripped.
	if cfg.BotHandle != "custom-bot" {
		t.Errorf("BotHandle = %q, want custom-bot", cfg.BotHandle)
	}
	if cfg.Model != "claude-sonnet-5-5" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.MinSeverity != "medium" {
		t.Errorf("MinSeverity = %q, want lowercased medium", cfg.MinSeverity)
	}
	if cfg.MinConfidence != 0.8 {
		t.Errorf("MinConfidence = %v", cfg.MinConfidence)
	}
	if len(cfg.ExcludeGlobs) != 2 {
		t.Errorf("ExcludeGlobs = %v, want 2 entries with blanks dropped", cfg.ExcludeGlobs)
	}
	if cfg.JobTimeout.Seconds() != 90 {
		t.Errorf("JobTimeout = %v", cfg.JobTimeout)
	}
	if cfg.DeliveryTTL.Hours() != 7*24 {
		t.Errorf("DeliveryTTL = %v", cfg.DeliveryTTL)
	}
}

func TestLoadReadsPrivateKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/key.pem"
	const pem = "-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----"
	if err := writeFile(path, pem); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", "a-webhook-secret")
	t.Setenv("SHURIKEN_DRY_RUN", "1")
	t.Setenv("SHURIKEN_GITHUB_PRIVATE_KEY_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.PrivateKeyPEM.Reveal() != pem {
		t.Error("the key file was not loaded")
	}
}

func TestLoadReportsMissingPrivateKeyFile(t *testing.T) {
	t.Setenv("SHURIKEN_GITHUB_WEBHOOK_SECRET", "a-webhook-secret")
	t.Setenv("SHURIKEN_DRY_RUN", "1")
	t.Setenv("SHURIKEN_GITHUB_PRIVATE_KEY_FILE", "/nonexistent/key.pem")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a missing key file")
	}
}
