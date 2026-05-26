package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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

// withGatekeeperURL temporarily replaces the package-level gatekeeperURL and
// restores it when the test completes.
func withGatekeeperURL(t *testing.T, u string) {
	t.Helper()
	orig := gatekeeperURL
	gatekeeperURL = u
	t.Cleanup(func() { gatekeeperURL = orig })
}

// withEndpoints replaces the global endpointsList for the duration of the test.
func withEndpoints(t *testing.T, entries []endpointEntry) {
	t.Helper()
	endpointsMu.Lock()
	orig := endpointsList
	endpointsList = entries
	endpointsMu.Unlock()
	t.Cleanup(func() {
		endpointsMu.Lock()
		endpointsList = orig
		endpointsMu.Unlock()
	})
}

// withServices replaces the global servicesMap for the duration of the test.
func withServices(t *testing.T, services map[string]serviceState) {
	t.Helper()
	servicesMu.Lock()
	orig := servicesMap
	servicesMap = services
	servicesMu.Unlock()
	t.Cleanup(func() {
		servicesMu.Lock()
		servicesMap = orig
		servicesMu.Unlock()
	})
}

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

// ── lookupEndpoint ────────────────────────────────────────────────────────────

func makeEntry(method, pattern, action, resource string) endpointEntry {
	return endpointEntry{
		method:      method,
		pattern:     compilePathPattern(pattern),
		action:      action,
		resource:    resource,
		serviceName: "testsvc",
	}
}

func TestLookupEndpoint_MatchesRegistered(t *testing.T) {
	withEndpoints(t, []endpointEntry{
		makeEntry(http.MethodGet, "/executions/{id}", "read", "execution"),
	})
	entry, ok := lookupEndpoint(http.MethodGet, "/executions/abc-123")
	if !ok {
		t.Fatal("expected match for registered path")
	}
	if entry.action != "read" || entry.resource != "execution" {
		t.Fatalf("expected read/execution, got %s/%s", entry.action, entry.resource)
	}
}

func TestLookupEndpoint_NoMatchReturnsNotFound(t *testing.T) {
	withEndpoints(t, nil)
	_, ok := lookupEndpoint(http.MethodPost, "/blueprints/create")
	if ok {
		t.Fatal("expected no match for unregistered path")
	}
}

func TestLookupEndpoint_MethodMismatchReturnsNotFound(t *testing.T) {
	withEndpoints(t, []endpointEntry{
		makeEntry(http.MethodGet, "/executions/{id}", "read", "execution"),
	})
	_, ok := lookupEndpoint(http.MethodDelete, "/executions/abc-123")
	if ok {
		t.Fatal("expected no match when method does not match any registered entry")
	}
}

func TestLookupEndpoint_ReturnsPublicFlag(t *testing.T) {
	entry := makeEntry(http.MethodPost, "/signup", "signup", "auth")
	entry.public = true
	withEndpoints(t, []endpointEntry{entry})
	got, ok := lookupEndpoint(http.MethodPost, "/signup")
	if !ok {
		t.Fatal("expected match")
	}
	if !got.public {
		t.Fatal("expected public=true")
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

// ── handleServiceProxy ────────────────────────────────────────────────────────

// mockBackend returns a test server that records whether it was called and
// echoes back the request path.
func mockBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func proxyTo(t *testing.T, target string) *httputil.ReverseProxy {
	t.Helper()
	u, _ := url.Parse(target)
	return httputil.NewSingleHostReverseProxy(u)
}

func TestHandleServiceProxy_UnregisteredPath_Returns404(t *testing.T) {
	withEndpoints(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/unknown/path", nil)
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unregistered path, got %d", w.Code)
	}
}

func TestHandleServiceProxy_ServiceUnavailable_Returns503(t *testing.T) {
	withEndpoints(t, []endpointEntry{
		makeEntry(http.MethodGet, "/things", "read", "thing"),
	})
	withServices(t, map[string]serviceState{}) // no service entry for "testsvc"

	req := httptest.NewRequest(http.MethodGet, "/things", nil)
	req.Header.Set("Authorization", "Bearer "+testToken(testUUID))
	w := httptest.NewRecorder()
	// userMiddleware would normally run; bypass it by making endpoint public for this test
	entry := makeEntry(http.MethodGet, "/things", "read", "thing")
	entry.public = true
	withEndpoints(t, []endpointEntry{entry})
	handleServiceProxy(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when service not in map, got %d", w.Code)
	}
}

func TestHandleServiceProxy_PublicEndpoint_NoAuthRequired(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	entry := makeEntry(http.MethodPost, "/signup", "signup", "auth")
	entry.public = true
	entry.serviceName = "gatekeeper"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"gatekeeper": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: true},
	})

	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected public endpoint to forward without auth, got %d", w.Code)
	}
}

func TestHandleServiceProxy_PrivateEndpoint_NoToken_Returns401(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	entry := makeEntry(http.MethodGet, "/users", "read", "user")
	entry.public = false
	entry.serviceName = "gatekeeper"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"gatekeeper": {url: backend.URL, proxy: proxyTo(t, backend.URL)},
	})

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for private endpoint with no token, got %d", w.Code)
	}
}

func TestHandleServiceProxy_PrivateEndpoint_MalformedToken_Returns401(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	entry := makeEntry(http.MethodGet, "/users", "read", "user")
	entry.serviceName = "gatekeeper"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"gatekeeper": {url: backend.URL, proxy: proxyTo(t, backend.URL)},
	})

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.uuid.token")
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for malformed token, got %d", w.Code)
	}
}

func TestHandleServiceProxy_ForwardAuth_PassesToken(t *testing.T) {
	var receivedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		// Respond to check_permissions
		if r.URL.Path == "/check_permissions" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"authorized": true})
			return
		}
		// Respond to user existence check
		if strings.HasPrefix(r.URL.Path, "/users/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	withGatekeeperURL(t, backend.URL)

	entry := makeEntry(http.MethodGet, "/profile", "read", "profile")
	entry.serviceName = "gatekeeper"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"gatekeeper": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: true},
	})

	token := testToken(testUUID)
	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if receivedAuth == "" {
		t.Fatal("expected Authorization header to be forwarded to service with forward_auth=true")
	}
}

func TestHandleServiceProxy_NoForwardAuth_StripsToken(t *testing.T) {
	var receivedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	gk := mockGatekeeper(t, http.StatusOK)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	// Extend the mock gatekeeper to handle check_permissions
	gkFull := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check_permissions" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"authorized": true})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/users/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gkFull.Close()
	withGatekeeperURL(t, gkFull.URL)

	entry := makeEntry(http.MethodGet, "/data", "read", "data")
	entry.serviceName = "datasvc"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"datasvc": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: false},
	})

	token := testToken(testUUID)
	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if receivedAuth != "" {
		t.Fatalf("expected Authorization header to be stripped for forward_auth=false, got %q", receivedAuth)
	}
}
