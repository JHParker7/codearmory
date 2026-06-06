package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"testing"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
)

func fakeGatekeeper(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperClient.URL
	origURL := gatekeeperURL
	gatekeeperClient.URL = srv.URL
	gatekeeperURL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		gatekeeperURL = origURL
		srv.Close()
	})
}

// fakeGatekeeperMulti sets up a fake that dispatches to different handlers
// based on request path: checkPermissions for /check_permissions,
// secretValue (or 404 if empty) for /internal/secrets/lookup.
func fakeGatekeeperMulti(t *testing.T, permBody string, secretValue string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/check_permissions" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(permBody)) //nolint:errcheck
			return
		}
		if r.URL.Path == "/internal/secrets/lookup" {
			if secretValue == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"value": secretValue}) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	orig := gatekeeperClient.URL
	origURL := gatekeeperURL
	gatekeeperClient.URL = srv.URL
	gatekeeperURL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		gatekeeperURL = origURL
		srv.Close()
	})
}

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = &http.Client{}
	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: "containers", HTTPClient: httpClient}
	registry = &registryClient{baseURL: "http://127.0.0.1:1", http: httpClient}
	ociProxy = &httputil.ReverseProxy{Director: func(r *http.Request) {}}
	getServiceKey = func() string { return "test-service-key" }
	os.Exit(m.Run())
}

// ── isOCIPath ─────────────────────────────────────────────────────────────────

func TestIsOCIPath_V2Root(t *testing.T) {
	if !isOCIPath("/v2") {
		t.Fatal("expected /v2 to be an OCI path")
	}
}

func TestIsOCIPath_V2Prefix(t *testing.T) {
	if !isOCIPath("/v2/myns/myimage/tags/list") {
		t.Fatal("expected /v2/... to be an OCI path")
	}
}

func TestIsOCIPath_NonOCI(t *testing.T) {
	if isOCIPath("/repositories") {
		t.Fatal("/repositories should not be an OCI path")
	}
}

func TestIsOCIPath_PartialMatch(t *testing.T) {
	if isOCIPath("/v2something") {
		t.Fatal("/v2something should not be an OCI path")
	}
}

// ── bearerToken ───────────────────────────────────────────────────────────────

func TestBearerToken_BearerHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	r.Header.Set("Authorization", "Bearer mytoken")
	if got := bearerToken(r); got != "mytoken" {
		t.Fatalf("got %q, want mytoken", got)
	}
}

func TestBearerToken_BasicAuth(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	encoded := base64.StdEncoding.EncodeToString([]byte("user:mypassword"))
	r.Header.Set("Authorization", "Basic "+encoded)
	if got := bearerToken(r); got != "mypassword" {
		t.Fatalf("got %q, want mypassword", got)
	}
}

func TestBearerToken_NoAuth(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	if got := bearerToken(r); got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

// ── v2ActionResource ──────────────────────────────────────────────────────────

func TestV2ActionResource_RootSlash(t *testing.T) {
	// /v2/ (with trailing slash) is the OCI discovery ping path.
	action, resource := v2ActionResource(http.MethodGet, "/v2/")
	if action != "listRepository" || resource != "containers/repositories" {
		t.Fatalf("got (%q, %q)", action, resource)
	}
}

func TestV2ActionResource_Catalog(t *testing.T) {
	action, resource := v2ActionResource(http.MethodGet, "/v2/_catalog")
	if action != "listRepository" || resource != "containers/repositories" {
		t.Fatalf("got (%q, %q)", action, resource)
	}
}

func TestV2ActionResource_TagsList(t *testing.T) {
	action, resource := v2ActionResource(http.MethodGet, "/v2/myns/myimage/tags/list")
	if action != "listTag" {
		t.Fatalf("action = %q, want listTag", action)
	}
	if resource != "containers/repositories/myns/myimage" {
		t.Fatalf("resource = %q", resource)
	}
}

func TestV2ActionResource_ManifestGet(t *testing.T) {
	action, resource := v2ActionResource(http.MethodGet, "/v2/myns/myimage/manifests/latest")
	if action != "pullImage" {
		t.Fatalf("action = %q, want pullImage", action)
	}
	if resource != "containers/repositories/myns/myimage" {
		t.Fatalf("resource = %q", resource)
	}
}

func TestV2ActionResource_ManifestDelete(t *testing.T) {
	action, resource := v2ActionResource(http.MethodDelete, "/v2/myns/myimage/manifests/sha256:abc")
	if action != "deleteImage" {
		t.Fatalf("action = %q, want deleteImage", action)
	}
	if resource != "containers/repositories/myns/myimage" {
		t.Fatalf("resource = %q", resource)
	}
}

func TestV2ActionResource_BlobUploadPost(t *testing.T) {
	action, _ := v2ActionResource(http.MethodPost, "/v2/myns/myimage/blobs/uploads")
	if action != "pushImage" {
		t.Fatalf("action = %q, want pushImage", action)
	}
}

func TestV2ActionResource_BlobUploadResumableGet(t *testing.T) {
	// GET on a /blobs/uploads/{uuid} URL is a resumable upload status check — still a push operation.
	action, _ := v2ActionResource(http.MethodGet, "/v2/myns/myimage/blobs/uploads/some-uuid")
	if action != "pushImage" {
		t.Fatalf("action = %q, want pushImage", action)
	}
}

func TestV2ActionResource_BlobPut(t *testing.T) {
	action, _ := v2ActionResource(http.MethodPut, "/v2/myns/myimage/blobs/sha256:abc")
	if action != "pushImage" {
		t.Fatalf("action = %q, want pushImage", action)
	}
}

// ── registryError / isRegistryNotFound ────────────────────────────────────────

func TestRegistryError_Error(t *testing.T) {
	e := &registryError{Status: 404, Body: "not found"}
	want := "registry 404: not found"
	if got := e.Error(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestIsRegistryNotFound_404(t *testing.T) {
	if !isRegistryNotFound(&registryError{Status: http.StatusNotFound}) {
		t.Fatal("expected true for 404 registryError")
	}
}

func TestIsRegistryNotFound_OtherStatus(t *testing.T) {
	if isRegistryNotFound(&registryError{Status: http.StatusBadRequest}) {
		t.Fatal("expected false for non-404 registryError")
	}
}

func TestIsRegistryNotFound_NonRegistryError(t *testing.T) {
	if isRegistryNotFound(http.ErrNoCookie) {
		t.Fatal("expected false for non-registryError")
	}
}

// ── handler auth (no token → 401) ────────────────────────────────────────────

func TestHandleListRepositories_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repositories", nil)
	w := httptest.NewRecorder()
	handleListRepositories(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListTags_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repositories/myns/myimage/tags", nil)
	r.SetPathValue("namespace", "myns")
	r.SetPathValue("image", "myimage")
	w := httptest.NewRecorder()
	handleListTags(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetManifest_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repositories/myns/myimage/manifests/latest", nil)
	r.SetPathValue("namespace", "myns")
	r.SetPathValue("image", "myimage")
	r.SetPathValue("reference", "latest")
	w := httptest.NewRecorder()
	handleGetManifest(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteManifest_InvalidDigest(t *testing.T) {
	// Digest validation happens before gatekeeper — no token needed.
	r := httptest.NewRequest(http.MethodDelete, "/repositories/myns/myimage/manifests/latest", nil)
	r.SetPathValue("namespace", "myns")
	r.SetPathValue("image", "myimage")
	r.SetPathValue("digest", "latest")
	w := httptest.NewRecorder()
	handleDeleteManifest(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleDeleteManifest_Unauthorized(t *testing.T) {
	// Valid sha256 digest but no token → 401 from gatekeeper check.
	r := httptest.NewRequest(http.MethodDelete, "/repositories/myns/myimage/manifests/sha256:abc", nil)
	r.SetPathValue("namespace", "myns")
	r.SetPathValue("image", "myimage")
	r.SetPathValue("digest", "sha256:abc123")
	w := httptest.NewRecorder()
	handleDeleteManifest(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── handleV2 ──────────────────────────────────────────────────────────────────

func TestHandleV2_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v2/myns/myimage/tags/list", nil)
	w := httptest.NewRecorder()
	handleV2(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if w.Header().Get("Docker-Distribution-API-Version") == "" {
		t.Error("missing Docker-Distribution-API-Version header")
	}
}

func TestHandleV2_DiscoveryPing(t *testing.T) {
	// /v2 discovery returns 200 directly after auth without proxying upstream.
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	r := httptest.NewRequest(http.MethodGet, "/v2", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleV2(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if w.Header().Get("Docker-Distribution-API-Version") == "" {
		t.Error("missing Docker-Distribution-API-Version header")
	}
}

func TestHandleV2_DiscoveryPingSlash(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleV2(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

// ── statusResponseWriter ──────────────────────────────────────────────────────

func TestStatusResponseWriter_WriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusNotFound {
		t.Fatalf("rw.status: got %d, want 404", rw.status)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder: got %d, want 404", w.Code)
	}
}

// ── resolveOrgCreds ───────────────────────────────────────────────────────────

func TestResolveOrgCreds_Success(t *testing.T) {
	orgID := "org-" + t.Name()
	t.Cleanup(func() { orgCredsCache.Delete(orgID) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"value": "forgejo-bot:tok-abc123"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	origURL := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = origURL })

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	creds, err := resolveOrgCreds(t.Context(), orgID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.username != "forgejo-bot" || creds.password != "tok-abc123" {
		t.Fatalf("got (%q, %q), want (forgejo-bot, tok-abc123)", creds.username, creds.password)
	}
}

func TestResolveOrgCreds_Cached(t *testing.T) {
	orgID := "org-" + t.Name()
	t.Cleanup(func() { orgCredsCache.Delete(orgID) })

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"value": "user:token"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	origURL := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = origURL })

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	for i := range 3 {
		if _, err := resolveOrgCreds(t.Context(), orgID); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 Gatekeeper call (cache hit on subsequent), got %d", calls)
	}
}

func TestResolveOrgCreds_NotFound(t *testing.T) {
	orgID := "org-" + t.Name()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	origURL := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = origURL })

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	if _, err := resolveOrgCreds(t.Context(), orgID); err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestResolveOrgCreds_InvalidFormat(t *testing.T) {
	orgID := "org-" + t.Name()
	t.Cleanup(func() { orgCredsCache.Delete(orgID) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"value": "no-colon-here"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	origURL := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = origURL })

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	if _, err := resolveOrgCreds(t.Context(), orgID); err == nil {
		t.Fatal("expected error for secret missing colon separator")
	}
}

func TestResolveOrgCreds_Disabled(t *testing.T) {
	origName := orgSecretName
	orgSecretName = ""
	t.Cleanup(func() { orgSecretName = origName })

	if _, err := resolveOrgCreds(t.Context(), "some-org"); err == nil {
		t.Fatal("expected error when orgSecretName is empty")
	}
}

func TestResolveOrgCreds_EmptyOrg(t *testing.T) {
	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	if _, err := resolveOrgCreds(t.Context(), ""); err == nil {
		t.Fatal("expected error when orgID is empty")
	}
}

// ── handleV2 per-org credential injection ────────────────────────────────────

// TestHandleV2_InjectsOrgCredsToProxy verifies that per-org credentials fetched
// from Gatekeeper are forwarded as Basic auth to the upstream OCI registry on
// proxied requests (not discovery pings). This exercises the Director code path.
func TestHandleV2_InjectsOrgCredsToProxy(t *testing.T) {
	orgID := "org-director-test"
	t.Cleanup(func() { orgCredsCache.Delete(orgID) })

	// Fake upstream registry records the Authorization header it receives.
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	// Wire ociProxy to the fake upstream.
	origProxy := ociProxy
	initOCIProxy()
	t.Cleanup(func() { ociProxy = origProxy })

	// Point registry baseURL at the fake upstream so initOCIProxy routes there.
	origReg := registry
	registry = &registryClient{baseURL: upstream.URL, http: httpClient}
	t.Cleanup(func() { registry = origReg })
	initOCIProxy()

	fakeGatekeeperMulti(t,
		`{"authorized":true,"user_id":"u1","org_id":"org-director-test"}`,
		"forgejo-bot:tok-xyz",
	)

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	// Send a manifest pull request — goes through ociProxy, not the early-return path.
	r := httptest.NewRequest(http.MethodGet, "/v2/myorg/myimage/manifests/latest", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleV2(w, r)

	if gotAuth == "" {
		t.Fatal("upstream received no Authorization header")
	}
	// SetBasicAuth encodes as "Basic base64(user:pass)".
	user, pass, ok := parseBasicAuth(gotAuth)
	if !ok {
		t.Fatalf("Authorization header is not Basic auth: %q", gotAuth)
	}
	if user != "forgejo-bot" || pass != "tok-xyz" {
		t.Fatalf("upstream received user=%q pass=%q, want forgejo-bot:tok-xyz", user, pass)
	}
}

// parseBasicAuth decodes an "Authorization: Basic ..." header value.
func parseBasicAuth(auth string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
		return "", "", false
	}
	encoded := auth[len(prefix):]
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", false
	}
	return u, p, true
}

func TestHandleV2_DiscoveryPingSkipsCredsLookup(t *testing.T) {
	// GET /v2 (discovery ping) must return 200 without contacting the secrets
	// endpoint — resolveOrgCreds is called only for proxied requests.
	lookupCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/check_permissions" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"authorized":true,"user_id":"u1","org_id":"org-ping"}`)) //nolint:errcheck
			return
		}
		if r.URL.Path == "/internal/secrets/lookup" {
			lookupCalls++
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	origURL := gatekeeperURL
	origClient := gatekeeperClient
	gatekeeperURL = srv.URL
	gatekeeperClient = &gk.Client{URL: srv.URL, Service: "containers", HTTPClient: httpClient}
	t.Cleanup(func() {
		gatekeeperURL = origURL
		gatekeeperClient = origClient
	})

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	r := httptest.NewRequest(http.MethodGet, "/v2", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleV2(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if lookupCalls != 0 {
		t.Fatalf("secrets lookup called %d times for /v2 ping, want 0", lookupCalls)
	}
}

func TestHandleV2_FallsBackWhenSecretMissing(t *testing.T) {
	// When the org secret doesn't exist, proxied requests must still succeed using
	// global credentials.
	orgID := "org-nosecret"
	t.Cleanup(func() { orgCredsCache.Delete(orgID) })

	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	origReg := registry
	registry = &registryClient{baseURL: upstream.URL, http: httpClient, username: "global-user", password: "global-pass"}
	t.Cleanup(func() { registry = origReg })
	initOCIProxy()

	fakeGatekeeperMulti(t,
		`{"authorized":true,"user_id":"u1","org_id":"org-nosecret"}`,
		"", // 404 from secrets endpoint
	)

	origName := orgSecretName
	orgSecretName = "registry-token"
	t.Cleanup(func() { orgSecretName = origName })

	r := httptest.NewRequest(http.MethodGet, "/v2/org/img/manifests/latest", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleV2(w, r)

	user, pass, ok := parseBasicAuth(gotAuth)
	if !ok || user != "global-user" || pass != "global-pass" {
		t.Fatalf("expected global creds fallback; got auth %q (user=%q, pass=%q, ok=%v)", gotAuth, user, pass, ok)
	}
}

func TestHandleV2_FallsBackWhenFeatureDisabled(t *testing.T) {
	// When REGISTRY_ORG_SECRET_NAME is not set, handleV2 should work normally with global creds.
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"org-abc"}`)

	origName := orgSecretName
	orgSecretName = ""
	t.Cleanup(func() { orgSecretName = origName })

	r := httptest.NewRequest(http.MethodGet, "/v2", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleV2(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (feature disabled should not break auth)", w.Code)
	}
}
