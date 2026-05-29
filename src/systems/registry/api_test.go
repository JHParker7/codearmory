package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// requireAuthWithRole — header-only checks (no DB required)
// ---------------------------------------------------------------------------

func TestRequireAuthWithRole_MissingHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if requireAuthWithRole(w, r, "") {
		t.Fatal("expected false when X-Service-Key header is absent")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireAuthWithRole_MalformedHeader_NoColon(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Service-Key", "nameonly") // no colon
	w := httptest.NewRecorder()
	if requireAuthWithRole(w, r, "") {
		t.Fatal("expected false for malformed header")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireAuthWithRole_MalformedHeader_EmptyName(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Service-Key", ":somekey") // name part is empty
	w := httptest.NewRecorder()
	if requireAuthWithRole(w, r, "") {
		t.Fatal("expected false for empty service name")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// DB-dependent auth success/failure is covered by the integration test suite.

// ---------------------------------------------------------------------------
// Pre-auth handler paths — pool is nil; only the auth rejection path runs
// ---------------------------------------------------------------------------

func TestHandleListServices_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/services", nil)
	// No X-Service-Key header → 401
	w := httptest.NewRecorder()
	handleListServices(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateService_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/services", nil)
	w := httptest.NewRecorder()
	handleCreateService(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleDeleteService_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/services/some-id", nil)
	w := httptest.NewRecorder()
	handleDeleteService(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// statusResponseWriter
// ---------------------------------------------------------------------------

func TestStatusResponseWriter_CapturesStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: rec, status: http.StatusOK}

	rw.WriteHeader(http.StatusCreated)

	if rw.status != http.StatusCreated {
		t.Fatalf("expected captured status %d, got %d", http.StatusCreated, rw.status)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected delegated status %d, got %d", http.StatusCreated, rec.Code)
	}
}

func TestStatusResponseWriter_DelegatesWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: rec, status: http.StatusOK}

	rw.WriteHeader(http.StatusNotFound)
	rw.Write([]byte("not found")) //nolint:errcheck

	if rw.status != http.StatusNotFound {
		t.Fatalf("expected captured status 404, got %d", rw.status)
	}
	if rec.Body.String() != "not found" {
		t.Fatalf("expected body 'not found', got %q", rec.Body.String())
	}
}
