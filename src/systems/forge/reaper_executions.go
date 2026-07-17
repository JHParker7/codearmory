package main

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Execution-reaper tunables, read once at startup.
//
//   - interval: how often to sweep for stuck executions.
//   - grace:    slack added on top of an execution's own timeout before a running
//     row is considered abandoned (covers pod scheduling / image-pull latency that
//     is not charged against the command timeout).
//   - pendingMaxAge: an absolute floor before a *pending* row is reaped, well above
//     any real queue wait, so work merely starved by a busy budget is left alone and
//     only long-abandoned queue entries are cleared.
var (
	execReaperInterval = time.Duration(envIntOrDefault("FORGE_EXEC_REAPER_INTERVAL_SECS", 60)) * time.Second
	execReaperGrace    = int64(envIntOrDefault("FORGE_EXEC_REAPER_GRACE_SECS", 600))
	execPendingMaxAge  = int64(envIntOrDefault("FORGE_EXEC_PENDING_MAX_AGE_SECS", 3600))
)

// startExecutionReaper periodically fails executions stuck in a non-terminal state
// past their deadline — the recovery path for work orphaned when forge restarts (its
// in-process monitor goroutine is gone, so it never records the pod's completion) or
// when a pod vanishes without reporting. This matters beyond tidiness: the scheduler
// (claimPendingExecution) charges every row still marked `running` for its runner
// class's CPU/memory against the cluster budget, so a handful of orphaned running rows
// permanently wedge the queue for every user until they are cleared. A pass runs
// immediately at startup so a restart that orphaned in-flight work unwedges the queue
// without waiting a full interval.
func startExecutionReaper(ctx context.Context, reg *runtimeRegistry) {
	if execReaperInterval <= 0 {
		return
	}
	reapStuckExecutions(ctx, reg)
	ticker := time.NewTicker(execReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapStuckExecutions(ctx, reg)
		}
	}
}

// reapStuckExecutions finds every past-deadline non-terminal execution, best-effort
// tears down any backing job that outlived forge's tracking, and marks the row failed.
func reapStuckExecutions(ctx context.Context, reg *runtimeRegistry) {
	ctx, span := otel.Tracer("forge").Start(ctx, "reap_stuck_executions")
	defer span.End()

	stuck, err := findStuckExecutions(ctx, execReaperGrace, execPendingMaxAge)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "exec reaper: query stuck executions", "error", err)
		return
	}
	for _, ex := range stuck {
		// Best-effort teardown of a job/container that outlived forge's tracking (e.g.
		// a pod still running past its timeout after a forge restart) so it stops
		// consuming real cluster resources. A missing job is expected — the usual case
		// is the pod is already gone — so a Cancel error is only logged, never fatal.
		if rt, gerr := reg.Get(ctx, ex.Backend); gerr == nil {
			if cerr := rt.Cancel(ctx, ex.ExecutionID); cerr != nil {
				slog.WarnContext(ctx, "exec reaper: backend cancel", "execution_id", ex.ExecutionID, "backend", ex.Backend, "error", cerr)
			}
		}
		changed, ferr := failStuckExecution(ctx, ex.ExecutionID)
		if ferr != nil {
			slog.ErrorContext(ctx, "exec reaper: mark failed", "execution_id", ex.ExecutionID, "error", ferr)
			continue
		}
		if changed {
			slog.WarnContext(ctx, "exec reaper: reaped orphaned execution",
				"execution_id", ex.ExecutionID, "prev_status", ex.Status,
				"age_secs", int(time.Since(ex.CreatedAt).Seconds()))
		}
	}
	span.SetStatus(codes.Ok, "")
}
