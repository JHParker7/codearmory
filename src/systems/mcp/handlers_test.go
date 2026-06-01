package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// makeReq builds a CallToolRequest with the given string arguments.
func makeReq(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// text extracts the first TextContent text from a tool result.
func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		return ""
	}
	tc, ok := r.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", r.Content[0])
	}
	return tc.Text
}

// fakeServer returns a test HTTP server that always responds with the given
// status and JSON body, plus a *client pointed at it with a valid token.
func fakeServer(t *testing.T, status int, body string) (*httptest.Server, *client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c := newClient(config{URL: srv.URL, Token: "test-token"})
	return srv, c
}

// ── argStr / argFloat ─────────────────────────────────────────────────────────

func TestArgStr_NilArgs(t *testing.T) {
	if got := argStr(makeReq(nil), "key"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestArgStr_KeyPresent(t *testing.T) {
	if got := argStr(makeReq(map[string]any{"key": "val"}), "key"); got != "val" {
		t.Fatalf("got %q, want val", got)
	}
}

func TestArgStr_KeyAbsent(t *testing.T) {
	if got := argStr(makeReq(map[string]any{}), "missing"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestArgFloat_NilArgs(t *testing.T) {
	if got := argFloat(makeReq(nil), "key"); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestArgFloat_KeyPresent(t *testing.T) {
	if got := argFloat(makeReq(map[string]any{"n": float64(3.14)}), "n"); got != 3.14 {
		t.Fatalf("got %v, want 3.14", got)
	}
}

// ── config.validate ───────────────────────────────────────────────────────────

func TestConfigValidate_EmptyToken(t *testing.T) {
	if err := (config{URL: "http://localhost:8082"}).validate(); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestConfigValidate_WithToken(t *testing.T) {
	if err := (config{URL: "http://localhost:8082", Token: "tok"}).validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ── loadConfig ────────────────────────────────────────────────────────────────

func TestLoadConfig_EnvOverrides(t *testing.T) {
	t.Setenv("CODEARMORY_URL", "http://myhost:9090")
	t.Setenv("CODEARMORY_TOKEN", "env-token")
	cfg := loadConfig()
	if cfg.URL != "http://myhost:9090" {
		t.Fatalf("URL = %q, want http://myhost:9090", cfg.URL)
	}
	if cfg.Token != "env-token" {
		t.Fatalf("Token = %q, want env-token", cfg.Token)
	}
}

func TestLoadConfig_DefaultURL(t *testing.T) {
	os.Unsetenv("CODEARMORY_URL")
	os.Unsetenv("CODEARMORY_TOKEN")
	cfg := loadConfig()
	if cfg.URL == "" {
		t.Fatal("expected a non-empty default URL")
	}
}

// ── ok / apiErr ───────────────────────────────────────────────────────────────

func TestOk_IndentsJSON(t *testing.T) {
	result := ok(json.RawMessage(`{"name":"test"}`))
	if result.IsError {
		t.Fatal("expected non-error result")
	}
	body := text(t, result)
	if !strings.Contains(body, "\n") {
		t.Errorf("expected indented JSON, got %q", body)
	}
}

func TestApiErr_IncludesStatusAndBody(t *testing.T) {
	result := apiErr(404, json.RawMessage(`{"message":"not found"}`))
	if !result.IsError {
		t.Fatal("expected error result")
	}
	body := text(t, result)
	if !strings.Contains(body, "404") {
		t.Errorf("expected status 404 in %q", body)
	}
}

// ── noAuth — all handler groups ───────────────────────────────────────────────

func TestHandleListWorkflows_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleListWorkflows(c)(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(text(t, result), "not authenticated") {
		t.Errorf("expected noAuth error, got isError=%v text=%q", result.IsError, text(t, result))
	}
}

func TestHandleGetWorkflow_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleGetWorkflow(c)(context.Background(), makeReq(map[string]any{"workflow_id": "wf-1"}))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleCreateWorkflow_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleCreateWorkflow(c)(context.Background(), makeReq(nil))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleTriggerRun_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleTriggerRun(c)(context.Background(), makeReq(map[string]any{"workflow_id": "wf-1"}))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleListRuns_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleListRuns(c)(context.Background(), makeReq(nil))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleListTickets_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleListTickets(c)(context.Background(), makeReq(nil))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleCreateTicket_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleCreateTicket(c)(context.Background(), makeReq(map[string]any{"title": "Bug"}))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleListExecutions_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleListExecutions(c)(context.Background(), makeReq(nil))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

func TestHandleCreateRule_NoAuth(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	result, err := handleCreateRule(c)(context.Background(), makeReq(nil))
	if err != nil || !result.IsError {
		t.Fatalf("expected noAuth error")
	}
}

// ── handleCreateWorkflow / handleUpdateWorkflow — invalid steps JSON ───────────

func TestHandleCreateWorkflow_InvalidStepsJSON(t *testing.T) {
	_, c := fakeServer(t, http.StatusCreated, `{}`)
	result, err := handleCreateWorkflow(c)(context.Background(), makeReq(map[string]any{
		"name":  "my-wf",
		"steps": "this is not json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for invalid steps JSON")
	}
	if !strings.Contains(text(t, result), "steps") {
		t.Errorf("expected 'steps' in error, got %q", text(t, result))
	}
}

func TestHandleUpdateWorkflow_InvalidStepsJSON(t *testing.T) {
	_, c := fakeServer(t, http.StatusOK, `{}`)
	result, err := handleUpdateWorkflow(c)(context.Background(), makeReq(map[string]any{
		"workflow_id": "wf-1",
		"name":        "my-wf",
		"steps":       "{bad json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for invalid steps JSON")
	}
}

// ── handleTriggerRun — invalid inputs JSON ────────────────────────────────────

func TestHandleTriggerRun_InvalidInputsJSON(t *testing.T) {
	_, c := fakeServer(t, http.StatusAccepted, `{}`)
	result, err := handleTriggerRun(c)(context.Background(), makeReq(map[string]any{
		"workflow_id": "wf-1",
		"inputs":      "{bad json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for invalid inputs JSON")
	}
	if !strings.Contains(text(t, result), "inputs") {
		t.Errorf("expected 'inputs' in error, got %q", text(t, result))
	}
}

// ── handler success / API error paths ────────────────────────────────────────

func TestHandleListWorkflows_Success(t *testing.T) {
	_, c := fakeServer(t, http.StatusOK, `[{"workflow_id":"wf-1"}]`)
	result, err := handleListWorkflows(c)(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text(t, result))
	}
	if !strings.Contains(text(t, result), "wf-1") {
		t.Errorf("expected response data in result, got %q", text(t, result))
	}
}

func TestHandleListWorkflows_APIError(t *testing.T) {
	_, c := fakeServer(t, http.StatusForbidden, `{"error":"forbidden"}`)
	result, err := handleListWorkflows(c)(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error result for 403 response")
	}
	if !strings.Contains(text(t, result), "403") {
		t.Errorf("expected status 403 in error, got %q", text(t, result))
	}
}

func TestHandleDeleteWorkflow_Success(t *testing.T) {
	_, c := fakeServer(t, http.StatusNoContent, "")
	result, err := handleDeleteWorkflow(c)(context.Background(), makeReq(map[string]any{"workflow_id": "wf-42"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text(t, result))
	}
	if !strings.Contains(text(t, result), "wf-42") {
		t.Errorf("expected workflow ID in result, got %q", text(t, result))
	}
}

func TestHandleCancelRun_Success(t *testing.T) {
	_, c := fakeServer(t, http.StatusNoContent, "")
	result, err := handleCancelRun(c)(context.Background(), makeReq(map[string]any{"run_id": "run-99"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text(t, result))
	}
	if !strings.Contains(text(t, result), "run-99") {
		t.Errorf("expected run ID in result, got %q", text(t, result))
	}
}

func TestHandleTriggerRun_Success(t *testing.T) {
	_, c := fakeServer(t, http.StatusAccepted, `{"run_id":"run-1"}`)
	result, err := handleTriggerRun(c)(context.Background(), makeReq(map[string]any{"workflow_id": "wf-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text(t, result))
	}
}

func TestHandleListRuns_WorkflowFilter(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c := newClient(config{URL: srv.URL, Token: "tok"})
	handleListRuns(c)(context.Background(), makeReq(map[string]any{"workflow_id": "wf-xyz"})) //nolint:errcheck
	if !strings.Contains(capturedPath, "wf-xyz") {
		t.Errorf("workflow_id not in query: %q", capturedPath)
	}
}

func TestHandleCreateTicket_Success(t *testing.T) {
	_, c := fakeServer(t, http.StatusCreated, `{"ticket_id":"t-1","title":"Bug"}`)
	result, err := handleCreateTicket(c)(context.Background(), makeReq(map[string]any{"title": "Bug"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text(t, result))
	}
}
