package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeGatekeeper spins up a test server that always returns the given status
// and body, overriding the package-level gatekeeperURL for the test duration.
func fakeGatekeeper(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperClient.URL
	gatekeeperClient.URL = srv.URL
	serviceURLs["gatekeeper"] = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		delete(serviceURLs, "gatekeeper")
		srv.Close()
	})
}

// fakeService spins up a test HTTP server and registers it under name in serviceURLs.
func fakeService(t *testing.T, name string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	serviceURLs[name] = srv.URL
	t.Cleanup(func() {
		delete(serviceURLs, name)
		srv.Close()
	})
	return srv
}

func TestMain(m *testing.M) {
	initMetrics()
	httpClient = initHTTPClient()
	gatekeeperClient = newGatekeeperClient()
	os.Exit(m.Run())
}

// ── checkGatekeeper ───────────────────────────────────────────────────────────

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listWorkflow", "workflows/workflows")
	if ok {
		t.Fatal("expected ok=false with no Bearer token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestCheckGatekeeper_Authorized(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-abc","org_id":"org-1"}`)
	r := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	id, org, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listWorkflow", "workflows/workflows")
	if !ok {
		t.Fatalf("expected ok=true (status %d: %s)", w.Code, w.Body.String())
	}
	if id != "user-abc" {
		t.Fatalf("got user_id %q, want %q", id, "user-abc")
	}
	if org != "org-1" {
		t.Fatalf("got org_id %q, want %q", org, "org-1")
	}
}

func TestCheckGatekeeper_Forbidden(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":false,"user_id":"user-abc"}`)
	r := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listWorkflow", "workflows/workflows")
	if ok {
		t.Fatal("expected ok=false when not authorized")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestCheckGatekeeper_GatekeeperDown(t *testing.T) {
	orig := gatekeeperClient.URL
	gatekeeperClient.URL = "http://127.0.0.1:1" // nothing listening
	t.Cleanup(func() { gatekeeperClient.URL = orig })

	r := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	_, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listWorkflow", "workflows/workflows")
	if ok {
		t.Fatal("expected ok=false when gatekeeper is unreachable")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", w.Code)
	}
}

// ── validateStepRequest ───────────────────────────────────────────────────────

func TestValidateStepRequest_MissingName(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Action: "forge/run", With: map[string]any{"image": "ubuntu:22.04", "run": "go test ./..."}})
	if !strings.Contains(msg, "name") {
		t.Fatalf("expected name error, got %q", msg)
	}
}

func TestValidateStepRequest_MissingAction(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Name: "build"})
	if !strings.Contains(msg, "action") {
		t.Fatalf("expected action error, got %q", msg)
	}
}

func TestValidateStepRequest_CatalogActionValid(t *testing.T) {
	// Catalog-registered actions (e.g. forge/run, tickets/create) are accepted at
	// step creation time; their With fields are validated at execution time.
	msg := validateStepRequest(createStepRequest{Name: "build", Action: "forge/run", With: map[string]any{"image": "ubuntu:22.04", "run": "go test ./..."}})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
}

func TestValidateStepRequest_UnknownActionAccepted(t *testing.T) {
	// Any non-empty action string is accepted at step creation time; the worker
	// validates against the catalog at execution time.
	msg := validateStepRequest(createStepRequest{Name: "build", Action: "some/future-service"})
	if msg != "" {
		t.Fatalf("expected no error for unknown action, got %q", msg)
	}
}

func TestValidateStepRequest_HTTPMissingService(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Name: "call", Action: ActionHTTP, With: map[string]any{"path": "/foo"}})
	if !strings.Contains(msg, "service") {
		t.Fatalf("expected service error, got %q", msg)
	}
}

func TestValidateStepRequest_HTTPMissingPath(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Name: "call", Action: ActionHTTP, With: map[string]any{"service": "forge"}})
	if !strings.Contains(msg, "path") {
		t.Fatalf("expected path error, got %q", msg)
	}
}

// ── substitute ────────────────────────────────────────────────────────────────

func TestSubstitute_NoPlaceholders(t *testing.T) {
	got := substitute("/executions", map[string]string{"FOO": "bar"})
	if got != "/executions" {
		t.Fatalf("got %q, want %q", got, "/executions")
	}
}

func TestSubstitute_Single(t *testing.T) {
	got := substitute("/deployments/${ENV}", map[string]string{"ENV": "production"})
	if got != "/deployments/production" {
		t.Fatalf("got %q, want %q", got, "/deployments/production")
	}
}

func TestSubstitute_Multiple(t *testing.T) {
	got := substitute("${A}-${B}", map[string]string{"A": "hello", "B": "world"})
	if got != "hello-world" {
		t.Fatalf("got %q, want %q", got, "hello-world")
	}
}

func TestSubstitute_UnknownKeyLeftAsIs(t *testing.T) {
	got := substitute("${MISSING}", map[string]string{})
	if got != "${MISSING}" {
		t.Fatalf("got %q, want %q", got, "${MISSING}")
	}
}

func TestSubstitute_EmptyInputs(t *testing.T) {
	got := substitute("${KEY}", nil)
	if got != "${KEY}" {
		t.Fatalf("got %q, want %q", got, "${KEY}")
	}
}

// ── executeHTTP (http action) ─────────────────────────────────────────────────

func TestExecuteHTTP_2xxSuccess(t *testing.T) {
	pool := &WorkerPool{}
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	step := Step{Action: ActionHTTP, With: map[string]any{"service": "svc", "path": "/ok", "method": "GET"}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestExecuteHTTP_4xxFails(t *testing.T) {
	pool := &WorkerPool{}
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	step := Step{Action: ActionHTTP, With: map[string]any{"service": "svc", "path": "/bad", "method": "GET"}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err == nil {
		t.Fatal("expected error for 4xx response")
	}
}

func TestExecuteHTTP_ExactMatchSuccess(t *testing.T) {
	pool := &WorkerPool{}
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	step := Step{Action: ActionHTTP, With: map[string]any{
		"service": "svc", "path": "/create", "method": "POST", "expected_status": float64(201),
	}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err != nil {
		t.Fatalf("expected success for exact match 201, got %v", err)
	}
}

func TestExecuteHTTP_ExactMatchFail(t *testing.T) {
	pool := &WorkerPool{}
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	step := Step{Action: ActionHTTP, With: map[string]any{
		"service": "svc", "path": "/create", "method": "POST", "expected_status": float64(201),
	}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err == nil {
		t.Fatal("expected error when status 200 != expected 201")
	}
}

// ── initServices ─────────────────────────────────────────────────────────────

func TestInitServices_GatekeeperAlwaysPresent(t *testing.T) {
	origURLs := serviceURLs
	serviceURLs = map[string]string{}
	t.Cleanup(func() { serviceURLs = origURLs })

	t.Setenv("SERVICES", "")
	initServices()
	if serviceURLs["gatekeeper"] == "" {
		t.Fatal("gatekeeper should always be registered")
	}
}

func TestInitServices_ParsesMultiple(t *testing.T) {
	origURLs := serviceURLs
	serviceURLs = map[string]string{}
	t.Cleanup(func() { serviceURLs = origURLs })

	t.Setenv("SERVICES", "forge=http://forge:8083,blueprints=http://blueprints:8081")
	initServices()
	if serviceURLs["forge"] != "http://forge:8083" {
		t.Errorf("forge URL = %q, want http://forge:8083", serviceURLs["forge"])
	}
	if serviceURLs["blueprints"] != "http://blueprints:8081" {
		t.Errorf("blueprints URL = %q, want http://blueprints:8081", serviceURLs["blueprints"])
	}
}

func TestInitServices_SkipsMalformed(t *testing.T) {
	origURLs := serviceURLs
	serviceURLs = map[string]string{}
	t.Cleanup(func() { serviceURLs = origURLs })

	t.Setenv("SERVICES", "forge=http://forge:8083,badentry,=noname")
	initServices()
	if _, ok := serviceURLs["forge"]; !ok {
		t.Error("valid entry should be registered")
	}
}

// ── WorkerPool.Cancel ─────────────────────────────────────────────────────────

func TestWorkerPool_Cancel_NotFound(t *testing.T) {
	pool := &WorkerPool{}
	if pool.Cancel("nonexistent-run-id") {
		t.Fatal("expected false for unknown run ID")
	}
}

func TestWorkerPool_Cancel_Found(t *testing.T) {
	pool := &WorkerPool{}
	called := false
	pool.cancels.Store("run-123", context.CancelFunc(func() { called = true }))
	if !pool.Cancel("run-123") {
		t.Fatal("expected true for known run ID")
	}
	if !called {
		t.Fatal("cancel func was not called")
	}
}

// ── executeStep (http action) ─────────────────────────────────────────────────

func TestExecuteStep_UnknownAction(t *testing.T) {
	pool := &WorkerPool{}
	step := Step{Action: "unknown/action"}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("expected unknown action error, got %v", err)
	}
}

func TestExecuteStep_HTTPUnknownService(t *testing.T) {
	pool := &WorkerPool{}
	step := Step{Action: ActionHTTP, With: map[string]any{"service": "no-such-svc", "path": "/foo"}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("expected unknown service error, got %v", err)
	}
}

func TestExecuteStep_HTTPSuccess(t *testing.T) {
	pool := &WorkerPool{}
	fakeService(t, "mysvc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	})

	step := Step{Action: ActionHTTP, With: map[string]any{"service": "mysvc", "path": "/health", "method": "GET"}}
	body, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("body = %q, missing expected content", body)
	}
}

func TestExecuteStep_HTTPSubstitutesInputsInPath(t *testing.T) {
	pool := &WorkerPool{}
	var capturedPath string
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	step := Step{Action: ActionHTTP, With: map[string]any{"service": "svc", "path": "/items/${ITEM_ID}", "method": "GET"}}
	pool.executeStep(context.Background(), newTokenStore("", ""), step, map[string]string{"ITEM_ID": "abc-123"}) //nolint:errcheck
	if capturedPath != "/items/abc-123" {
		t.Errorf("path = %q, want /items/abc-123", capturedPath)
	}
}

func TestExecuteStep_HTTPForwardsAuthHeader(t *testing.T) {
	pool := &WorkerPool{}
	var capturedAuth string
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})

	step := Step{Action: ActionHTTP, With: map[string]any{"service": "svc", "path": "/endpoint", "method": "GET"}}
	pool.executeStep(context.Background(), newTokenStore("my-token", ""), step, nil) //nolint:errcheck
	if capturedAuth != "Bearer my-token" {
		t.Errorf("Authorization = %q, want \"Bearer my-token\"", capturedAuth)
	}
}

func TestExecuteStep_HTTPSendsBody(t *testing.T) {
	pool := &WorkerPool{}
	var capturedBody []byte
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io_read(r)
		w.WriteHeader(http.StatusCreated)
	})

	step := Step{Action: ActionHTTP, With: map[string]any{
		"service": "svc", "path": "/run", "method": "POST",
		"body": map[string]any{"image": "alpine:3.19"},
		"expected_status": float64(201),
	}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(capturedBody), "alpine:3.19") {
		t.Errorf("body %q missing expected content", string(capturedBody))
	}
}

func TestExecuteStep_HTTPSubstitutesInputsInBody(t *testing.T) {
	pool := &WorkerPool{}
	var capturedBodyBytes []byte
	fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		capturedBodyBytes, _ = io_read(r)
		w.WriteHeader(http.StatusOK)
	})

	step := Step{Action: ActionHTTP, With: map[string]any{
		"service": "svc", "path": "/deploy", "method": "POST",
		"body": map[string]any{"tag": "${IMAGE_TAG}"},
	}}
	pool.executeStep(context.Background(), newTokenStore("", ""), step, map[string]string{"IMAGE_TAG": "v1.2.3"}) //nolint:errcheck
	if !strings.Contains(string(capturedBodyBytes), "v1.2.3") {
		t.Errorf("body %q missing substituted value", string(capturedBodyBytes))
	}
}

// ── handler auth (no-DB) ──────────────────────────────────────────────────────

func TestHandleCreateWorkflow_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/workflows", nil)
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateWorkflow_InvalidBody(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString("not-json"))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateWorkflow_MissingName(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"steps":[{"step_id":"some-id"}]}`
	r := httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateWorkflow_StepWithoutID(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"name":"my-wf","steps":[{}]}`
	r := httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleTriggerRun_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/workflows/some-id/runs", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleTriggerRun(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListWorkflows_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	w := httptest.NewRecorder()
	handleListWorkflows(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetRun_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/runs/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleGetRun(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── envOrDefault / secret ────────────────────────────────────────────────────

func TestEnvOrDefault_Set(t *testing.T) {
	t.Setenv("WF_TEST_KEY", "myvalue")
	if got := envOrDefault("WF_TEST_KEY", "default"); got != "myvalue" {
		t.Fatalf("got %q, want %q", got, "myvalue")
	}
}

func TestEnvOrDefault_Fallback(t *testing.T) {
	os.Unsetenv("WF_ABSENT_KEY")
	if got := envOrDefault("WF_ABSENT_KEY", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

func TestSecret_FromEnv(t *testing.T) {
	t.Setenv("WF_MY_SECRET", "direct-value")
	if got := secret("WF_MY_SECRET"); got != "direct-value" {
		t.Fatalf("got %q, want %q", got, "direct-value")
	}
}

func TestSecret_FromFile(t *testing.T) {
	f, err := os.CreateTemp("", "wf-secret-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("file-secret-value")
	f.Close()
	t.Setenv("WF_FILE_SECRET_FILE", f.Name())
	os.Unsetenv("WF_FILE_SECRET")
	if got := secret("WF_FILE_SECRET"); got != "file-secret-value" {
		t.Fatalf("got %q, want %q", got, "file-secret-value")
	}
}

// ── statusResponseWriter ─────────────────────────────────────────────────────

func TestStatusResponseWriter_WriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusNotFound {
		t.Fatalf("rw.status: got %d, want 404", rw.status)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder: got %d, want 404", w.Code)
	}
}

// io_read is a test helper to read an http.Request body.
func io_read(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	buf.ReadFrom(r.Body) //nolint:errcheck
	return buf.Bytes(), nil
}
