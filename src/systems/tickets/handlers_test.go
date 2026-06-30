package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

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

func TestMain(m *testing.M) {
	initMetrics()
	gatekeeperClient = newGatekeeperClient()
	os.Exit(m.Run())
}

// ── CheckPermissions ──────────────────────────────────────────────────────────

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/tickets", nil)
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listTicket", "tickets/tickets")
	if ok {
		t.Fatal("expected ok=false with no Bearer token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestCheckGatekeeper_Authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-abc","org_id":"org-1"}`)
	r := httptest.NewRequest(http.MethodGet, "/tickets", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	id, org, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listTicket", "tickets/tickets")
	if !ok {
		t.Fatalf("expected ok=true (status %d: %s)", w.Code, w.Body.String())
	}
	if id != "user-abc" {
		t.Fatalf("got user_id %q, want user-abc", id)
	}
	if org != "org-1" {
		t.Fatalf("got org_id %q, want org-1", org)
	}
}

func TestCheckGatekeeper_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":false,"user_id":"user-abc"}`)
	r := httptest.NewRequest(http.MethodGet, "/tickets", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listTicket", "tickets/tickets")
	if ok {
		t.Fatal("expected ok=false when not authorized")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestCheckGatekeeper_GatekeeperDown(t *testing.T) {
	orig := gatekeeperClient.URL
	gatekeeperClient.URL = "http://127.0.0.1:1"
	t.Cleanup(func() { gatekeeperClient.URL = orig })

	r := httptest.NewRequest(http.MethodGet, "/tickets", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listTicket", "tickets/tickets")
	if ok {
		t.Fatal("expected ok=false when gatekeeper is unreachable")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", w.Code)
	}
}

// ── canAccessTicket ───────────────────────────────────────────────────────────

func TestCanAccessTicket_Owner(t *testing.T) {
	ticket := Ticket{CreatedBy: "user-1", OrgID: "org-1"}
	if !canAccessTicket(ticket, "user-1", "") {
		t.Error("owner should have access")
	}
}

func TestCanAccessTicket_SameOrg(t *testing.T) {
	ticket := Ticket{CreatedBy: "user-1", OrgID: "org-1"}
	if !canAccessTicket(ticket, "user-2", "org-1") {
		t.Error("same-org user should have access")
	}
}

func TestCanAccessTicket_DifferentOrg(t *testing.T) {
	ticket := Ticket{CreatedBy: "user-1", OrgID: "org-1"}
	if canAccessTicket(ticket, "user-2", "org-2") {
		t.Error("different-org user should not have access")
	}
}

func TestCanAccessTicket_NoOrg(t *testing.T) {
	ticket := Ticket{CreatedBy: "user-1", OrgID: ""}
	if canAccessTicket(ticket, "user-2", "") {
		t.Error("no-org user should not access another user's ticket")
	}
}

func TestCanAccessTicket_OrgMatchOnNoOrgTicket(t *testing.T) {
	// Ticket with no org — org membership cannot grant access.
	ticket := Ticket{CreatedBy: "user-1", OrgID: ""}
	if canAccessTicket(ticket, "user-2", "org-1") {
		t.Error("org membership should not grant access to a no-org ticket")
	}
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestHandleCreateTicket_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/tickets", nil)
	w := httptest.NewRecorder()
	handleCreateTicket(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateTicket_MissingTitle(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"description":"desc"}`
	r := httptest.NewRequest(http.MethodPost, "/tickets", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateTicket(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateTicket_InvalidPriority(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"title":"Fix bug","priority":"urgent"}`
	r := httptest.NewRequest(http.MethodPost, "/tickets", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateTicket(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateTicket_InvalidBody(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodPost, "/tickets", bytes.NewBufferString("not-json"))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateTicket(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleListTickets_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/tickets", nil)
	w := httptest.NewRecorder()
	handleListTickets(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListTickets_InvalidStatusFilter(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodGet, "/tickets?status=invalid", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleListTickets(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleListTickets_InvalidPriorityFilter(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodGet, "/tickets?priority=extreme", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleListTickets(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleGetTicket_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/tickets/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleGetTicket(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteTicket_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/tickets/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleDeleteTicket(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleAddComment_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/tickets/some-id/comments", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleAddComment(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── updateTicketRequest validation ────────────────────────────────────────────

func TestHandleUpdateTicket_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/tickets/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleUpdateTicket(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── envOrDefault / secret ────────────────────────────────────────────────────

func TestEnvOrDefault_Set(t *testing.T) {
	t.Setenv("TKT_TEST_KEY", "myvalue")
	if got := envOrDefault("TKT_TEST_KEY", "default"); got != "myvalue" {
		t.Fatalf("got %q, want myvalue", got)
	}
}

func TestEnvOrDefault_Fallback(t *testing.T) {
	os.Unsetenv("TKT_ABSENT_KEY")
	if got := envOrDefault("TKT_ABSENT_KEY", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want fallback", got)
	}
}

func TestSecret_FromEnv(t *testing.T) {
	t.Setenv("TKT_MY_SECRET", "direct")
	if got := secret("TKT_MY_SECRET"); got != "direct" {
		t.Fatalf("got %q, want direct", got)
	}
}

func TestSecret_FromFile(t *testing.T) {
	f, err := os.CreateTemp("", "tkt-secret-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("file-secret\n")
	f.Close()
	t.Setenv("TKT_FILE_SECRET_FILE", f.Name())
	os.Unsetenv("TKT_FILE_SECRET")
	if got := secret("TKT_FILE_SECRET"); got != "file-secret" {
		t.Fatalf("got %q, want file-secret", got)
	}
}

// ── validStatuses / validPriorities ──────────────────────────────────────────

func TestValidStatusesAndPriorities(t *testing.T) {
	for _, s := range []string{"open", "in_progress", "resolved", "closed"} {
		found := false
		for _, v := range validStatuses {
			if v == s {
				found = true
			}
		}
		if !found {
			t.Errorf("status %q missing from validStatuses", s)
		}
	}
	for _, p := range []string{"low", "medium", "high", "critical"} {
		found := false
		for _, v := range validPriorities {
			if v == p {
				found = true
			}
		}
		if !found {
			t.Errorf("priority %q missing from validPriorities", p)
		}
	}
}

// ── handleDeleteComment auth ──────────────────────────────────────────────────

func TestHandleDeleteComment_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/tickets/t-1/comments/c-1", nil)
	r.SetPathValue("id", "t-1")
	r.SetPathValue("comment_id", "c-1")
	w := httptest.NewRecorder()
	handleDeleteComment(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}



// ── statusResponseWriter ─────────────────────────────────────────────────────

func TestStatusResponseWriter(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusNotFound {
		t.Fatalf("rw.status: got %d, want 404", rw.status)
	}
}

// ── Ticket JSON shape ─────────────────────────────────────────────────────────

func TestTicketJSON_NullableFieldsOmitted(t *testing.T) {
	ticket := Ticket{
		TicketID:  "t1",
		Title:     "Test",
		CreatedBy: "u1",
		Comments:  []TicketComment{},
	}
	data, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"assignee_id", "workflow_id", "run_id", "forge_execution_id"} {
		if _, ok := m[field]; ok {
			t.Errorf("field %q should be omitted when nil", field)
		}
	}
}
