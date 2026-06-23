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
