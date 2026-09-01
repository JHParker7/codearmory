package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stubGatekeeperRouting points both the SDK client and the package gatekeeperURL
// at one server that authorizes /check_permissions and mints a run token on
// /internal/run-tokens, and installs a gatekeeperKey so token minting runs.
func stubGatekeeperRouting(t *testing.T, user, org string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "run-tokens") {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"token":"run-token","session_id":"sess-1"}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"authorized":true,"user_id":"` + user + `","org_id":"` + org + `"}`)) //nolint:errcheck
	}))
	origURL, origGKURL, origKey := gatekeeperClient.URL, gatekeeperURL, gatekeeperKey
	gatekeeperClient.URL = srv.URL
	gatekeeperURL = srv.URL
	gatekeeperKey = func() string { return "k" }
	t.Cleanup(func() {
		gatekeeperClient.URL = origURL
		gatekeeperURL = origGKURL
		gatekeeperKey = origKey
		srv.Close()
	})
}

// okGate authorizes every request as user/org via the fake gatekeeper.
func okGate(t *testing.T, user, org string) {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"`+user+`","org_id":"`+org+`"}`)
}

func authReq(method, target string, body []byte) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer test-jwt")
	return r
}

// seedStep inserts an active step owned by user/org and registers cleanup.
func seedStep(t *testing.T, user, org string) Step {
	t.Helper()
	now := time.Now().UTC()
	s := Step{
		StepID: uuid.New().String(), Name: "s-" + uuid.New().String(), Action: "echo",
		With: map[string]any{"k": "v"}, Timeout: 30, CreatedBy: user, OrgID: org,
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Add(context.Background()); err != nil {
		t.Fatalf("seedStep: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM steps WHERE step_id = ?`, s.StepID) }) //nolint:errcheck
	return s
}

// ── Token encryption ───────────────────────────────────────────────────────────

func TestTokenEncryption_RoundTrip(t *testing.T) {
	// 32-byte hex key enables AES-256-GCM.
	t.Setenv("WORKFLOWS_TOKEN_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	orig := tokenEncKey
	initTokenEncryption()
	t.Cleanup(func() { tokenEncKey = orig })

	ct, err := encryptToken("a-secret-jwt")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == "a-secret-jwt" {
		t.Fatal("ciphertext equals plaintext — not encrypted")
	}
	pt, err := decryptToken(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt != "a-secret-jwt" {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestTokenEncryption_NoKeyPassthrough(t *testing.T) {
	orig := tokenEncKey
	tokenEncKey = nil
	t.Cleanup(func() { tokenEncKey = orig })
	ct, _ := encryptToken("plain")
	if ct != "plain" {
		t.Errorf("no-key encrypt should passthrough, got %q", ct)
	}
}

// ── Direct db CRUD ─────────────────────────────────────────────────────────────

func TestStep_CRUD(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	s := seedStep(t, "u1", "o1")

	got, err := (Step{StepID: s.StepID}).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.(Step).Name != s.Name {
		t.Errorf("Get name mismatch")
	}

	s.Description = "updated"
	if err := s.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}

	list, err := (Step{CreatedBy: "u1", OrgID: "o1"}).List(ctx, 100, 0)
	if err != nil || len(list) == 0 {
		t.Fatalf("List: %v len=%d", err, len(list))
	}

	if err := s.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := (Step{StepID: s.StepID}).Get(ctx); err == nil {
		t.Error("Get after Remove should fail (soft-deleted)")
	}
}

func TestWorkflowRun_CRUD_Dequeue_Complete(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	run := WorkflowRun{
		RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u1", OrgID: "o1",
		Status: "pending", Inputs: map[string]string{"a": "b"}, CreatedAt: time.Now().UTC(),
	}
	if err := run.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck

	got, err := (WorkflowRun{RunID: run.RunID}).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.(WorkflowRun).Status != "pending" {
		t.Errorf("status = %q", got.(WorkflowRun).Status)
	}

	if _, err := (WorkflowRun{}).List(ctx, 50, 0); err != nil {
		t.Fatalf("List: %v", err)
	}

	// Dequeue claims the pending run and flips it to running.
	claimed, err := (WorkflowRun{}).Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if claimed == nil || claimed.RunID != run.RunID {
		t.Fatalf("Dequeue did not return the pending run: %+v", claimed)
	}
	after, _ := (WorkflowRun{RunID: run.RunID}).Get(ctx)
	if after.(WorkflowRun).Status != "running" {
		t.Errorf("after dequeue status = %q, want running", after.(WorkflowRun).Status)
	}

	// Complete finalises it.
	run.Status = "running"
	run.Complete(ctx, "completed", nil)
	done, _ := (WorkflowRun{RunID: run.RunID}).Get(ctx)
	if done.(WorkflowRun).Status != "completed" {
		t.Errorf("after complete status = %q", done.(WorkflowRun).Status)
	}
}

func TestWorkflowStepRun_CRUD(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	sr := WorkflowStepRun{
		StepRunID: uuid.New().String(), RunID: "r1", StepIndex: 0, StepName: "build", Status: "pending",
	}
	if err := sr.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_step_runs WHERE step_run_id = ?`, sr.StepRunID) }) //nolint:errcheck

	if _, err := (WorkflowStepRun{StepRunID: sr.StepRunID}).Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := (WorkflowStepRun{RunID: "r1"}).List(ctx, 0, 0); err != nil {
		t.Fatalf("List: %v", err)
	}
	out := "done"
	logs := "hello from stdout\n"
	var used, lim int64 = 12, 256
	sr.Complete(ctx, "completed", &out, &logs, &used, &lim)
	got, _ := (WorkflowStepRun{StepRunID: sr.StepRunID}).Get(ctx)
	if got.(WorkflowStepRun).Status != "completed" {
		t.Errorf("step run status = %q", got.(WorkflowStepRun).Status)
	}
	if l := got.(WorkflowStepRun).Logs; l == nil || *l != logs {
		t.Errorf("step run logs = %v, want %q", l, logs)
	}
}

// ── Step handlers (success lifecycle) ──────────────────────────────────────────

func TestStepHandlers_Lifecycle(t *testing.T) {
	requireDB(t)
	okGate(t, "su", "so")

	// Create
	body, _ := json.Marshal(map[string]any{"name": "step-" + uuid.New().String(), "action": "echo", "with": map[string]any{"x": "y"}})
	w := httptest.NewRecorder()
	handleCreateStep(w, authReq(http.MethodPost, "/steps", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create got %d, want 201: %s", w.Code, w.Body.String())
	}
	var created Step
	json.Unmarshal(w.Body.Bytes(), &created)                                                    //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM steps WHERE step_id = ?`, created.StepID) }) //nolint:errcheck

	// Duplicate name → 409
	w2 := httptest.NewRecorder()
	dup, _ := json.Marshal(map[string]any{"name": created.Name, "action": "echo"})
	handleCreateStep(w2, authReq(http.MethodPost, "/steps", dup))
	if w2.Code != http.StatusConflict {
		t.Errorf("duplicate got %d, want 409", w2.Code)
	}

	// List
	lw := httptest.NewRecorder()
	handleListSteps(lw, authReq(http.MethodGet, "/steps", nil))
	if lw.Code != http.StatusOK {
		t.Fatalf("list got %d", lw.Code)
	}

	// Get
	gr := authReq(http.MethodGet, "/steps/"+created.StepID, nil)
	gr.SetPathValue("id", created.StepID)
	gw := httptest.NewRecorder()
	handleGetStep(gw, gr)
	if gw.Code != http.StatusOK {
		t.Fatalf("get got %d", gw.Code)
	}

	// Update
	ub, _ := json.Marshal(map[string]any{"name": created.Name, "action": "echo", "description": "upd"})
	ur := authReq(http.MethodPut, "/steps/"+created.StepID, ub)
	ur.SetPathValue("id", created.StepID)
	uw := httptest.NewRecorder()
	handleUpdateStep(uw, ur)
	if uw.Code != http.StatusOK {
		t.Fatalf("update got %d: %s", uw.Code, uw.Body.String())
	}

	// Delete
	dr := authReq(http.MethodDelete, "/steps/"+created.StepID, nil)
	dr.SetPathValue("id", created.StepID)
	dw := httptest.NewRecorder()
	handleDeleteStep(dw, dr)
	if dw.Code != http.StatusNoContent && dw.Code != http.StatusOK {
		t.Fatalf("delete got %d", dw.Code)
	}
}

func TestRunHandlers_TriggerListGet(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "ru", "ro")
	step := seedStep(t, "ru", "ro")

	// Create a workflow to trigger.
	body, _ := json.Marshal(map[string]any{"name": "wf-" + uuid.New().String(), "steps": []map[string]any{{"step_id": step.StepID}}})
	cw := httptest.NewRecorder()
	handleCreateWorkflow(cw, authReq(http.MethodPost, "/pipelines", body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d: %s", cw.Code, cw.Body.String())
	}
	var wf Workflow
	json.Unmarshal(cw.Body.Bytes(), &wf) //nolint:errcheck
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID) //nolint:errcheck
		connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID)     //nolint:errcheck
	})

	// Trigger a run.
	tr := authReq(http.MethodPost, "/pipelines/"+wf.WorkflowID+"/runs", []byte(`{"inputs":{"k":"v"}}`))
	tr.SetPathValue("id", wf.WorkflowID)
	tw := httptest.NewRecorder()
	handleTriggerRun(tw, tr)
	if tw.Code != http.StatusCreated && tw.Code != http.StatusAccepted && tw.Code != http.StatusOK {
		t.Fatalf("trigger run got %d: %s", tw.Code, tw.Body.String())
	}
	var run WorkflowRun
	json.Unmarshal(tw.Body.Bytes(), &run) //nolint:errcheck

	// List runs.
	lw := httptest.NewRecorder()
	handleListRuns(lw, authReq(http.MethodGet, "/runs", nil))
	if lw.Code != http.StatusOK {
		t.Fatalf("list runs got %d", lw.Code)
	}

	// Get the run.
	if run.RunID != "" {
		gr := authReq(http.MethodGet, "/runs/"+run.RunID, nil)
		gr.SetPathValue("id", run.RunID)
		gw := httptest.NewRecorder()
		handleGetRun(gw, gr)
		if gw.Code != http.StatusOK {
			t.Fatalf("get run got %d: %s", gw.Code, gw.Body.String())
		}
	}
}

func TestStepHandlers_GetNotFound(t *testing.T) {
	requireDB(t)
	okGate(t, "su", "so")
	id := uuid.New().String()
	r := authReq(http.MethodGet, "/steps/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetStep(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// ── Workflow + run handlers ────────────────────────────────────────────────────

func TestWorkflowHandlers_Lifecycle(t *testing.T) {
	requireDB(t)
	okGate(t, "wu", "wo")
	step := seedStep(t, "wu", "wo")

	// Create workflow referencing the step.
	body, _ := json.Marshal(map[string]any{
		"name":  "wf-" + uuid.New().String(),
		"steps": []map[string]any{{"step_id": step.StepID}},
	})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d, want 201: %s", w.Code, w.Body.String())
	}
	var wf Workflow
	json.Unmarshal(w.Body.Bytes(), &wf)                                                                //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID) }) //nolint:errcheck

	// List
	lw := httptest.NewRecorder()
	handleListWorkflows(lw, authReq(http.MethodGet, "/pipelines", nil))
	if lw.Code != http.StatusOK {
		t.Fatalf("list got %d", lw.Code)
	}

	// Get
	gr := authReq(http.MethodGet, "/pipelines/"+wf.WorkflowID, nil)
	gr.SetPathValue("id", wf.WorkflowID)
	gw := httptest.NewRecorder()
	handleGetWorkflow(gw, gr)
	if gw.Code != http.StatusOK {
		t.Fatalf("get got %d", gw.Code)
	}

	// Delete workflow
	dr := authReq(http.MethodDelete, "/pipelines/"+wf.WorkflowID, nil)
	dr.SetPathValue("id", wf.WorkflowID)
	dw := httptest.NewRecorder()
	handleDeleteWorkflow(dw, dr)
	if dw.Code != http.StatusNoContent && dw.Code != http.StatusOK {
		t.Fatalf("delete workflow got %d", dw.Code)
	}
}
