package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
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
	httpClient = initHTTPClient()
	gatekeeperClient = newGatekeeperClient()
	os.Exit(m.Run())
}

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/apps", nil)
	w := httptest.NewRecorder()
	if _, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listApp", "argo/apps"); ok {
		t.Fatal("expected ok=false with no Bearer token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestCheckGatekeeper_Authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":"o1"}`)
	r := httptest.NewRequest(http.MethodGet, "/apps", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	id, org, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listApp", "argo/apps")
	if !ok || id != "u1" || org != "o1" {
		t.Fatalf("got (%q,%q,%v), want (u1,o1,true)", id, org, ok)
	}
}

func TestHandleListApps_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/apps", nil)
	w := httptest.NewRecorder()
	handleListApps(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleSyncApp_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/apps/x/sync", nil)
	r.SetPathValue("name", "x")
	w := httptest.NewRecorder()
	handleSyncApp(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetSync_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/syncs/x", nil)
	r.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	handleGetSync(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalEvent_BadToken(t *testing.T) {
	body := `{"event_id":"e1","integration":"argo","type":"app-state","payload":{"app_name":"a"}}`
	r := httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleInternalEvent(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalEvent_ValidTokenNoAppName(t *testing.T) {
	prev := outpostInternalKey
	outpostInternalKey = "unit-key"
	t.Cleanup(func() { outpostInternalKey = prev })

	body := `{"event_id":"e1","integration":"argo","type":"app-state","payload":{}}`
	token, ts := signInternal("event", []byte(body))
	r := httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewBufferString(body))
	r.Header.Set("X-Internal-Token", token)
	r.Header.Set("X-Internal-Timestamp", ts)
	w := httptest.NewRecorder()
	handleInternalEvent(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestOrgScope(t *testing.T) {
	if got := orgScope("org-1", "u1"); got != "org-1" {
		t.Errorf("org takes precedence: got %q", got)
	}
	if got := orgScope("", "u1"); got != "u:u1" {
		t.Errorf("no-org falls back to user: got %q", got)
	}
}

func TestSyncStatusMessage(t *testing.T) {
	if got := syncStatusMessage("Synced", "Healthy", "Failed"); got != "operation Failed" {
		t.Errorf("failed phase message: %q", got)
	}
	if got := syncStatusMessage("Synced", "Healthy", "Succeeded"); got != "sync=Synced health=Healthy" {
		t.Errorf("normal message: %q", got)
	}
}
