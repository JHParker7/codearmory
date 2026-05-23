package main

import (
	"context"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCreateRole_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "createRole", "gatekeeper/roles")

	b, _ := json.Marshal(roleRequest{PermissionsIDs: []string{"p-1"}})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/roles", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateRole(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Role
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.RoleID == "" {
		t.Fatal("expected non-empty role_id in response")
	}
}

func TestCreateRole_Forbidden(t *testing.T) {
	actor := createTestUser(t)

	b, _ := json.Marshal(roleRequest{})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/roles", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateRole(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestGetRole_Success(t *testing.T) {
	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-1"}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "getRole", "gatekeeper/roles/"+role.RoleID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/roles/"+role.RoleID, nil), actor.UserID)
	r.SetPathValue("id", role.RoleID)
	w := httptest.NewRecorder()
	handleGetRole(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Role
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RoleID != role.RoleID {
		t.Fatalf("got role_id %s, want %s", resp.RoleID, role.RoleID)
	}
}

func TestGetRole_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "getRole", "gatekeeper/roles/"+id)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/roles/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetRole(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetRole_Forbidden(t *testing.T) {
	role := Role{RoleID: uuid.New().String()}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/roles/"+role.RoleID, nil), actor.UserID)
	r.SetPathValue("id", role.RoleID)
	w := httptest.NewRecorder()
	handleGetRole(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestUpdateRole_Success(t *testing.T) {
	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-1"}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updateRole", "gatekeeper/roles/"+role.RoleID)

	b, _ := json.Marshal(roleRequest{PermissionsIDs: []string{"p-1", "p-2"}})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/roles/"+role.RoleID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", role.RoleID)
	w := httptest.NewRecorder()
	handleUpdateRole(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp Role
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.PermissionsIDs) != 2 {
		t.Fatalf("expected 2 permissions_ids, got %d", len(resp.PermissionsIDs))
	}
}

func TestUpdateRole_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "updateRole", "gatekeeper/roles/"+id)

	b, _ := json.Marshal(roleRequest{})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/roles/"+id, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleUpdateRole(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateRole_Forbidden(t *testing.T) {
	role := Role{RoleID: uuid.New().String()}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	actor := createTestUser(t)

	b, _ := json.Marshal(roleRequest{})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/roles/"+role.RoleID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", role.RoleID)
	w := httptest.NewRecorder()
	handleUpdateRole(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeleteRole_Success(t *testing.T) {
	role := Role{RoleID: uuid.New().String()}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "deleteRole", "gatekeeper/roles/"+role.RoleID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/roles/"+role.RoleID, nil), actor.UserID)
	r.SetPathValue("id", role.RoleID)
	w := httptest.NewRecorder()
	handleDeleteRole(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (Role{RoleID: role.RoleID}).Get(context.Background()); err == nil {
		t.Fatal("expected role to be inaccessible after delete")
	}
}

func TestDeleteRole_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "deleteRole", "gatekeeper/roles/"+id)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/roles/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteRole(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteRole_Forbidden(t *testing.T) {
	role := Role{RoleID: uuid.New().String()}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/roles/"+role.RoleID, nil), actor.UserID)
	r.SetPathValue("id", role.RoleID)
	w := httptest.NewRecorder()
	handleDeleteRole(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}
