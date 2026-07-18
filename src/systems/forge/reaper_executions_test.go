package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// insertExecutionTimed inserts an execution with explicit timeout, status, and
// created/started offsets (in seconds, relative to now — negative = in the past),
// so the reaper's deadline logic can be exercised deterministically.
func insertExecutionTimed(t *testing.T, execID, status string, timeoutSecs, createdOffset int, startedOffset *int) {
	t.Helper()
	started := "NULL"
	args := []any{execID, timeoutSecs, status, createdOffset}
	if startedOffset != nil {
		started = "now() + make_interval(secs => ?)"
		args = append(args, *startedOffset)
	}
	sqlStr := `INSERT INTO executions (execution_id, user_id, image, command, env, timeout_secs, status, created_at, started_at)
	           VALUES (?, 'reaper-user', 'alpine:3.19', '["echo","x"]'::jsonb, '{}'::jsonb, ?, ?, now() + make_interval(secs => ?), ` + started + `)`
	if err := connect().Exec(sqlStr, args...).Error; err != nil {
		t.Fatalf("insertExecutionTimed: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM executions WHERE execution_id = ?`, execID) //nolint:errcheck
	})
}

func statusOf(t *testing.T, execID string) string {
	t.Helper()
	var s string
	if err := connect().Raw(`SELECT status FROM executions WHERE execution_id = ?`, execID).Scan(&s).Error; err != nil {
		t.Fatalf("statusOf: %v", err)
	}
	return s
}

// TestReaper_FindsAndFailsStuckRunning: a running execution past started_at +
// timeout + grace is reaped; one still within its deadline is left alone.
func TestReaper_FindsAndFailsStuckRunning(t *testing.T) {
	requireForgeDB(t)
	ctx := context.Background()
	grace := int64(600)
	pendingAge := int64(3600)

	off := func(v int) *int { return &v }

	// Stuck: started 2h ago, timeout 30s → far past deadline.
	stuckID := "reap-stuck-" + uuid.New().String()
	insertExecutionTimed(t, stuckID, StatusRunning, 30, -7200, off(-7200))
	// Fresh: started just now, timeout 30s → deadline is 30s+grace away, not reapable.
	freshID := "reap-fresh-" + uuid.New().String()
	insertExecutionTimed(t, freshID, StatusRunning, 30, -5, off(-5))

	rows, err := findStuckExecutions(ctx, grace, pendingAge)
	if err != nil {
		t.Fatalf("findStuckExecutions: %v", err)
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.ExecutionID] = true
	}
	if !found[stuckID] {
		t.Errorf("stuck running execution not found by reaper")
	}
	if found[freshID] {
		t.Errorf("fresh running execution wrongly flagged for reaping")
	}

	changed, err := failStuckExecution(ctx, stuckID)
	if err != nil || !changed {
		t.Fatalf("failStuckExecution(stuck) = (%v,%v), want (true,nil)", changed, err)
	}
	if got := statusOf(t, stuckID); got != StatusFailed {
		t.Errorf("stuck status = %q, want %q", got, StatusFailed)
	}
	// The fresh one must be untouched.
	if got := statusOf(t, freshID); got != StatusRunning {
		t.Errorf("fresh status = %q, want %q (must not be reaped)", got, StatusRunning)
	}
}

// TestReaper_PendingFloor: a pending execution is reaped only after the absolute
// pending floor, not merely after timeout — so work starved by a busy budget is
// left to run while long-abandoned queue entries are cleared.
func TestReaper_PendingFloor(t *testing.T) {
	requireForgeDB(t)
	ctx := context.Background()
	grace := int64(600)
	pendingAge := int64(3600)

	// Starved-but-recent: created 20m ago, timeout 30s. Past timeout+grace, but under
	// the 1h pending floor → must NOT be reaped.
	recentID := "reap-pend-recent-" + uuid.New().String()
	insertExecutionTimed(t, recentID, StatusPending, 30, -1200, nil)
	// Abandoned: created 2h ago → past the pending floor → reapable.
	oldID := "reap-pend-old-" + uuid.New().String()
	insertExecutionTimed(t, oldID, StatusPending, 30, -7200, nil)

	rows, err := findStuckExecutions(ctx, grace, pendingAge)
	if err != nil {
		t.Fatalf("findStuckExecutions: %v", err)
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.ExecutionID] = true
	}
	if found[recentID] {
		t.Errorf("recent starved pending execution wrongly flagged (below pending floor)")
	}
	if !found[oldID] {
		t.Errorf("abandoned pending execution not found by reaper")
	}
}

// TestReaper_FailGuardsTerminal: failStuckExecution must not clobber a row that
// already reached a terminal state between scan and update.
func TestReaper_FailGuardsTerminal(t *testing.T) {
	requireForgeDB(t)
	ctx := context.Background()
	doneID := "reap-done-" + uuid.New().String()
	insertExecutionTimed(t, doneID, StatusCompleted, 30, -7200, nil)
	changed, err := failStuckExecution(ctx, doneID)
	if err != nil {
		t.Fatalf("failStuckExecution: %v", err)
	}
	if changed {
		t.Errorf("failStuckExecution changed a completed row; must guard on non-terminal status")
	}
	if got := statusOf(t, doneID); got != StatusCompleted {
		t.Errorf("completed row status = %q, want unchanged %q", got, StatusCompleted)
	}
}
