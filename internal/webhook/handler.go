package webhook

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	gh "github.com/google/go-github/v92/github"

	"github.com/sos-academy/shuriken/internal/store"
	"github.com/sos-academy/shuriken/internal/worker"
)

// maxBodyBytes caps a webhook body. GitHub's own limit is 25 MB; we refuse
// anything larger than needed so a bad actor who learns the secret still
// cannot exhaust memory.
const maxBodyBytes = 8 << 20 // 8 MiB

// Router decides what work a parsed event implies.
type Router interface {
	// Route returns the tasks a delivery should produce. It must not perform
	// I/O: the HTTP handler calls it inline, before responding to GitHub.
	Route(delivery Delivery) []worker.Task
}

// Delivery is one verified, parsed webhook delivery.
type Delivery struct {
	ID      string
	Event   string
	Payload any
	// InstallationID is the app installation the event belongs to.
	InstallationID int64
}

// Handler serves POST /webhook and GET /healthz.
type Handler struct {
	secret []byte
	store  store.Store
	pool   Pool
	router Router
	log    *slog.Logger
	dryRun bool
}

// Pool is the subset of worker.Pool the handler needs.
type Pool interface {
	Submit(worker.Task) error
	Depth() int
}

// NewHandler builds the webhook handler.
func NewHandler(secret []byte, s store.Store, pool Pool, router Router, log *slog.Logger, dryRun bool) *Handler {
	return &Handler{secret: secret, store: s, pool: pool, router: router, log: log, dryRun: dryRun}
}

// Routes registers the service's HTTP endpoints.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.handleWebhook)
	mux.HandleFunc("GET /healthz", h.handleHealth)
	return mux
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","queueDepth":%d,"dryRun":%t}`+"\n", h.pool.Depth(), h.dryRun)
}

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	// Signature first: nothing else looks at the body until it is proven to
	// have come from GitHub.
	if err := VerifySignature(r.Header.Get(SignatureHeader), body, h.secret); err != nil {
		// The reason is logged but not returned, so a prober learns nothing
		// about why it failed.
		h.log.WarnContext(r.Context(), "rejected webhook delivery",
			slog.String("deliveryId", gh.DeliveryID(r)),
			slog.String("event", gh.WebHookType(r)),
			slog.Any("error", err))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := gh.WebHookType(r)
	deliveryID := gh.DeliveryID(r)
	log := h.log.With(slog.String("deliveryId", deliveryID), slog.String("event", event))

	payload, err := gh.ParseWebHook(event, body)
	if err != nil {
		// A 400 here stops GitHub retrying something we will never parse.
		log.WarnContext(r.Context(), "unparseable payload", slog.Any("error", err))
		http.Error(w, "unparseable payload", http.StatusBadRequest)
		return
	}

	// Idempotency: a redelivery of an id we have already seen is dropped. The
	// per-activity unique key is the real guard; this is the cheap one.
	if h.store != nil && deliveryID != "" {
		fresh, err := h.store.MarkDelivery(r.Context(), deliveryID, event)
		if err != nil {
			// Storage trouble must not make us reject deliveries GitHub will
			// then retry forever. Continue and rely on the activity dedupe key.
			log.WarnContext(r.Context(), "delivery ledger unavailable", slog.Any("error", err))
		} else if !fresh {
			log.InfoContext(r.Context(), "duplicate delivery ignored")
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	delivery := Delivery{
		ID:             deliveryID,
		Event:          event,
		Payload:        payload,
		InstallationID: installationID(payload),
	}

	tasks := h.router.Route(delivery)
	for _, task := range tasks {
		if err := h.pool.Submit(task); err != nil {
			if errors.Is(err, worker.ErrQueueFull) {
				// 503 asks GitHub to redeliver, which is exactly what we want
				// when we are saturated.
				log.ErrorContext(r.Context(), "queue full, shedding delivery",
					slog.String("task", task.Name))
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			log.ErrorContext(r.Context(), "could not enqueue task", slog.Any("error", err))
		}
	}

	// Answer immediately. GitHub times out slow webhooks, so everything real
	// happens on a worker.
	w.WriteHeader(http.StatusAccepted)
}

// installationID pulls the installation out of whichever event shape arrived.
func installationID(payload any) int64 {
	type hasInstallation interface {
		GetInstallation() *gh.Installation
	}
	if e, ok := payload.(hasInstallation); ok {
		return e.GetInstallation().GetID()
	}
	return 0
}
