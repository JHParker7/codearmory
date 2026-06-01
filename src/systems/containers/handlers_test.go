package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
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
	gatekeeperClient.URL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		srv.Close()
	})
}

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = &http.Client{}
	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: "containers", HTTPClient: httpClient}
	registry = &registryClient{baseURL: "http://127.0.0.1:1", http: httpClient}
	ociProxy = &httputil.ReverseProxy{Director: func(r *http.Request) {}}
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
