package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A matrix whose value list resolves to nothing must FAIL the run and record a
// visible failed step run — not silently skip the step and pass. Regression for
// "pipeline missing matrix but showing as passed": the empty matrix used to be
// dropped with no step run, so the step vanished from the run view while the run
// still completed. See worker.executeRun's len(tasks)==0 branch.
func TestExecuteRun_EmptyMatrixFailsRun(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	paths := recordingService(t, "emsvc")
	step := seedHTTPStep(t, "tu", "to", "emsvc", "/r/${matrix.region}")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": step.StepID, "matrix": map[string]any{"var": "region", "values_from": "${inputs.regions}"}},
	})

	// The value source resolves to an empty JSON array — the matrix fans out to
	// zero executions.
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: wf.WorkflowID, TriggeredBy: "tu", OrgID: "to", Status: "pending", Inputs: map[string]string{"regions": `[]`}, CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                               //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)       //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id=?`, run.RunID) }) //nolint:errcheck
	newWorkerPool().executeRun(context.Background(), run.RunID, wf.WorkflowID, "", "", "tu", run.Inputs, 0, "")

	got, _ := getRun(context.Background(), run.RunID)
	if got.Status != StatusFailed {
		t.Fatalf("empty-matrix run status = %q, want failed", got.Status)
	}
	// No execution should have been dispatched.
	if len(*paths) != 0 {
		t.Fatalf("empty matrix dispatched %d requests, want 0 (%v)", len(*paths), *paths)
	}
	// The matrix step must be visible in the run view as a failed step run naming
	// the cause, not missing entirely.
	srs, _ := getStepRuns(context.Background(), run.RunID)
	if len(srs) != 1 {
		t.Fatalf("expected 1 step run for the empty matrix, got %d", len(srs))
	}
	if srs[0].Status != StatusFailed {
		t.Errorf("empty-matrix step run status = %q, want failed", srs[0].Status)
	}
	if srs[0].Output == nil || !strings.Contains(*srs[0].Output, "no values") {
		t.Errorf("empty-matrix step run output = %v, want it to name the missing values", srs[0].Output)
	}
}
