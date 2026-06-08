package main

import (
	"context"
	"encoding/json"
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
	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return
	}

	// pendingRow holds the raw JSONB bytes for manual unmarshal; using []byte avoids
	// needing GORM's serializer in a raw-SQL scan path.
	type pendingRow struct {
		ExecutionID string `gorm:"column:execution_id"`
		UserID      string `gorm:"column:user_id"`
		Image       string `gorm:"column:image"`
		Command     []byte `gorm:"column:command"`
		Env         []byte `gorm:"column:env"`
		TimeoutSecs int64  `gorm:"column:timeout_secs"`
		RunnerClass string `gorm:"column:runner_class"`
	}
	var raw pendingRow

	// FOR UPDATE SKIP LOCKED lets multiple workers run in parallel: each goroutine
	// locks exactly one pending row and skips any already locked by a sibling,
	// so workers never block each other on the same row.
	result := tx.Raw(`
		SELECT execution_id, user_id, image, command, env, timeout_secs, runner_class
		FROM executions WHERE status = 'pending' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
	`).Scan(&raw)
	if result.Error != nil {
		tx.Rollback()
		slog.Error("worker: query pending row", "error", result.Error)
		return
	}
	if result.RowsAffected == 0 {
		tx.Rollback()
		return
	}

	var exec Execution
	exec.ExecutionID = raw.ExecutionID
	exec.UserID = raw.UserID
	exec.Image = raw.Image
	exec.TimeoutSecs = raw.TimeoutSecs
	exec.RunnerClass = raw.RunnerClass

	if err := json.Unmarshal(raw.Command, &exec.Command); err != nil {
		tx.Rollback()
		slog.Error("worker: unmarshal command", "execution_id", exec.ExecutionID, "error", err)
		return
	}
	if err := json.Unmarshal(raw.Env, &exec.Env); err != nil {
		tx.Rollback()
		slog.Error("worker: unmarshal env", "execution_id", exec.ExecutionID, "error", err)
		return
	}

	if err := tx.Exec(`UPDATE executions SET status = 'running', started_at = now() WHERE execution_id = ?`, exec.ExecutionID).Error; err != nil {
		tx.Rollback()
		slog.Error("worker: mark running", "execution_id", exec.ExecutionID, "error", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
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
		} else if errors.Is(runErr, context.DeadlineExceeded) || isTimed(runErr) {
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

	// context.Background() rather than ctx: the worker ctx may already be cancelled
	// (shutdown or user cancel), but the result must always be persisted.
	if err := connect().WithContext(context.Background()).Exec(`
		UPDATE executions
		SET status = ?, exit_code = ?, stdout = ?, stderr = ?, ended_at = now()
		WHERE execution_id = ?`,
		status, result.ExitCode, result.Stdout, result.Stderr, exec.ExecutionID,
	).Error; err != nil {
		slog.Error("worker: update execution result", "execution_id", exec.ExecutionID, "error", err)
	}
}

// isTimed reports whether err is a timeout from the Kubernetes runtime.
// The K8s runtime returns fmt.Errorf("timed out after %ds", ...) which wraps no
// sentinel, so errors.Is(err, context.DeadlineExceeded) does not match it.
func isTimed(err error) bool {
	return err != nil && err.Error() != "" &&
		len(err.Error()) > 5 && err.Error()[:5] == "timed"
}
