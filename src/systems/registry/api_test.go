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
	if _, ok := requireAuthWithRole(w, r, ""); ok {
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
	if _, ok := requireAuthWithRole(w, r, ""); ok {
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
	if _, ok := requireAuthWithRole(w, r, ""); ok {
		t.Fatal("expected false for empty service name")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// DB-dependent auth success/failure is covered by the integration test suite.

// ---------------------------------------------------------------------------
// matchesSeedKey — bootstrap-key recovery fallback (no DB required)
// ---------------------------------------------------------------------------

func TestMatchesSeedKey_Match(t *testing.T) {
	seedServiceKeys = map[string]seedAccount{"conductor": {key: "boot-secret", role: "read"}}
	t.Cleanup(func() { seedServiceKeys = map[string]seedAccount{} })

	role, ok := matchesSeedKey("conductor", "boot-secret")
	if !ok {
		t.Fatal("expected bootstrap key to match seeded key")
	}
	if role != "read" {
		t.Fatalf("expected role %q, got %q", "read", role)
	}
}

func TestMatchesSeedKey_WrongKey(t *testing.T) {
	seedServiceKeys = map[string]seedAccount{"conductor": {key: "boot-secret", role: "read"}}
	t.Cleanup(func() { seedServiceKeys = map[string]seedAccount{} })

	if _, ok := matchesSeedKey("conductor", "rotated-or-bad-key"); ok {
		t.Fatal("expected non-matching key to be rejected")
	}
}

func TestMatchesSeedKey_UnknownAccount(t *testing.T) {
	seedServiceKeys = map[string]seedAccount{"conductor": {key: "boot-secret", role: "read"}}
	t.Cleanup(func() { seedServiceKeys = map[string]seedAccount{} })

	if _, ok := matchesSeedKey("ghost", "boot-secret"); ok {
		t.Fatal("expected unknown account to be rejected")
	}
}

func TestMatchesSeedKey_EmptySeedKeyNeverMatches(t *testing.T) {
	// A blank seeded key must never authenticate, even against a blank presented key.
	seedServiceKeys = map[string]seedAccount{"conductor": {key: "", role: "read"}}
	t.Cleanup(func() { seedServiceKeys = map[string]seedAccount{} })

	if _, ok := matchesSeedKey("conductor", ""); ok {
		t.Fatal("expected empty seeded key to never match")
	}
}

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
// Handlers that use requireReadAuth — missing X-Service-Key → 401
// ---------------------------------------------------------------------------

func TestHandleListActions_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/actions", nil)
	w := httptest.NewRecorder()
	handleListActions(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleListDefaultGrants_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/default-grants", nil)
	w := httptest.NewRecorder()
	handleListDefaultGrants(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleSystemHealth_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/system_health", nil)
	w := httptest.NewRecorder()
	handleSystemHealth(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleUpdateServiceEndpoints_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/services/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleUpdateServiceEndpoints(w, r)
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
