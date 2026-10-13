package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sos-academy/shuriken/internal/store/storetest"
	"github.com/sos-academy/shuriken/internal/worker"
)

var secret = []byte("test-webhook-secret-value")

type capturingPool struct {
	mu    sync.Mutex
	tasks []worker.Task
	err   error
}

func (p *capturingPool) Submit(t worker.Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.tasks = append(p.tasks, t)
	return nil
}

func (p *capturingPool) Depth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tasks)
}

func (p *capturingPool) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.tasks))
	for _, t := range p.tasks {
		out = append(out, t.Name)
	}
	return out
}

// countingRouter produces a fixed number of trivial tasks per delivery.
type countingRouter struct {
	tasks int
	seen  []Delivery
}

func (r *countingRouter) Route(d Delivery) []worker.Task {
	r.seen = append(r.seen, d)
	out := make([]worker.Task, 0, r.tasks)
	for i := 0; i < r.tasks; i++ {
		out = append(out, worker.Task{Name: "task", Run: func(context.Context) error { return nil }})
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func prOpenedBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"id": 6001, "number": 2,
			"user":       map[string]any{"id": 101, "login": "alice", "type": "User"},
			"html_url":   "https://github.com/sos-academy/dojo/pull/2",
			"created_at": "2026-03-01T12:00:00Z",
		},
		"repository": map[string]any{
			"id": 7001, "name": "dojo", "full_name": "sos-academy/dojo",
			"owner": map[string]any{"login": "sos-academy"},
		},
		"installation": map[string]any{"id": 4242},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func request(t *testing.T, body []byte, event, deliveryID, signature string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	if signature != "" {
		req.Header.Set(SignatureHeader, signature)
	}
	return req
}

func TestHandlerAcceptsValidDelivery(t *testing.T) {
	body := prOpenedBody(t)
	fake := storetest.New()
	pool := &capturingPool{}
	router := &countingRouter{tasks: 1}

	h := NewHandler(secret, fake, pool, router, quiet(), false)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "d-1", Sign(body, secret)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	if len(pool.names()) != 1 {
		t.Fatalf("enqueued %v, want 1 task", pool.names())
	}
	if len(router.seen) != 1 {
		t.Fatal("router was not called")
	}
	if got := router.seen[0]; got.InstallationID != 4242 {
		t.Errorf("InstallationID = %d, want 4242", got.InstallationID)
	}
	if router.seen[0].ID != "d-1" {
		t.Errorf("delivery id = %q", router.seen[0].ID)
	}
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	body := prOpenedBody(t)

	tests := []struct {
		name      string
		signature string
	}{
		{"missing", ""},
		{"wrong secret", Sign(body, []byte("a-completely-different-secret"))},
		{"sha1", "sha1=aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"},
		{"garbage", "sha256=nonsense"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := &capturingPool{}
			router := &countingRouter{tasks: 1}
			h := NewHandler(secret, storetest.New(), pool, router, quiet(), false)

			rec := httptest.NewRecorder()
			h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "d-1", tc.signature))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			// Nothing may be parsed, routed or enqueued on a failed signature.
			if len(pool.names()) != 0 {
				t.Error("work was enqueued for an unverified delivery")
			}
			if len(router.seen) != 0 {
				t.Error("the router saw an unverified delivery")
			}
		})
	}
}

// A tampered body must fail even though the signature is well formed.
func TestHandlerRejectsTamperedBody(t *testing.T) {
	body := prOpenedBody(t)
	signature := Sign(body, secret)
	tampered := append(body[:len(body)-1], []byte(`,"injected":true}`)...)

	h := NewHandler(secret, storetest.New(), &capturingPool{}, &countingRouter{}, quiet(), false)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, request(t, tampered, "pull_request", "d-1", signature))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A redelivery of a delivery id we have already processed must be dropped.
func TestHandlerIsIdempotentOnDeliveryID(t *testing.T) {
	body := prOpenedBody(t)
	signature := Sign(body, secret)

	fake := storetest.New()
	pool := &capturingPool{}
	router := &countingRouter{tasks: 1}
	h := NewHandler(secret, fake, pool, router, quiet(), false)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "same-delivery", signature))
		if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d", i, rec.Code)
		}
	}

	if len(pool.names()) != 1 {
		t.Fatalf("enqueued %d tasks across 3 redeliveries, want 1", len(pool.names()))
	}
	if len(router.seen) != 1 {
		t.Fatalf("router ran %d times, want 1", len(router.seen))
	}
}

// Distinct delivery ids are distinct work, even for the same payload.
func TestHandlerProcessesDistinctDeliveries(t *testing.T) {
	body := prOpenedBody(t)
	signature := Sign(body, secret)

	pool := &capturingPool{}
	h := NewHandler(secret, storetest.New(), pool, &countingRouter{tasks: 1}, quiet(), false)

	for _, id := range []string{"d-1", "d-2"} {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, request(t, body, "pull_request", id, signature))
	}
	if len(pool.names()) != 2 {
		t.Fatalf("enqueued %d, want 2", len(pool.names()))
	}
}

// When the ledger is unavailable we must keep accepting deliveries; the
// per-activity unique key is the real idempotency guard.
func TestHandlerContinuesWhenLedgerFails(t *testing.T) {
	body := prOpenedBody(t)
	fake := storetest.New()
	fake.Err = io.ErrUnexpectedEOF

	pool := &capturingPool{}
	h := NewHandler(secret, fake, pool, &countingRouter{tasks: 1}, quiet(), false)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "d-1", Sign(body, secret)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(pool.names()) != 1 {
		t.Error("a ledger outage must not drop the delivery")
	}
}

// A saturated queue returns 503 so GitHub redelivers later.
func TestHandlerShedsLoadWhenQueueFull(t *testing.T) {
	body := prOpenedBody(t)
	pool := &capturingPool{err: worker.ErrQueueFull}

	h := NewHandler(secret, storetest.New(), pool, &countingRouter{tasks: 1}, quiet(), false)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "d-1", Sign(body, secret)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandlerRejectsUnparseablePayload(t *testing.T) {
	body := []byte(`{not json`)
	h := NewHandler(secret, storetest.New(), &capturingPool{}, &countingRouter{}, quiet(), false)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "d-1", Sign(body, secret)))

	// 400 stops GitHub retrying something we will never parse.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerAcceptsEventWithNoTasks(t *testing.T) {
	body := prOpenedBody(t)
	pool := &capturingPool{}
	h := NewHandler(secret, storetest.New(), pool, &countingRouter{tasks: 0}, quiet(), false)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, request(t, body, "pull_request", "d-1", Sign(body, secret)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(pool.names()) != 0 {
		t.Error("no tasks were expected")
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(secret, storetest.New(), &capturingPool{}, &countingRouter{}, quiet(), true)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var payload struct {
		Status     string `json:"status"`
		QueueDepth int    `json:"queueDepth"`
		DryRun     bool   `json:"dryRun"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("healthz is not valid JSON: %v (%s)", err, rec.Body)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q", payload.Status)
	}
	if !payload.DryRun {
		t.Error("dryRun should be reported")
	}
}

func TestWebhookRejectsWrongMethod(t *testing.T) {
	h := NewHandler(secret, storetest.New(), &capturingPool{}, &countingRouter{}, quiet(), false)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))

	if rec.Code == http.StatusAccepted {
		t.Fatal("GET /webhook must not be accepted")
	}
}
