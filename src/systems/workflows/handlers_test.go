package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// testDBReady is set in TestMain when the in-memory test DB is migrated. The
// dequeue's FOR UPDATE SKIP LOCKED and the run timestamps are dialect-portable
// (skipLocked + CURRENT_TIMESTAMP), so the suite runs on hermetic sqlite.
var testDBReady bool

// requireDB skips a test when the test database failed to initialise.
func requireDB(t *testing.T) {
	t.Helper()
	if !testDBReady {
		t.Skip("workflows test database not available")
	}
}

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

	// Hermetic in-memory sqlite (mirrors gatekeeper) — no external Postgres.
	conn, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err == nil {
		if migrateErr := conn.AutoMigrate(&Step{}, &Workflow{}, &WorkflowRun{}, &WorkflowStepRun{}); migrateErr == nil {
			dbInitMu.Lock()
			gormDB = conn
			gormDBRead = conn
			dbInitMu.Unlock()
			testDBReady = true
		}
	}

	os.Exit(m.Run())
}

// ── checkGatekeeper ───────────────────────────────────────────────────────────

func TestCheckGatekeeper_NoToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
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
	r := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
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
	r := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
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

	r := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
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
	got := substitute("/executions", substContext{inputs: map[string]string{"FOO": "bar"}})
	if got != "/executions" {
		t.Fatalf("got %q, want %q", got, "/executions")
	}
}

func TestSubstitute_Single(t *testing.T) {
	got := substitute("/deployments/${ENV}", substContext{inputs: map[string]string{"ENV": "production"}})
	if got != "/deployments/production" {
		t.Fatalf("got %q, want %q", got, "/deployments/production")
	}
}

func TestSubstitute_Multiple(t *testing.T) {
	got := substitute("${A}-${B}", substContext{inputs: map[string]string{"A": "hello", "B": "world"}})
	if got != "hello-world" {
		t.Fatalf("got %q, want %q", got, "hello-world")
	}
}

func TestSubstitute_UnknownKeyLeftAsIs(t *testing.T) {
	got := substitute("${MISSING}", substContext{inputs: map[string]string{}})
	if got != "${MISSING}" {
		t.Fatalf("got %q, want %q", got, "${MISSING}")
	}
}

func TestSubstitute_EmptyInputs(t *testing.T) {
	got := substitute("${KEY}", substContext{})
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
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
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
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
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
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
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
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
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
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("expected unknown action error, got %v", err)
	}
}

func TestExecuteStep_HTTPUnknownService(t *testing.T) {
	pool := &WorkerPool{}
	step := Step{Action: ActionHTTP, With: map[string]any{"service": "no-such-svc", "path": "/foo"}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
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
	res, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Errorf("output = %q, missing expected content", res.Output)
	}
}

// TestExecuteAction_ThreadsForgeMemory verifies a forge-backed async step carries
// the execution's memory_used_mb/memory_limit_mb (read from the poll response)
// into the step result, alongside the normal output.
func TestExecuteAction_ThreadsForgeMemory(t *testing.T) {
	srv := fakeService(t, "forge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"execution_id":"exec-1"}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"completed","stdout":"hi","memory_used_mb":180,"memory_limit_mb":256}`)) //nolint:errcheck
	})
	def := ActionDef{
		Name:       "forge/run",
		ServiceURL: srv.URL,
		Method:     http.MethodPost,
		Path:       "/executions",
		Async: &AsyncConfig{
			IDField:          "execution_id",
			PollPath:         "/executions/{id}",
			PollIntervalSecs: 1,
			StatusField:      "status",
			SuccessStates:    []string{"completed"},
			FailureStates:    []string{"failed"},
			OutputField:      "stdout",
		},
	}

	res, err := (&WorkerPool{}).executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{"image": "alpine"}, "", 0)
	if err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	if res.Output != "hi" {
		t.Errorf("output = %q, want hi", res.Output)
	}
	if res.MemoryUsedMB == nil || *res.MemoryUsedMB != 180 {
		t.Errorf("MemoryUsedMB = %v, want 180", res.MemoryUsedMB)
	}
	if res.MemoryLimitMB == nil || *res.MemoryLimitMB != 256 {
		t.Errorf("MemoryLimitMB = %v, want 256", res.MemoryLimitMB)
	}
}

// TestExecuteAction_NonForgeNoMemory checks an async step whose poll response
// carries no memory fields leaves the memory pointers nil — memory is forge-only.
func TestExecuteAction_NonForgeNoMemory(t *testing.T) {
	srv := fakeService(t, "svc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"id":"job-1"}`)) //nolint:errcheck
			return
		}
		w.Write([]byte(`{"state":"done","result":"ok"}`)) //nolint:errcheck
	})
	def := ActionDef{
		Name: "other/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/jobs",
		Async: &AsyncConfig{IDField: "id", PollPath: "/jobs/{id}", PollIntervalSecs: 1, StatusField: "state", SuccessStates: []string{"done"}, OutputField: "result"},
	}
	res, err := (&WorkerPool{}).executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{}, "", 0)
	if err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	if res.MemoryUsedMB != nil || res.MemoryLimitMB != nil {
		t.Errorf("non-forge step carried memory: used=%v limit=%v", res.MemoryUsedMB, res.MemoryLimitMB)
	}
}

// TestJSONInt64Ptr covers the JSON-number-to-*int64 conversion used to pull memory
// figures out of a decoded poll response (default Unmarshal yields float64).
func TestJSONInt64Ptr(t *testing.T) {
	if p := jsonInt64Ptr(float64(180)); p == nil || *p != 180 {
		t.Errorf("float64(180) -> %v, want 180", p)
	}
	if p := jsonInt64Ptr(nil); p != nil {
		t.Errorf("nil -> %v, want nil", p)
	}
	if p := jsonInt64Ptr("nope"); p != nil {
		t.Errorf("string -> %v, want nil", p)
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
	pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{inputs: map[string]string{"ITEM_ID": "abc-123"}}) //nolint:errcheck
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
	pool.executeStep(context.Background(), newTokenStore("my-token", ""), step, substContext{}) //nolint:errcheck
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
		"body":            map[string]any{"image": "alpine:3.19"},
		"expected_status": float64(201),
	}}
	_, err := pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{})
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
	pool.executeStep(context.Background(), newTokenStore("", ""), step, substContext{inputs: map[string]string{"IMAGE_TAG": "v1.2.3"}}) //nolint:errcheck
	if !strings.Contains(string(capturedBodyBytes), "v1.2.3") {
		t.Errorf("body %q missing substituted value", string(capturedBodyBytes))
	}
}

// ── handler auth (no-DB) ──────────────────────────────────────────────────────

func TestHandleCreateWorkflow_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/pipelines", nil)
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateWorkflow_InvalidBody(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodPost, "/pipelines", bytes.NewBufferString("not-json"))
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
	r := httptest.NewRequest(http.MethodPost, "/pipelines", bytes.NewBufferString(body))
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
	r := httptest.NewRequest(http.MethodPost, "/pipelines", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleTriggerRun_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/pipelines/some-id/runs", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleTriggerRun(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListWorkflows_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
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

// ── canAccessStep ─────────────────────────────────────────────────────────────

func TestCanAccessStep_Owner(t *testing.T) {
	s := Step{CreatedBy: "user-1", OrgID: ""}
	if !canAccessStep(s, "user-1", "") {
		t.Fatal("owner should have access")
	}
}

func TestCanAccessStep_SameOrg(t *testing.T) {
	s := Step{CreatedBy: "user-1", OrgID: "org-x"}
	if !canAccessStep(s, "user-2", "org-x") {
		t.Fatal("same-org user should have access")
	}
}

func TestCanAccessStep_DifferentOrg(t *testing.T) {
	s := Step{CreatedBy: "user-1", OrgID: "org-x"}
	if canAccessStep(s, "user-2", "org-y") {
		t.Fatal("different-org user should not have access")
	}
}

func TestCanAccessStep_NoOrg(t *testing.T) {
	s := Step{CreatedBy: "user-1", OrgID: ""}
	if canAccessStep(s, "user-2", "") {
		t.Fatal("unrelated user with no org should not have access")
	}
}

// ── validateStepRequest (additional cases) ────────────────────────────────────

func TestValidateStepRequest_TimeoutTooLarge(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Name: "t", Action: "forge/run", Timeout: maxTimeout + 1})
	if msg == "" {
		t.Fatal("expected error for timeout exceeding max")
	}
}

func TestValidateStepRequest_NegativeTimeout(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Name: "t", Action: "forge/run", Timeout: -1})
	if msg == "" {
		t.Fatal("expected error for negative timeout")
	}
}

func TestValidateStepRequest_ZeroTimeoutAllowed(t *testing.T) {
	msg := validateStepRequest(createStepRequest{Name: "t", Action: "forge/run", Timeout: 0})
	if msg != "" {
		t.Fatalf("zero timeout should be allowed (means use default), got %q", msg)
	}
}

// ── groupSteps ────────────────────────────────────────────────────────────────

func TestGroupSteps_AllSequential(t *testing.T) {
	steps := []WorkflowStep{
		{Step: Step{StepID: "a"}},
		{Step: Step{StepID: "b"}},
	}
	groups := groupSteps(steps)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	for _, g := range groups {
		if len(g.steps) != 1 {
			t.Fatalf("each group should have 1 step, got %d", len(g.steps))
		}
	}
}

func TestGroupSteps_AllParallel(t *testing.T) {
	pg := 1
	steps := []WorkflowStep{
		{Step: Step{StepID: "a"}, ParallelGroup: &pg},
		{Step: Step{StepID: "b"}, ParallelGroup: &pg},
		{Step: Step{StepID: "c"}, ParallelGroup: &pg},
	}
	groups := groupSteps(steps)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	if len(groups[0].steps) != 3 {
		t.Fatalf("want 3 steps in group, got %d", len(groups[0].steps))
	}
}

func TestGroupSteps_Mixed(t *testing.T) {
	pg := 1
	steps := []WorkflowStep{
		{Step: Step{StepID: "seq1"}},
		{Step: Step{StepID: "p1"}, ParallelGroup: &pg},
		{Step: Step{StepID: "p2"}, ParallelGroup: &pg},
		{Step: Step{StepID: "seq2"}},
	}
	groups := groupSteps(steps)
	if len(groups) != 3 {
		t.Fatalf("want 3 groups (seq, parallel, seq), got %d", len(groups))
	}
	// Assert membership, not just sizes: a bug that swapped which steps landed
	// in which group while preserving group sizes must still fail.
	if len(groups[0].steps) != 1 || groups[0].steps[0].StepID != "seq1" {
		t.Errorf("group[0] should be [seq1], got %v", stepIDsOf(groups[0]))
	}
	if ids := stepIDsOf(groups[1]); len(ids) != 2 || ids[0] != "p1" || ids[1] != "p2" {
		t.Errorf("group[1] should be [p1 p2], got %v", ids)
	}
	if len(groups[2].steps) != 1 || groups[2].steps[0].StepID != "seq2" {
		t.Errorf("group[2] should be [seq2], got %v", stepIDsOf(groups[2]))
	}
}

func stepIDsOf(g stepGroup) []string {
	ids := make([]string, len(g.steps))
	for i, s := range g.steps {
		ids[i] = s.StepID
	}
	return ids
}

// ── substituteWith ────────────────────────────────────────────────────────────

func TestSubstituteWith_StringValue(t *testing.T) {
	with := map[string]any{"tag": "${IMAGE_TAG}"}
	result := substituteWith(with, substContext{inputs: map[string]string{"IMAGE_TAG": "v1.2.3"}})
	if result["tag"] != "v1.2.3" {
		t.Fatalf("got %q, want v1.2.3", result["tag"])
	}
}

func TestSubstituteWith_NestedMap(t *testing.T) {
	with := map[string]any{"inner": map[string]any{"key": "${VAL}"}}
	result := substituteWith(with, substContext{inputs: map[string]string{"VAL": "hello"}})
	inner, _ := result["inner"].(map[string]any)
	if inner["key"] != "hello" {
		t.Fatalf("nested substitution failed: got %v", inner["key"])
	}
}

func TestSubstituteWith_SliceValues(t *testing.T) {
	with := map[string]any{"items": []any{"${A}", "${B}"}}
	result := substituteWith(with, substContext{inputs: map[string]string{"A": "x", "B": "y"}})
	items, _ := result["items"].([]any)
	if len(items) != 2 || items[0] != "x" || items[1] != "y" {
		t.Fatalf("slice substitution failed: %v", items)
	}
}

func TestSubstituteWith_NoInputs(t *testing.T) {
	with := map[string]any{"k": "${V}"}
	result := substituteWith(with, substContext{})
	// No inputs → with map returned as-is (unmodified).
	if result["k"] != "${V}" {
		t.Fatalf("got %q, want ${V}", result["k"])
	}
}

// ── verifyHooksTrigger ────────────────────────────────────────────────────────

func TestVerifyHooksTrigger_NoKey(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = ""
	defer func() { hooksTriggerKey = orig }()
	if verifyHooksTrigger("wf", "user", "tok", "123") {
		t.Fatal("expected false when key is empty")
	}
}

func TestVerifyHooksTrigger_InvalidTimestamp(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "secret"
	defer func() { hooksTriggerKey = orig }()
	if verifyHooksTrigger("wf", "user", "tok", "not-a-number") {
		t.Fatal("expected false for non-numeric timestamp")
	}
}

func TestVerifyHooksTrigger_ExpiredTimestamp(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "secret"
	defer func() { hooksTriggerKey = orig }()
	oldTS := fmt.Sprintf("%d", time.Now().Unix()-60)
	if verifyHooksTrigger("wf", "user", "tok", oldTS) {
		t.Fatal("expected false for timestamp older than 30 s")
	}
}

func TestVerifyHooksTrigger_WrongSignature(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "secret"
	defer func() { hooksTriggerKey = orig }()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	if verifyHooksTrigger("wf", "user", "deadbeef", ts) {
		t.Fatal("expected false for wrong signature")
	}
}

func TestVerifyHooksTrigger_ValidSignature(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "test-key-123"
	defer func() { hooksTriggerKey = orig }()

	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks:%s:%s:%s", "wf-id", "user-id", ts)
	token := hex.EncodeToString(mac.Sum(nil))

	if !verifyHooksTrigger("wf-id", "user-id", token, ts) {
		t.Fatal("expected true for valid signature")
	}
}

// ── step handler auth (no-DB) ─────────────────────────────────────────────────

func TestHandleCreateStep_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/steps", nil)
	w := httptest.NewRecorder()
	handleCreateStep(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateStep_InvalidBody(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodPost, "/steps", bytes.NewBufferString("not-json"))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateStep(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateStep_MissingName(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"action":"forge/run"}`
	r := httptest.NewRequest(http.MethodPost, "/steps", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateStep(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "name") {
		t.Errorf("expected 'name' in error, got %q", w.Body.String())
	}
}

func TestHandleCreateStep_MissingAction(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"name":"build"}`
	r := httptest.NewRequest(http.MethodPost, "/steps", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateStep(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "action") {
		t.Errorf("expected 'action' in error, got %q", w.Body.String())
	}
}

func TestHandleCreateStep_HTTPMissingService(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"name":"call","action":"http","with":{"path":"/foo"}}`
	r := httptest.NewRequest(http.MethodPost, "/steps", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateStep(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateStep_HTTPMissingPath(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	body := `{"name":"call","action":"http","with":{"service":"forge"}}`
	r := httptest.NewRequest(http.MethodPost, "/steps", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateStep(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleListSteps_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/steps", nil)
	w := httptest.NewRecorder()
	handleListSteps(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetStep_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/steps/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleGetStep(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleUpdateStep_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/steps/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleUpdateStep(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteStep_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/steps/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleDeleteStep(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── workflow handler auth (no-DB) ─────────────────────────────────────────────

func TestHandleGetWorkflow_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/pipelines/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleGetWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleUpdateWorkflow_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/pipelines/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleUpdateWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteWorkflow_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/pipelines/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleDeleteWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── run handler auth (no-DB) ──────────────────────────────────────────────────

func TestHandleListRuns_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/runs", nil)
	w := httptest.NewRecorder()
	handleListRuns(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCancelRun_Unauthorized(t *testing.T) {
	pool := newWorkerPool()
	handler := handleCancelRun(pool)
	r := httptest.NewRequest(http.MethodDelete, "/runs/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── internal endpoint auth (no-DB) ───────────────────────────────────────────

func TestHandleInternalTriggerRun_InvalidBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/internal/workflows/wf-1/runs", bytes.NewBufferString("bad"))
	r.SetPathValue("id", "wf-1")
	w := httptest.NewRecorder()
	handleInternalTriggerRun(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleInternalTriggerRun_NoKey(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = ""
	defer func() { hooksTriggerKey = orig }()

	body := `{"triggered_by":"user-1","org_id":"org-1"}`
	r := httptest.NewRequest(http.MethodPost, "/internal/workflows/wf-1/runs", bytes.NewBufferString(body))
	r.SetPathValue("id", "wf-1")
	w := httptest.NewRecorder()
	handleInternalTriggerRun(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalTriggerRun_WrongSignature(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = "test-key"
	defer func() { hooksTriggerKey = orig }()

	body := `{"triggered_by":"user-1","org_id":"org-1"}`
	r := httptest.NewRequest(http.MethodPost, "/internal/workflows/wf-1/runs", bytes.NewBufferString(body))
	r.SetPathValue("id", "wf-1")
	r.Header.Set("X-Hooks-Token", "wrongtoken")
	r.Header.Set("X-Hooks-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	w := httptest.NewRecorder()
	handleInternalTriggerRun(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalGetWorkflow_NoKey(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = ""
	defer func() { hooksTriggerKey = orig }()

	r := httptest.NewRequest(http.MethodGet, "/internal/workflows/wf-1", nil)
	r.SetPathValue("id", "wf-1")
	w := httptest.NewRecorder()
	handleInternalGetWorkflow(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleInternalGetRun_NoKey(t *testing.T) {
	orig := hooksTriggerKey
	hooksTriggerKey = ""
	defer func() { hooksTriggerKey = orig }()

	r := httptest.NewRequest(http.MethodGet, "/internal/runs/run-1", nil)
	r.SetPathValue("id", "run-1")
	w := httptest.NewRecorder()
	handleInternalGetRun(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── handleCatalogRefresh ──────────────────────────────────────────────────────

func TestHandleCatalogRefresh_NoKeyConfigured(t *testing.T) {
	orig := registryNotifyKey
	registryNotifyKey = ""
	defer func() { registryNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/catalog/refresh", nil)
	r.Header.Set("X-Service-Key", "registry:somekey")
	w := httptest.NewRecorder()
	handleCatalogRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 when WORKFLOWS_NOTIFY_KEY is empty", w.Code)
	}
}

func TestHandleCatalogRefresh_WrongKey(t *testing.T) {
	orig := registryNotifyKey
	registryNotifyKey = "correct-key"
	defer func() { registryNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/catalog/refresh", nil)
	r.Header.Set("X-Service-Key", "registry:wrong-key")
	w := httptest.NewRecorder()
	handleCatalogRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for wrong key", w.Code)
	}
}

func TestHandleCatalogRefresh_MissingHeader(t *testing.T) {
	orig := registryNotifyKey
	registryNotifyKey = "correct-key"
	defer func() { registryNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/catalog/refresh", nil)
	w := httptest.NewRecorder()
	handleCatalogRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 when X-Service-Key header is absent", w.Code)
	}
}

func TestHandleCatalogRefresh_ValidKey(t *testing.T) {
	orig := registryNotifyKey
	registryNotifyKey = "correct-key"
	defer func() { registryNotifyKey = orig }()

	r := httptest.NewRequest(http.MethodPost, "/internal/catalog/refresh", nil)
	r.Header.Set("X-Service-Key", "registry:correct-key")
	w := httptest.NewRecorder()
	handleCatalogRefresh(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 for valid key", w.Code)
	}
}

// ── handleListActions (no-DB, returns empty catalog) ─────────────────────────

func TestHandleListActions_EmptyCatalog(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodGet, "/actions", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleListActions(w, r)
	// No DB required: the catalog is an in-memory map seeded at startup.
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
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

// ── tokenStore ────────────────────────────────────────────────────────────────

func TestTokenStore_GetToken(t *testing.T) {
	ts := newTokenStore("tok1", "sid1")
	if got := ts.getToken(); got != "tok1" {
		t.Fatalf("got %q, want tok1", got)
	}
}

func TestTokenStore_GetSessionID(t *testing.T) {
	ts := newTokenStore("tok1", "sid1")
	if got := ts.getSessionID(); got != "sid1" {
		t.Fatalf("got %q, want sid1", got)
	}
}

func TestTokenStore_Swap_ReturnsOldSessionID(t *testing.T) {
	ts := newTokenStore("tok1", "sid1")
	oldSID := ts.swap("tok2", "sid2")
	if oldSID != "sid1" {
		t.Fatalf("swap returned old session ID %q, want sid1", oldSID)
	}
}

func TestTokenStore_Swap_UpdatesValues(t *testing.T) {
	ts := newTokenStore("tok1", "sid1")
	ts.swap("tok2", "sid2")
	if got := ts.getToken(); got != "tok2" {
		t.Fatalf("after swap token = %q, want tok2", got)
	}
	if got := ts.getSessionID(); got != "sid2" {
		t.Fatalf("after swap session ID = %q, want sid2", got)
	}
}

// ── rotationInterval ─────────────────────────────────────────────────────────

func TestRotationInterval_InBounds(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := rotationInterval()
		if d < 30*time.Minute || d >= 60*time.Minute {
			t.Fatalf("rotationInterval() = %v, want in [30m, 60m)", d)
		}
	}
}

// io_read is a test helper to read an http.Request body.
func io_read(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	buf.ReadFrom(r.Body) //nolint:errcheck
	return buf.Bytes(), nil
}
