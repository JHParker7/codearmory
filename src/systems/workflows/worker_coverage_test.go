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

// TestExecuteRun_HTTPStepCompletes drives the worker end-to-end: a workflow with a
// single HTTP step against a stub service, executed via executeRun, must finish
// 'completed' and record a step run. Exercises executeRun/executeStep/startStepRun/
// finishStepRun/snapshotOutputs/Complete/SetCurrentStep/rotateToken.
func TestExecuteRun_HTTPStepCompletes(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "wk", "wko") // token minting for rotateToken
	fakeService(t, "mysvc", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	})

	// Step that calls the stub service.
	now := time.Now().UTC()
	step := Step{
		StepID: uuid.New().String(), Name: "http-" + uuid.New().String(), Action: ActionHTTP,
		With:    map[string]any{"service": "mysvc", "path": "/run", "method": "GET"},
		Timeout: 30, CreatedBy: "wk", OrgID: "wko", Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := step.Add(context.Background()); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM steps WHERE step_id = ?`, step.StepID) }) //nolint:errcheck

	// Workflow referencing the step (created through the handler so Steps are enriched/persisted).
	body := []byte(`{"name":"wf-` + uuid.New().String() + `","steps":[{"step_id":"` + step.StepID + `"}]}`)
	cw := httptest.NewRecorder()
	handleCreateWorkflow(cw, authReq(http.MethodPost, "/pipelines", body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d: %s", cw.Code, cw.Body.String())
	}
	var wf Workflow
	if err := json.Unmarshal(cw.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode wf: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID)                                                             //nolint:errcheck
		connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID)                                                         //nolint:errcheck
		connect().Exec(`DELETE FROM workflow_step_runs WHERE run_id IN (SELECT run_id FROM workflow_runs WHERE workflow_id = ?)`, wf.WorkflowID) //nolint:errcheck
	})

	// Pending run for the worker to execute.
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: wf.WorkflowID, TriggeredBy: "wk", OrgID: "wko", Status: "pending", CreatedAt: time.Now().UTC()}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// The worker is normally handed a run already flipped to 'running' by Dequeue;
	// executeRun's Complete only finalises a 'running' run, so mirror that here.
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id = ?`, run.RunID) //nolint:errcheck

	pool := newWorkerPool()
	pool.executeRun(context.Background(), run.RunID, wf.WorkflowID, "tok", "sess", "wk", map[string]string{}, 0, "")

	got, err := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if st := got.(WorkflowRun).Status; st != StatusCompleted {
		t.Fatalf("run status = %q, want completed", st)
	}
	// A step run should have been recorded for the run.
	srs, err := (WorkflowStepRun{RunID: run.RunID}).List(context.Background(), 0, 0)
	if err != nil || len(srs) == 0 {
		t.Fatalf("expected step runs recorded: err=%v len=%d", err, len(srs))
	}
}

func TestRecoverStuckRunsDB(t *testing.T) {
	requireDB(t)
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "pending", CreatedAt: time.Now().UTC()}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	// Force it to 'running' so recovery flips it to failed.
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id = ?`, run.RunID) //nolint:errcheck

	n := recoverStuckRunsDB()
	if n < 1 {
		t.Fatalf("recoverStuckRunsDB returned %d, want >= 1", n)
	}
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).Status != "failed" {
		t.Errorf("stuck run status = %q, want failed", got.(WorkflowRun).Status)
	}
}

// ── middleware / helpers ───────────────────────────────────────────────────────

func TestHandleOpenAPIYAML_Workflows(t *testing.T) {
	w := httptest.NewRecorder()
	handleOpenAPIYAML(w, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("openapi code=%d len=%d", w.Code, w.Body.Len())
	}
}

func TestSecretOrDefault_Workflows(t *testing.T) {
	t.Setenv("WF_TEST", "v")
	if secretOrDefault("WF_TEST", "d") != "v" {
		t.Error("env value not returned")
	}
	if secretOrDefault("WF_MISSING", "d") != "d" {
		t.Error("default not returned")
	}
}

func TestBearerToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if bearerToken(r) != "abc123" {
		t.Errorf("bearerToken = %q", bearerToken(r))
	}
	if bearerToken(httptest.NewRequest(http.MethodGet, "/x", nil)) != "" {
		t.Error("missing header should yield empty token")
	}
}

func TestStrPtr(t *testing.T) {
	if p := strPtr("hi"); p == nil || *p != "hi" {
		t.Error("strPtr broken")
	}
}

func TestRequestLogger_And_LimitBody(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	w := httptest.NewRecorder()
	(&requestLogger{handler: inner}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusCreated {
		t.Errorf("requestLogger status = %d", w.Code)
	}

	served := false
	lb := limitBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	lb.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	if !served {
		t.Error("limitBody did not call next")
	}
}
