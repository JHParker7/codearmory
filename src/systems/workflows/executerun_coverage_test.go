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

// createWorkflowWithRoutes is createWorkflowWith plus an explicit edge list — the
// only way to author a fork, since a bare array derives to a chain.
func createWorkflowWithRoutes(t *testing.T, steps []map[string]any, routes []map[string]any) Workflow {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "wf-" + uuid.New().String(), "steps": steps, "routes": routes})
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
	run.Add(context.Background())                                                         //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID) //nolint:errcheck
	newWorkerPool().executeRun(context.Background(), run.RunID, wf.WorkflowID, "", "", "tu", map[string]string{}, 0, "")
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

// Two steps routed off a common predecessor run concurrently and both complete.
// This is the whole of parallelism now: a fork is two edges out of one node.
func TestExecuteRun_ForkedRoutesBothComplete(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "psvc", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s0 := seedHTTPStep(t, "tu", "to", "psvc", "/fan")
	s1 := seedHTTPStep(t, "tu", "to", "psvc", "/a")
	s2 := seedHTTPStep(t, "tu", "to", "psvc", "/b")
	wf := createWorkflowWithRoutes(t,
		[]map[string]any{
			{"step_id": s0.StepID, "name": "fan"},
			{"step_id": s1.StepID, "name": "a"},
			{"step_id": s2.StepID, "name": "b"},
		},
		[]map[string]any{
			{"from": "fan", "to": "a"},
			{"from": "fan", "to": "b"},
		})
	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("forked run status = %q, want completed", got.Status)
	}
	srs, _ := getStepRuns(context.Background(), got.RunID)
	if len(srs) < 3 {
		t.Errorf("expected 3 step runs across the fork, got %d", len(srs))
	}
	// Both branches must actually have run — a fork that silently dropped one
	// would still report completed.
	seen := map[string]bool{}
	for _, sr := range srs {
		seen[sr.StepName] = true
	}
	for _, n := range []string{"fan", "a", "b"} {
		if !seen[n] {
			t.Errorf("step %q never ran; step runs = %v", n, seen)
		}
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
