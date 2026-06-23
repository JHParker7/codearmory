package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWithInt64_Cases(t *testing.T) {
	if withInt64(map[string]any{"n": int64(7)}, "n") != 7 {
		t.Error("int64 case")
	}
	if withInt64(map[string]any{"n": "notnum"}, "n") != 0 {
		t.Error("non-numeric → 0")
	}
	if withInt64(nil, "x") != 0 {
		t.Error("nil map → 0")
	}
}

func TestComplete_StatusVariants(t *testing.T) {
	requireDB(t)
	for _, status := range []string{StatusFailed, StatusCancelled, StatusCompleted} {
		run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w", TriggeredBy: "u", OrgID: "o", Status: "running", CreatedAt: time.Now().UTC()}
		run.Add(context.Background())                                                                 //nolint:errcheck
		connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)          //nolint:errcheck
		rid := run.RunID
		t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, rid) }) //nolint:errcheck
		run.Status = "running"
		run.Complete(context.Background(), status)
		got, _ := (WorkflowRun{RunID: rid}).Get(context.Background())
		if got.(WorkflowRun).Status != status {
			t.Errorf("Complete(%q) → %q", status, got.(WorkflowRun).Status)
		}
	}
}

func TestHandleListSteps_NameFilter(t *testing.T) {
	requireDB(t)
	okGate(t, "ls", "lso")
	step := seedStep(t, "ls", "lso")
	r := authReq(http.MethodGet, "/steps?name="+step.Name, nil)
	w := httptest.NewRecorder()
	handleListSteps(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var steps []Step
	json.Unmarshal(w.Body.Bytes(), &steps) //nolint:errcheck
	if len(steps) != 1 || steps[0].StepID != step.StepID {
		t.Errorf("name filter returned %d steps", len(steps))
	}
}

func TestValidateStepRefs_CrossOrg(t *testing.T) {
	requireDB(t)
	// Step owned by org-A.
	stepA := seedStep(t, "ua", "org-A")
	// Caller is org-B → referencing org-A's step must be rejected on create.
	stubGatekeeperRouting(t, "ub", "org-B")
	body, _ := json.Marshal(map[string]any{"name": "wf", "steps": []map[string]any{{"step_id": stepA.StepID}}})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("cross-org step ref got %d, want 4xx", w.Code)
	}
}

func TestHandleInternalGetRun_NotFound(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk")
	id := uuid.New().String()
	tok, ts := hooksToken("hooks-poll", id)
	r := httptest.NewRequest(http.MethodGet, "/internal/runs/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalGetRun(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleInternalGetWorkflow_NotFound(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk")
	id := uuid.New().String()
	tok, ts := hooksToken("hooks-check", id)
	r := httptest.NewRequest(http.MethodGet, "/internal/pipelines/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalGetWorkflow(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestVerifyHooks_ExpiredTimestamp(t *testing.T) {
	setHooksKey(t, "hk")
	// A far-past timestamp must be rejected even with a valid signature.
	old := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	if verifyHooksPoll("r", "anytoken", old) {
		t.Error("expired timestamp should be rejected")
	}
	if verifyHooksWorkflowCheck("w", "anytoken", old) {
		t.Error("expired timestamp should be rejected")
	}
}
