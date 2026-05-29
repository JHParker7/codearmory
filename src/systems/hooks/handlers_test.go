package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeGatekeeper spins up a test server that always returns the given status
// and body, overriding the package-level gatekeeperURL for the test duration.
func fakeGatekeeper(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() {
		gatekeeperURL = orig
		srv.Close()
	})
}

func TestMain(m *testing.M) {
	initMetrics()
	os.Exit(m.Run())
}

// ── checkGatekeeper ───────────────────────────────────────────────────────────

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	w := httptest.NewRecorder()
	_, _, ok := checkGatekeeper(r.Context(), w, r, "listRule", "hooks/rules")
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
	id, org, ok := checkGatekeeper(r.Context(), w, r, "listRule", "hooks/rules")
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
	_, _, ok := checkGatekeeper(r.Context(), w, r, "listRule", "hooks/rules")
	if ok {
		t.Fatal("expected ok=false when not authorized")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestCheckGatekeeper_GatekeeperDown(t *testing.T) {
	orig := gatekeeperURL
	gatekeeperURL = "http://127.0.0.1:1" // nothing listening
	t.Cleanup(func() { gatekeeperURL = orig })

	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := checkGatekeeper(r.Context(), w, r, "listRule", "hooks/rules")
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

	tok1, ts1 := signTrigger("wf-1", "user-1")
	tok2, ts2 := signTrigger("wf-1", "user-1")

	// Tokens may differ only if timestamps differ.
	if ts1 != ts2 {
		if tok1 == tok2 {
			t.Fatal("tokens with different timestamps should differ")
		}
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

// ── handleWebhook validation ──────────────────────────────────────────────────

func TestHandleWebhook_MissingRepo(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"event": "push"})
	r := httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleWebhook_MissingEvent(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"repo": "myorg/myrepo"})
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
