package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// seedNamedSvc inserts an active service account with the given name + a known key.
func seedNamedSvc(t *testing.T, name string) string {
	t.Helper()
	gormDB.Unscoped().Where("service_name = ?", name).Delete(&ServiceAccount{}) //nolint:errcheck
	key := "key-" + uuid.NewString()[:8]
	hash, _ := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
	svc := ServiceAccount{ServiceAccountID: uuid.NewString(), ServiceName: name, HashedKey: string(hash), Active: true}
	if err := svc.Add(context.Background()); err != nil {
		t.Fatalf("seed svc %s: %v", name, err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("service_name = ?", name).Delete(&ServiceAccount{}) }) //nolint:errcheck
	return key
}

func seedTestUser(t *testing.T) User {
	t.Helper()
	u := User{UserID: uuid.NewString(), Email: uuid.NewString() + "@x.com", Username: "u" + uuid.NewString()[:8], HashedPassword: "x"}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("user_id = ?", u.UserID).Delete(&User{}) }) //nolint:errcheck
	return u
}

func runTokenReq(svcKey string, body any) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/internal/run-tokens", bytes.NewReader(b))
	r.Header.Set("X-Service-Key", "workflows:"+svcKey)
	return r
}

func TestHandleCreateRunToken_Success(t *testing.T) {
	key := seedNamedSvc(t, "workflows")
	u := seedTestUser(t)
	w := httptest.NewRecorder()
	handleCreateRunToken(w, runTokenReq(key, map[string]any{"user_id": u.UserID}))
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["token"] == "" || resp["session_id"] == "" {
		t.Errorf("expected token + session_id, got %v", resp)
	}
}

func TestHandleCreateRunToken_Forbidden(t *testing.T) {
	key := seedNamedSvc(t, "not-workflows")
	r := httptest.NewRequest(http.MethodPost, "/internal/run-tokens", bytes.NewReader([]byte(`{"user_id":"u"}`)))
	r.Header.Set("X-Service-Key", "not-workflows:"+key)
	w := httptest.NewRecorder()
	handleCreateRunToken(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-workflows svc got %d, want 403", w.Code)
	}
}

// The run-token minters are an explicit allowlist, not a general service capability:
// a minted role can only ever be a subset of the owner's own access, but only these
// services are trusted to choose that subset. git_connector is on it so it can
// authorize a clone against git-factory as the user the clone is for, rather than
// holding a shared east-west key.
func TestCanMintScopedRoles_Allowlist(t *testing.T) {
	for _, svc := range []string{"workflows", "forge", "git_connector"} {
		if !canMintScopedRoles(svc) {
			t.Errorf("%s must be allowed to mint scoped roles", svc)
		}
	}
	for _, svc := range []string{"", "gatekeeper", "registry", "builder", "tickets", "codearmory_git_factory"} {
		if canMintScopedRoles(svc) {
			t.Errorf("%s must NOT be allowed to mint scoped roles", svc)
		}
	}
}

func TestHandleCreateRunToken_MissingUserAndNotFound(t *testing.T) {
	key := seedNamedSvc(t, "workflows")
	// Missing user_id → 400.
	w := httptest.NewRecorder()
	handleCreateRunToken(w, runTokenReq(key, map[string]any{}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing user_id got %d, want 400", w.Code)
	}
	// Unknown user → 404.
	w2 := httptest.NewRecorder()
	handleCreateRunToken(w2, runTokenReq(key, map[string]any{"user_id": uuid.NewString()}))
	if w2.Code != http.StatusNotFound {
		t.Fatalf("unknown user got %d, want 404", w2.Code)
	}
}

func TestHandleRevokeRunToken(t *testing.T) {
	key := seedNamedSvc(t, "workflows")
	id := uuid.NewString()
	r := httptest.NewRequest(http.MethodDelete, "/internal/run-tokens/"+id, nil)
	r.SetPathValue("session_id", id)
	r.Header.Set("X-Service-Key", "workflows:"+key)
	w := httptest.NewRecorder()
	handleRevokeRunToken(w, r)
	// Revoking an unknown session is idempotent (2xx), not an error.
	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("revoke got %d, want 2xx", w.Code)
	}
}

func TestHandleCreateWorkflowRole_BadRequests(t *testing.T) {
	key := seedNamedSvc(t, "workflows")
	// Missing workflow_id/user_id → 400.
	b, _ := json.Marshal(map[string]any{"org_id": "o"})
	r := httptest.NewRequest(http.MethodPost, "/internal/workflow-roles", bytes.NewReader(b))
	r.Header.Set("X-Service-Key", "workflows:"+key)
	w := httptest.NewRecorder()
	handleCreateWorkflowRole(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing fields got %d, want 400", w.Code)
	}
	// Non-workflows service → 403.
	other := seedNamedSvc(t, "not-wf")
	r2 := httptest.NewRequest(http.MethodPost, "/internal/workflow-roles", bytes.NewReader([]byte(`{"workflow_id":"w","user_id":"u"}`)))
	r2.Header.Set("X-Service-Key", "not-wf:"+other)
	w2 := httptest.NewRecorder()
	handleCreateWorkflowRole(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("non-workflows got %d, want 403", w2.Code)
	}
}

// An org-less owner (and a caller that passes an empty org_id) must produce a
// role whose OrgID is NULL, not a dangling empty-string pointer — the latter
// violates fk_roles_org on Postgres, fails the whole creation, and silently
// drops the run back to the user's full permissions. The role must be created.
func TestHandleCreateWorkflowRole_OrglessOwnerStoresNilOrg(t *testing.T) {
	key := seedNamedSvc(t, "workflows")
	owner := seedTestUser(t) // seedTestUser leaves OrgID nil
	wfID := uuid.NewString()

	body, _ := json.Marshal(map[string]any{
		"workflow_id": wfID,
		"user_id":     owner.UserID,
		"org_id":      "", // the trigger: a caller-supplied empty org id
		"permissions": []map[string]string{
			{"service": "forge", "action": "createExecution", "resource": "forge/executions"},
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/internal/workflow-roles", bytes.NewReader(body))
	r.Header.Set("X-Service-Key", "workflows:"+key)
	w := httptest.NewRecorder()
	handleCreateWorkflowRole(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create workflow role got %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["role_id"] == "" {
		t.Fatal("expected role_id in response")
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("role_id = ?", resp["role_id"]).Delete(&Role{}) }) //nolint:errcheck

	var role Role
	if err := gormDB.Where("role_id = ?", resp["role_id"]).First(&role).Error; err != nil {
		t.Fatalf("load created role: %v", err)
	}
	if role.OrgID != nil {
		t.Errorf("org-less owner: role OrgID = %q, want nil (must not store a dangling empty org)", *role.OrgID)
	}
}
