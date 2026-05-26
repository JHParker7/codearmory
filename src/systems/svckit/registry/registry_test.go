package registry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() ServiceConfig {
	return ServiceConfig{
		Name:        "testsvc",
		Description: "A test service",
		Roles:       []RoleDef{{Name: "admin", Description: "Admin role"}},
		Endpoints:   []EndpointDef{{Method: "GET", Path: "/foo", Action: "read", Resource: "foo"}},
	}
}

func TestRegisterEmptyServiceKey(t *testing.T) {
	// Should return immediately without making any HTTP calls.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)

	Register(context.Background(), testConfig(), "")
	if called {
		t.Error("expected no HTTP call when serviceKey is empty")
	}
}

func TestRegisterSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_URL", "http://testsvc:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	Register(ctx, testConfig(), "mykey")
	// If Register returned without cancellation, the test passes.
}

func TestRegisterPayloadContents(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_URL", "http://testsvc:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg := testConfig()
	Register(ctx, cfg, "secret123")

	var payload map[string]any
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatalf("invalid JSON payload: %v", err)
	}
	if payload["name"] != "testsvc" {
		t.Errorf("name = %v, want testsvc", payload["name"])
	}
	if payload["service_key"] != "secret123" {
		t.Errorf("service_key = %v, want secret123", payload["service_key"])
	}
	if payload["url"] != "http://testsvc:8080" {
		t.Errorf("url = %v, want http://testsvc:8080", payload["url"])
	}
	if payload["description"] != "A test service" {
		t.Errorf("description = %v, want 'A test service'", payload["description"])
	}
}

func TestRegisterServiceNameEnvOverride(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		gotName, _ = payload["name"].(string)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_NAME", "overridden")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	Register(ctx, testConfig(), "key")
	if gotName != "overridden" {
		t.Errorf("name = %q, want %q", gotName, "overridden")
	}
}

func TestRegisterRetryOnBadStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)

	// Patch sleep so the test doesn't actually wait 5s per retry.
	orig := sleepFn
	sleepFn = func(d time.Duration) {}
	defer func() { sleepFn = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	Register(ctx, testConfig(), "key")
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls (2 retries + 1 success), got %d", calls.Load())
	}
}

func TestRegisterContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Never respond with 204 so Register keeps retrying.
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)

	orig := sleepFn
	sleepFn = func(d time.Duration) { time.Sleep(10 * time.Millisecond) }
	defer func() { sleepFn = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Register(ctx, testConfig(), "key")
	}()

	<-started
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Register did not return after context cancellation")
	}
}
