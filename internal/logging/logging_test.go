package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// capture builds a logger writing to a buffer, with the redacting handler in
// front of it exactly as New does.
func capture(secrets []string) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(&redactingHandler{inner: inner, secrets: nonTrivial(secrets)}), &buf
}

const (
	apiKey   = "sk-ant-api-key-that-must-never-be-logged"
	mongoURI = "mongodb://admin:hunter2hunter2@cluster0.example.com:27017/db"
)

func TestRedactsSecretsEverywhere(t *testing.T) {
	log, buf := capture([]string{apiKey, mongoURI})

	tests := []struct {
		name string
		emit func()
	}{
		{"in the message", func() { log.Info("connecting with " + apiKey) }},
		{"in a string attribute", func() { log.Info("connect", slog.String("uri", mongoURI)) }},
		{"in an error attribute", func() {
			log.Error("failed", slog.Any("error", errors.New("dial "+mongoURI+" refused")))
		}},
		{"in a wrapped error", func() {
			inner := errors.New("bad credentials for " + apiKey)
			log.Error("failed", slog.Any("error", errors.Join(inner, errors.New("giving up"))))
		}},
		{"inside a group", func() {
			log.Info("cfg", slog.Group("db", slog.String("uri", mongoURI)))
		}},
		{"via WithAttrs", func() {
			log.With(slog.String("key", apiKey)).Info("startup")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			tc.emit()

			out := buf.String()
			if out == "" {
				t.Fatal("nothing was logged")
			}
			for _, secret := range []string{apiKey, mongoURI, "hunter2hunter2"} {
				if strings.Contains(out, secret) {
					t.Errorf("leaked %q:\n%s", secret, out)
				}
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("expected a redaction marker:\n%s", out)
			}
		})
	}
}

// Redaction must not corrupt the surrounding record.
func TestOutputStaysValidJSON(t *testing.T) {
	log, buf := capture([]string{apiKey})
	log.Info("startup", slog.String("key", apiKey), slog.Int("port", 8080))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf)
	}
	if record["key"] != "[REDACTED]" {
		t.Errorf("key = %v, want [REDACTED]", record["key"])
	}
	if record["port"] != float64(8080) {
		t.Errorf("port = %v, want 8080 untouched", record["port"])
	}
	if record["msg"] != "startup" {
		t.Errorf("msg = %v", record["msg"])
	}
}

// A short or empty "secret" would mangle unrelated output without protecting
// anything, so it is not registered.
func TestIgnoresTrivialSecrets(t *testing.T) {
	if got := nonTrivial([]string{"", "a", "abc", "1234567"}); len(got) != 0 {
		t.Errorf("nonTrivial kept %v, want nothing", got)
	}
	if got := nonTrivial([]string{"12345678"}); len(got) != 1 {
		t.Errorf("nonTrivial dropped an 8-character secret: %v", got)
	}

	// An empty secret list must not turn every message into [REDACTED].
	log, buf := capture([]string{"", "x"})
	log.Info("a perfectly ordinary message")
	if !strings.Contains(buf.String(), "a perfectly ordinary message") {
		t.Errorf("message was mangled:\n%s", buf)
	}
}

func TestNewRespectsLevel(t *testing.T) {
	tests := []struct {
		level     string
		wantDebug bool
	}{
		{"debug", true},
		{"info", false},
		{"warn", false},
		{"error", false},
		{"nonsense", false}, // defaults to info
	}

	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			log := New(tc.level, nil)
			if got := log.Enabled(context.Background(), slog.LevelDebug); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}
			if !log.Enabled(context.Background(), slog.LevelError) {
				t.Error("error level must always be enabled")
			}
		})
	}
}
