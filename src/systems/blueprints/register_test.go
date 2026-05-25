package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// fakeRegistry starts an httptest.Server acting as the registry service,
// using the provided handler. It points the REGISTRY_URL env var at it for
// the duration of the test.
func fakeRegistry(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("REGISTRY_URL", srv.URL)
	return srv
}

// ── registerWithGatekeeper ────────────────────────────────────────────────────

// TestRegisterNoServiceKey verifies that registration is skipped when
// SERVICE_KEY is absent.
func TestRegisterNoServiceKey(t *testing.T) {
	os.Unsetenv("SERVICE_KEY")

	var called atomic.Bool
	fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	registerWithGatekeeper(ctx)

	if called.Load() {
		t.Fatal("registry should not be called when SERVICE_KEY is empty")
	}
}

// TestRegisterSuccess verifies that a successful registration POSTs valid JSON
// to /services/register and returns after receiving 204 No Content.
func TestRegisterSuccess(t *testing.T) {
	t.Setenv("SERVICE_KEY", "test-key")
	t.Setenv("SERVICE_NAME", "blueprints-test")
	t.Setenv("SERVICE_URL", "http://blueprints:8081")

	var (
		gotMethod      string
		gotPath        string
		gotContentType string
		gotBody        []byte
	)

	fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	registerWithGatekeeper(ctx)

	if gotMethod != http.MethodPost {
		t.Fatalf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/services/register" {
		t.Fatalf("path: got %q, want /services/register", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", gotContentType)
	}

	var payload struct {
		Name       string        `json:"name"`
		ServiceKey string        `json:"service_key"`
		URL        string        `json:"url"`
		Endpoints  []endpointDef `json:"endpoints"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if payload.Name != "blueprints-test" {
		t.Fatalf("name: got %q, want %q", payload.Name, "blueprints-test")
	}
	if payload.ServiceKey != "test-key" {
		t.Fatalf("service_key: got %q, want %q", payload.ServiceKey, "test-key")
	}
	if payload.URL != "http://blueprints:8081" {
		t.Fatalf("url: got %q, want %q", payload.URL, "http://blueprints:8081")
	}
	if len(payload.Endpoints) != len(blueprintsEndpoints) {
		t.Fatalf("endpoints: got %d, want %d", len(payload.Endpoints), len(blueprintsEndpoints))
	}
}

// TestRegisterRetriesOnBadStatus verifies that the function retries when the
// registry returns a non-204 status, then succeeds on a subsequent attempt.
func TestRegisterRetriesOnBadStatus(t *testing.T) {
	t.Setenv("SERVICE_KEY", "retry-key")
	// Use the default service name so SERVICE_NAME env doesn't leak from
	// TestRegisterSuccess if tests run in the same process.
	t.Setenv("SERVICE_NAME", "blueprints")

	var attempts atomic.Int32

	fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	registerWithGatekeeper(ctx)

	if attempts.Load() < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempts.Load())
	}
}

// TestRegisterContextCancelled verifies that cancelling the context causes the
// function to return without looping forever.
func TestRegisterContextCancelled(t *testing.T) {
	t.Setenv("SERVICE_KEY", "cancel-key")
	t.Setenv("SERVICE_NAME", "blueprints")

	var attempts atomic.Int32

	fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel as soon as the first attempt reaches the server.
	go func() {
		for attempts.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		registerWithGatekeeper(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Good — function exited after context cancellation.
	case <-time.After(15 * time.Second):
		t.Fatal("registerWithGatekeeper did not return after context cancellation")
	}
}

// ── blueprintsEndpoints ───────────────────────────────────────────────────────

// TestBlueprintsEndpointsContent checks that the endpoint list is non-empty
// and contains both user-scoped and org-scoped routes.
func TestBlueprintsEndpointsContent(t *testing.T) {
	if len(blueprintsEndpoints) == 0 {
		t.Fatal("blueprintsEndpoints must not be empty")
	}

	var hasUserScoped, hasOrgScoped bool
	for _, ep := range blueprintsEndpoints {
		if ep.Path == "/state/{username}/{workspace}" {
			hasUserScoped = true
		}
		if ep.Path == "/{org}/state/{team}/{workspace}" {
			hasOrgScoped = true
		}
	}
	if !hasUserScoped {
		t.Fatal("expected user-scoped endpoint /state/{username}/{workspace}")
	}
	if !hasOrgScoped {
		t.Fatal("expected org-scoped endpoint /{org}/state/{team}/{workspace}")
	}
}
