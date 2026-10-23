package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/sos-academy/shuriken/internal/store"
)

// Options configure the publisher.
type Options struct {
	// URL is the consumer's endpoint. Empty disables publishing entirely,
	// which is the single-tenant-without-a-consumer case.
	URL string
	// Secret signs every request body.
	Secret string
	// Interval is how often the outbox is drained.
	Interval time.Duration
	// Batch is the most events claimed per tick.
	Batch int
	// MaxAttempts before an event is dead-lettered.
	MaxAttempts int
	// Timeout per HTTP attempt.
	Timeout time.Duration
	// DryRun logs what would be sent and marks nothing.
	DryRun bool
}

// Publisher drains the outbox and POSTs each event to the consumer.
type Publisher struct {
	store  store.Store
	client *http.Client
	log    *slog.Logger
	opts   Options
}

// New builds a Publisher. A zero URL yields a Publisher whose Run returns
// immediately, so the rest of the service does not need to care.
func New(s store.Store, log *slog.Logger, opts Options) *Publisher {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Batch <= 0 {
		opts.Batch = 50
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	return &Publisher{
		store:  s,
		client: &http.Client{Timeout: opts.Timeout},
		log:    log,
		opts:   opts,
	}
}

// Enabled reports whether publishing can run at all. It needs both a consumer
// endpoint and a store to drain.
func (p *Publisher) Enabled() bool { return p.opts.URL != "" && p.store != nil }

// disabledReason names the missing precondition, so the log says which one
// rather than guessing at the endpoint.
func (p *Publisher) disabledReason() string {
	switch {
	case p.opts.URL == "" && p.store == nil:
		return "no consumer endpoint and no store"
	case p.opts.URL == "":
		return "no consumer endpoint configured"
	default:
		return "no store configured, so there is no outbox to drain"
	}
}

// Run drains the outbox until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) {
	if !p.Enabled() {
		p.log.Info("publisher disabled", slog.String("reason", p.disabledReason()))
		return
	}

	p.log.Info("publisher started",
		slog.String("url", p.opts.URL),
		slog.Duration("interval", p.opts.Interval),
		slog.Int("batch", p.opts.Batch),
		slog.Int("maxAttempts", p.opts.MaxAttempts))

	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("publisher stopping")
			return
		case <-ticker.C:
			if n, err := p.drain(ctx); err != nil {
				// A drain failure is transient by assumption; the next tick
				// retries. Events stay in the outbox regardless.
				p.log.WarnContext(ctx, "outbox drain failed",
					slog.Int("delivered", n), slog.Any("error", err))
			}
		}
	}
}

// drain claims and delivers one batch, returning how many succeeded.
func (p *Publisher) drain(ctx context.Context) (int, error) {
	// The lease must outlast a full batch of attempts, or a slow consumer lets
	// another publisher claim rows that are still in flight.
	lease := p.opts.Timeout*time.Duration(p.opts.Batch) + p.opts.Interval

	events, err := p.store.ClaimDue(ctx, p.opts.Batch, lease)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	delivered := 0
	for _, e := range events {
		if ctx.Err() != nil {
			// Shutting down. Unsent events keep their lease and are retried
			// once it lapses.
			return delivered, ctx.Err()
		}
		if p.deliver(ctx, e) {
			delivered++
		}
	}
	return delivered, nil
}

// deliver makes one attempt and records the outcome. It returns whether the
// event was accepted.
func (p *Publisher) deliver(ctx context.Context, e *store.Outbound) bool {
	env := Envelope{
		ID:             e.ID,
		Type:           e.Type,
		IdempotencyKey: e.IdempotencyKey,
		OccurredAt:     e.OccurredAt,
		SentAt:         time.Now().UTC(),
		Attempt:        e.Attempts + 1,
		Data:           e.Payload,
	}

	body, err := json.Marshal(env)
	if err != nil {
		// Unserialisable payload will never succeed, so burn attempts fast
		// rather than retrying a permanent failure.
		p.fail(ctx, e, fmt.Sprintf("marshal envelope: %v", err), 0)
		return false
	}

	if p.opts.DryRun {
		p.log.InfoContext(ctx, "DRY RUN would publish event",
			slog.String("type", e.Type),
			slog.String("idempotencyKey", e.IdempotencyKey),
			slog.String("url", p.opts.URL),
			slog.String("body", string(body)))
		return false
	}

	status, respBody, err := p.post(ctx, body)
	if err != nil {
		p.fail(ctx, e, err.Error(), p.backoff(e.Attempts))
		return false
	}

	switch {
	case status >= 200 && status < 300:
		if err := p.store.MarkPublished(ctx, e.ID); err != nil {
			// Delivered but not recorded: the consumer will see it again and
			// must dedupe. That is why every event carries a key.
			p.log.ErrorContext(ctx, "delivered but could not mark published",
				slog.String("id", e.ID),
				slog.String("idempotencyKey", e.IdempotencyKey),
				slog.Any("error", err))
			return false
		}
		p.log.DebugContext(ctx, "event published",
			slog.String("type", e.Type),
			slog.String("idempotencyKey", e.IdempotencyKey),
			slog.Int("attempt", env.Attempt))
		return true

	case status == http.StatusTooManyRequests || status >= 500:
		// Consumer is unwell. Retry with backoff.
		p.fail(ctx, e, fmt.Sprintf("consumer returned %d: %s", status, respBody),
			p.backoff(e.Attempts))
		return false

	default:
		// 4xx other than 429 means the consumer rejected the event itself.
		// Retrying an unchanged body will not help, so go straight to the
		// dead-letter by exhausting the attempt budget.
		p.log.ErrorContext(ctx, "consumer rejected event, dead-lettering",
			slog.String("id", e.ID),
			slog.String("idempotencyKey", e.IdempotencyKey),
			slog.Int("status", status),
			slog.String("response", respBody))
		if err := p.store.MarkFailed(ctx, e.ID,
			fmt.Sprintf("consumer returned %d: %s", status, respBody), 0, 1); err != nil {
			p.log.ErrorContext(ctx, "could not dead-letter event", slog.Any("error", err))
		}
		return false
	}
}

func (p *Publisher) post(ctx context.Context, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.opts.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}

	var env Envelope
	_ = json.Unmarshal(body, &env)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "shuriken-publisher")
	req.Header.Set(HeaderEvent, env.Type)
	req.Header.Set(HeaderDelivery, env.ID)
	req.Header.Set(HeaderIdempotency, env.IdempotencyKey)
	// Signed over the exact bytes being sent, so the consumer must verify
	// against the raw body and not a re-serialised form.
	req.Header.Set(HeaderSignature, Sign(body, []byte(p.opts.Secret)))

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("post to consumer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read a bounded amount: the response is only used for diagnostics.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, string(bytes.TrimSpace(snippet)), nil
}

func (p *Publisher) fail(ctx context.Context, e *store.Outbound, cause string, retryAfter time.Duration) {
	if err := p.store.MarkFailed(ctx, e.ID, cause, retryAfter, p.opts.MaxAttempts); err != nil {
		p.log.ErrorContext(ctx, "could not record delivery failure",
			slog.String("id", e.ID), slog.Any("error", err))
		return
	}
	level := slog.LevelWarn
	if e.Attempts+1 >= p.opts.MaxAttempts {
		// Last attempt: this event is about to be dead-lettered, which means a
		// consumer will never hear about it without manual replay.
		level = slog.LevelError
	}
	p.log.Log(ctx, level, "event delivery failed",
		slog.String("type", e.Type),
		slog.String("idempotencyKey", e.IdempotencyKey),
		slog.Int("attempt", e.Attempts+1),
		slog.Int("maxAttempts", p.opts.MaxAttempts),
		slog.Duration("retryIn", retryAfter),
		slog.String("cause", cause))
}

// backoff is exponential with a ceiling, so a consumer that is down for an
// hour is retried on a sane cadence rather than hammered.
func (p *Publisher) backoff(attempts int) time.Duration {
	const (
		base = 15 * time.Second
		max  = 30 * time.Minute
	)
	d := time.Duration(math.Pow(2, float64(attempts))) * base
	if d > max || d <= 0 {
		return max
	}
	return d
}

// ErrDisabled is returned by PublishNow when no consumer is configured.
var ErrDisabled = errors.New("publisher: no consumer endpoint configured")

// PublishNow drains once, for tests and for a manual flush.
func (p *Publisher) PublishNow(ctx context.Context) (int, error) {
	if !p.Enabled() {
		return 0, ErrDisabled
	}
	return p.drain(ctx)
}
