package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// authReq builds a bearer-authenticated request (the fake gatekeeper authorizes it).
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

func okGatekeeper(t *testing.T, user, org string) {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"`+user+`","org_id":"`+org+`"}`)
}

// ── Rule CRUD success / dispatch-validation paths ──────────────────────────────

func TestHandleCreateRule_Success(t *testing.T) {
	requireDB(t)
	okGatekeeper(t, "u1", "o1")
	withTriggerKey(t, "trigger-key") // required by fetchWorkflowOrgID
	fakeWorkflows(t, map[string]string{"w1": "o1"}, "run-1") // workflow belongs to caller org

	name := "cr-" + uuid.New().String()
	body, _ := json.Marshal(map[string]any{
		"name": name, "source": "repo/x", "events": []string{"push"},
		"workflow_id": "w1", "secret": "shh",
	})
	w := httptest.NewRecorder()
	handleCreateRule(w, authReq(http.MethodPost, "/rules", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body.String())
	}
	var created PipelineRule
	json.Unmarshal(w.Body.Bytes(), &created) //nolint:errcheck
	if created.RuleID == "" {
		t.Fatal("expected a rule id in the response")
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM pipeline_rules WHERE rule_id = ?`, created.RuleID) }) //nolint:errcheck
	// Secret must never be echoed back.
	if created.Secret != nil {
		t.Error("secret must be write-only, not returned")
	}
}

func TestHandleCreateRule_WorkflowNotFound(t *testing.T) {
	requireDB(t)
	okGatekeeper(t, "u1", "o1")
	withTriggerKey(t, "trigger-key")
	fakeWorkflows(t, map[string]string{}, "") // unknown workflow → 404 from fetchWorkflowOrgID

	body, _ := json.Marshal(map[string]any{
		"name": "x", "source": "repo/x", "events": []string{"push"},
		"workflow_id": "ghost", "secret": "shh",
	})
	w := httptest.NewRecorder()
	handleCreateRule(w, authReq(http.MethodPost, "/rules", body))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", w.Code)
	}
}

func TestHandleCreateRule_CrossOrgWorkflowRejected(t *testing.T) {
	requireDB(t)
	okGatekeeper(t, "u1", "o1")
	withTriggerKey(t, "trigger-key")
	fakeWorkflows(t, map[string]string{"w1": "other-org"}, "") // workflow in a different org

	body, _ := json.Marshal(map[string]any{
		"name": "x", "source": "repo/x", "events": []string{"push"},
		"workflow_id": "w1", "secret": "shh",
	})
	w := httptest.NewRecorder()
	handleCreateRule(w, authReq(http.MethodPost, "/rules", body))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-org workflow got %d, want 422", w.Code)
	}
}

func TestHandleListRules_Success(t *testing.T) {
	requireDB(t)
	owner := "u-" + uuid.New().String()
	r1 := insertRule(t, PipelineRule{Name: "a", Source: "s/a", Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, OrgID: "o1", Active: true})
	okGatekeeper(t, owner, "o1")
	w := httptest.NewRecorder()
	handleListRules(w, authReq(http.MethodGet, "/rules", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var rules []PipelineRule
	json.Unmarshal(w.Body.Bytes(), &rules) //nolint:errcheck
	found := false
	for _, rr := range rules {
		if rr.RuleID == r1.RuleID {
			found = true
		}
	}
	if !found {
		t.Error("owned rule missing from list")
	}
}

func TestHandleGetRule_SuccessAndIsolation(t *testing.T) {
	requireDB(t)
	owner := "u-" + uuid.New().String()
	rule := insertRule(t, PipelineRule{Name: "g", Source: "s/g", Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, OrgID: "o1", Active: true})

	// Owner sees it.
	okGatekeeper(t, owner, "o1")
	r := authReq(http.MethodGet, "/rules/"+rule.RuleID, nil)
	r.SetPathValue("id", rule.RuleID)
	w := httptest.NewRecorder()
	handleGetRule(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("owner get got %d, want 200", w.Code)
	}

	// A different tenant gets 404 (isolation), not the rule.
	okGatekeeper(t, "intruder", "other-org")
	r2 := authReq(http.MethodGet, "/rules/"+rule.RuleID, nil)
	r2.SetPathValue("id", rule.RuleID)
	w2 := httptest.NewRecorder()
	handleGetRule(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get got %d, want 404", w2.Code)
	}
}

func TestHandleGetRule_NotFound(t *testing.T) {
	requireDB(t)
	okGatekeeper(t, "u1", "o1")
	id := uuid.New().String()
	r := authReq(http.MethodGet, "/rules/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetRule(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestHandleUpdateRule_Success(t *testing.T) {
	requireDB(t)
	owner := "u-" + uuid.New().String()
	rule := insertRule(t, PipelineRule{Name: "u", Source: "s/u", Events: []string{"push"}, WorkflowID: "w1", Secret: secretPtr("k"), CreatedBy: owner, OrgID: "o1", Active: true})
	okGatekeeper(t, owner, "o1")
	withTriggerKey(t, "trigger-key")
	fakeWorkflows(t, map[string]string{"w1": "o1"}, "run-1")

	body, _ := json.Marshal(map[string]any{
		"name": "updated", "source": "s/u", "events": []string{"push", "tag"}, "workflow_id": "w1",
	})
	r := authReq(http.MethodPut, "/rules/"+rule.RuleID, body)
	r.SetPathValue("id", rule.RuleID)
	w := httptest.NewRecorder()
	handleUpdateRule(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var updated PipelineRule
	json.Unmarshal(w.Body.Bytes(), &updated) //nolint:errcheck
	if updated.Name != "updated" {
		t.Errorf("name = %q, want updated", updated.Name)
	}
}

func TestHandleDeleteRule_Success(t *testing.T) {
	requireDB(t)
	owner := "u-" + uuid.New().String()
	rule := insertRule(t, PipelineRule{Name: "d", Source: "s/d", Events: []string{"push"}, WorkflowID: "w", Secret: secretPtr("k"), CreatedBy: owner, OrgID: "o1", Active: true})
	okGatekeeper(t, owner, "o1")
	r := authReq(http.MethodDelete, "/rules/"+rule.RuleID, nil)
	r.SetPathValue("id", rule.RuleID)
	w := httptest.NewRecorder()
	handleDeleteRule(w, r)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("got %d, want 204/200: %s", w.Code, w.Body.String())
	}
}

func TestHandleListEvents_Success(t *testing.T) {
	requireDB(t)
	okGatekeeper(t, "u1", "o1")
	w := httptest.NewRecorder()
	handleListEvents(w, authReq(http.MethodGet, "/events", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestHandleGetEvent_NotFound(t *testing.T) {
	requireDB(t)
	okGatekeeper(t, "u1", "o1")
	id := uuid.New().String()
	r := authReq(http.MethodGet, "/events/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetEvent(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// ── Webhook → match → dispatch chain ───────────────────────────────────────────

// postWebhook builds a webhook request with a correct HMAC signature for secret.
func postWebhook(t *testing.T, source, event, secret string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"source": source, "event": event, "payload": map[string]string{"ref": "refs/heads/main"}})
	r := httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(body))
	if secret != "" {
		r.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(secret, body))
	}
	w := httptest.NewRecorder()
	handleWebhook(w, r)
	return w
}

func TestHandleWebhook_DispatchesMatchingRule(t *testing.T) {
	requireDB(t)
	src := "repo/" + uuid.New().String()
	rule := insertRule(t, PipelineRule{Name: "wh", Source: src, Events: []string{"push"}, WorkflowID: "w1", Secret: secretPtr("topsecret"), CreatedBy: "u1", OrgID: "o1", Active: true})
	cleanupEvent := func(id string) { connect().Exec(`DELETE FROM hook_triggers WHERE rule_id = ?`, id) } //nolint:errcheck
	t.Cleanup(func() { cleanupEvent(rule.RuleID) })
	fakeWorkflows(t, map[string]string{"w1": "o1"}, "run-xyz")

	w := postWebhook(t, src, "push", "topsecret")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var ev HookEvent
	json.Unmarshal(w.Body.Bytes(), &ev) //nolint:errcheck
	if ev.Status != "triggered" {
		t.Fatalf("event status = %q, want triggered (matched=%d)", ev.Status, ev.RulesMatched)
	}
	if ev.RulesMatched != 1 {
		t.Errorf("rules matched = %d, want 1", ev.RulesMatched)
	}
}

func TestHandleWebhook_NoMatchingRule(t *testing.T) {
	requireDB(t)
	w := postWebhook(t, "repo/"+uuid.New().String(), "push", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var ev HookEvent
	json.Unmarshal(w.Body.Bytes(), &ev) //nolint:errcheck
	if ev.Status != "received" || ev.RulesMatched != 0 {
		t.Errorf("status=%q matched=%d, want received/0", ev.Status, ev.RulesMatched)
	}
}

func TestHandleWebhook_BadHMACSkipsRule(t *testing.T) {
	requireDB(t)
	src := "repo/" + uuid.New().String()
	insertRule(t, PipelineRule{Name: "wh2", Source: src, Events: []string{"push"}, WorkflowID: "w1", Secret: secretPtr("realsecret"), CreatedBy: "u1", OrgID: "o1", Active: true})
	// Sign with the WRONG secret → HMAC mismatch → rule skipped.
	w := postWebhook(t, src, "push", "wrongsecret")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var ev HookEvent
	json.Unmarshal(w.Body.Bytes(), &ev) //nolint:errcheck
	if ev.RulesMatched != 0 {
		t.Errorf("matched=%d, want 0 (bad HMAC must skip)", ev.RulesMatched)
	}
}

func TestHandleWebhook_MissingSourceOrEvent(t *testing.T) {
	requireDB(t)
	// missing source
	body, _ := json.Marshal(map[string]any{"event": "push"})
	w := httptest.NewRecorder()
	handleWebhook(w, httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing source got %d, want 400", w.Code)
	}
	// invalid json
	w2 := httptest.NewRecorder()
	handleWebhook(w2, httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewBufferString("{not json")))
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("bad json got %d, want 400", w2.Code)
	}
}

// ── helpers / middleware ───────────────────────────────────────────────────────

func TestSecretOrDefault_Hooks(t *testing.T) {
	t.Setenv("HK_TEST", "v")
	if secretOrDefault("HK_TEST", "d") != "v" {
		t.Error("env value not returned")
	}
	if secretOrDefault("HK_MISSING", "d") != "d" {
		t.Error("default not returned")
	}
}

func TestHandleOpenAPIYAML_Hooks(t *testing.T) {
	w := httptest.NewRecorder()
	handleOpenAPIYAML(w, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("openapi: code=%d len=%d", w.Code, w.Body.Len())
	}
}
