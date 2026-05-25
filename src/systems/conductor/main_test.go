package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ── TestMain ──────────────────────────────────────────────────────────────────

func TestMain(m *testing.M) {
	initMetrics()
	os.Exit(m.Run())
}

// ── helpers ───────────────────────────────────────────────────────────────────

// testToken builds a syntactically valid 3-part JWT whose payload contains
// only the sub claim set to userID. The signature part is a fake value.
func testToken(userID string) string {
	payload, _ := json.Marshal(map[string]string{"sub": userID})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return "eyJhbGciOiJFUzI1NiJ9." + enc + ".fakesig"
}

const testUUID = "550e8400-e29b-41d4-a716-446655440000"

// ── envOrDefault ──────────────────────────────────────────────────────────────

func TestEnvOrDefault_ReturnsEnvWhenSet(t *testing.T) {
	t.Setenv("TEST_KEY_ENVODEFAULT", "hello")
	got := envOrDefault("TEST_KEY_ENVODEFAULT", "fallback")
	if got != "hello" {
		t.Fatalf("expected %q, got %q", "hello", got)
	}
}

func TestEnvOrDefault_ReturnsDefaultWhenNotSet(t *testing.T) {
	os.Unsetenv("TEST_KEY_MISSING_XYZ")
	got := envOrDefault("TEST_KEY_MISSING_XYZ", "fallback")
	if got != "fallback" {
		t.Fatalf("expected %q, got %q", "fallback", got)
	}
}

func TestEnvOrDefault_ReturnsDefaultWhenEmpty(t *testing.T) {
	t.Setenv("TEST_KEY_EMPTY_XYZ", "")
	got := envOrDefault("TEST_KEY_EMPTY_XYZ", "fallback")
	if got != "fallback" {
		t.Fatalf("expected %q, got %q", "fallback", got)
	}
}

// ── readAndRestore ────────────────────────────────────────────────────────────

func TestReadAndRestore_SmallBody(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	w := httptest.NewRecorder()

	data, ok := readAndRestore(w, req)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("returned data mismatch: got %q, want %q", data, body)
	}

	// Body must be restored so it can be read again.
	var buf bytes.Buffer
	buf.ReadFrom(req.Body)
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatalf("restored body mismatch: got %q, want %q", buf.Bytes(), body)
	}
}

func TestReadAndRestore_BodyTooLarge(t *testing.T) {
	// Create a body that is larger than bodyMax (64 KB).
	huge := bytes.Repeat([]byte("x"), bodyMax+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(huge))
	w := httptest.NewRecorder()

	data, ok := readAndRestore(w, req)
	if ok {
		t.Fatal("expected ok=false for oversized body")
	}
	if data != nil {
		t.Fatal("expected nil data for oversized body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

// ── validateSignupBody ────────────────────────────────────────────────────────

func signupRequest(t *testing.T, body map[string]string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	return httptest.NewRecorder(), req
}

func TestValidateSignupBody_Valid(t *testing.T) {
	w, r := signupRequest(t, map[string]string{
		"email":    "user@example.com",
		"username": "valid_user",
		"password": "securepass",
	})
	if !validateSignupBody(w, r) {
		t.Fatalf("expected true, got false (body: %s)", w.Body.String())
	}
}

func TestValidateSignupBody_MissingEmail(t *testing.T) {
	w, r := signupRequest(t, map[string]string{
		"username": "valid_user",
		"password": "securepass",
	})
	if validateSignupBody(w, r) {
		t.Fatal("expected false when email is missing")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidateSignupBody_BadEmail(t *testing.T) {
	w, r := signupRequest(t, map[string]string{
		"email":    "notanemail",
		"username": "valid_user",
		"password": "securepass",
	})
	if validateSignupBody(w, r) {
		t.Fatal("expected false for bad email format")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidateSignupBody_BadUsername(t *testing.T) {
	w, r := signupRequest(t, map[string]string{
		"email":    "user@example.com",
		"username": "bad username!", // spaces and ! are invalid
		"password": "securepass",
	})
	if validateSignupBody(w, r) {
		t.Fatal("expected false for bad username")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidateSignupBody_ShortPassword(t *testing.T) {
	w, r := signupRequest(t, map[string]string{
		"email":    "user@example.com",
		"username": "valid_user",
		"password": "short",
	})
	if validateSignupBody(w, r) {
		t.Fatal("expected false for short password")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidateSignupBody_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader("not-json"))
	w := httptest.NewRecorder()
	if validateSignupBody(w, req) {
		t.Fatal("expected false for invalid JSON")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── validateLoginBody ─────────────────────────────────────────────────────────

func loginRequest(t *testing.T, body map[string]string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b))
	return httptest.NewRecorder(), req
}

func TestValidateLoginBody_Valid(t *testing.T) {
	w, r := loginRequest(t, map[string]string{
		"email":    "user@example.com",
		"password": "anypass",
	})
	if !validateLoginBody(w, r) {
		t.Fatalf("expected true, got false (body: %s)", w.Body.String())
	}
}

func TestValidateLoginBody_MissingEmail(t *testing.T) {
	w, r := loginRequest(t, map[string]string{"password": "anypass"})
	if validateLoginBody(w, r) {
		t.Fatal("expected false when email is missing")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidateLoginBody_MissingPassword(t *testing.T) {
	w, r := loginRequest(t, map[string]string{"email": "user@example.com"})
	if validateLoginBody(w, r) {
		t.Fatal("expected false when password is missing")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidateLoginBody_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	if validateLoginBody(w, req) {
		t.Fatal("expected false for invalid JSON")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── getUserID ─────────────────────────────────────────────────────────────────

func TestGetUserID_ValidToken(t *testing.T) {
	token := testToken(testUUID)
	id, ok := getUserID(token)
	if !ok {
		t.Fatal("expected ok=true for valid token")
	}
	if id != testUUID {
		t.Fatalf("expected %q, got %q", testUUID, id)
	}
}

func TestGetUserID_NonThreePartToken(t *testing.T) {
	_, ok := getUserID("only.twoparts")
	if ok {
		t.Fatal("expected ok=false for non-3-part token")
	}
}

func TestGetUserID_BadBase64(t *testing.T) {
	_, ok := getUserID("header.!!!invalid!!!.sig")
	if ok {
		t.Fatal("expected ok=false for bad base64")
	}
}

func TestGetUserID_MissingSub(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"foo": "bar"})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	token := "header." + enc + ".sig"
	_, ok := getUserID(token)
	if ok {
		t.Fatal("expected ok=false when sub is missing")
	}
}

func TestGetUserID_EmptySub(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"sub": ""})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	token := "header." + enc + ".sig"
	_, ok := getUserID(token)
	if ok {
		t.Fatal("expected ok=false when sub is empty")
	}
}

func TestGetUserID_NonUUIDSub(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"sub": "not-a-uuid"})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	token := "header." + enc + ".sig"
	_, ok := getUserID(token)
	if ok {
		t.Fatal("expected ok=false when sub is not a UUID")
	}
}

// ── methodToAction ────────────────────────────────────────────────────────────

func TestMethodToAction(t *testing.T) {
	cases := []struct {
		method string
		want   string
	}{
		{http.MethodGet, "read"},
		{http.MethodHead, "read"},
		{http.MethodDelete, "delete"},
		{http.MethodPost, "write"},
		{http.MethodPut, "write"},
		{http.MethodPatch, "write"},
	}
	for _, tc := range cases {
		got := methodToAction(tc.method)
		if got != tc.want {
			t.Errorf("methodToAction(%q) = %q, want %q", tc.method, got, tc.want)
		}
	}
}

// ── compilePathPattern ────────────────────────────────────────────────────────

func TestCompilePathPattern_MatchesConcreteSegment(t *testing.T) {
	re := compilePathPattern("/executions/{id}")
	if !re.MatchString("/executions/abc-123") {
		t.Error("pattern should match /executions/abc-123")
	}
}

func TestCompilePathPattern_DoesNotMatchMultipleSegments(t *testing.T) {
	re := compilePathPattern("/executions/{id}")
	if re.MatchString("/executions/a/b") {
		t.Error("pattern should not match /executions/a/b")
	}
}

func TestCompilePathPattern_NoParams(t *testing.T) {
	re := compilePathPattern("/health")
	if !re.MatchString("/health") {
		t.Error("pattern should match /health")
	}
	if re.MatchString("/health/extra") {
		t.Error("pattern should not match /health/extra")
	}
}

// ── resolveEndpoint ───────────────────────────────────────────────────────────

func makeEntry(method, pattern, action, resource string) endpointEntry {
	return endpointEntry{
		method:   method,
		pattern:  compilePathPattern(pattern),
		action:   action,
		resource: resource,
	}
}

func TestResolveEndpoint_MatchesRegistered(t *testing.T) {
	entries := []endpointEntry{
		makeEntry(http.MethodGet, "/executions/{id}", "read", "execution"),
	}
	action, resource := resolveEndpoint(entries, http.MethodGet, "/executions/abc-123")
	if action != "read" || resource != "execution" {
		t.Fatalf("expected read/execution, got %s/%s", action, resource)
	}
}

func TestResolveEndpoint_FallbackFirstSegment(t *testing.T) {
	action, resource := resolveEndpoint(nil, http.MethodPost, "/blueprints/create")
	if action != "write" {
		t.Fatalf("expected write, got %s", action)
	}
	if resource != "blueprints" {
		t.Fatalf("expected blueprints, got %s", resource)
	}
}

func TestResolveEndpoint_FallbackRootPath(t *testing.T) {
	action, resource := resolveEndpoint(nil, http.MethodGet, "/")
	if action != "read" {
		t.Fatalf("expected read, got %s", action)
	}
	if resource != "*" {
		t.Fatalf("expected *, got %s", resource)
	}
}

func TestResolveEndpoint_MethodMismatchFallsBack(t *testing.T) {
	entries := []endpointEntry{
		makeEntry(http.MethodGet, "/executions/{id}", "read", "execution"),
	}
	action, resource := resolveEndpoint(entries, http.MethodDelete, "/executions/abc-123")
	if action != "delete" {
		t.Fatalf("expected delete, got %s", action)
	}
	if resource != "executions" {
		t.Fatalf("expected executions, got %s", resource)
	}
}

// ── validUUID ─────────────────────────────────────────────────────────────────

func TestValidUUID_Valid(t *testing.T) {
	check := validUUID("id")
	mux := http.NewServeMux()
	mux.HandleFunc("/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/items/"+testUUID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid UUID, got %d", w.Code)
	}
}

func TestValidUUID_Invalid(t *testing.T) {
	check := validUUID("id")
	mux := http.NewServeMux()
	mux.HandleFunc("/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/items/not-a-uuid", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d", w.Code)
	}
}

// ── validJSON ─────────────────────────────────────────────────────────────────

func TestValidJSON_GetPassesThrough(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if !validJSON(w, req) {
		t.Fatal("GET should pass through validJSON")
	}
}

func TestValidJSON_DeletePassesThrough(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	if !validJSON(w, req) {
		t.Fatal("DELETE should pass through validJSON")
	}
}

func TestValidJSON_HeadPassesThrough(t *testing.T) {
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	w := httptest.NewRecorder()
	if !validJSON(w, req) {
		t.Fatal("HEAD should pass through validJSON")
	}
}

func TestValidJSON_PostEmptyBodyPasses(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	w := httptest.NewRecorder()
	if !validJSON(w, req) {
		t.Fatal("POST with empty body should pass validJSON")
	}
}

func TestValidJSON_PostValidJSONPasses(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"key":"val"}`))
	w := httptest.NewRecorder()
	if !validJSON(w, req) {
		t.Fatal("POST with valid JSON should pass")
	}
}

func TestValidJSON_PostInvalidJSONRejects(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{bad json`))
	w := httptest.NewRecorder()
	if validJSON(w, req) {
		t.Fatal("POST with invalid JSON should fail")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidJSON_PutInvalidJSONRejects(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`notjson`))
	w := httptest.NewRecorder()
	if validJSON(w, req) {
		t.Fatal("PUT with invalid JSON should fail")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidJSON_PatchInvalidJSONRejects(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`[`))
	w := httptest.NewRecorder()
	if validJSON(w, req) {
		t.Fatal("PATCH with invalid JSON should fail")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── statusResponseWriter ──────────────────────────────────────────────────────

func TestStatusResponseWriter_CapturesStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: rec, status: http.StatusOK}
	rw.WriteHeader(http.StatusTeapot)

	if rw.status != http.StatusTeapot {
		t.Fatalf("expected rw.status=%d, got %d", http.StatusTeapot, rw.status)
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected underlying recorder status=%d, got %d", http.StatusTeapot, rec.Code)
	}
}

// ── userMiddleware ────────────────────────────────────────────────────────────

// mockGatekeeper creates a test server that responds with statusCode for
// GET /users/:id requests.
func mockGatekeeper(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/") {
			w.WriteHeader(statusCode)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// withGatekeeperURL temporarily replaces the package-level gatekeeperURL and
// restores it when the test completes.
func withGatekeeperURL(t *testing.T, u string) {
	t.Helper()
	orig := gatekeeperURL
	gatekeeperURL = u
	t.Cleanup(func() { gatekeeperURL = orig })
}

func TestUserMiddleware_NoAuthHeader(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusOK)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := userMiddleware(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestUserMiddleware_NonBearerToken(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusOK)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := userMiddleware(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestUserMiddleware_MalformedJWT(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusOK)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := userMiddleware(next)

	// Build a token whose sub is not a UUID.
	payload, _ := json.Marshal(map[string]string{"sub": "not-a-uuid"})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	token := "eyJhbGciOiJFUzI1NiJ9." + enc + ".fakesig"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestUserMiddleware_ValidJWT_GatekeeperOK(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusOK)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})
	handler := userMiddleware(next)

	token := testToken(testUUID)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !nextCalled {
		t.Fatal("expected next handler to be called")
	}
}

func TestUserMiddleware_ValidJWT_GatekeeperNotFound(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusNotFound)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := userMiddleware(next)

	token := testToken(testUUID)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ── checkUserExists ───────────────────────────────────────────────────────────

func TestCheckUserExists_Returns200(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusOK)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	token := testToken(testUUID)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	if !checkUserExists(ctx, token, testUUID) {
		t.Fatal("expected checkUserExists=true when gatekeeper returns 200")
	}
}

func TestCheckUserExists_Returns404(t *testing.T) {
	srv := mockGatekeeper(t, http.StatusNotFound)
	defer srv.Close()
	withGatekeeperURL(t, srv.URL)

	token := testToken(testUUID)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	if checkUserExists(ctx, token, testUUID) {
		t.Fatal("expected checkUserExists=false when gatekeeper returns 404")
	}
}
