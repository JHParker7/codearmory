package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// checkKey
// ---------------------------------------------------------------------------

func TestCheckKey_EmptyExpected(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	if checkKey(r, "") {
		t.Fatal("expected false when expected key is empty")
	}
}

func TestCheckKey_NoBearer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "sometoken") // no "Bearer " prefix
	if checkKey(r, "sometoken") {
		t.Fatal("expected false when Authorization header lacks 'Bearer ' prefix")
	}
}

func TestCheckKey_WrongToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer wrongtoken")
	if checkKey(r, "correcttoken") {
		t.Fatal("expected false when token does not match expected")
	}
}

func TestCheckKey_CorrectToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer secretkey")
	if !checkKey(r, "secretkey") {
		t.Fatal("expected true when token matches expected")
	}
}

// ---------------------------------------------------------------------------
// requireAdminKey
// ---------------------------------------------------------------------------

func TestRequireAdminKey_NoKeySet(t *testing.T) {
	// ADMIN_KEY not set → empty string → checkKey returns false → 401
	t.Setenv("ADMIN_KEY", "")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	if requireAdminKey(w, r) {
		t.Fatal("expected false (unauthorized) when ADMIN_KEY is not set")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireAdminKey_WrongKey(t *testing.T) {
	t.Setenv("ADMIN_KEY", "correctadminkey")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer wrongkey")
	w := httptest.NewRecorder()
	if requireAdminKey(w, r) {
		t.Fatal("expected false (unauthorized) when key does not match")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireAdminKey_CorrectKey(t *testing.T) {
	t.Setenv("ADMIN_KEY", "correctadminkey")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer correctadminkey")
	w := httptest.NewRecorder()
	if !requireAdminKey(w, r) {
		t.Fatal("expected true when ADMIN_KEY matches")
	}
	// No response body should have been written (status stays 200 default, body empty)
	if body := w.Body.String(); body != "" {
		t.Fatalf("expected no body on success, got %q", body)
	}
}

// ---------------------------------------------------------------------------
// requireReadKey
// ---------------------------------------------------------------------------

func TestRequireReadKey_NeitherKeySet(t *testing.T) {
	t.Setenv("READ_KEY", "")
	t.Setenv("ADMIN_KEY", "")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	if requireReadKey(w, r) {
		t.Fatal("expected false when neither READ_KEY nor ADMIN_KEY is set")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireReadKey_WithReadKey(t *testing.T) {
	t.Setenv("READ_KEY", "myreadkey")
	t.Setenv("ADMIN_KEY", "")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer myreadkey")
	w := httptest.NewRecorder()
	if !requireReadKey(w, r) {
		t.Fatal("expected true when READ_KEY matches")
	}
}

func TestRequireReadKey_WithAdminKey(t *testing.T) {
	t.Setenv("READ_KEY", "")
	t.Setenv("ADMIN_KEY", "myadminkey")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer myadminkey")
	w := httptest.NewRecorder()
	if !requireReadKey(w, r) {
		t.Fatal("expected true when ADMIN_KEY matches (admin can read)")
	}
}

func TestRequireReadKey_BothSet_ReadMatches(t *testing.T) {
	t.Setenv("READ_KEY", "myreadkey")
	t.Setenv("ADMIN_KEY", "myadminkey")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer myreadkey")
	w := httptest.NewRecorder()
	if !requireReadKey(w, r) {
		t.Fatal("expected true when READ_KEY matches (both keys set)")
	}
}

func TestRequireReadKey_BothSet_AdminMatches(t *testing.T) {
	t.Setenv("READ_KEY", "myreadkey")
	t.Setenv("ADMIN_KEY", "myadminkey")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer myadminkey")
	w := httptest.NewRecorder()
	if !requireReadKey(w, r) {
		t.Fatal("expected true when ADMIN_KEY matches (both keys set)")
	}
}

func TestRequireReadKey_BothSet_NeitherMatches(t *testing.T) {
	t.Setenv("READ_KEY", "myreadkey")
	t.Setenv("ADMIN_KEY", "myadminkey")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer wrongkey")
	w := httptest.NewRecorder()
	if requireReadKey(w, r) {
		t.Fatal("expected false when neither key matches")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Pre-auth handler paths — pool is nil, only test auth rejection (no valid key)
// ---------------------------------------------------------------------------

func TestHandleListServices_Unauthorized(t *testing.T) {
	t.Setenv("READ_KEY", "readkey")
	t.Setenv("ADMIN_KEY", "adminkey")
	r := httptest.NewRequest(http.MethodGet, "/services", nil)
	// No Authorization header → 401
	w := httptest.NewRecorder()
	handleListServices(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateService_Unauthorized(t *testing.T) {
	t.Setenv("ADMIN_KEY", "adminkey")
	r := httptest.NewRequest(http.MethodPost, "/services", nil)
	// No Authorization header → 401
	w := httptest.NewRecorder()
	handleCreateService(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleDeleteService_Unauthorized(t *testing.T) {
	t.Setenv("ADMIN_KEY", "adminkey")
	r := httptest.NewRequest(http.MethodDelete, "/services/some-id", nil)
	// No Authorization header → 401
	w := httptest.NewRecorder()
	handleDeleteService(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// handleServiceSelfRegister decodes the JSON body first (before any DB access),
// so we can test the 400 path without a database pool.

func TestHandleServiceSelfRegister_MissingName(t *testing.T) {
	body := `{"service_key":"somekey","url":"http://localhost:9000"}`
	r := httptest.NewRequest(http.MethodPost, "/services/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleServiceSelfRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when name is missing, got %d", w.Code)
	}
}

func TestHandleServiceSelfRegister_MissingServiceKey(t *testing.T) {
	body := `{"name":"mysvc","url":"http://localhost:9000"}`
	r := httptest.NewRequest(http.MethodPost, "/services/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleServiceSelfRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when service_key is missing, got %d", w.Code)
	}
}

func TestHandleServiceSelfRegister_EmptyBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/services/register", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleServiceSelfRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty JSON body, got %d", w.Code)
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
	rw.Write([]byte("not found"))

	if rw.status != http.StatusNotFound {
		t.Fatalf("expected captured status 404, got %d", rw.status)
	}
	if rec.Body.String() != "not found" {
		t.Fatalf("expected body 'not found', got %q", rec.Body.String())
	}
}
