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

func TestFailRun(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "u", "o") // revokeRunToken target
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "running", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)          //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) })  //nolint:errcheck

	newWorkerPool().failRun(run.RunID, "sess")
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.(WorkflowRun).Status)
	}
}

func TestTryOne_ExecutesPendingRun(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "svc", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	step := seedStep(t, "tu", "to")
	// Reuse the step as an HTTP action so executeRun makes a real (stubbed) call.
	connect().Exec(`UPDATE steps SET action='http', config=? WHERE step_id=?`,
		`{"service":"svc","path":"/","method":"GET"}`, step.StepID) //nolint:errcheck

	body, _ := json.Marshal(map[string]any{"name": "wf-" + uuid.New().String(), "steps": []map[string]any{{"step_id": step.StepID}}})
	cw := httptest.NewRecorder()
	handleCreateWorkflow(cw, authReq(http.MethodPost, "/pipelines", body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d: %s", cw.Code, cw.Body.String())
	}
	var wf Workflow
	json.Unmarshal(cw.Body.Bytes(), &wf) //nolint:errcheck
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID)     //nolint:errcheck
		connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID) //nolint:errcheck
	})

	// Ensure tryOne claims OUR run (clear any stray pending rows first).
	connect().Exec(`DELETE FROM workflow_runs WHERE status='pending'`) //nolint:errcheck
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: wf.WorkflowID, TriggeredBy: "tu", OrgID: "to", Status: "pending", CreatedAt: time.Now().UTC()}
	run.Add(context.Background()) //nolint:errcheck

	if !newWorkerPool().tryOne(context.Background()) {
		t.Fatal("tryOne returned false, expected it to claim the pending run")
	}
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if st := got.(WorkflowRun).Status; st != StatusCompleted && st != StatusFailed {
		t.Errorf("run status = %q, want a terminal state after tryOne", st)
	}
}

func TestHandleCancelRun_Success(t *testing.T) {
	requireDB(t)
	okGate(t, "cu", "co")
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "cu", OrgID: "co", Status: "running", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)          //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) })  //nolint:errcheck

	r := authReq(http.MethodDelete, "/runs/"+run.RunID, nil)
	r.SetPathValue("id", run.RunID)
	w := httptest.NewRecorder()
	handleCancelRun(newWorkerPool())(w, r)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent && w.Code != http.StatusAccepted {
		t.Fatalf("cancel got %d, want 2xx: %s", w.Code, w.Body.String())
	}
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", got.(WorkflowRun).Status)
	}
}
