package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPoolRunsTasks(t *testing.T) {
	p := New(10, 2, time.Second, quiet())
	p.Start(context.Background())

	var ran atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		if err := p.Submit(Task{Name: "t", Run: func(context.Context) error {
			defer wg.Done()
			ran.Add(1)
			return nil
		}}); err != nil {
			t.Fatalf("Submit() error: %v", err)
		}
	}

	wg.Wait()
	if err := p.Shutdown(ctxWithTimeout(t, 2*time.Second)); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if got := ran.Load(); got != 5 {
		t.Fatalf("ran %d tasks, want 5", got)
	}
}

// The queue is bounded on purpose: shedding load beats growing memory until
// the process dies.
func TestPoolSubmitRejectsWhenFull(t *testing.T) {
	// No workers started, so nothing drains the queue.
	p := New(2, 1, time.Second, quiet())

	noop := Task{Name: "t", Run: func(context.Context) error { return nil }}
	if err := p.Submit(noop); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := p.Submit(noop); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if err := p.Submit(noop); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third submit = %v, want ErrQueueFull", err)
	}
	if got := p.Depth(); got != 2 {
		t.Errorf("Depth() = %d, want 2", got)
	}
}

// A panic in one job must not take down the process or the other workers.
func TestPoolRecoversFromPanic(t *testing.T) {
	p := New(4, 1, time.Second, quiet())
	p.Start(context.Background())

	var wg sync.WaitGroup
	wg.Add(2)

	if err := p.Submit(Task{Name: "boom", Run: func(context.Context) error {
		defer wg.Done()
		panic("deliberate")
	}}); err != nil {
		t.Fatal(err)
	}

	var survived atomic.Bool
	if err := p.Submit(Task{Name: "after", Run: func(context.Context) error {
		defer wg.Done()
		survived.Store(true)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}

	wg.Wait()
	if err := p.Shutdown(ctxWithTimeout(t, 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !survived.Load() {
		t.Fatal("the worker died with the panicking task")
	}
}

// Each task gets its own deadline, so one slow job cannot hang a worker.
func TestPoolAppliesPerTaskTimeout(t *testing.T) {
	p := New(2, 1, 50*time.Millisecond, quiet())
	p.Start(context.Background())

	done := make(chan error, 1)
	if err := p.Submit(Task{Name: "slow", Run: func(ctx context.Context) error {
		<-ctx.Done()
		done <- ctx.Err()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("task context error = %v, want DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task was never cancelled")
	}

	if err := p.Shutdown(ctxWithTimeout(t, 2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestPoolShutdownRunsQueuedWork(t *testing.T) {
	p := New(10, 1, time.Second, quiet())

	var ran atomic.Int32
	for i := 0; i < 5; i++ {
		if err := p.Submit(Task{Name: "t", Run: func(context.Context) error {
			ran.Add(1)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
	}

	// Start only now, then immediately shut down.
	p.Start(context.Background())
	if err := p.Shutdown(ctxWithTimeout(t, 3*time.Second)); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if got := ran.Load(); got != 5 {
		t.Fatalf("ran %d of 5 queued tasks during shutdown", got)
	}
}

func TestPoolShutdownIsIdempotent(t *testing.T) {
	p := New(1, 1, time.Second, quiet())
	p.Start(context.Background())

	if err := p.Shutdown(ctxWithTimeout(t, time.Second)); err != nil {
		t.Fatal(err)
	}
	// A second call must not panic on a closed channel.
	if err := p.Shutdown(ctxWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("second Shutdown() error: %v", err)
	}
}

func TestPoolTaskErrorsDoNotStopTheWorker(t *testing.T) {
	p := New(4, 1, time.Second, quiet())
	p.Start(context.Background())

	var wg sync.WaitGroup
	wg.Add(2)

	_ = p.Submit(Task{Name: "fails", Run: func(context.Context) error {
		defer wg.Done()
		return errors.New("expected failure")
	}})

	var after atomic.Bool
	_ = p.Submit(Task{Name: "after", Run: func(context.Context) error {
		defer wg.Done()
		after.Store(true)
		return nil
	}})

	wg.Wait()
	_ = p.Shutdown(ctxWithTimeout(t, time.Second))
	if !after.Load() {
		t.Fatal("a failing task stopped the worker")
	}
}

func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
