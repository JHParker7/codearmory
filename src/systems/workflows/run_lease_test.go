package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedRunningRun inserts a run and forces it to 'running' with the given lease SQL
// expression for heartbeat_at (a bare NULL, CURRENT_TIMESTAMP, or an aged literal).
func seedRunningRun(t *testing.T, heartbeatExpr string) WorkflowRun {
	t.Helper()
	run := WorkflowRun{
		RunID: uuid.New().String(), WorkflowID: "w-lease", TriggeredBy: "u", OrgID: "o",
		Status: StatusPending, CreatedAt: time.Now().UTC(),
	}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	if err := connect().Exec(
		`UPDATE workflow_runs SET status='running', started_at=`+heartbeatExpr+`, heartbeat_at=`+heartbeatExpr+` WHERE run_id=?`,
		run.RunID).Error; err != nil {
		t.Fatalf("force running: %v", err)
	}
	return run
}

func runStatus(t *testing.T, runID string) string {
	t.Helper()
	got, err := (WorkflowRun{RunID: runID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return got.(WorkflowRun).Status
}

// Startup recovery must not touch a run a PEER pod is executing right now.
//
// The deployment runs several replicas and a rolling deploy surges a new pod while the
// old one is still working, so an unscoped "fail everything still running" sweep killed
// live runs: the peer's worker kept executing (its in-memory token stays valid, so the
// side effects still happened) while its Complete() — guarded on status='running' —
// silently no-opped, permanently recording a successful run as failed.
func TestRecoverStuckRuns_LeavesRunWithFreshLease(t *testing.T) {
	requireDB(t)
	live := seedRunningRun(t, "CURRENT_TIMESTAMP")

	recoverStuckRunsDB()

	if st := runStatus(t, live.RunID); st != StatusRunning {
		t.Fatalf("a run whose lease is fresh was reclaimed (status %q) — this fails runs executing on peer pods", st)
	}
}

// ...but a run whose worker died leaves its lease frozen, and that one IS reclaimed,
// so a crash still cannot leave a run 'running' forever.
func TestRecoverStuckRuns_ReclaimsStaleLease(t *testing.T) {
	requireDB(t)
	// A lease older than runLeaseStaleAfter (5 min); the literal is compared with the
	// same text/timestamp semantics recoverStuckRunsDB uses.
	dead := seedRunningRun(t, "'2000-01-01 00:00:00'")
	// A pre-lease row (both timestamps NULL) must still be recoverable rather than stuck.
	legacy := seedRunningRun(t, "NULL")

	if n := recoverStuckRunsDB(); n < 2 {
		t.Fatalf("recoverStuckRunsDB reclaimed %d runs, want >= 2", n)
	}
	if st := runStatus(t, dead.RunID); st != StatusFailed {
		t.Errorf("stale-lease run status = %q, want failed", st)
	}
	if st := runStatus(t, legacy.RunID); st != StatusFailed {
		t.Errorf("lease-less run status = %q, want failed", st)
	}
}

// Heartbeat is what keeps a live run out of the sweep's reach, and it only applies to a
// run that is still running — a beat racing a terminal transition must not revive a
// finished run's lease.
func TestRunHeartbeat_RefreshesLeaseOnlyWhileRunning(t *testing.T) {
	requireDB(t)
	run := seedRunningRun(t, "'2000-01-01 00:00:00'")

	run.Heartbeat(context.Background())
	if n := recoverStuckRunsDB(); n > 0 {
		if st := runStatus(t, run.RunID); st != StatusRunning {
			t.Fatalf("run reclaimed after a heartbeat: status %q", st)
		}
	}

	// Terminal run: the beat must not move its lease (nothing to keep alive).
	connect().Exec(`UPDATE workflow_runs SET status='completed', heartbeat_at='2000-01-01 00:00:00' WHERE run_id=?`, run.RunID) //nolint:errcheck
	run.Heartbeat(context.Background())
	var hb *time.Time
	if err := connectRead().Raw(`SELECT heartbeat_at FROM workflow_runs WHERE run_id=?`, run.RunID).Scan(&hb).Error; err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if hb != nil && hb.Year() != 2000 {
		t.Errorf("heartbeat on a terminal run moved the lease to %v", hb)
	}
}

// Dequeue stamps the lease as it claims the run: a run must never sit 'running' with a
// NULL lease, or a peer's recovery would fall back to started_at and could reclaim it
// before the worker's first beat lands.
func TestDequeue_StampsLease(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{
		RunID: uuid.New().String(), WorkflowID: "w-lease-dq", TriggeredBy: "u", OrgID: "o",
		Status: StatusPending, CreatedAt: time.Now().UTC(),
	}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck

	claimed, err := (WorkflowRun{}).Dequeue(context.Background())
	if err != nil || claimed == nil {
		t.Fatalf("dequeue: run=%v err=%v", claimed, err)
	}
	var hb *time.Time
	if err := connectRead().Raw(`SELECT heartbeat_at FROM workflow_runs WHERE run_id=?`, claimed.RunID).Scan(&hb).Error; err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if hb == nil {
		t.Fatal("Dequeue left heartbeat_at NULL — the claimed run has no lease")
	}
	// Cleanup for whichever run was actually claimed (the queue may hold others).
	connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, claimed.RunID) //nolint:errcheck
}

// TestRecoverStuckRunsWithJobs_CapturesTokenBeforeNullingIt is the property the whole
// job-cancellation path depends on. The sweep nulls token as it fails the run, so the
// credential needed to cancel that run's outstanding forge executions exists for
// exactly one moment: before the UPDATE. If the capture ran after — or read the row
// back afterwards — every reclaimed run would come back with an empty token, nothing
// could be cancelled, and the executions would keep their admission reservations until
// the step's own timeout elapsed. That is precisely the 45-minute freeze this fixes.
func TestRecoverStuckRunsWithJobs_CapturesTokenBeforeNullingIt(t *testing.T) {
	requireDB(t)
	dead := seedRunningRun(t, "'2000-01-01 00:00:00'")
	if err := connect().Exec(
		`UPDATE workflow_runs SET token='sealed-token' WHERE run_id=?`, dead.RunID).Error; err != nil {
		t.Fatalf("seed token: %v", err)
	}

	runs, n := recoverStuckRunsWithJobs()
	if n < 1 {
		t.Fatalf("reclaimed %d runs, want >= 1", n)
	}

	var got *abandonedRun
	for i := range runs {
		if runs[i].RunID == dead.RunID {
			got = &runs[i]
		}
	}
	if got == nil {
		t.Fatalf("reclaimed run %s was not returned for job cancellation", dead.RunID)
	}
	if got.Token != "sealed-token" {
		t.Errorf("captured token = %q, want the token as it stood BEFORE the sweep nulled it", got.Token)
	}
	// And the sweep still did its own job.
	if st := runStatus(t, dead.RunID); st != StatusFailed {
		t.Errorf("status = %q, want failed", st)
	}
}

// TestRecoverStuckRunsWithJobs_LeavesFreshLeaseAlone — the capture must not widen what
// the sweep touches. A run being executed by a peer pod is neither failed nor returned
// for cancellation, or a scale-up would cancel live work.
func TestRecoverStuckRunsWithJobs_LeavesFreshLeaseAlone(t *testing.T) {
	requireDB(t)
	live := seedRunningRun(t, "CURRENT_TIMESTAMP")

	runs, _ := recoverStuckRunsWithJobs()
	for _, r := range runs {
		if r.RunID == live.RunID {
			t.Fatalf("run %s has a fresh lease but was returned for job cancellation", live.RunID)
		}
	}
	if st := runStatus(t, live.RunID); st != StatusRunning {
		t.Errorf("status = %q, want running", st)
	}
}
