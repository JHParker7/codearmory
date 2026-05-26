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

// credentialsResponse returns a valid bootstrap 200 response body.
func credentialsResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"client_id": "cid-123", "client_secret": "sec-abc"})
}

func TestRegisterNoCredentials(t *testing.T) {
	// Should return immediately without making any HTTP calls when neither
	// SERVICE_KEY nor CLIENT_ID+CLIENT_SECRET are set.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")

	Register(context.Background(), testConfig(), "")
	if called {
		t.Error("expected no HTTP call when no credentials are set")
	}
}

func TestRegisterBootstrap_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentialsResponse(w)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_URL", "http://testsvc:8080")
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	Register(ctx, testConfig(), "mykey")
}

func TestRegisterBootstrap_PayloadContents(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		credentialsResponse(w)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_URL", "http://testsvc:8080")
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")

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

func TestRegisterCredentialMode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_URL", "http://testsvc:8080")
	t.Setenv("CLIENT_ID", "cid-xyz")
	t.Setenv("CLIENT_SECRET", "sec-xyz")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	Register(ctx, testConfig(), "") // no service key needed in credential mode
}

func TestRegisterCredentialMode_PayloadContents(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_URL", "http://testsvc:8080")
	t.Setenv("CLIENT_ID", "cid-xyz")
	t.Setenv("CLIENT_SECRET", "sec-xyz")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	Register(ctx, testConfig(), "should-be-ignored")

	var payload map[string]any
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatalf("invalid JSON payload: %v", err)
	}
	if payload["client_id"] != "cid-xyz" {
		t.Errorf("client_id = %v, want cid-xyz", payload["client_id"])
	}
	if payload["client_secret"] != "sec-xyz" {
		t.Errorf("client_secret = %v, want sec-xyz", payload["client_secret"])
	}
	if _, ok := payload["service_key"]; ok {
		t.Error("service_key should not be present in credential mode payload")
	}
}

func TestRegisterServiceNameEnvOverride(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		gotName, _ = payload["name"].(string)
		credentialsResponse(w)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("SERVICE_NAME", "overridden")
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")

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
			credentialsResponse(w)
		}
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")

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
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_URL", srv.URL)
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")

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
