package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const pollInterval = 1 * time.Second

// WorkerPool runs executions pulled from the pending queue in PostgreSQL.
type WorkerPool struct {
	db      *pgxpool.Pool
	rt      Runtime
	cancels sync.Map // executionID -> context.CancelFunc
}

func newWorkerPool(db *pgxpool.Pool, rt Runtime) *WorkerPool {
	return &WorkerPool{db: db, rt: rt}
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
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)

	var exec Execution
	var cmdJSON, envJSON []byte

	row := tx.QueryRow(ctx, `
		SELECT execution_id, user_id, image, command, env, timeout_secs
		FROM executions
		WHERE status = 'pending'
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`)
	err = row.Scan(&exec.ExecutionID, &exec.UserID, &exec.Image, &cmdJSON, &envJSON, &exec.TimeoutSecs)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Error("worker: scan pending row", "error", err)
		return
	}

	if err := json.Unmarshal(cmdJSON, &exec.Command); err != nil {
		slog.Error("worker: unmarshal command", "execution_id", exec.ExecutionID, "error", err)
		return
	}
	if err := json.Unmarshal(envJSON, &exec.Env); err != nil {
		slog.Error("worker: unmarshal env", "execution_id", exec.ExecutionID, "error", err)
		return
	}

	_, err = tx.Exec(ctx,
		`UPDATE executions SET status = 'running', started_at = now() WHERE execution_id = $1`,
		exec.ExecutionID,
	)
	if err != nil {
		slog.Error("worker: mark running", "execution_id", exec.ExecutionID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
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
			status = StatusFailed
		}
	} else if result.ExitCode != 0 {
		status = StatusFailed
	}

	meterComplete.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
	slog.Info("worker: execution done", "execution_id", exec.ExecutionID, "status", status, "exit_code", result.ExitCode)

	_, err := p.db.Exec(context.Background(),
		`UPDATE executions
		 SET status    = $1,
		     exit_code = $2,
		     stdout    = $3,
		     stderr    = $4,
		     ended_at  = now()
		 WHERE execution_id = $5`,
		status, result.ExitCode, result.Stdout, result.Stderr, exec.ExecutionID,
	)
	if err != nil {
		slog.Error("worker: update execution result", "execution_id", exec.ExecutionID, "error", err)
	}
}

func isTimed(err error) bool {
	return err != nil && err.Error() != "" &&
		len(err.Error()) > 5 && err.Error()[:5] == "timed"
}
