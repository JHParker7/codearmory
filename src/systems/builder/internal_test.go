package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func withInternalKey(t *testing.T, key string) {
	t.Helper()
	orig := builderInternalKey
	builderInternalKey = key
	t.Cleanup(func() { builderInternalKey = orig })
}

func TestRequireInternalKey_Unset(t *testing.T) {
	withInternalKey(t, "")
	r := httptest.NewRequest(http.MethodGet, "/internal/org-services/effective", nil)
	r.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	if requireInternalKey(w, r) {
		t.Fatal("expected false when the internal key is unset")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestRequireInternalKey_WrongToken(t *testing.T) {
	withInternalKey(t, "secret-key")
	r := httptest.NewRequest(http.MethodGet, "/internal/org-services/effective", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	if requireInternalKey(w, r) {
		t.Fatal("expected false for a wrong token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestRequireInternalKey_Correct(t *testing.T) {
	withInternalKey(t, "secret-key")
	r := httptest.NewRequest(http.MethodGet, "/internal/org-services/effective", nil)
	r.Header.Set("Authorization", "Bearer secret-key")
	w := httptest.NewRecorder()
	if !requireInternalKey(w, r) {
		t.Fatalf("expected true for the correct token (got %d)", w.Code)
	}
}

func TestHandleInternalEffective_Unauthorized(t *testing.T) {
	withInternalKey(t, "secret-key")
	r := httptest.NewRequest(http.MethodGet, "/internal/org-services/effective?org_id=o1", nil)
	r.Header.Set("Authorization", "Bearer nope")
	w := httptest.NewRecorder()
	handleInternalEffective(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}
