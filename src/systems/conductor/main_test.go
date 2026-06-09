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

const testUUID = "550e8400-e29b-41d4-a716-446655440000"

// withGatekeeperURL temporarily replaces the package-level gatekeeperURL and
// gatekeeperClient, then restores them when the test completes.
func withGatekeeperURL(t *testing.T, u string) {
	t.Helper()
	origURL := gatekeeperURL
	origClient := gatekeeperClient
	gatekeeperURL = u
	gatekeeperClient = &http.Client{}
	t.Cleanup(func() {
		gatekeeperURL = origURL
		gatekeeperClient = origClient
	})
}

// withEndpoints replaces the global endpointsList for the duration of the test.
func withEndpoints(t *testing.T, entries []endpointEntry) {
	t.Helper()
	routingMu.Lock()
	orig := endpointsList
	endpointsList = entries
	routingMu.Unlock()
	t.Cleanup(func() {
		routingMu.Lock()
		endpointsList = orig
		routingMu.Unlock()
	})
}

// withServices replaces the global servicesMap for the duration of the test.
func withServices(t *testing.T, services map[string]serviceState) {
	t.Helper()
	routingMu.Lock()
	orig := servicesMap
	servicesMap = services
	routingMu.Unlock()
	t.Cleanup(func() {
		routingMu.Lock()
		servicesMap = orig
		routingMu.Unlock()
	})
}

// makeTestJWT returns a minimal JWT-shaped token whose payload contains sub=id.
// The header and signature are stubs; only the payload is meaningful for tests.
func makeTestJWT(id string) string {
	payload, _ := json.Marshal(map[string]string{"sub": id})
	return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// mockGatekeeper creates a test server that simulates Gatekeeper's
// GET /users/{id} endpoint. authorized controls whether it returns 200 or 401.
func mockGatekeeper(t *testing.T, authorized bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/") && r.Method == http.MethodGet {
			if authorized {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"user_id": testUUID})
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

// ── checkUserAuth ─────────────────────────────────────────────────────────────

func TestCheckUserAuth_Allowed(t *testing.T) {
	gk := mockGatekeeper(t, true)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))

	got, id := checkUserAuth(r)
	if got != authAllowed {
		t.Fatalf("expected authAllowed, got %d", got)
	}
	if id != testUUID {
		t.Fatalf("expected user_id %q, got %q", testUUID, id)
	}
}

func TestCheckUserAuth_Unauthorized_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	if got, _ := checkUserAuth(r); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized, got %d", got)
	}
}

func TestCheckUserAuth_Unauthorized_MalformedToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer notajwt")
	if got, _ := checkUserAuth(r); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized for malformed JWT, got %d", got)
	}
}

func TestCheckUserAuth_Unauthorized_GatekeeperRejects(t *testing.T) {
	gk := mockGatekeeper(t, false)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))

	if got, _ := checkUserAuth(r); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized when gatekeeper rejects, got %d", got)
	}
}

// ── block list ────────────────────────────────────────────────────────────────

func TestBlockList_NotBlockedInitially(t *testing.T) {
	if isBlocked("1.2.3.4") {
		t.Fatal("IP should not be blocked before any failures")
	}
}

func TestBlockList_BlocksAfterThreshold(t *testing.T) {
	ip := "10.0.0.1"
	suspectMu.Lock()
	delete(suspectHits, ip)
	delete(blockedIPs, ip)
	suspectMu.Unlock()

	for i := 0; i < suspectThreshold; i++ {
		recordSuspect(ip, testUUID, http.MethodGet, "/secret")
	}
	if !isBlocked(ip) {
		t.Fatalf("IP should be blocked after %d failures", suspectThreshold)
	}

	// Clean up.
	suspectMu.Lock()
	delete(suspectHits, ip)
	delete(blockedIPs, ip)
	suspectMu.Unlock()
}

func TestBlockList_NotBlockedBelowThreshold(t *testing.T) {
	ip := "10.0.0.2"
	suspectMu.Lock()
	delete(suspectHits, ip)
	delete(blockedIPs, ip)
	suspectMu.Unlock()

	for i := 0; i < suspectThreshold-1; i++ {
		recordSuspect(ip, testUUID, http.MethodGet, "/secret")
	}
	if isBlocked(ip) {
		t.Fatalf("IP should not be blocked after only %d failures", suspectThreshold-1)
	}

	suspectMu.Lock()
	delete(suspectHits, ip)
	delete(blockedIPs, ip)
	suspectMu.Unlock()
}

func TestBlockList_ResetClearsCounter(t *testing.T) {
	ip := "10.0.0.3"
	suspectMu.Lock()
	delete(suspectHits, ip)
	delete(blockedIPs, ip)
	suspectMu.Unlock()

	// Accumulate failures below the threshold.
	for i := 0; i < suspectThreshold-1; i++ {
		recordSuspect(ip, testUUID, http.MethodGet, "/secret")
	}
	resetSuspect(ip)

	// After a reset, the full threshold must be reached again before blocking.
	for i := 0; i < suspectThreshold-1; i++ {
		recordSuspect(ip, testUUID, http.MethodGet, "/secret")
	}
	if isBlocked(ip) {
		t.Fatal("IP should not be blocked: counter was reset by a successful auth")
	}

	suspectMu.Lock()
	delete(suspectHits, ip)
	delete(blockedIPs, ip)
	suspectMu.Unlock()
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
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
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
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
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
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
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
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
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

// ── handleInternalRefresh ─────────────────────────────────────────────────────

func TestHandleInternalRefresh_NoKeyConfigured(t *testing.T) {
	orig := conductorNotifyKey
	conductorNotifyKey = ""
	defer func() { conductorNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/refresh", nil)
	r.Header.Set("X-Service-Key", "registry:somekey")
	w := httptest.NewRecorder()
	handleInternalRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 when CONDUCTOR_NOTIFY_KEY is empty", w.Code)
	}
}

func TestHandleInternalRefresh_WrongKey(t *testing.T) {
	orig := conductorNotifyKey
	conductorNotifyKey = "correct-key"
	defer func() { conductorNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/refresh", nil)
	r.Header.Set("X-Service-Key", "registry:wrong-key")
	w := httptest.NewRecorder()
	handleInternalRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for wrong key", w.Code)
	}
}

func TestHandleInternalRefresh_MissingHeader(t *testing.T) {
	orig := conductorNotifyKey
	conductorNotifyKey = "correct-key"
	defer func() { conductorNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/refresh", nil)
	w := httptest.NewRecorder()
	handleInternalRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 when X-Service-Key header is absent", w.Code)
	}
}

func TestHandleInternalRefresh_ValidKey(t *testing.T) {
	orig := conductorNotifyKey
	conductorNotifyKey = "correct-key"
	defer func() { conductorNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/refresh", nil)
	r.Header.Set("X-Service-Key", "registry:correct-key")
	w := httptest.NewRecorder()
	handleInternalRefresh(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 for valid key", w.Code)
	}
}
