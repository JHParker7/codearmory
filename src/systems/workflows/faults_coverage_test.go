package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// withBrokenDB swaps the gorm singletons for a closed connection so DB ops error,
// exercising the handlers' 500 branches (auth is stubbed, so control reaches them).
func withBrokenDB(t *testing.T) {
	t.Helper()
	broken, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open broken db: %v", err)
	}
	if sqldb, err := broken.DB(); err == nil {
		sqldb.Close()
	}
	dbInitMu.Lock()
	ow, or := gormDB, gormDBRead
	gormDB, gormDBRead = broken, broken
	dbInitMu.Unlock()
	t.Cleanup(func() {
		dbInitMu.Lock()
		gormDB, gormDBRead = ow, or
		dbInitMu.Unlock()
	})
}

func TestHandlers_DBError(t *testing.T) {
	okGate(t, "u", "o")
	withBrokenDB(t)
	cases := []struct {
		name string
		h    http.HandlerFunc
		m    string
		body []byte
	}{
		{"createStep", handleCreateStep, http.MethodPost, []byte(`{"name":"n","action":"echo"}`)},
		{"listSteps", handleListSteps, http.MethodGet, nil},
		{"listWorkflows", handleListWorkflows, http.MethodGet, nil},
		{"listRuns", handleListRuns, http.MethodGet, nil},
	}
	for _, c := range cases {
		r := authReq(c.m, "/x", c.body)
		w := httptest.NewRecorder()
		c.h(w, r)
		if w.Code < 500 {
			t.Errorf("%s under broken DB got %d, want 5xx", c.name, w.Code)
		}
	}
}

func TestHandleTriggerRun_TokenFailure(t *testing.T) {
	requireDB(t)
	// Authorize + a workflow WITH a step (empty pipelines are rejected 400 before
	// token minting), but no gatekeeper key → createRunToken fails → 500.
	stubGatekeeperRouting(t, "u", "o")
	step := seedStep(t, "u", "o")
	wf := createWorkflowWith(t, []map[string]any{{"step_id": step.StepID}})
	gatekeeperKey = func() string { return "" } // overrides the stub key after creation

	r := authReq(http.MethodPost, "/pipelines/"+wf.WorkflowID+"/runs", []byte(`{"inputs":{}}`))
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleTriggerRun(w, r)
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx when run-token minting fails", w.Code)
	}
}

func TestHandleInternalTriggerRun_TokenFailure(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk")
	wf := seedWorkflow(t, "u", "org-tf")
	origKey := gatekeeperKey
	gatekeeperKey = func() string { return "" } // createRunToken fails
	t.Cleanup(func() { gatekeeperKey = origKey })

	tok, ts := hooksToken("hooks", wf.WorkflowID, "u")
	r := httptest.NewRequest(http.MethodPost, "/internal/pipelines/"+wf.WorkflowID+"/runs", bytes.NewReader([]byte(`{"triggered_by":"u","org_id":"org-tf"}`)))
	r.SetPathValue("id", wf.WorkflowID)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalTriggerRun(w, r)
	if w.Code < 500 {
		t.Fatalf("got %d, want 5xx when run-token minting fails", w.Code)
	}
}
