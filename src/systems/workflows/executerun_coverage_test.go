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

// seedHTTPStep inserts an active step that calls the named service via the HTTP action.
func seedHTTPStep(t *testing.T, user, org, service, path string) Step {
	t.Helper()
	s := seedStep(t, user, org)
	connect().Exec(`UPDATE steps SET action='http', config=? WHERE step_id=?`,
		`{"service":"`+service+`","path":"`+path+`","method":"GET"}`, s.StepID) //nolint:errcheck
	return s
}

func createWorkflowWith(t *testing.T, steps []map[string]any) Workflow {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "wf-" + uuid.New().String(), "steps": steps})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d: %s", w.Code, w.Body.String())
	}
	var wf Workflow
	json.Unmarshal(w.Body.Bytes(), &wf) //nolint:errcheck
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID)     //nolint:errcheck
		connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID) //nolint:errcheck
	})
	return wf
}

func runOnce(t *testing.T, wf Workflow) WorkflowRun {
	t.Helper()
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: wf.WorkflowID, TriggeredBy: "tu", OrgID: "to", Status: "pending", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                          //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)  //nolint:errcheck
	newWorkerPool().executeRun(context.Background(), run.RunID, wf.WorkflowID, "", "", "tu", map[string]string{}, 0)
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	return got.(WorkflowRun)
}

func TestExecuteRun_StepFailureFailsRun(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "failsvc", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	step := seedHTTPStep(t, "tu", "to", "failsvc", "/x")
	wf := createWorkflowWith(t, []map[string]any{{"step_id": step.StepID}})
	if got := runOnce(t, wf); got.Status != StatusFailed {
		t.Fatalf("run status = %q, want failed", got.Status)
	}
}

func TestExecuteRun_ParallelGroupCompletes(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "psvc", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s1 := seedHTTPStep(t, "tu", "to", "psvc", "/a")
	s2 := seedHTTPStep(t, "tu", "to", "psvc", "/b")
	grp := 0
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": s1.StepID, "parallel_group": grp},
		{"step_id": s2.StepID, "parallel_group": grp},
	})
	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("parallel run status = %q, want completed", got.Status)
	}
	srs, _ := (WorkflowStepRun{RunID: got.RunID}).List(context.Background(), 0, 0)
	if len(srs) < 2 {
		t.Errorf("expected 2 step runs for the parallel group, got %d", len(srs))
	}
}

// ── handler not-found / error branches ─────────────────────────────────────────

func TestHandlers_NotFound(t *testing.T) {
	requireDB(t)
	okGate(t, "nf", "nfo")
	id := uuid.New().String()
	cases := []struct {
		name string
		h    http.HandlerFunc
		m    string
	}{
		{"getStep", handleGetStep, http.MethodGet},
		{"updateStep", handleUpdateStep, http.MethodPut},
		{"getWorkflow", handleGetWorkflow, http.MethodGet},
		{"deleteWorkflow", handleDeleteWorkflow, http.MethodDelete},
		{"getRun", handleGetRun, http.MethodGet},
	}
	for _, c := range cases {
		var body []byte
		if c.m == http.MethodPut {
			body = []byte(`{"name":"x","action":"echo"}`)
		}
		r := authReq(c.m, "/x/"+id, body)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		c.h(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", c.name, w.Code)
		}
	}
}

func TestHandleTriggerRun_WorkflowNotFound(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	id := uuid.New().String()
	r := authReq(http.MethodPost, "/pipelines/"+id+"/runs", []byte(`{"inputs":{}}`))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleTriggerRun(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestHandleListActions_OK(t *testing.T) {
	okGate(t, "la", "lao")
	w := httptest.NewRecorder()
	handleListActions(w, authReq(http.MethodGet, "/actions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}
