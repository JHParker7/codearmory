package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPollAction_FailureState(t *testing.T) {
	pool := &WorkerPool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"execution_id":"j1"}`)) //nolint:errcheck
			return
		}
		w.Write([]byte(`{"status":"failed","stderr":"it broke"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	def := ActionDef{
		Name: "forge/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/executions",
		Async: &AsyncConfig{IDField: "execution_id", PollPath: "/executions/{id}", PollIntervalSecs: 1,
			StatusField: "status", SuccessStates: []string{"completed"}, FailureStates: []string{"failed"},
			OutputField: "stdout", ErrorFields: []string{"stderr"}},
	}
	if _, err := pool.executeAction(context.Background(), newTokenStore("", ""), def, nil); err == nil {
		t.Error("expected error on failure state")
	}
}

func TestExecuteAction_BodyTransforms(t *testing.T) {
	pool := &WorkerPool{}
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	def := ActionDef{
		Name: "forge/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/executions",
		BodyTransforms: []BodyTransform{{FromKey: "run", ToKey: "command", Wrap: []string{"sh", "-c"}}},
	}
	if _, err := pool.executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{"run": "echo hi"}); err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	cmd, ok := got["command"].([]any)
	if !ok || len(cmd) != 3 || cmd[0] != "sh" || cmd[2] != "echo hi" {
		t.Errorf("body transform wrong: %+v", got)
	}
}

func TestTryOne_BadTokenFailsRun(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "u", "o") // revoke target in failRun
	// Enable encryption so an undecryptable token errors (rather than passthrough).
	t.Setenv("WORKFLOWS_TOKEN_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	origKey := tokenEncKey
	initTokenEncryption()
	t.Cleanup(func() { tokenEncKey = origKey })

	connect().Exec(`DELETE FROM workflow_runs WHERE status='pending'`) //nolint:errcheck
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "pending", Token: "not-valid-ciphertext", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck

	if !(&WorkerPool{}).tryOne(context.Background()) {
		t.Fatal("tryOne should claim the run")
	}
	got, _ := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
	if got.(WorkflowRun).Status != StatusFailed {
		t.Errorf("status = %q, want failed (bad token)", got.(WorkflowRun).Status)
	}
}

func TestHandleInternalTriggerRun_CrossOrgAndNotFound(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk")
	wf := seedWorkflow(t, "u1", "org-A")

	// Cross-org: workflow is org-A but the request claims org-B → 403.
	tok, ts := hooksToken("hooks", wf.WorkflowID, "u1")
	body, _ := json.Marshal(map[string]any{"triggered_by": "u1", "org_id": "org-B"})
	r := httptest.NewRequest(http.MethodPost, "/internal/pipelines/"+wf.WorkflowID+"/runs", strings.NewReader(string(body)))
	r.SetPathValue("id", wf.WorkflowID)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalTriggerRun(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org got %d, want 403", w.Code)
	}

	// Not found: signed token for a workflow that doesn't exist → 404.
	id := uuid.New().String()
	tok2, ts2 := hooksToken("hooks", id, "u1")
	body2, _ := json.Marshal(map[string]any{"triggered_by": "u1", "org_id": "x"})
	r2 := httptest.NewRequest(http.MethodPost, "/internal/pipelines/"+id+"/runs", strings.NewReader(string(body2)))
	r2.SetPathValue("id", id)
	r2.Header.Set("X-Hooks-Token", tok2)
	r2.Header.Set("X-Hooks-Timestamp", ts2)
	w2 := httptest.NewRecorder()
	handleInternalTriggerRun(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("not-found got %d, want 404", w2.Code)
	}
}
