package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const pollInterval = 1 * time.Second

// WorkerPool runs executions pulled from the pending queue in PostgreSQL.
type WorkerPool struct {
	rt      Runtime
	cancels sync.Map // executionID -> context.CancelFunc
}

func newWorkerPool(rt Runtime) *WorkerPool {
	return &WorkerPool{rt: rt}
}

// Start launches n worker goroutines. Call with a context that lives for the
// duration of the process; cancel it to drain the pool on shutdown.
func (p *WorkerPool) Start(ctx context.Context, n int) {
	for range n {
		go p.loop(ctx)
	}
}

// Cancel terminates a running execution. Returns false if the execution is not
// currently tracked (already finished or not yet started).
func (p *WorkerPool) Cancel(executionID string) bool {
	if fn, ok := p.cancels.Load(executionID); ok {
		fn.(context.CancelFunc)()
		return true
	}
	return false
}

func (p *WorkerPool) loop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tryOne(ctx)
		}
	}
}

func (p *WorkerPool) tryOne(ctx context.Context) {
	exec, ok := claimPendingExecution(ctx)
	if !ok {
		return
	}
	p.run(ctx, exec)
}

func (p *WorkerPool) run(ctx context.Context, exec Execution) {
	runCtx, cancel := context.WithCancel(ctx)
	p.cancels.Store(exec.ExecutionID, cancel)
	defer func() {
		cancel()
		p.cancels.Delete(exec.ExecutionID)
	}()

	slog.Info("worker: starting execution", "execution_id", exec.ExecutionID, "image", exec.Image)
	result, runErr := p.rt.Run(runCtx, exec)

	status := StatusCompleted
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			status = StatusCancelled
		} else if errors.Is(runErr, context.DeadlineExceeded) {
			status = StatusTimedOut
		} else {
			slog.Error("worker: runtime error", "execution_id", exec.ExecutionID, "error", runErr)
			status = StatusFailed
		}
	} else if result.ExitCode != 0 {
		status = StatusFailed
	}

	meterComplete.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
	slog.Info("worker: execution done", "execution_id", exec.ExecutionID, "status", status, "exit_code", result.ExitCode)

	if err := exec.Complete(ctx, status, result); err != nil {
		slog.Error("worker: update execution result", "execution_id", exec.ExecutionID, "error", err)
	}
}

