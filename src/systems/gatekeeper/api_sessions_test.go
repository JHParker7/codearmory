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

func TestGetSession_Success(t *testing.T) {
	u := createTestUser(t)
	_, session := makeSession(t, u.UserID, makeExpiry())
	actor := createAuthorizedUser(t, "getSession", "gatekeeper/sessions/"+session.SessionID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/sessions/"+session.SessionID, nil), actor.UserID)
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
	u := createTestUser(t)
	_, session := makeSession(t, u.UserID, makeExpiry())
	actor := createAuthorizedUser(t, "deleteSession", "gatekeeper/sessions/"+session.SessionID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/sessions/"+session.SessionID, nil), actor.UserID)
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
