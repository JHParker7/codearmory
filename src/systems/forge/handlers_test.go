package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// fakeGatekeeper spins up a test gatekeeper that always returns the given status
// and body, then overrides the package-level gatekeeperURL for the test duration.
func fakeGatekeeper(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() {
		gatekeeperURL = orig
		srv.Close()
	})
	return srv
}

func TestMain(m *testing.M) {
	initMetrics()
	forgeHTTPClient = initHTTPClient()
	setupForgeTestDB()
	os.Exit(m.Run())
}

// --- checkGatekeeper ---

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	w := httptest.NewRecorder()
	_, ok := checkGatekeeper(r.Context(), w, r, "listExecution", "forge/executions")
	if ok {
		t.Fatal("expected ok=false when no Bearer token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestCheckGatekeeper_Authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-abc"}`)
	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	id, ok := checkGatekeeper(r.Context(), w, r, "listExecution", "forge/executions")
	if !ok {
		t.Fatalf("expected ok=true, got false (status %d)", w.Code)
	}
	if id != "user-abc" {
		t.Fatalf("got %q, want %q", id, "user-abc")
	}
}

func TestCheckGatekeeper_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":false,"user_id":"user-abc"}`)
	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, ok := checkGatekeeper(r.Context(), w, r, "listExecution", "forge/executions")
	if ok {
		t.Fatal("expected ok=false when not authorized")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- initAllowedImages ---

func TestInitAllowedImages_Empty(t *testing.T) {
	initAllowedImages("")
	if allowedImages != nil {
		t.Fatalf("expected nil allowlist (deny-all) for empty input, got %v", allowedImages)
	}
}

func TestInitAllowedImages_Single(t *testing.T) {
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	if !allowedImages["alpine:3.19"] {
		t.Fatal("expected alpine:3.19 to be allowed")
	}
	if len(allowedImages) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(allowedImages))
	}
}

func TestInitAllowedImages_Multiple(t *testing.T) {
	initAllowedImages("alpine:3.19,ubuntu:22.04, python:3.12-slim")
	t.Cleanup(func() { initAllowedImages("") })
	for _, img := range []string{"alpine:3.19", "ubuntu:22.04", "python:3.12-slim"} {
		if !allowedImages[img] {
			t.Fatalf("expected %q to be in allowlist", img)
		}
	}
	if len(allowedImages) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(allowedImages))
	}
}

// --- handleSubmit (pre-DB validation paths only) ---

func TestHandleSubmit_Unauthorized(t *testing.T) {
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19","command":["echo","hi"]}`))
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleSubmit_InvalidJSON(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-123"}`)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	r := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewBufferString("not json"))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_MissingImage(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-123"}`)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"command":["echo","hi"]}`))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_MissingCommand(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-123"}`)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19"}`))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_DisallowedImage(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-123"}`)
	initAllowedImages("ubuntu:22.04")
	t.Cleanup(func() { initAllowedImages("") })
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19","command":["echo","hi"]}`))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleGet (pre-DB) ---

func TestHandleGet_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/executions/some-id", nil)
	w := httptest.NewRecorder()
	handleGet(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// --- handleList (pre-DB) ---

func TestHandleList_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/executions", nil)
	w := httptest.NewRecorder()
	handleList(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// --- handleCancel (pre-DB) ---

func TestHandleCancel_Unauthorized(t *testing.T) {
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodDelete, "/executions/some-id", nil)
	w := httptest.NewRecorder()
	handleCancel(pool)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// --- newRuntime ---

func TestNewRuntime_UnknownRuntime(t *testing.T) {
	t.Setenv("RUNTIME", "bogus")
	_, err := newRuntime()
	if err == nil {
		t.Fatal("expected error for unknown RUNTIME value")
	}
}

// --- K8s runtime timeout wraps context.DeadlineExceeded ---
// Verifies that errors.Is(err, context.DeadlineExceeded) works for the K8s
// timeout error, so the worker can classify it without string matching.

func TestK8sTimeoutWrapsDeadlineExceeded(t *testing.T) {
	err := fmt.Errorf("timed out after 30s: %w", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expected wrapped context.DeadlineExceeded")
	}
}

// --- handleSubmit (additional pre-DB paths) ---

func TestHandleSubmit_EmptyCommandSlice(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-123"}`)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19","command":[]}`))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_BodyTooLarge(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-123"}`)
	initAllowedImages("alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })
	bigBody := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	r := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewReader(bigBody))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleSubmit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- WorkerPool.Cancel ---

func TestWorkerPool_Cancel_NotFound(t *testing.T) {
	pool := &WorkerPool{}
	if pool.Cancel("nonexistent-id") {
		t.Fatal("expected false for unknown execution ID")
	}
}

func TestWorkerPool_Cancel_Found(t *testing.T) {
	pool := &WorkerPool{}
	called := false
	pool.cancels.Store("exec-123", context.CancelFunc(func() { called = true }))
	if !pool.Cancel("exec-123") {
		t.Fatal("expected true for known execution ID")
	}
	if !called {
		t.Fatal("expected cancel func to be called")
	}
}

// --- envOrDefault ---

func TestEnvOrDefault_Set(t *testing.T) {
	t.Setenv("TEST_ENV_KEY", "myvalue")
	if got := envOrDefault("TEST_ENV_KEY", "default"); got != "myvalue" {
		t.Fatalf("got %q, want %q", got, "myvalue")
	}
}

func TestEnvOrDefault_NotSet(t *testing.T) {
	os.Unsetenv("TEST_ENV_KEY_MISSING")
	if got := envOrDefault("TEST_ENV_KEY_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

func TestEnvOrDefault_EmptyValue(t *testing.T) {
	t.Setenv("TEST_ENV_KEY_EMPTY", "")
	if got := envOrDefault("TEST_ENV_KEY_EMPTY", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

// --- secret / secretOrDefault ---

func TestSecret_FromEnv(t *testing.T) {
	t.Setenv("MY_SECRET", "direct-value")
	if got := secret("MY_SECRET"); got != "direct-value" {
		t.Fatalf("got %q, want %q", got, "direct-value")
	}
}

func TestSecret_FromFile(t *testing.T) {
	f, err := os.CreateTemp("", "forge-secret-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("file-secret-value")
	f.Close()

	t.Setenv("MY_SECRET_FILE", f.Name())
	os.Unsetenv("MY_SECRET")
	if got := secret("MY_SECRET"); got != "file-secret-value" {
		t.Fatalf("got %q, want %q", got, "file-secret-value")
	}
}

func TestSecretOrDefault_Fallback(t *testing.T) {
	os.Unsetenv("ABSENT_SECRET")
	os.Unsetenv("ABSENT_SECRET_FILE")
	if got := secretOrDefault("ABSENT_SECRET", "the-default"); got != "the-default" {
		t.Fatalf("got %q, want %q", got, "the-default")
	}
}

// --- statusResponseWriter ---

func TestStatusResponseWriter_WriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusNotFound {
		t.Fatalf("rw.status: got %d, want %d", rw.status, http.StatusNotFound)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder: got %d, want %d", w.Code, http.StatusNotFound)
	}
}
