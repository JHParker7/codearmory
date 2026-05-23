package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── extractToken ──────────────────────────────────────────────────────────────

func TestExtractTokenBearer(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer mytoken")

	tok, ok := extractToken(r.Context(), r)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if tok != "mytoken" {
		t.Fatalf("got %q, want %q", tok, "mytoken")
	}
}

func TestExtractTokenBasicAuth(t *testing.T) {
	srv := fakeGatekeeper(t, http.StatusOK, `{"token":"gk-token"}`)
	_ = srv

	r, _ := http.NewRequest("GET", "/", nil)
	r.SetBasicAuth("user@example.com", "secret")

	tok, ok := extractToken(r.Context(), r)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if tok != "gk-token" {
		t.Fatalf("got %q, want %q", tok, "gk-token")
	}
}

func TestExtractTokenBasicAuthGatekeeperFailure(t *testing.T) {
	fakeGatekeeper(t, http.StatusUnauthorized, ``)

	r, _ := http.NewRequest("GET", "/", nil)
	r.SetBasicAuth("bad@example.com", "wrong")

	_, ok := extractToken(r.Context(), r)
	if ok {
		t.Fatal("expected ok=false when gatekeeper rejects credentials")
	}
}

func TestExtractTokenNoAuth(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	_, ok := extractToken(r.Context(), r)
	if ok {
		t.Fatal("expected ok=false with no Authorization header")
	}
}

func TestExtractTokenMalformedBasic(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic !!!not-base64!!!")
	_, ok := extractToken(r.Context(), r)
	if ok {
		t.Fatal("expected ok=false for malformed Basic credentials")
	}
}

// ── checkPermissions ──────────────────────────────────────────────────────────

func TestCheckPermissionsAuthorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true}`)

	if !checkPermissions(context.Background(), "tok", "blueprints/states/u/ws", "getState") {
		t.Fatal("expected authorized=true")
	}
}

func TestCheckPermissionsDenied(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":false}`)

	if checkPermissions(context.Background(), "tok", "blueprints/states/u/ws", "getState") {
		t.Fatal("expected authorized=false")
	}
}

func TestCheckPermissionsGatekeeperError(t *testing.T) {
	fakeGatekeeper(t, http.StatusInternalServerError, ``)

	if checkPermissions(context.Background(), "tok", "blueprints/states/u/ws", "getState") {
		t.Fatal("expected authorized=false on gatekeeper error")
	}
}

func TestCheckPermissionsGatekeeperDown(t *testing.T) {
	origURL := gatekeeperURL
	gatekeeperURL = "http://127.0.0.1:1" // nothing listening
	t.Cleanup(func() { gatekeeperURL = origURL })

	if checkPermissions(context.Background(), "tok", "blueprints/states/u/ws", "getState") {
		t.Fatal("expected authorized=false when gatekeeper is unreachable")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// fakeGatekeeper starts a test server that replies with the given status and
// body for any request, points gatekeeperURL at it, and restores both on
// cleanup.
func fakeGatekeeper(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = orig })

	return srv
}
