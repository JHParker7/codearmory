package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	initMetrics()
	setupForgeTestDB()
	os.Exit(m.Run())
}

// testBearerToken builds a JWT-shaped token with the given userID as the sub claim.
// Signature verification is Conductor's job; Forge only decodes the payload.
func testBearerToken(userID string) string {
	payload, _ := json.Marshal(map[string]string{"sub": userID})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return "Bearer eyJhbGciOiJFUzI1NiJ9." + encoded + ".fakesig"
}

// --- extractUserID ---

func TestExtractUserID_Valid(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/executions", nil)
	r.Header.Set("Authorization", testBearerToken("user-123"))
	id, ok := extractUserID(r)
	if !ok {
		t.Fatal("expected ok=true for valid token")
	}
	if id != "user-123" {
		t.Fatalf("got %q, want %q", id, "user-123")
	}
}

func TestExtractUserID_NoHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/executions", nil)
	_, ok := extractUserID(r)
	if ok {
		t.Fatal("expected ok=false for missing Authorization header")
	}
}

func TestExtractUserID_WrongPrefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/executions", nil)
	r.Header.Set("Authorization", "Token sometoken")
	_, ok := extractUserID(r)
	if ok {
		t.Fatal("expected ok=false for non-Bearer prefix")
	}
}

func TestExtractUserID_TwoPartToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/executions", nil)
	r.Header.Set("Authorization", "Bearer header.payload")
	_, ok := extractUserID(r)
	if ok {
		t.Fatal("expected ok=false for JWT with only 2 parts")
	}
}

func TestExtractUserID_InvalidBase64(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/executions", nil)
	r.Header.Set("Authorization", "Bearer header.!!!invalid!!!.sig")
	_, ok := extractUserID(r)
	if ok {
		t.Fatal("expected ok=false for invalid base64 payload")
	}
}

func TestExtractUserID_NoSubClaim(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"iss": "forge"})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	r := httptest.NewRequest(http.MethodPost, "/executions", nil)
	r.Header.Set("Authorization", "Bearer header."+encoded+".sig")
	_, ok := extractUserID(r)
	if ok {
		t.Fatal("expected ok=false for JWT with no sub claim")
	}
}

// --- initAllowedImages ---

func TestInitAllowedImages_Empty(t *testing.T) {
	initAllowedImages("")
	if len(allowedImages) != 0 {
		t.Fatalf("expected empty allowlist for empty input, got %v", allowedImages)
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
	initAllowedImages("")
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19","command":["echo","hi"]}`))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleSubmit_InvalidJSON(t *testing.T) {
	initAllowedImages("")
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewBufferString("not json"))
	r.Header.Set("Authorization", testBearerToken("user-123"))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_MissingImage(t *testing.T) {
	initAllowedImages("")
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"command":["echo","hi"]}`))
	r.Header.Set("Authorization", testBearerToken("user-123"))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_MissingCommand(t *testing.T) {
	initAllowedImages("")
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19"}`))
	r.Header.Set("Authorization", testBearerToken("user-123"))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_DisallowedImage(t *testing.T) {
	initAllowedImages("ubuntu:22.04")
	t.Cleanup(func() { initAllowedImages("") })
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19","command":["echo","hi"]}`))
	r.Header.Set("Authorization", testBearerToken("user-123"))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
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

// --- isTimed ---

func TestIsTimed_TimedOutError(t *testing.T) {
	if !isTimed(errors.New("timed out after 30s")) {
		t.Fatal("expected true for 'timed out' error")
	}
}

func TestIsTimed_OtherError(t *testing.T) {
	if isTimed(errors.New("context canceled")) {
		t.Fatal("expected false for non-timed error")
	}
}

func TestIsTimed_NilError(t *testing.T) {
	if isTimed(nil) {
		t.Fatal("expected false for nil error")
	}
}

func TestIsTimed_ShortMessage(t *testing.T) {
	if isTimed(errors.New("tim")) {
		t.Fatal("expected false for error message shorter than 5 chars")
	}
}

func TestIsTimed_ExactlyFiveChars(t *testing.T) {
	if isTimed(errors.New("timed")) {
		t.Fatal("expected false for 5-char error (len > 5 requires at least 6)")
	}
}

// --- handleSubmit (additional pre-DB paths) ---

func TestHandleSubmit_EmptyCommandSlice(t *testing.T) {
	initAllowedImages("")
	pool := &WorkerPool{}
	r := httptest.NewRequest(http.MethodPost, "/executions",
		bytes.NewBufferString(`{"image":"alpine:3.19","command":[]}`))
	r.Header.Set("Authorization", testBearerToken("user-123"))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmit_BodyTooLarge(t *testing.T) {
	initAllowedImages("")
	pool := &WorkerPool{}
	bigBody := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	r := httptest.NewRequest(http.MethodPost, "/executions", bytes.NewReader(bigBody))
	r.Header.Set("Authorization", testBearerToken("user-123"))
	w := httptest.NewRecorder()
	handleSubmit(pool)(w, r)
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
