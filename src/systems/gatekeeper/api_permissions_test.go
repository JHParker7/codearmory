package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCreatePermissions_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "createPermissions", "gatekeeper/permissions")

	b, _ := json.Marshal(permissionsRequest{Service: "svc", Actions: []string{"read"}, Resources: []string{"res"}})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/permissions", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreatePermissions(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Permissions
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.Service != "svc" {
		t.Fatalf("unexpected service: %s", resp.Service)
	}
}

func TestCreatePermissions_MissingService(t *testing.T) {
	actor := createAuthorizedUser(t, "createPermissions", "gatekeeper/permissions")

	b, _ := json.Marshal(permissionsRequest{Actions: []string{"read"}})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/permissions", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreatePermissions_Forbidden(t *testing.T) {
	actor := createTestUser(t)

	b, _ := json.Marshal(permissionsRequest{Service: "svc"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/permissions", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreatePermissions(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestGetPermissions_Success(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc", Actions: []string{"read"}, Resources: []string{"res"}}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "getPermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/permissions/"+perm.PermissionsID, nil), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleGetPermissions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Permissions
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.PermissionsID != perm.PermissionsID {
		t.Fatalf("got permissions_id %s, want %s", resp.PermissionsID, perm.PermissionsID)
	}
}

func TestGetPermissions_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "getPermissions", "gatekeeper/permissions/"+id)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/permissions/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetPermissions(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetPermissions_Forbidden(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc"}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/permissions/"+perm.PermissionsID, nil), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleGetPermissions(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestUpdatePermissions_Success(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc", Actions: []string{"read"}, Resources: []string{"res"}}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	b, _ := json.Marshal(permissionsRequest{Service: "svc", Actions: []string{"read", "write"}, Resources: []string{"res"}})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+perm.PermissionsID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp Permissions
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(resp.Actions))
	}
}

func TestUpdatePermissions_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+id)

	b, _ := json.Marshal(permissionsRequest{Service: "svc"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+id, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdatePermissions_Forbidden(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc"}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createTestUser(t)

	b, _ := json.Marshal(permissionsRequest{Service: "svc"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+perm.PermissionsID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeletePermissions_Success(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc"}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "deletePermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/permissions/"+perm.PermissionsID, nil), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleDeletePermissions(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (Permissions{PermissionsID: perm.PermissionsID}).Get(context.Background()); err == nil {
		t.Fatal("expected permissions to be inaccessible after delete")
	}
}

func TestDeletePermissions_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "deletePermissions", "gatekeeper/permissions/"+id)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/permissions/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeletePermissions(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeletePermissions_Forbidden(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc"}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/permissions/"+perm.PermissionsID, nil), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleDeletePermissions(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- handleCreatePermissions: empty actions/resources validation ---

func TestCreatePermissions_EmptyActionRejected(t *testing.T) {
	actor := createAuthorizedUser(t, "createPermissions", "gatekeeper/permissions")

	b, _ := json.Marshal(permissionsRequest{Service: "svc", Actions: []string{"read", ""}, Resources: []string{"res"}})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/permissions", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty action string, got %d", w.Code)
	}
}

func TestCreatePermissions_EmptyResourceRejected(t *testing.T) {
	actor := createAuthorizedUser(t, "createPermissions", "gatekeeper/permissions")

	b, _ := json.Marshal(permissionsRequest{Service: "svc", Actions: []string{"read"}, Resources: []string{"res", ""}})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/permissions", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty resource string, got %d", w.Code)
	}
}

func TestCreatePermissions_InvalidBody(t *testing.T) {
	actor := createAuthorizedUser(t, "createPermissions", "gatekeeper/permissions")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/permissions", bytes.NewReader([]byte("not json"))), actor.UserID)
	w := httptest.NewRecorder()
	handleCreatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d", w.Code)
	}
}

// --- handleUpdatePermissions: missing service, empty actions/resources ---

func TestUpdatePermissions_MissingService(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc", Actions: []string{"read"}, Resources: []string{"res"}}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	b, _ := json.Marshal(permissionsRequest{Actions: []string{"read"}})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+perm.PermissionsID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing service, got %d", w.Code)
	}
}

func TestUpdatePermissions_EmptyActionRejected(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc", Actions: []string{"read"}, Resources: []string{"res"}}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	b, _ := json.Marshal(permissionsRequest{Service: "svc", Actions: []string{"read", ""}, Resources: []string{"res"}})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+perm.PermissionsID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty action string, got %d", w.Code)
	}
}

func TestUpdatePermissions_EmptyResourceRejected(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc", Actions: []string{"read"}, Resources: []string{"res"}}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	b, _ := json.Marshal(permissionsRequest{Service: "svc", Actions: []string{"read"}, Resources: []string{"res", ""}})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+perm.PermissionsID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty resource string, got %d", w.Code)
	}
}

func TestUpdatePermissions_InvalidBody(t *testing.T) {
	perm := Permissions{PermissionsID: uuid.New().String(), Service: "svc"}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+perm.PermissionsID)

	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+perm.PermissionsID, bytes.NewReader([]byte("not json"))), actor.UserID)
	r.SetPathValue("id", perm.PermissionsID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d", w.Code)
	}
}
