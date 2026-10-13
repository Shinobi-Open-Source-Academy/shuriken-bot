// Package logging builds the application logger and guarantees that known
// secrets never reach a log record, even when they are nested inside an
// error or an attribute value.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger at the requested level, wrapped in a handler that
// redacts registered secret values anywhere they appear.
func New(level string, secrets []string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(&redactingHandler{inner: base, secrets: nonTrivial(secrets)})
}

// nonTrivial drops empty and very short values. Redacting a 1-2 character
// string would mangle unrelated log output without protecting anything.
func nonTrivial(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if len(s) >= 8 {
			out = append(out, s)
		}
	}
	return out
}

type redactingHandler struct {
	inner   slog.Handler
	secrets []string
	attrs   []slog.Attr
	groups  []string
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, h.scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(h.scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = h.scrubAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(scrubbed), secrets: h.secrets}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), secrets: h.secrets}
}

func (h *redactingHandler) scrubAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.scrub(a.Value.String()))
	case slog.KindAny:
		// Errors are the common carrier of a leaked connection string.
		if err, ok := a.Value.Any().(error); ok && err != nil {
			return slog.String(a.Key, h.scrub(err.Error()))
		}
		return a
	case slog.KindGroup:
		src := a.Value.Group()
		out := make([]any, 0, len(src))
		for _, g := range src {
			out = append(out, h.scrubAttr(g))
		}
		return slog.Group(a.Key, out...)
	default:
		return a
	}
}

// Scrub replaces every registered secret in s with a placeholder. Exported for
// use in error paths that build strings outside the logger.
func (h *redactingHandler) scrub(s string) string {
	for _, secret := range h.secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}
