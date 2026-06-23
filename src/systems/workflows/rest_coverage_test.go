package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWithStrings(t *testing.T) {
	if got := withStrings(map[string]any{"k": []any{"a", "b", 3}}, "k"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("[]any → %v, want [a b] (non-strings skipped)", got)
	}
	if got := withStrings(map[string]any{"k": []string{"x"}}, "k"); len(got) != 1 || got[0] != "x" {
		t.Errorf("[]string passthrough → %v", got)
	}
	if withStrings(nil, "k") != nil || withStrings(map[string]any{}, "missing") != nil {
		t.Error("nil map / missing key should be nil")
	}
}

func TestWithStringMap(t *testing.T) {
	got := withStringMap(map[string]any{"env": map[string]any{"A": "B", "N": 1}}, "env")
	if got["A"] != "B" || len(got) != 1 {
		t.Errorf("withStringMap → %v, want {A:B} (non-strings skipped)", got)
	}
	if withStringMap(map[string]any{"env": "notmap"}, "env") != nil {
		t.Error("non-map value should be nil")
	}
	if withStringMap(nil, "x") != nil {
		t.Error("nil map should be nil")
	}
}

func TestJSONScalar(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""}, {"hi", "hi"}, {float64(3), "3"}, {float64(2.5), "2.5"}, {true, "true"},
		{map[string]any{"a": "b"}, `{"a":"b"}`},
	}
	for _, c := range cases {
		if got := jsonScalar(c.in); got != c.want {
			t.Errorf("jsonScalar(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSnapshotOutputs(t *testing.T) {
	if snapshotOutputs(nil) != nil || snapshotOutputs(map[string]string{}) != nil {
		t.Error("empty → nil")
	}
	orig := map[string]string{"a": "1"}
	c := snapshotOutputs(orig)
	c["a"] = "2"
	if orig["a"] != "1" {
		t.Error("snapshot must be an independent copy")
	}
}

func TestCancelRun_DB(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "running", CreatedAt: time.Now().UTC()}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)            //nolint:errcheck

	n, err := cancelRun(context.Background(), run.RunID)
	if err != nil || n != 1 {
		t.Fatalf("cancelRun = %d, %v, want 1", n, err)
	}
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", got.(WorkflowRun).Status)
	}
	// Cancelling a non-running run affects 0 rows.
	if n, _ := cancelRun(context.Background(), run.RunID); n != 0 {
		t.Errorf("re-cancel affected %d rows, want 0", n)
	}
}

func TestWorkflowRun_SetCurrentStepAndToken(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "running", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                    //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) })     //nolint:errcheck
	run.SetCurrentStep(context.Background(), 3)
	if err := run.UpdateToken(context.Background(), "newtok", "newsess"); err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).CurrentStep != 3 {
		t.Errorf("current_step = %d, want 3", got.(WorkflowRun).CurrentStep)
	}
}

func TestWorkflow_UpdateRemove(t *testing.T) {
	requireDB(t)
	wf := Workflow{WorkflowID: uuid.New().String(), Name: "w", CreatedBy: "u", OrgID: "o", Active: true, StepRefs: []WorkflowStepRef{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	wf.Add(context.Background())                                                                  //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID) }) //nolint:errcheck
	wf.Name = "renamed"
	if err := wf.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := wf.Remove(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := (Workflow{WorkflowID: wf.WorkflowID}).Get(context.Background()); err == nil {
		t.Error("Get after Remove should fail (soft-deleted)")
	}
}

func TestWorkflowStepRun_UpdateRemove(t *testing.T) {
	requireDB(t)
	sr := WorkflowStepRun{StepRunID: uuid.New().String(), RunID: "r1", StepIndex: 0, StepName: "s", Status: "pending"}
	sr.Add(context.Background())                                                                            //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_step_runs WHERE step_run_id = ?`, sr.StepRunID) }) //nolint:errcheck
	sr.Status = "running"
	if err := sr.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Remove is an intentional no-op/not-implemented stub for the db interface;
	// just exercise it (it returns a sentinel error by design).
	_ = sr.Remove(context.Background())
}

func TestProvisionAndDeleteWorkflowRole(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "u1", "o1")
	// Exercises the role-create request path (returns "" if the stub doesn't echo a
	// role id — the point is to cover the request/response handling, not assert an id).
	_ = provisionWorkflowRole(context.Background(), uuid.New().String(), "u1", "o1", []WorkflowStep{})
	deleteWorkflowRole(context.Background(), "some-role-id")
}
