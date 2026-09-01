package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWorkflow_List(t *testing.T) {
	requireDB(t)
	wf := seedWorkflow(t, "lu", "lo")
	list, err := (Workflow{CreatedBy: "lu", OrgID: "lo"}).List(context.Background(), 100, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, d := range list {
		if d.(Workflow).WorkflowID == wf.WorkflowID {
			found = true
		}
	}
	if !found {
		t.Error("seeded workflow not in List result")
	}
}

func TestWorkflowRun_UpdateRemove(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "pending", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	run.Status = "running"
	if err := run.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := run.Remove(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestRecoverStuckRuns(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "pending", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)         //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	recoverStuckRuns()                                                                            // wrapper around recoverStuckRunsDB; must not panic and should fail stuck runs
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).Status != "failed" {
		t.Errorf("stuck run status = %q, want failed", got.(WorkflowRun).Status)
	}
}

func TestHandleDeleteStep_Success(t *testing.T) {
	requireDB(t)
	okGate(t, "ds", "dso")
	step := seedStep(t, "ds", "dso")
	r := authReq(http.MethodDelete, "/steps/"+step.StepID, nil)
	r.SetPathValue("id", step.StepID)
	w := httptest.NewRecorder()
	handleDeleteStep(w, r)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("got %d, want 2xx", w.Code)
	}
}

func TestHandleUpdateStep_Success(t *testing.T) {
	requireDB(t)
	okGate(t, "us", "uso")
	step := seedStep(t, "us", "uso")
	body, _ := json.Marshal(map[string]any{"name": step.Name, "action": "echo", "description": "updated"})
	r := authReq(http.MethodPut, "/steps/"+step.StepID, body)
	r.SetPathValue("id", step.StepID)
	w := httptest.NewRecorder()
	handleUpdateStep(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestWithInt64(t *testing.T) {
	// float64 (JSON number) and int both coerce; missing → 0.
	if got := withInt64(map[string]any{"n": float64(42)}, "n"); got != 42 {
		t.Errorf("float64 → %d, want 42", got)
	}
	if got := withInt64(map[string]any{}, "missing"); got != 0 {
		t.Errorf("missing → %d, want 0", got)
	}
}
