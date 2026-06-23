package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)


// fakeGatekeeper spins up a test server that always returns the given status
// and body, overriding gatekeeperClient.URL for the test duration.
func fakeGatekeeper(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperClient.URL
	gatekeeperClient.URL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		srv.Close()
	})
}

// ── CheckPermissions ──────────────────────────────────────────────────────────

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listRule", "hooks/rules")
	if ok {
		t.Fatal("expected ok=false with no Bearer token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestCheckGatekeeper_Authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-abc","org_id":"org-1"}`)
	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	id, org, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listRule", "hooks/rules")
	if !ok {
		t.Fatalf("expected ok=true (status %d: %s)", w.Code, w.Body.String())
	}
	if id != "user-abc" {
		t.Fatalf("got user_id %q, want %q", id, "user-abc")
	}
	if org != "org-1" {
		t.Fatalf("got org_id %q, want %q", org, "org-1")
	}
}

func TestCheckGatekeeper_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":false,"user_id":"user-abc"}`)
	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listRule", "hooks/rules")
	if ok {
		t.Fatal("expected ok=false when not authorized")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestCheckGatekeeper_GatekeeperDown(t *testing.T) {
	orig := gatekeeperClient.URL
	gatekeeperClient.URL = "http://127.0.0.1:1" // nothing listening
	t.Cleanup(func() { gatekeeperClient.URL = orig })

	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listRule", "hooks/rules")
	if ok {
		t.Fatal("expected ok=false when gatekeeper is unreachable")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", w.Code)
	}
}

// ── canAccessRule ─────────────────────────────────────────────────────────────

func TestCanAccessRule_Owner(t *testing.T) {
	rule := PipelineRule{CreatedBy: "user-1", OrgID: ""}
	if !canAccessRule(rule, "user-1", "") {
		t.Fatal("owner should have access")
	}
}

func TestCanAccessRule_SameOrg(t *testing.T) {
	rule := PipelineRule{CreatedBy: "user-1", OrgID: "org-x"}
	if !canAccessRule(rule, "user-2", "org-x") {
		t.Fatal("same-org user should have access")
	}
}

func TestCanAccessRule_DifferentOrg(t *testing.T) {
	rule := PipelineRule{CreatedBy: "user-1", OrgID: "org-x"}
	if canAccessRule(rule, "user-2", "org-y") {
		t.Fatal("different-org user should not have access")
	}
}

func TestCanAccessRule_NoOrg(t *testing.T) {
	rule := PipelineRule{CreatedBy: "user-1", OrgID: ""}
	if canAccessRule(rule, "user-2", "") {
		t.Fatal("unrelated user with no org should not have access")
	}
}

// ── matchesRefFilter ──────────────────────────────────────────────────────────

func TestMatchesRefFilter_Empty(t *testing.T) {
	if !matchesRefFilter("", "refs/heads/main") {
		t.Fatal("empty filter should match any ref")
	}
	if !matchesRefFilter("", "") {
		t.Fatal("empty filter should match empty ref")
	}
}

func TestMatchesRefFilter_ExactMatch(t *testing.T) {
	if !matchesRefFilter("refs/heads/main", "refs/heads/main") {
		t.Fatal("exact filter should match equal ref")
	}
	if matchesRefFilter("refs/heads/main", "refs/heads/dev") {
		t.Fatal("exact filter should not match different ref")
	}
}

func TestMatchesRefFilter_PrefixGlob(t *testing.T) {
	if !matchesRefFilter("refs/heads/*", "refs/heads/main") {
		t.Fatal("glob filter should match prefix")
	}
	if !matchesRefFilter("refs/heads/*", "refs/heads/feature/foo") {
		t.Fatal("glob filter should match deeper path")
	}
	if matchesRefFilter("refs/heads/*", "refs/tags/v1.0") {
		t.Fatal("glob filter should not match outside prefix")
	}
}

func TestMatchesRefFilter_WildcardGlob(t *testing.T) {
	if !matchesRefFilter("refs/heads/feat*", "refs/heads/feature-login") {
		t.Fatal("wildcard pattern should match branch starting with prefix")
	}
	if !matchesRefFilter("refs/tags/v[0-9]*", "refs/tags/v1.2.3") {
		t.Fatal("character-class pattern should match tagged version")
	}
	if matchesRefFilter("refs/heads/feat*", "refs/heads/main") {
		t.Fatal("wildcard pattern should not match branch outside prefix")
	}
	if matchesRefFilter("refs/heads/feat*", "refs/heads/feat/sub") {
		t.Fatal("wildcard * should not cross a path separator")
	}
}

func TestMatchesRefFilter_Mismatch(t *testing.T) {
	if matchesRefFilter("refs/heads/main", "refs/heads/develop") {
		t.Fatal("should not match mismatched exact ref")
	}
}

// ── signTrigger ───────────────────────────────────────────────────────────────

func TestSignTrigger_Consistent(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "test-secret"
	t.Cleanup(func() { hooksTriggerKey = orig })

	tok1, ts1 := signTrigger("wf-1", "user-1")
	if tok1 == "" {
		t.Fatal("expected non-empty token")
	}
	if ts1 == "" {
		t.Fatal("expected non-empty timestamp")
	}

	// Same call should produce a token of the same length (hex SHA256 = 64 chars).
	if len(tok1) != 64 {
		t.Fatalf("expected 64-char hex token, got %d chars", len(tok1))
	}
}

func TestSignTrigger_DifferentTimestamps(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "test-secret"
	t.Cleanup(func() { hooksTriggerKey = orig })

	tokA, tsA := signTriggerAt("wf-1", "user-1", 1000)
	tokB, tsB := signTriggerAt("wf-1", "user-1", 1000)
	tokC, _ := signTriggerAt("wf-1", "user-1", 1001)
	tokD, _ := signTriggerAt("wf-2", "user-1", 1000)

	if tsA != "1000" {
		t.Errorf("timestamp = %q, want 1000", tsA)
	}
	if tokA != tokB || tsA != tsB {
		t.Error("same inputs and timestamp must produce an identical token")
	}
	if tokA == tokC {
		t.Error("a different timestamp must produce a different token")
	}
	if tokA == tokD {
		t.Error("a different workflow id must produce a different token")
	}
}

// ── handler auth (no-DB) ──────────────────────────────────────────────────────

func TestHandleCreateRule_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/rules", nil)
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListRules_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	w := httptest.NewRecorder()
	handleListRules(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetRule_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/rules/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleGetRule(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── handleWebhook validation (generic endpoint) ───────────────────────────────

func TestHandleWebhook_MissingSource(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"event": "deploy"})
	r := httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleWebhook_MissingEvent(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"source": "myapp"})
	r := httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "event") {
		t.Fatalf("expected 'event' in error body, got %q", w.Body.String())
	}
}

// ── handleGitWebhook validation (git adapter) ─────────────────────────────────

func TestHandleGitWebhook_MissingRepo(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"event": "push"})
	r := httptest.NewRequest(http.MethodPost, "/hooks/git", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleGitWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleGitWebhook_MissingEvent(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"repo": "myorg/myrepo"})
	r := httptest.NewRequest(http.MethodPost, "/hooks/git", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleGitWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "event") {
		t.Fatalf("expected 'event' in error body, got %q", w.Body.String())
	}
}

// ── handleCreateRule validation ───────────────────────────────────────────────

func TestHandleCreateRule_MissingName(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	body := `{"source":"owner/repo","events":["push"],"workflow_id":"wf-1","secret":"s3cr3t"}`
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateRule_MissingSource(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	body := `{"name":"ci","events":["push"],"workflow_id":"wf-1","secret":"s3cr3t"}`
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateRule_EmptyEvents(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	body := `{"name":"ci","source":"owner/repo","events":[],"workflow_id":"wf-1","secret":"s3cr3t"}`
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateRule_MissingWorkflowID(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	body := `{"name":"ci","source":"owner/repo","events":["push"],"secret":"s3cr3t"}`
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateRule_MissingSecret(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	body := `{"name":"ci","source":"owner/repo","events":["push"],"workflow_id":"wf-1"}`
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateRule_EmptySecret(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	empty := ""
	body, _ := json.Marshal(createRuleRequest{
		Name: "ci", Source: "owner/repo", Events: []string{"push"}, WorkflowID: "wf-1", Secret: &empty,
	})
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateRule_InvalidBody(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-1"}`)
	r := httptest.NewRequest(http.MethodPost, "/rules", bytes.NewBufferString("not-json"))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateRule(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── handleUpdateRule auth and validation ──────────────────────────────────────

func TestHandleUpdateRule_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/rules/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleUpdateRule(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── handleDeleteRule auth ─────────────────────────────────────────────────────

func TestHandleDeleteRule_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/rules/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleDeleteRule(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── handleListEvents / handleGetEvent auth ────────────────────────────────────

func TestHandleListEvents_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	w := httptest.NewRecorder()
	handleListEvents(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetEvent_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/events/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleGetEvent(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}


// ── computeHMAC ───────────────────────────────────────────────────────────────

func TestComputeHMAC_Deterministic(t *testing.T) {
	body := []byte("test payload")
	got1 := computeHMAC("secret", body)
	got2 := computeHMAC("secret", body)
	if got1 != got2 {
		t.Fatal("computeHMAC is not deterministic")
	}
	if len(got1) != 64 {
		t.Fatalf("expected 64-char hex HMAC-SHA256, got %d chars", len(got1))
	}
}

func TestComputeHMAC_DifferentSecrets(t *testing.T) {
	body := []byte("test payload")
	h1 := computeHMAC("secret1", body)
	h2 := computeHMAC("secret2", body)
	if h1 == h2 {
		t.Fatal("different secrets should produce different HMACs")
	}
}

func TestComputeHMAC_DifferentBodies(t *testing.T) {
	h1 := computeHMAC("secret", []byte("body1"))
	h2 := computeHMAC("secret", []byte("body2"))
	if h1 == h2 {
		t.Fatal("different bodies should produce different HMACs")
	}
}

func TestComputeHMAC_MatchesWebhookFormat(t *testing.T) {
	// Verify computeHMAC produces the value expected by the "sha256=" check in matchAndDispatch.
	body := []byte(`{"repo":"org/repo","event":"push"}`)
	secret := "webhook-secret"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body) //nolint:errcheck
	want := hex.EncodeToString(mac.Sum(nil))

	if got := computeHMAC(secret, body); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ── statusResponseWriter ──────────────────────────────────────────────────────

func TestStatusResponseWriter_WriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusNotFound {
		t.Fatalf("rw.status: got %d, want 404", rw.status)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder: got %d, want 404", w.Code)
	}
}
