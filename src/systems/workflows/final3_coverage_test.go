package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHandleUpdateStep_InvalidBody(t *testing.T) {
	requireDB(t)
	okGate(t, "us2", "uso2")
	step := seedStep(t, "us2", "uso2") // exists: getStep precedes decode
	r := authReq(http.MethodPut, "/steps/"+step.StepID, []byte("{bad"))
	r.SetPathValue("id", step.StepID)
	w := httptest.NewRecorder()
	handleUpdateStep(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleDeleteWorkflow_Success(t *testing.T) {
	requireDB(t)
	okGate(t, "dw", "dwo")
	wf := seedWorkflow(t, "dw", "dwo")
	r := authReq(http.MethodDelete, "/pipelines/"+wf.WorkflowID, nil)
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleDeleteWorkflow(w, r)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("got %d, want 2xx", w.Code)
	}
}

func TestHandleCancelRun_PendingRun(t *testing.T) {
	requireDB(t)
	okGate(t, "cp", "cpo")
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "cp", OrgID: "cpo", Status: "pending", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	r := authReq(http.MethodDelete, "/runs/"+run.RunID, nil)
	r.SetPathValue("id", run.RunID)
	w := httptest.NewRecorder()
	handleCancelRun(newWorkerPool())(w, r)
	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("cancel pending got %d, want 2xx", w.Code)
	}
}

func TestHandleGetWorkflow_Success(t *testing.T) {
	requireDB(t)
	okGate(t, "gw", "gwo")
	wf := seedWorkflow(t, "gw", "gwo")
	r := authReq(http.MethodGet, "/pipelines/"+wf.WorkflowID, nil)
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleGetWorkflow(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}
