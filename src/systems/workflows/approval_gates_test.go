package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A single pending gate needs no selector — this is the only shape a linear
// pipeline can produce, and it is what keeps existing clients (portal, CLI)
// working unchanged.
func TestSelectApprovalGate_SingleGateNeedsNoSelector(t *testing.T) {
	gates := []WorkflowStepRun{{StepRunID: "sr-1", StepName: "gate"}}
	got, msg := selectApprovalGate(gates, "")
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.StepRunID != "sr-1" {
		t.Errorf("selected %q, want sr-1", got.StepRunID)
	}
}

// With several gates parked, an unqualified decision must NOT pick one: approving
// the wrong branch cannot be undone.
func TestSelectApprovalGate_AmbiguousIsRejected(t *testing.T) {
	gates := []WorkflowStepRun{
		{StepRunID: "sr-1", StepName: "gate-a"},
		{StepRunID: "sr-2", StepName: "gate-b"},
	}
	_, msg := selectApprovalGate(gates, "")
	if !strings.Contains(msg, "multiple gates") {
		t.Fatalf("msg = %q, want an ambiguity error", msg)
	}
	// The message names the gates so a client can choose.
	if !strings.Contains(msg, "gate-a") || !strings.Contains(msg, "sr-2") {
		t.Errorf("msg = %q, want it to list the pending gates", msg)
	}
}

func TestSelectApprovalGate_SelectorPicksTheRightGate(t *testing.T) {
	gates := []WorkflowStepRun{
		{StepRunID: "sr-1", StepName: "gate-a"},
		{StepRunID: "sr-2", StepName: "gate-b"},
	}
	got, msg := selectApprovalGate(gates, "sr-2")
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.StepName != "gate-b" {
		t.Errorf("selected %q, want gate-b", got.StepName)
	}
	if _, msg := selectApprovalGate(gates, "sr-nope"); !strings.Contains(msg, "no such pending approval gate") {
		t.Errorf("msg = %q, want unknown-gate error", msg)
	}
}

// seedPausedRunWithGates creates a paused run parked on n gates.
func seedPausedRunWithGates(t *testing.T, n int) (runID string, stepRunIDs []string) {
	t.Helper()
	runID = uuid.New().String()
	run := WorkflowRun{RunID: runID, WorkflowID: uuid.New().String(), Status: StatusAwaitingApproval}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	connect().Exec(`UPDATE workflow_runs SET status=? WHERE run_id=?`, StatusAwaitingApproval, runID) //nolint:errcheck
	for i := range n {
		sid := uuid.New().String()
		stepRunIDs = append(stepRunIDs, sid)
		sr := WorkflowStepRun{StepRunID: sid, RunID: runID, StepIndex: i, StepName: "gate"}
		if err := sr.Add(context.Background()); err != nil {
			t.Fatalf("seed step run: %v", err)
		}
		connect().Exec(`UPDATE workflow_step_runs SET status=? WHERE step_run_id=?`, StatusAwaitingApproval, sid) //nolint:errcheck
	}
	return runID, stepRunIDs
}

// Deciding one of several gates must leave the run paused: re-queueing it while
// another branch is still awaiting a human would resume a run that isn't ready.
func TestResumeAfterApproval_WaitsForTheLastGate(t *testing.T) {
	runID, sids := seedPausedRunWithGates(t, 2)

	if err := resumeAfterApproval(context.Background(), runID, sids[0], "approved by a"); err != nil {
		t.Fatalf("approve first gate: %v", err)
	}
	run, err := getRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != StatusAwaitingApproval {
		t.Fatalf("run status = %s after 1 of 2 gates, want still awaiting_approval", run.Status)
	}

	if err := resumeAfterApproval(context.Background(), runID, sids[1], "approved by b"); err != nil {
		t.Fatalf("approve second gate: %v", err)
	}
	run, err = getRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != StatusPending {
		t.Fatalf("run status = %s after the last gate, want pending", run.Status)
	}
}

// Two simultaneous decisions on the SAME gate: one wins, the other gets a
// conflict rather than both being recorded.
func TestResumeAfterApproval_DoubleDecideIsRejected(t *testing.T) {
	runID, sids := seedPausedRunWithGates(t, 1)
	if err := resumeAfterApproval(context.Background(), runID, sids[0], "approved by a"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	err := resumeAfterApproval(context.Background(), runID, sids[0], "approved by b")
	if err != errRunNotAwaiting {
		t.Fatalf("second approve err = %v, want errRunNotAwaiting", err)
	}
}

// A rejection fails the whole run, so sibling gates must not be left stranded in
// awaiting_approval under a failed run.
func TestRejectAfterApproval_CancelsSiblingGates(t *testing.T) {
	runID, sids := seedPausedRunWithGates(t, 2)
	if err := rejectAfterApproval(context.Background(), runID, sids[0], "rejected by a"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	run, err := getRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
	remaining, err := approvalStepRuns(context.Background(), runID)
	if err != nil {
		t.Fatalf("approvalStepRuns: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d gates still awaiting under a failed run, want 0", len(remaining))
	}
}

func TestApprovalStepRuns_ReturnsEveryPendingGate(t *testing.T) {
	runID, _ := seedPausedRunWithGates(t, 3)
	gates, err := approvalStepRuns(context.Background(), runID)
	if err != nil {
		t.Fatalf("approvalStepRuns: %v", err)
	}
	if len(gates) != 3 {
		t.Fatalf("got %d gates, want 3", len(gates))
	}
	// Ordered by step index, so the lowest-indexed gate is first.
	for i, g := range gates {
		if g.StepIndex != i {
			t.Errorf("gate %d has step_index %d, want %d", i, g.StepIndex, i)
		}
	}
}
