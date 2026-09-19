package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestMCPObjBody_OmitsEmptyAndMergesExtra(t *testing.T) {
	b := objBody(map[string]string{"title": "hi", "status": "", "project": "ops"},
		map[string]any{"count": 3})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if _, ok := m["status"]; ok {
		t.Fatal("empty status should be omitted")
	}
	if m["title"] != "hi" || m["project"] != "ops" {
		t.Fatalf("missing fields: %v", m)
	}
	if m["count"].(float64) != 3 {
		t.Fatalf("extra not merged: %v", m)
	}
}

func TestMCPJSON_ReusesCLIClient(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Write([]byte(`[{"workflow_id":"w1","name":"agent-chain"}]`))
	}))
	defer srv.Close()

	oldURL, oldTok := flagURL, flagToken
	flagURL, flagToken = srv.URL, "test-token"
	defer func() { flagURL, flagToken = oldURL, oldTok }()

	res, err := mcpJSON(http.MethodGet, "/workflows/pipelines", nil)
	if err != nil {
		t.Fatalf("mcpJSON error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("client did not forward the CLI token, got %q", gotAuth)
	}
	if gotPath != "/workflows/pipelines" {
		t.Fatalf("wrong path: %q", gotPath)
	}
	text := toolText(res)
	if !strings.Contains(text, "agent-chain") {
		t.Fatalf("result missing body: %s", text)
	}
}

func TestMCPJSON_APIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	oldURL, oldTok := flagURL, flagToken
	flagURL, flagToken = srv.URL, "t"
	defer func() { flagURL, flagToken = oldURL, oldTok }()

	res, _ := mcpJSON(http.MethodGet, "/x", nil)
	if !res.IsError {
		t.Fatal("a 403 should produce a tool error")
	}
}

func TestMCPRegister_DoesNotPanic(t *testing.T) {
	s := server.NewMCPServer("t", "0", server.WithToolCapabilities(false))
	registerMCPTools(s) // panics on a malformed tool spec
}

// toolText pulls the text out of a tool result for assertions.
func toolText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
