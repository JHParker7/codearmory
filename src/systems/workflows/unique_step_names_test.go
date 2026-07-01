package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Two blocks backed by the SAME step definition with no per-occurrence name share
// one effective name, which used to silently collide in the run output map. Create
// must now reject it with a clear 400.
func TestCreateWorkflow_DuplicateStepNameRejected(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	s := seedHTTPStep(t, "tu", "to", "svc", "/x") // one definition, reused twice
	body, _ := json.Marshal(map[string]any{
		"name": "wf-dup-" + uuid.New().String(),
		"steps": []map[string]any{
			{"step_id": s.StepID},
			{"step_id": s.StepID},
		},
	})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unique name") {
		t.Errorf("error message missing the unique-name hint: %s", w.Body.String())
	}
}

// A per-occurrence name on each block resolves the collision -> accepted.
func TestCreateWorkflow_DuplicateResolvedByUniqueName(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	s := seedHTTPStep(t, "tu", "to", "svc", "/x")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": s.StepID, "name": "first"},
		{"step_id": s.StepID, "name": "second"},
	})
	if wf.WorkflowID == "" {
		t.Fatal("expected workflow to be created")
	}
}
