package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── handleGetState ────────────────────────────────────────────────────────────

func TestHandleGetState_MissingAuth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)

	handleGetState(w, r, "user/workspace")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetState_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusForbidden, ``)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer testtoken")

	handleGetState(w, r, "user/workspace")

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// ── handleUpdateState ─────────────────────────────────────────────────────────

func TestHandleUpdateState_MissingAuth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"some":"state"}`))

	handleUpdateState(w, r, "user/workspace")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleUpdateState_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusForbidden, ``)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"some":"state"}`))
	r.Header.Set("Authorization", "Bearer testtoken")

	handleUpdateState(w, r, "user/workspace")

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// ── handleDeleteState ─────────────────────────────────────────────────────────

func TestHandleDeleteState_MissingAuth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/", nil)

	handleDeleteState(w, r, "user/workspace")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteState_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusForbidden, ``)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/", nil)
	r.Header.Set("Authorization", "Bearer testtoken")

	handleDeleteState(w, r, "user/workspace")

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// ── handleLockState ───────────────────────────────────────────────────────────

func TestHandleLockState_MissingAuth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("LOCK", "/", strings.NewReader(`{"ID":"lock-1"}`))

	handleLockState(w, r, "user/workspace")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleLockState_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusForbidden, ``)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("LOCK", "/", strings.NewReader(`{"ID":"lock-1"}`))
	r.Header.Set("Authorization", "Bearer testtoken")

	handleLockState(w, r, "user/workspace")

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

// ── handleUnlockState ─────────────────────────────────────────────────────────

func TestHandleUnlockState_MissingAuth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("UNLOCK", "/", strings.NewReader(`{"ID":"lock-1"}`))

	handleUnlockState(w, r, "user/workspace")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleUnlockState_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusForbidden, ``)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("UNLOCK", "/", strings.NewReader(`{"ID":"lock-1"}`))
	r.Header.Set("Authorization", "Bearer testtoken")

	handleUnlockState(w, r, "user/workspace")

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}
