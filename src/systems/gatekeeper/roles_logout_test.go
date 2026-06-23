package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestHandleLogout(t *testing.T) {
	w := httptest.NewRecorder()
	handleLogout(w, httptest.NewRequest(http.MethodPost, "/logout", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", w.Code)
	}
	// The session cookie must be cleared (MaxAge<0).
	if len(w.Result().Cookies()) == 0 {
		t.Error("expected a cookie-clearing Set-Cookie header")
	}
}

func TestHandleListRoles_Authorized(t *testing.T) {
	actor := createAuthorizedUser(t, "listRole", "gatekeeper/roles")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/roles", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListRoles(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestHandleListRoles_Forbidden(t *testing.T) {
	actor := createTestUser(t) // no listRole permission
	r := withUserID(httptest.NewRequest(http.MethodGet, "/roles", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListRoles(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("unauthorized caller got 200, want 4xx")
	}
}

func seedRole(t *testing.T, name string) Role {
	t.Helper()
	role := Role{RoleID: uuid.NewString(), Name: name, OwnerID: "u1"}
	if err := role.Add(context.Background()); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("role_id = ?", role.RoleID).Delete(&Role{}) }) //nolint:errcheck
	return role
}

func TestHandleDeleteWorkflowRole(t *testing.T) {
	key := seedNamedSvc(t, "workflows")
	do := func(roleID string) int {
		r := httptest.NewRequest(http.MethodDelete, "/internal/workflow-roles/"+roleID, nil)
		r.SetPathValue("role_id", roleID)
		r.Header.Set("X-Service-Key", "workflows:"+key)
		w := httptest.NewRecorder()
		handleDeleteWorkflowRole(w, r)
		return w.Code
	}

	// Unknown role → idempotent 204.
	if code := do(uuid.NewString()); code != http.StatusNoContent {
		t.Errorf("unknown role got %d, want 204", code)
	}
	// Non-workflow role → 403 (must not delete arbitrary roles).
	nonWF := seedRole(t, "org-admin")
	if code := do(nonWF.RoleID); code != http.StatusForbidden {
		t.Errorf("non-workflow role got %d, want 403", code)
	}
	// Workflow-provisioned role → deleted (2xx).
	wfRole := seedRole(t, "workflow:wf-"+uuid.NewString()[:8])
	if code := do(wfRole.RoleID); code < 200 || code >= 300 {
		t.Errorf("workflow role delete got %d, want 2xx", code)
	}
}
