// Package worker provides the bounded queue and goroutine pool that let the
// webhook endpoint answer GitHub immediately and do the real work afterwards.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Task is a unit of deferred work. The context it receives carries the per-job
// timeout, not the HTTP request's lifetime.
type Task struct {
	// Name identifies the task in logs.
	Name string
	// Run does the work.
	Run func(ctx context.Context) error
}

// Pool is a fixed-size worker pool fed by a bounded queue.
//
// The queue is bounded on purpose: if GitHub floods us, shedding load with a
// clear log line is better than growing memory until the process dies.
type Pool struct {
	queue   chan Task
	workers int
	timeout time.Duration
	log     *slog.Logger

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// ErrQueueFull is returned by Submit when the queue has no room.
var ErrQueueFull = errors.New("worker: queue full")

// New builds a Pool. It does not start the workers.
func New(queueSize, workers int, timeout time.Duration, log *slog.Logger) *Pool {
	return &Pool{
		queue:   make(chan Task, queueSize),
		workers: workers,
		timeout: timeout,
		log:     log,
	}
}

// Start launches the workers. They drain the queue until it is closed, so work
// already accepted still runs during shutdown.
func (p *Pool) Start(base context.Context) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(id int) {
			defer p.wg.Done()
			log := p.log.With(slog.Int("worker", id))
			for task := range p.queue {
				p.run(base, log, task)
			}
		}(i)
	}
}

func (p *Pool) run(base context.Context, log *slog.Logger, task Task) {
	// A panic in one job must not take down the process and lose the others.
	defer func() {
		if r := recover(); r != nil {
			log.Error("task panicked", slog.String("task", task.Name), slog.Any("panic", r))
		}
	}()

	ctx, cancel := context.WithTimeout(base, p.timeout)
	defer cancel()

	started := time.Now()
	if err := task.Run(ctx); err != nil {
		log.ErrorContext(ctx, "task failed",
			slog.String("task", task.Name),
			slog.Duration("elapsed", time.Since(started)),
			slog.Any("error", err))
		return
	}
	log.DebugContext(ctx, "task done",
		slog.String("task", task.Name),
		slog.Duration("elapsed", time.Since(started)))
}

// Submit enqueues a task without blocking. It returns ErrQueueFull when the
// queue is saturated.
func (p *Pool) Submit(task Task) error {
	select {
	case p.queue <- task:
		return nil
	default:
		return ErrQueueFull
	}
}

// Depth reports the number of queued tasks, for the health endpoint.
func (p *Pool) Depth() int { return len(p.queue) }

// Shutdown stops accepting work and waits for in-flight tasks, or for ctx to
// expire.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.queue) })

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
