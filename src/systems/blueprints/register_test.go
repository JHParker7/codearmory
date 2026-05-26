package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"codearmory.local/svckit/registry"
)

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

// ── serviceConfig content ────────────────────────────────────────────────────

func TestServiceConfigLoaded(t *testing.T) {
	if serviceConfig.Name == "" {
		t.Fatal("serviceConfig.Name must not be empty")
	}
	if len(serviceConfig.Endpoints) == 0 {
		t.Fatal("serviceConfig.Endpoints must not be empty")
	}
	if len(serviceConfig.Roles) == 0 {
		t.Fatal("serviceConfig.Roles must not be empty")
	}
}

func TestServiceConfigEndpoints(t *testing.T) {
	var hasUserScoped, hasOrgScoped bool
	for _, ep := range serviceConfig.Endpoints {
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

// ── registry.Register behaviour ──────────────────────────────────────────────

func TestRegisterNoServiceKey(t *testing.T) {
	os.Unsetenv("SERVICE_KEY")

	var called atomic.Bool
	fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	registry.Register(ctx, serviceConfig, "")

	if called.Load() {
		t.Fatal("registry should not be called when service key is empty")
	}
}

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

	registry.Register(ctx, serviceConfig, os.Getenv("SERVICE_KEY"))

	if gotMethod != http.MethodPost {
		t.Fatalf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/services/register" {
		t.Fatalf("path: got %q, want /services/register", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", gotContentType)
	}
	if len(gotBody) == 0 {
		t.Fatal("expected non-empty request body")
	}
}

func TestRegisterRetriesOnBadStatus(t *testing.T) {
	t.Setenv("SERVICE_KEY", "retry-key")
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

	registry.Register(ctx, serviceConfig, os.Getenv("SERVICE_KEY"))

	if attempts.Load() < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempts.Load())
	}
}

func TestRegisterContextCancelled(t *testing.T) {
	t.Setenv("SERVICE_KEY", "cancel-key")
	t.Setenv("SERVICE_NAME", "blueprints")

	var attempts atomic.Int32

	fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for attempts.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		registry.Register(ctx, serviceConfig, os.Getenv("SERVICE_KEY"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("registry.Register did not return after context cancellation")
	}
}
