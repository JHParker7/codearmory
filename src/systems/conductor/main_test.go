package main

import (
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

// mockGatekeeper creates a test server that simulates Gatekeeper's
// POST /check_permissions endpoint. authorized controls whether it returns
// 200+{"authorized":true,"user_id":"test-user"} or 401.
func mockGatekeeper(t *testing.T, authorized bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check_permissions" && r.Method == http.MethodPost {
			if authorized {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"authorized": true, "user_id": "test-user"})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
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

// ── checkAuth ─────────────────────────────────────────────────────────────────

func TestCheckAuth_Allowed(t *testing.T) {
	gk := mockGatekeeper(t, true)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer sometoken")

	if got, _ := checkAuth(r, "testsvc", "read", "foo"); got != authAllowed {
		t.Fatalf("expected authAllowed, got %d", got)
	}
}

func TestCheckAuth_Unauthorized_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	// No Authorization header.
	if got, _ := checkAuth(r, "testsvc", "read", "foo"); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized, got %d", got)
	}
}

func TestCheckAuth_Unauthorized_GatekeeperRejects(t *testing.T) {
	gk := mockGatekeeper(t, false)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer badtoken")

	if got, _ := checkAuth(r, "testsvc", "read", "foo"); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized, got %d", got)
	}
}

func TestCheckAuth_Forbidden_NotAuthorized(t *testing.T) {
	gk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check_permissions" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"authorized": false})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer validtoken")

	if got, _ := checkAuth(r, "testsvc", "read", "foo"); got != authForbidden {
		t.Fatalf("expected authForbidden, got %d", got)
	}
}

// ── handleServiceProxy ────────────────────────────────────────────────────────

// mockBackend returns a test server that responds 200 OK.
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
	entry := makeEntry(http.MethodGet, "/things", "read", "thing")
	entry.public = true
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{}) // no service entry for "testsvc"

	req := httptest.NewRequest(http.MethodGet, "/things", nil)
	w := httptest.NewRecorder()
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

func TestHandleServiceProxy_PrivateEndpoint_GatekeeperAllows_Proxies(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	gk := mockGatekeeper(t, true)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	entry := makeEntry(http.MethodGet, "/profile", "read", "profile")
	entry.serviceName = "gatekeeper"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"gatekeeper": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.Header.Set("Authorization", "Bearer validtoken")
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when gatekeeper allows, got %d", w.Code)
	}
}

func TestHandleServiceProxy_PrivateEndpoint_GatekeeperDenies_Returns401(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	gk := mockGatekeeper(t, false)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	entry := makeEntry(http.MethodGet, "/admin", "admin", "resource")
	entry.serviceName = "testsvc"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"testsvc": {url: backend.URL, proxy: proxyTo(t, backend.URL)},
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer badtoken")
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when gatekeeper denies, got %d", w.Code)
	}
}

func TestHandleServiceProxy_ForwardAuth_PassesToken(t *testing.T) {
	var receivedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/check_permissions" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"authorized": true})
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

	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.Header.Set("Authorization", "Bearer mytoken")
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

	gk := mockGatekeeper(t, true)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	entry := makeEntry(http.MethodGet, "/data", "read", "data")
	entry.serviceName = "datasvc"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"datasvc": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: false},
	})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Authorization", "Bearer mytoken")
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if receivedAuth != "" {
		t.Fatalf("expected Authorization header to be stripped for forward_auth=false, got %q", receivedAuth)
	}
}

func TestHandleServiceProxy_SpoofHeaders_Stripped(t *testing.T) {
	var got struct {
		xUserID   string
		xFwdHost  string
		xRealIP   string
		xFwdFor   string
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.xUserID = r.Header.Get("X-User-ID")
		got.xFwdHost = r.Header.Get("X-Forwarded-Host")
		got.xRealIP = r.Header.Get("X-Real-IP")
		got.xFwdFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	entry := makeEntry(http.MethodGet, "/pub", "read", "pub")
	entry.public = true
	entry.serviceName = "testsvc"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"testsvc": {url: backend.URL, proxy: proxyTo(t, backend.URL)},
	})

	req := httptest.NewRequest(http.MethodGet, "/pub", nil)
	req.Header.Set("X-User-ID", "injected")
	req.Header.Set("X-Forwarded-Host", "evil.com")
	req.Header.Set("X-Real-IP", "1.2.3.4")
	req.Header.Set("X-Forwarded-For", "evil-chain")
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if got.xUserID != "" {
		t.Errorf("X-User-ID should be stripped, got %q", got.xUserID)
	}
	if got.xFwdHost != "" {
		t.Errorf("X-Forwarded-Host should be stripped, got %q", got.xFwdHost)
	}
	if got.xRealIP != "" {
		t.Errorf("X-Real-IP should be stripped, got %q", got.xRealIP)
	}
	// X-Forwarded-For should be overwritten with req.RemoteAddr, not the spoofed value.
	if got.xFwdFor == "evil-chain" {
		t.Error("X-Forwarded-For should not pass through spoofed value")
	}
}
