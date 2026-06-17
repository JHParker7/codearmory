package main

import (
	"bytes"
	"context"
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

func TestInitAllowedImages_BuildsSortedDedupedList(t *testing.T) {
	initAllowedImages("ubuntu:22.04, alpine:3.19, ubuntu:22.04")
	t.Cleanup(func() { initAllowedImages("") })
	want := []string{"alpine:3.19", "ubuntu:22.04"}
	if len(allowedImageList) != len(want) {
		t.Fatalf("allowedImageList = %v, want %v", allowedImageList, want)
	}
	for i := range want {
		if allowedImageList[i] != want[i] {
			t.Fatalf("allowedImageList = %v, want %v (sorted, deduped)", allowedImageList, want)
		}
	}
}

// --- handleListImages ---

func TestHandleListImages_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/images", nil)
	w := httptest.NewRecorder()
	handleListImages(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleListImages_ReturnsAllowlist(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-1"}`)
	initAllowedImages("ubuntu:22.04,alpine:3.19")
	t.Cleanup(func() { initAllowedImages("") })

	r := httptest.NewRequest(http.MethodGet, "/images", nil)
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleListImages(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "[\"alpine:3.19\",\"ubuntu:22.04\"]\n" {
		t.Fatalf("body = %q, want sorted allowlist array", got)
	}
}

func TestHandleListImages_DenyAll_ReturnsEmptyArray(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-1"}`)
	initAllowedImages("") // deny-all

	r := httptest.NewRequest(http.MethodGet, "/images", nil)
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleListImages(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "[]\n" {
		t.Fatalf("body = %q, want empty array (not null)", got)
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

// --- buildRuntime / defaultRuntimeType ---

func TestBuildRuntime_UnknownType(t *testing.T) {
	_, err := buildRuntime(RuntimeBackend{Name: "x", Type: "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown runtime type")
	}
}

func TestDefaultRuntimeType(t *testing.T) {
	t.Run("unset defaults to docker", func(t *testing.T) {
		t.Setenv("RUNTIME", "")
		if got := defaultRuntimeType(); got != "docker" {
			t.Fatalf("got %q, want docker", got)
		}
	})
	t.Run("passes through set value", func(t *testing.T) {
		t.Setenv("RUNTIME", "kubernetes")
		if got := defaultRuntimeType(); got != "kubernetes" {
			t.Fatalf("got %q, want kubernetes", got)
		}
	})
}

// --- K8s runtime timeout is classified as TimedOut ---
// The k8s runtime wraps context.DeadlineExceeded (runtime_k8s.go) so the worker
// can classify a job timeout via errors.Is rather than string matching. This
// exercises classifyResult with the exact error shape the runtime produces.

func TestK8sTimeoutClassifiedAsTimedOut(t *testing.T) {
	err := fmt.Errorf("timed out after 30s: %w", context.DeadlineExceeded)
	status, res, _ := classifyResult(Execution{}, RunResult{ExitCode: ptr(1)}, err)
	if status != StatusTimedOut {
		t.Fatalf("classifyResult(timeout err) = %q, want %q", status, StatusTimedOut)
	}
	if res.ExitCode != nil {
		t.Errorf("exit code = %v, want nil (a timeout has no real exit code)", *res.ExitCode)
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
	pool.cancels.Store("exec-123", runningExec{cancel: func() { called = true }})
	if !pool.Cancel("exec-123") {
		t.Fatal("expected true for known execution ID")
	}
	if !called {
		t.Fatal("expected cancel func to be called")
	}
}

// TestWorkerPool_Cancel_CallsRuntimeCancel verifies Cancel also drives the
// resolved runtime's Cancel (stop+destroy for proxmox, job delete for k8s).
func TestWorkerPool_Cancel_CallsRuntimeCancel(t *testing.T) {
	pool := &WorkerPool{}
	rt := &fakeRuntime{}
	pool.cancels.Store("exec-9", runningExec{cancel: func() {}, rt: rt})
	if !pool.Cancel("exec-9") {
		t.Fatal("expected true for known execution ID")
	}
	if rt.cancelled != "exec-9" {
		t.Fatalf("runtime Cancel got %q, want exec-9", rt.cancelled)
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
