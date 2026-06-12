package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func makeExpiry() time.Time {
	return time.Now().Add(24 * time.Hour)
}

// createOwnerWithSession creates a test user, a session owned by that user,
// and grants the user the given action on their own session. The resource is
// stored with the user's username prefix to match checkPermissions' scoping.
func createOwnerWithSession(t *testing.T, action string) (User, Session) {
	t.Helper()
	u := createTestUser(t)
	_, session := makeSession(t, u.UserID, makeExpiry())
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "gatekeeper",
		Actions:       []string{action},
		Resources:     []string{u.Username + "/gatekeeper/sessions/" + session.SessionID},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatalf("createOwnerWithSession perm: %v", err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })
	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatalf("createOwnerWithSession role: %v", err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })
	connect().WithContext(context.Background()).Model(&User{}).
		Where("user_id = ?", u.UserID).Update("role_id", role.RoleID) //nolint:errcheck
	return u, session
}

func TestGetSession_Success(t *testing.T) {
	u, session := createOwnerWithSession(t, "getSession")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/sessions/"+session.SessionID, nil), u.UserID)
	r.SetPathValue("id", session.SessionID)
	w := httptest.NewRecorder()
	handleGetSession(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp sessionResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SessionID != session.SessionID {
		t.Fatalf("got session_id %s, want %s", resp.SessionID, session.SessionID)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "getSession", "gatekeeper/sessions/"+id)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetSession(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetSession_Forbidden(t *testing.T) {
	u := createTestUser(t)
	_, session := makeSession(t, u.UserID, makeExpiry())
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/sessions/"+session.SessionID, nil), actor.UserID)
	r.SetPathValue("id", session.SessionID)
	w := httptest.NewRecorder()
	handleGetSession(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeleteSession_Success(t *testing.T) {
	u, session := createOwnerWithSession(t, "deleteSession")

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/sessions/"+session.SessionID, nil), u.UserID)
	r.SetPathValue("id", session.SessionID)
	w := httptest.NewRecorder()
	handleDeleteSession(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (Session{SessionID: session.SessionID}).Get(context.Background()); err == nil {
		t.Fatal("expected session to be inaccessible after delete")
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "deleteSession", "gatekeeper/sessions/"+id)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/sessions/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteSession(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteSession_Forbidden(t *testing.T) {
	u := createTestUser(t)
	_, session := makeSession(t, u.UserID, makeExpiry())
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/sessions/"+session.SessionID, nil), actor.UserID)
	r.SetPathValue("id", session.SessionID)
	w := httptest.NewRecorder()
	handleDeleteSession(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- toSessionResponse ---

func TestGetSession_ResponseOmitsJWTAndPubKey(t *testing.T) {
	u, session := createOwnerWithSession(t, "getSession")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/sessions/"+session.SessionID, nil), u.UserID)
	r.SetPathValue("id", session.SessionID)
	w := httptest.NewRecorder()
	handleGetSession(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := raw["jwt"]; ok {
		t.Fatal("response must not include jwt")
	}
	if _, ok := raw["pub_key"]; ok {
		t.Fatal("response must not include pub_key")
	}
	// session_id must be present.
	if _, ok := raw["session_id"]; !ok {
		t.Fatal("response must include session_id")
	}
}
