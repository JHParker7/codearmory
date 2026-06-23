package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ghAppWithStub builds a githubApp whose GitHub API base points at a test server.
func ghAppWithStub(t *testing.T, h http.HandlerFunc) (*githubApp, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL) // newGithubApp reads this for apiBase
	return newTestGithubApp(t, "whsec"), srv
}

func TestInstallationToken(t *testing.T) {
	var hits int
	app, _ := ghAppWithStub(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"inst-tok-123","expires_at":"2099-01-01T00:00:00Z"}`)) //nolint:errcheck
	})
	tok, err := app.installationToken(context.Background(), 42)
	if err != nil || tok != "inst-tok-123" {
		t.Fatalf("installationToken = %q, %v", tok, err)
	}
	// Second call must be served from cache (no extra GitHub hit).
	tok2, _ := app.installationToken(context.Background(), 42)
	if tok2 != "inst-tok-123" || hits != 1 {
		t.Errorf("expected cached token (hits=%d)", hits)
	}
}

func TestInstallationToken_Error(t *testing.T) {
	app, _ := ghAppWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad creds", http.StatusUnauthorized)
	})
	if _, err := app.installationToken(context.Background(), 42); err == nil {
		t.Error("expected error on non-2xx installation token response")
	}
}

func TestCreateCheckRun(t *testing.T) {
	app, _ := ghAppWithStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/check-runs") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":555}`)) //nolint:errcheck
	})
	id, err := app.createCheckRun(context.Background(), "tok", "org/repo", "abc123", "ci", "ext-1")
	if err != nil || id != 555 {
		t.Fatalf("createCheckRun = %d, %v, want 555", id, err)
	}
}

func TestCreateCheckRun_Error(t *testing.T) {
	app, _ := ghAppWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := app.createCheckRun(context.Background(), "tok", "org/repo", "sha", "ci", "ext"); err == nil {
		t.Error("expected error on non-2xx create check run")
	}
}

func TestCompleteCheckRun(t *testing.T) {
	var method, path string
	app, _ := ghAppWithStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	// No return value; must not panic and must PATCH the check-run resource.
	app.completeCheckRun(context.Background(), "tok", "org/repo", 555, "success")
	if method != http.MethodPatch || !strings.Contains(path, "/check-runs/555") {
		t.Errorf("completeCheckRun called %s %s, want PATCH .../check-runs/555", method, path)
	}
	// Non-2xx must be handled gracefully (logged, no panic).
	app2, _ := ghAppWithStub(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", http.StatusNotFound) })
	app2.completeCheckRun(context.Background(), "tok", "org/repo", 1, "failure")
}
