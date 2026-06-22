package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeGatekeeper points the SDK CheckPermissions client at a stub that returns
// the given status/body, so handler tests can simulate authorised/forbidden
// callers without a real gatekeeper.
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
	httpClient = initHTTPClient()
	gatekeeperClient = newGatekeeperClient()
	os.Exit(m.Run())
}

// authorized makes the stub gatekeeper grant the request.
func authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"o1"}`)
}

// putReq builds a PUT /orgs/{id}/services/{service} request with path values set
// and a Bearer token so CheckPermissions runs.
func putReq(id, service, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/orgs/"+id+"/services/"+service, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	r.SetPathValue("id", id)
	r.SetPathValue("service", service)
	return r
}

// ── Auth gates ────────────────────────────────────────────────────────────────

func TestHandleListOrgServices_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/orgs/o1/services", nil)
	r.SetPathValue("id", "o1")
	w := httptest.NewRecorder()
	handleListOrgServices(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleSetOrgService_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/orgs/o1/services/forge", strings.NewReader(`{"enabled":false}`))
	r.SetPathValue("id", "o1")
	r.SetPathValue("service", "forge")
	w := httptest.NewRecorder()
	handleSetOrgService(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteOrgService_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/orgs/o1/services/forge", nil)
	r.SetPathValue("id", "o1")
	r.SetPathValue("service", "forge")
	w := httptest.NewRecorder()
	handleDeleteOrgService(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── Validation (runs after auth, before any DB write) ──────────────────────────

func TestHandleSetOrgService_CoreRejected(t *testing.T) {
	authorized(t)
	w := httptest.NewRecorder()
	handleSetOrgService(w, putReq("o1", "gatekeeper", `{"enabled":false}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for core service", w.Code)
	}
}

func TestHandleSetOrgService_BadKind(t *testing.T) {
	authorized(t)
	w := httptest.NewRecorder()
	handleSetOrgService(w, putReq("o1", "forge", `{"enabled":true,"kind":"bogus"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for bad kind", w.Code)
	}
}

func TestHandleSetOrgService_CustomMissingImage(t *testing.T) {
	authorized(t)
	w := httptest.NewRecorder()
	handleSetOrgService(w, putReq("o1", "mysvc", `{"enabled":true,"kind":"custom","port":9000}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for custom without image", w.Code)
	}
}

func TestHandleSetOrgService_CustomBadPort(t *testing.T) {
	authorized(t)
	w := httptest.NewRecorder()
	handleSetOrgService(w, putReq("o1", "mysvc", `{"enabled":true,"kind":"custom","image":"repo/x:1"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for custom without a valid port", w.Code)
	}
}

func TestHandleSetOrgService_InvalidJSON(t *testing.T) {
	authorized(t)
	w := httptest.NewRecorder()
	handleSetOrgService(w, putReq("o1", "forge", `{not json`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for invalid body", w.Code)
	}
}

func TestHandleSetOrgService_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":false}`)
	w := httptest.NewRecorder()
	handleSetOrgService(w, putReq("o1", "forge", `{"enabled":false}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}
