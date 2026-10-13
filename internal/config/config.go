// Package config loads and validates Shuriken's configuration from the
// environment. Secrets are held in dedicated types that refuse to render
// themselves, so an accidental %v or slog call cannot leak them.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Secret wraps a sensitive string. Its String, GoString and MarshalJSON
// methods all redact, so it is safe to log a struct that contains one.
type Secret string

func (s Secret) String() string               { return "[REDACTED]" }
func (s Secret) GoString() string             { return "[REDACTED]" }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// Reveal returns the underlying value. Call it only where the secret is
// actually used, never in a log or error path.
func (s Secret) Reveal() string { return string(s) }

// Empty reports whether the secret was unset.
func (s Secret) Empty() bool { return len(s) == 0 }

// DefaultExcludeGlobs is the noise filter applied to every PR diff. These are
// files whose contents are generated, vendored, minified or binary, where a
// review comment is never useful.
var DefaultExcludeGlobs = []string{
	// Lockfiles
	"**/package-lock.json", "**/pnpm-lock.yaml", "**/yarn.lock", "**/go.sum",
	"**/Cargo.lock", "**/poetry.lock", "**/composer.lock", "**/Gemfile.lock",
	"**/*.lock",
	// Vendored and generated
	"**/vendor/**", "**/node_modules/**", "**/dist/**", "**/build/**",
	"**/out/**", "**/.next/**", "**/coverage/**", "**/__generated__/**",
	"**/*.pb.go", "**/*_pb2.py", "**/*.gen.go", "**/*.generated.*",
	"**/*.d.ts",
	// Minified
	"**/*.min.js", "**/*.min.css", "**/*.map",
	// Snapshots
	"**/__snapshots__/**", "**/*.snap",
	// Binaries and media
	"**/*.png", "**/*.jpg", "**/*.jpeg", "**/*.gif", "**/*.ico", "**/*.webp",
	"**/*.svg", "**/*.pdf", "**/*.zip", "**/*.gz", "**/*.tar", "**/*.woff",
	"**/*.woff2", "**/*.ttf", "**/*.eot", "**/*.mp4", "**/*.wasm",
	"**/*.so", "**/*.dylib", "**/*.dll", "**/*.exe", "**/*.bin",
}

// Config is the fully resolved configuration. It is immutable after Load.
type Config struct {
	// HTTP
	Addr            string
	ShutdownTimeout time.Duration

	// GitHub App
	AppID         int64
	PrivateKeyPEM Secret
	WebhookSecret Secret
	BotHandle     string
	ReviewLabel   string

	// Anthropic
	AnthropicAPIKey Secret
	Model           string
	Effort          string
	MaxOutputTokens int64
	MaxToolCalls    int

	// Review tuning
	MaxDiffBytes      int
	MaxInlineComments int
	MinConfidence     float64
	MinSeverity       string
	ExcludeGlobs      []string

	// Pricing, USD per million tokens, for cost logging only.
	PriceInputPerMTok      float64
	PriceOutputPerMTok     float64
	PriceCacheReadPerMTok  float64
	PriceCacheWritePerMTok float64

	// Storage
	MongoURI      Secret
	MongoDatabase string
	DeliveryTTL   time.Duration
	EnsureIndexes bool

	// Runtime
	QueueSize  int
	Workers    int
	JobTimeout time.Duration
	DryRun     bool
	LogLevel   string
}

// Load reads configuration from the environment and validates it. In dry-run
// mode the GitHub, Anthropic and Mongo credentials are optional, so the
// pipeline can be exercised without any live dependency.
func Load() (*Config, error) {
	c := &Config{
		Addr:            env("SHURIKEN_ADDR", ":8080"),
		ShutdownTimeout: envDuration("SHURIKEN_SHUTDOWN_TIMEOUT", 20*time.Second),

		PrivateKeyPEM: Secret(env("SHURIKEN_GITHUB_PRIVATE_KEY", "")),
		WebhookSecret: Secret(env("SHURIKEN_GITHUB_WEBHOOK_SECRET", "")),
		BotHandle:     strings.TrimPrefix(env("SHURIKEN_BOT_HANDLE", "shuriken-bot"), "@"),
		ReviewLabel:   env("SHURIKEN_REVIEW_LABEL", "needs-review"),

		AnthropicAPIKey: Secret(env("ANTHROPIC_API_KEY", "")),
		Model:           env("SHURIKEN_ANTHROPIC_MODEL", "claude-opus-5-5"),
		Effort:          env("SHURIKEN_EFFORT", "high"),
		MaxOutputTokens: int64(envInt("SHURIKEN_MAX_OUTPUT_TOKENS", 8000)),
		MaxToolCalls:    envInt("SHURIKEN_MAX_TOOL_CALLS", 8),

		MaxDiffBytes:      envInt("SHURIKEN_MAX_DIFF_BYTES", 200_000),
		MaxInlineComments: envInt("SHURIKEN_MAX_INLINE_COMMENTS", 10),
		MinConfidence:     envFloat("SHURIKEN_MIN_CONFIDENCE", 0.6),
		MinSeverity:       strings.ToLower(env("SHURIKEN_MIN_SEVERITY", "low")),

		PriceInputPerMTok:      envFloat("SHURIKEN_PRICE_INPUT_PER_MTOK", 4.00),
		PriceOutputPerMTok:     envFloat("SHURIKEN_PRICE_OUTPUT_PER_MTOK", 20.00),
		PriceCacheReadPerMTok:  envFloat("SHURIKEN_PRICE_CACHE_READ_PER_MTOK", 0.20),
		PriceCacheWritePerMTok: envFloat("SHURIKEN_PRICE_CACHE_WRITE_PER_MTOK", 5.00),

		MongoURI:      Secret(env("SHURIKEN_MONGODB_URI", "")),
		MongoDatabase: env("SHURIKEN_MONGODB_DATABASE", "sos-academy"),
		DeliveryTTL:   time.Duration(envInt("SHURIKEN_DELIVERY_TTL_DAYS", 30)) * 24 * time.Hour,
		EnsureIndexes: envBool("SHURIKEN_ENSURE_INDEXES", false),

		QueueSize:  envInt("SHURIKEN_QUEUE_SIZE", 256),
		Workers:    envInt("SHURIKEN_WORKERS", 4),
		JobTimeout: envDuration("SHURIKEN_JOB_TIMEOUT", 5*time.Minute),
		DryRun:     envBool("SHURIKEN_DRY_RUN", false),
		LogLevel:   strings.ToLower(env("SHURIKEN_LOG_LEVEL", "info")),
	}

	if raw := env("SHURIKEN_GITHUB_APP_ID", ""); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("SHURIKEN_GITHUB_APP_ID must be an integer: %w", err)
		}
		c.AppID = id
	}

	// A key file is the usual deployment shape; the inline PEM is for local work.
	if path := env("SHURIKEN_GITHUB_PRIVATE_KEY_FILE", ""); path != "" {
		pem, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading SHURIKEN_GITHUB_PRIVATE_KEY_FILE: %w", err)
		}
		c.PrivateKeyPEM = Secret(pem)
	}

	if globs := env("SHURIKEN_EXCLUDE_GLOBS", ""); globs != "" {
		c.ExcludeGlobs = splitList(globs)
	} else {
		c.ExcludeGlobs = DefaultExcludeGlobs
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error

	// Always required: without the webhook secret we cannot authenticate
	// anything, which is true in dry-run mode too.
	if c.WebhookSecret.Empty() {
		errs = append(errs, errors.New("SHURIKEN_GITHUB_WEBHOOK_SECRET is required"))
	}
	if c.BotHandle == "" {
		errs = append(errs, errors.New("SHURIKEN_BOT_HANDLE must not be empty"))
	}

	// Credentials for live side effects. Dry-run never calls out, so it can
	// run without them.
	if !c.DryRun {
		if c.AppID == 0 {
			errs = append(errs, errors.New("SHURIKEN_GITHUB_APP_ID is required unless SHURIKEN_DRY_RUN=1"))
		}
		if c.PrivateKeyPEM.Empty() {
			errs = append(errs, errors.New("SHURIKEN_GITHUB_PRIVATE_KEY or SHURIKEN_GITHUB_PRIVATE_KEY_FILE is required unless SHURIKEN_DRY_RUN=1"))
		}
		if c.AnthropicAPIKey.Empty() {
			errs = append(errs, errors.New("ANTHROPIC_API_KEY is required unless SHURIKEN_DRY_RUN=1"))
		}
		if c.MongoURI.Empty() {
			errs = append(errs, errors.New("SHURIKEN_MONGODB_URI is required unless SHURIKEN_DRY_RUN=1"))
		}
	}

	if !validSeverity(c.MinSeverity) {
		errs = append(errs, fmt.Errorf("SHURIKEN_MIN_SEVERITY must be high, medium or low, got %q", c.MinSeverity))
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		errs = append(errs, fmt.Errorf("SHURIKEN_MIN_CONFIDENCE must be in [0,1], got %v", c.MinConfidence))
	}
	for _, chk := range []struct {
		name string
		val  int
	}{
		{"SHURIKEN_WORKERS", c.Workers},
		{"SHURIKEN_QUEUE_SIZE", c.QueueSize},
		{"SHURIKEN_MAX_TOOL_CALLS", c.MaxToolCalls},
		{"SHURIKEN_MAX_DIFF_BYTES", c.MaxDiffBytes},
		{"SHURIKEN_MAX_INLINE_COMMENTS", c.MaxInlineComments},
	} {
		if chk.val <= 0 {
			errs = append(errs, fmt.Errorf("%s must be > 0, got %d", chk.name, chk.val))
		}
	}
	if c.MaxOutputTokens <= 0 {
		errs = append(errs, fmt.Errorf("SHURIKEN_MAX_OUTPUT_TOKENS must be > 0, got %d", c.MaxOutputTokens))
	}

	return errors.Join(errs...)
}

func validSeverity(s string) bool {
	switch s {
	case "high", "medium", "low":
		return true
	}
	return false
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return def
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
