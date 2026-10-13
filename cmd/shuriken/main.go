// Command shuriken is the SOS Academy review and activity bot.
//
// It serves two endpoints:
//
//	POST /webhook   verified GitHub App webhook deliveries
//	GET  /healthz   liveness and queue depth
//
// All configuration comes from the environment. See the README for the full
// list and for the GitHub App setup.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sos-academy/shuriken/internal/activity"
	"github.com/sos-academy/shuriken/internal/config"
	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
	"github.com/sos-academy/shuriken/internal/logging"
	"github.com/sos-academy/shuriken/internal/review"
	"github.com/sos-academy/shuriken/internal/store"
	"github.com/sos-academy/shuriken/internal/store/mongostore"
	"github.com/sos-academy/shuriken/internal/webhook"
	"github.com/sos-academy/shuriken/internal/worker"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet, so this goes to stderr directly.
		// config.Load never puts a secret in an error.
		fmt.Fprintf(os.Stderr, "shuriken: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Every secret is registered with the logger, so a value that leaks into
	// an error message elsewhere is still redacted on the way out.
	log := logging.New(cfg.LogLevel, []string{
		cfg.WebhookSecret.Reveal(),
		cfg.AnthropicAPIKey.Reveal(),
		cfg.MongoURI.Reveal(),
		cfg.PrivateKeyPEM.Reveal(),
	})
	slog.SetDefault(log)

	log.Info("starting shuriken",
		slog.String("addr", cfg.Addr),
		slog.String("model", cfg.Model),
		slog.String("botHandle", cfg.BotHandle),
		slog.Bool("dryRun", cfg.DryRun),
		slog.Int("workers", cfg.Workers),
		slog.Int("queueSize", cfg.QueueSize))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	if st != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := st.Close(closeCtx); err != nil {
				log.Warn("store close failed", slog.Any("error", err))
			}
		}()
	}

	clients, err := openGitHub(cfg, log)
	if err != nil {
		return err
	}

	var model llm.Client
	if cfg.DryRun && cfg.AnthropicAPIKey.Empty() {
		// Dry run without a key: the pipeline is exercised up to the model
		// call, which returns a canned empty review.
		log.Warn("dry run without ANTHROPIC_API_KEY, using a stub model")
		model = stubModel{}
	} else {
		model = llm.New(llm.Options{
			APIKey:          cfg.AnthropicAPIKey.Reveal(),
			Model:           cfg.Model,
			Effort:          cfg.Effort,
			MaxOutputTokens: cfg.MaxOutputTokens,
			MaxToolCalls:    cfg.MaxToolCalls,
		})
	}

	pool := worker.New(cfg.QueueSize, cfg.Workers, cfg.JobTimeout, log)
	pool.Start(context.Background())

	reviews := review.NewService(clients, model, st, log, review.Options{
		BotHandle:         cfg.BotHandle,
		ExcludeGlobs:      cfg.ExcludeGlobs,
		MaxDiffBytes:      cfg.MaxDiffBytes,
		MaxInlineComments: cfg.MaxInlineComments,
		MinConfidence:     cfg.MinConfidence,
		MinSeverity:       store.Severity(cfg.MinSeverity),
		Model:             cfg.Model,
		DryRun:            cfg.DryRun,
		Pricing: llm.Pricing{
			InputPerMTok:      cfg.PriceInputPerMTok,
			OutputPerMTok:     cfg.PriceOutputPerMTok,
			CacheReadPerMTok:  cfg.PriceCacheReadPerMTok,
			CacheWritePerMTok: cfg.PriceCacheWritePerMTok,
		},
	})

	router := webhook.NewRouter(
		activity.NewRecorder(st, log, cfg.DryRun),
		reviews, clients, cfg.BotHandle, cfg.ReviewLabel, log, cfg.DryRun,
	)

	handler := webhook.NewHandler(
		[]byte(cfg.WebhookSecret.Reveal()), st, pool, router, log, cfg.DryRun,
	)

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler.Routes(),
		// GitHub expects a fast answer; these are generous but bounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Stop taking new deliveries first, then let queued work finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown incomplete", slog.Any("error", err))
	}
	if err := pool.Shutdown(shutdownCtx); err != nil {
		log.Warn("worker shutdown incomplete",
			slog.Int("queued", pool.Depth()), slog.Any("error", err))
	}
	log.Info("stopped")
	return nil
}

// openStore connects to MongoDB. In dry-run mode without a URI it returns nil,
// and every component treats a nil store as "record nothing".
func openStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (store.Store, error) {
	if cfg.MongoURI.Empty() {
		if cfg.DryRun {
			log.Warn("dry run without SHURIKEN_MONGODB_URI, nothing will be persisted")
			return nil, nil
		}
		return nil, errors.New("SHURIKEN_MONGODB_URI is required")
	}

	st, err := mongostore.New(ctx, mongostore.Options{
		URI:      cfg.MongoURI.Reveal(),
		Database: cfg.MongoDatabase,
	})
	if err != nil {
		return nil, err
	}
	log.Info("connected to mongodb", slog.String("database", cfg.MongoDatabase))

	if cfg.EnsureIndexes {
		log.Warn("SHURIKEN_ENSURE_INDEXES is set, creating indexes; use migrations in shared environments")
		if err := st.EnsureIndexes(ctx, cfg.DeliveryTTL); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// openGitHub builds the installation client factory, or a dry-run stub.
func openGitHub(cfg *config.Config, log *slog.Logger) (review.ClientFactory, error) {
	if cfg.DryRun && (cfg.PrivateKeyPEM.Empty() || cfg.AppID == 0) {
		log.Warn("dry run without GitHub App credentials, GitHub reads will fail")
		return stubClients{}, nil
	}
	return ghc.NewAppClientFactory(cfg.AppID, []byte(cfg.PrivateKeyPEM.Reveal()))
}
