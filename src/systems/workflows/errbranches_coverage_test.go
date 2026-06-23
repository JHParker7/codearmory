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

func TestHandleCreateWorkflow_StepNotFound(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "u", "o")
	// References a step that does not exist → validateStepRefs rejects.
	body, _ := json.Marshal(map[string]any{"name": "wf", "steps": []map[string]any{{"step_id": uuid.New().String()}}})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("got %d, want a 4xx for unknown step", w.Code)
	}
}

func TestHandleUpdateWorkflow_NotFound(t *testing.T) {
	requireDB(t)
	okGate(t, "u", "o")
	step := seedStep(t, "u", "o")
	id := uuid.New().String()
	body, _ := json.Marshal(map[string]any{"name": "x", "steps": []map[string]any{{"step_id": step.StepID}}})
	r := authReq(http.MethodPut, "/pipelines/"+id, body)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleUpdateWorkflow(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleUpdateWorkflow_InvalidBody(t *testing.T) {
	requireDB(t)
	okGate(t, "ub", "ubo")
	wf := seedWorkflow(t, "ub", "ubo") // must exist: getWorkflow precedes body decode
	r := authReq(http.MethodPut, "/pipelines/"+wf.WorkflowID, []byte("{bad"))
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleUpdateWorkflow(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleDeleteStep_NotFound(t *testing.T) {
	requireDB(t)
	okGate(t, "u", "o")
	id := uuid.New().String()
	r := authReq(http.MethodDelete, "/steps/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteStep(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleCancelRun_NotFound(t *testing.T) {
	requireDB(t)
	okGate(t, "u", "o")
	id := uuid.New().String()
	r := authReq(http.MethodDelete, "/runs/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleCancelRun(newWorkerPool())(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleGetRun_CrossTenant(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "owner", OrgID: "owner-org", Status: "running", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	okGate(t, "intruder", "other-org")
	r := authReq(http.MethodGet, "/runs/"+run.RunID, nil)
	r.SetPathValue("id", run.RunID)
	w := httptest.NewRecorder()
	handleGetRun(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get run got %d, want 404", w.Code)
	}
}

func TestHandleGetRun_Success(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "owner", OrgID: "owner-org", Status: "running", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	okGate(t, "owner", "owner-org")
	r := authReq(http.MethodGet, "/runs/"+run.RunID, nil)
	r.SetPathValue("id", run.RunID)
	w := httptest.NewRecorder()
	handleGetRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get run got %d, want 200", w.Code)
	}
}

func TestHandleDeleteWorkflow_CrossTenant(t *testing.T) {
	requireDB(t)
	wf := seedWorkflow(t, "owner", "owner-org")
	okGate(t, "intruder", "other-org")
	r := authReq(http.MethodDelete, "/pipelines/"+wf.WorkflowID, nil)
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleDeleteWorkflow(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete got %d, want 404", w.Code)
	}
}
