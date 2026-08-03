package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

func setHooksKey(t *testing.T, key string) {
	t.Helper()
	orig := eventsTriggerKey
	eventsTriggerKey = key
	t.Cleanup(func() { eventsTriggerKey = orig })
}

// hooksToken signs an HMAC token matching the production verifyHooks* helpers:
// "<prefix>:<part>...:<unix-ts>". Returns the token and the timestamp it covers.
func hooksToken(prefix string, parts ...string) (token, ts string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	msg := prefix
	for _, p := range parts {
		msg += ":" + p
	}
	msg += ":" + ts
	mac := hmac.New(sha256.New, []byte(eventsTriggerKey))
	mac.Write([]byte(msg)) //nolint:errcheck
	return hex.EncodeToString(mac.Sum(nil)), ts
}

func seedWorkflow(t *testing.T, user, org string) Workflow {
	t.Helper()
	now := time.Now().UTC()
	wf := Workflow{
		WorkflowID: uuid.New().String(), Name: "wf-" + uuid.New().String(),
		CreatedBy: user, OrgID: org, Active: true, StepRefs: []WorkflowStepRef{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := wf.Add(context.Background()); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID) }) //nolint:errcheck
	return wf
}

func TestHandleInternalGetWorkflow_Success(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk-key")
	wf := seedWorkflow(t, "u1", "org-internal")
	tok, ts := hooksToken("hooks-check", wf.WorkflowID)

	r := httptest.NewRequest(http.MethodGet, "/internal/pipelines/"+wf.WorkflowID, nil)
	r.SetPathValue("id", wf.WorkflowID)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalGetWorkflow(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["org_id"] != "org-internal" {
		t.Errorf("org_id = %q", resp["org_id"])
	}
}

func TestHandleInternalGetWorkflow_Unauthorized(t *testing.T) {
	setHooksKey(t, "hk-key")
	id := uuid.New().String()
	r := httptest.NewRequest(http.MethodGet, "/internal/pipelines/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set("X-Hooks-Token", "bad")
	r.Header.Set("X-Hooks-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	w := httptest.NewRecorder()
	handleInternalGetWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalGetRun_Success(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk-key")
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u1", OrgID: "o1", Status: "running", CreatedAt: time.Now().UTC()}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck

	tok, ts := hooksToken("hooks-poll", run.RunID)
	r := httptest.NewRequest(http.MethodGet, "/internal/runs/"+run.RunID, nil)
	r.SetPathValue("id", run.RunID)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalGetRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestHandleInternalTriggerRun_Success(t *testing.T) {
	requireDB(t)
	setHooksKey(t, "hk-key")
	stubGatekeeperRouting(t, "u1", "org-trig") // mints the run token
	wf := seedWorkflow(t, "u1", "org-trig")
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID) }) //nolint:errcheck

	tok, ts := hooksToken("hooks", wf.WorkflowID, "u1")
	body, _ := json.Marshal(map[string]any{"triggered_by": "u1", "org_id": "org-trig", "inputs": map[string]string{"k": "v"}})
	r := httptest.NewRequest(http.MethodPost, "/internal/pipelines/"+wf.WorkflowID+"/runs", bytes.NewReader(body))
	r.SetPathValue("id", wf.WorkflowID)
	r.Header.Set("X-Hooks-Token", tok)
	r.Header.Set("X-Hooks-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalTriggerRun(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateWorkflow_Success(t *testing.T) {
	requireDB(t)
	okGate(t, "uw", "uwo")
	step := seedStep(t, "uw", "uwo")
	wf := Workflow{WorkflowID: uuid.New().String(), Name: "u", CreatedBy: "uw", OrgID: "uwo", Active: true, StepRefs: []WorkflowStepRef{{StepID: step.StepID}}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := wf.Add(context.Background()); err != nil {
		t.Fatalf("seed wf: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID) }) //nolint:errcheck

	body, _ := json.Marshal(map[string]any{"name": "renamed", "steps": []map[string]any{{"step_id": step.StepID}}})
	r := httptest.NewRequest(http.MethodPut, "/pipelines/"+wf.WorkflowID, bytes.NewReader(body))
	r.SetPathValue("id", wf.WorkflowID)
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleUpdateWorkflow(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
}
