package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// POST /check_permissions endpoint. authorized controls whether it returns an
// authorized response or 401.
func mockGatekeeper(t *testing.T, authorized bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check_permissions" && r.Method == http.MethodPost {
			if authorized {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"authorized": true, "user_id": testUUID})
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

	got, id, _, _ := checkUserAuth(r, "testsvc", "read", "testsvc/things")
	if got != authAllowed {
		t.Fatalf("expected authAllowed, got %d", got)
	}
	if id != testUUID {
		t.Fatalf("expected user_id %q, got %q", testUUID, id)
	}
}

func TestCheckUserAuth_Unauthorized_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	if got, _, _, _ := checkUserAuth(r, "testsvc", "read", "testsvc/things"); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized, got %d", got)
	}
}

func TestCheckUserAuth_Unauthorized_MalformedToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer notajwt")
	if got, _, _, _ := checkUserAuth(r, "testsvc", "read", "testsvc/things"); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized for malformed JWT, got %d", got)
	}
}

func TestCheckUserAuth_Unauthorized_GatekeeperRejects(t *testing.T) {
	gk := mockGatekeeper(t, false)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))

	if got, _, _, _ := checkUserAuth(r, "testsvc", "read", "testsvc/things"); got != authUnauthorized {
		t.Fatalf("expected authUnauthorized when gatekeeper rejects, got %d", got)
	}
}

func TestCheckUserAuth_Forbidden_ReturnsReason(t *testing.T) {
	const reason = `permission denied: role "developer" is not allowed to "write" on testsvc resource "alice/things"`
	gk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"authorized": false, "user_id": testUUID, "reason": reason})
	}))
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	r := httptest.NewRequest(http.MethodGet, "/foo", nil)
	r.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))

	got, id, _, denyReason := checkUserAuth(r, "testsvc", "write", "testsvc/things")
	if got != authForbidden {
		t.Fatalf("expected authForbidden, got %d", got)
	}
	if id != testUUID {
		t.Fatalf("expected suspect id %q, got %q", testUUID, id)
	}
	if denyReason != reason {
		t.Fatalf("expected denyReason %q, got %q", reason, denyReason)
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

// ── signForwardedUserID ───────────────────────────────────────────────────────

// expectedForwardToken recomputes the wire format conductor signs so the test
// pins "conductor:<userID>:<ts>" HMAC-SHA256 independently of the implementation.
func expectedForwardToken(key, userID, ts string) string {
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "conductor:%s:%s", userID, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSignForwardedUserID_FormatAndDeterminism(t *testing.T) {
	origKey := conductorForwardKey
	conductorForwardKey = "test-forward-key"
	t.Cleanup(func() { conductorForwardKey = origKey })

	tok, ts := signForwardedUserID(testUUID)

	// The token is a hex-encoded HMAC-SHA256: 32 bytes → 64 hex chars.
	if len(tok) != 64 {
		t.Fatalf("expected 64-char hex token, got %d chars: %q", len(tok), tok)
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Fatalf("token is not valid hex: %v", err)
	}

	// The token must match the pinned "conductor:<userID>:<ts>" wire format.
	if want := expectedForwardToken(conductorForwardKey, testUUID, ts); tok != want {
		t.Fatalf("token %q does not match expected wire format %q", tok, want)
	}

	// Signing the same (userID, ts) again is deterministic.
	if got := expectedForwardToken(conductorForwardKey, testUUID, ts); got != tok {
		t.Fatalf("signing is not deterministic: got %q, want %q", got, tok)
	}
}

func TestSignForwardedUserID_KeyAndUserSensitivity(t *testing.T) {
	origKey := conductorForwardKey
	t.Cleanup(func() { conductorForwardKey = origKey })

	const ts = "1700000000"
	const otherUser = "11111111-1111-1111-1111-111111111111"

	conductorForwardKey = "key-A"
	base := expectedForwardToken(conductorForwardKey, testUUID, ts)

	// A different user id under the same key yields a different token.
	if other := expectedForwardToken(conductorForwardKey, otherUser, ts); other == base {
		t.Fatal("different user ids must produce different tokens")
	}

	// A different signing key for the same user yields a different token.
	conductorForwardKey = "key-B"
	if other := expectedForwardToken(conductorForwardKey, testUUID, ts); other == base {
		t.Fatal("different signing keys must produce different tokens")
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

func TestHandleServiceProxy_UnknownService_Returns404(t *testing.T) {
	withEndpoints(t, nil)
	withServices(t, map[string]serviceState{}) // no registered services

	req := httptest.NewRequest(http.MethodGet, "/testsvc/things", nil)
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown service prefix, got %d", w.Code)
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

	req := httptest.NewRequest(http.MethodPost, "/gatekeeper/signup", strings.NewReader(`{}`))
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

	req := httptest.NewRequest(http.MethodGet, "/gatekeeper/users", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/gatekeeper/profile", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/testsvc/admin", nil)
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when gatekeeper denies, got %d", w.Code)
	}
}

func TestHandleServiceProxy_ForwardAuth_PassesToken(t *testing.T) {
	// The gatekeeper (permission check) and the backend (forward target) must be
	// distinct servers — otherwise a 200/empty permission response can masquerade
	// as a successful proxy and the test passes without ever forwarding.
	var receivedAuth string
	var backendHit bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
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

	bearer := "Bearer " + makeTestJWT(testUUID)
	req := httptest.NewRequest(http.MethodGet, "/gatekeeper/profile", nil)
	req.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after proxying, got %d", w.Code)
	}
	if !backendHit {
		t.Fatal("backend was never reached — request was not proxied")
	}
	if receivedAuth != bearer {
		t.Fatalf("forward_auth=true must forward the raw bearer; backend saw %q, want %q", receivedAuth, bearer)
	}
}

func TestHandleServiceProxy_NoForwardAuth_StripsToken(t *testing.T) {
	var receivedAuth, receivedUserID, receivedToken, receivedTS string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedUserID = r.Header.Get("X-User-ID")
		receivedToken = r.Header.Get("X-Conductor-Token")
		receivedTS = r.Header.Get("X-Conductor-Timestamp")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// A signing key must be configured for conductor to emit the X-Conductor-*
	// headers that let the backend confirm X-User-ID came through conductor.
	origKey := conductorForwardKey
	conductorForwardKey = "test-forward-key"
	t.Cleanup(func() { conductorForwardKey = origKey })

	gk := mockGatekeeper(t, true)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	entry := makeEntry(http.MethodGet, "/data", "read", "data")
	entry.serviceName = "datasvc"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"datasvc": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: false},
	})

	req := httptest.NewRequest(http.MethodGet, "/datasvc/data", nil)
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	// The raw bearer must be stripped...
	if receivedAuth != "" {
		t.Fatalf("expected Authorization header to be stripped for forward_auth=false, got %q", receivedAuth)
	}
	// ...and conductor must inject the verified identity in its place.
	if receivedUserID != testUUID {
		t.Errorf("expected injected X-User-ID=%q, got %q", testUUID, receivedUserID)
	}
	if len(receivedToken) != 64 { // hex-encoded HMAC-SHA256
		t.Errorf("expected a signed X-Conductor-Token, got %q", receivedToken)
	}
	if receivedTS == "" {
		t.Error("expected X-Conductor-Timestamp to be injected")
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

	req := httptest.NewRequest(http.MethodGet, "/testsvc/pub", nil)
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

func TestHandleServiceProxy_AuthenticatedSpoofedUserID_Overwritten(t *testing.T) {
	// On an authenticated forward_auth=false request, a client-supplied X-User-ID
	// must be overwritten with the gatekeeper-verified identity, not passed through.
	var receivedUserID, receivedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUserID = r.Header.Get("X-User-ID")
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// A signing key is required for conductor to emit the X-Conductor-* headers.
	origKey := conductorForwardKey
	conductorForwardKey = "test-forward-key"
	t.Cleanup(func() { conductorForwardKey = origKey })

	// mockGatekeeper(t, true) authorizes and returns user_id == testUUID.
	gk := mockGatekeeper(t, true)
	defer gk.Close()
	withGatekeeperURL(t, gk.URL)

	entry := makeEntry(http.MethodGet, "/data", "read", "data")
	entry.serviceName = "datasvc"
	withEndpoints(t, []endpointEntry{entry})
	withServices(t, map[string]serviceState{
		"datasvc": {url: backend.URL, proxy: proxyTo(t, backend.URL), forwardAuth: false},
	})

	req := httptest.NewRequest(http.MethodGet, "/datasvc/data", nil)
	req.Header.Set("Authorization", "Bearer "+makeTestJWT(testUUID))
	req.Header.Set("X-User-ID", "attacker") // spoofed identity the client tries to inject
	w := httptest.NewRecorder()
	handleServiceProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after proxying, got %d", w.Code)
	}
	// The spoofed X-User-ID must be replaced by the gatekeeper-verified identity.
	if receivedUserID != testUUID {
		t.Fatalf("expected backend X-User-ID=%q (verified identity), got %q", testUUID, receivedUserID)
	}
	if receivedUserID == "attacker" {
		t.Fatal("spoofed X-User-ID passed through to backend")
	}
	// The raw bearer must be stripped for forward_auth=false services.
	if receivedAuth != "" {
		t.Fatalf("expected Authorization to be stripped for forward_auth=false, got %q", receivedAuth)
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

// handleListServices is a cheap, public read of the live routing table used by the
// portal sidebar and CLI hub to show only services that are actually registered.
func TestHandleListServices_ReturnsRegisteredNamesSorted(t *testing.T) {
	withServices(t, map[string]serviceState{
		"forge":      {url: "http://forge", description: "exec"},
		"gatekeeper": {url: "http://gk"},
		"argo":       {url: "http://argo", description: "gitops"},
	})

	r := httptest.NewRequest(http.MethodGet, "/services", nil)
	w := httptest.NewRecorder()
	handleListServices(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Services []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make([]string, len(body.Services))
	for i, s := range body.Services {
		got[i] = s.Name
	}
	if want := "argo,forge,gatekeeper"; strings.Join(got, ",") != want {
		t.Fatalf("names = %v, want sorted %q", got, want)
	}
	// forge sorts second; its description passes through, empty ones are omitted.
	if body.Services[1].Description != "exec" {
		t.Fatalf("forge description = %q, want \"exec\"", body.Services[1].Description)
	}
}
