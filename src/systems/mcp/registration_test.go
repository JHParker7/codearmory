package main

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// buildServer constructs an MCPServer and registers every tool group exactly
// as main() does, but without ServeStdio — so the wiring can be introspected
// in-process with no stdio or network.
func buildServer(c *client) *server.MCPServer {
	s := server.NewMCPServer("CodeArmory", "test", server.WithToolCapabilities(false))
	registerWorkflowTools(s, c)
	registerTicketTools(s, c)
	registerForgeTools(s, c)
	registerHookTools(s, c)
	registerContainerTools(s, c)
	return s
}

// expectedTools is the complete set of tool names the MCP server must expose.
// It is derived from the register*Tools functions in workflows.go, tickets.go,
// forge.go, hooks.go, and containers.go. If a tool is added, removed, renamed,
// or registered under the wrong name, TestRegisteredToolNames fails and this
// list must be updated deliberately.
var expectedTools = []string{
	// workflows.go
	"list_workflows", "get_workflow", "create_workflow", "update_workflow",
	"delete_workflow", "trigger_run", "list_runs", "get_run", "cancel_run",
	// tickets.go
	"list_tickets", "create_ticket", "get_ticket", "update_ticket",
	"delete_ticket", "add_ticket_comment", "delete_ticket_comment",
	// forge.go
	"run_forge", "list_executions", "get_execution", "cancel_execution",
	// hooks.go
	"list_rules", "get_rule", "create_rule", "update_rule", "delete_rule",
	"list_events", "get_event",
	// containers.go
	"list_repositories", "list_tags", "get_manifest", "delete_manifest",
}

// TestRegisteredToolNames pins the exact set of tool names the server exposes.
// ListTools() (from the mcp-go SDK) is keyed by the registered name, so a tool
// bound under the wrong name lands under the wrong key and is caught here.
func TestRegisteredToolNames(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	s := buildServer(c)

	tools := s.ListTools()

	got := make([]string, 0, len(tools))
	for name := range tools {
		got = append(got, name)
	}
	sort.Strings(got)

	want := append([]string(nil), expectedTools...)
	sort.Strings(want)

	// Report any name that is registered but not expected, and vice versa, so a
	// rename surfaces as both a missing and an unexpected entry.
	gotSet := make(map[string]bool, len(got))
	for _, n := range got {
		gotSet[n] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, n := range want {
		wantSet[n] = true
	}
	for _, n := range want {
		if !gotSet[n] {
			t.Errorf("expected tool %q is not registered", n)
		}
	}
	for _, n := range got {
		if !wantSet[n] {
			t.Errorf("unexpected tool %q is registered (add to expectedTools if intentional)", n)
		}
	}

	if len(got) != len(want) {
		t.Errorf("registered %d tools, expected %d\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
}

// TestEveryRegisteredToolHasHandler asserts that each registered tool name is
// bound to a non-nil handler. A tool registered without a handler (or with a
// nil one) would otherwise pass the name-set check but fail at call time.
func TestEveryRegisteredToolHasHandler(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	s := buildServer(c)

	for name, st := range s.ListTools() {
		if st.Handler == nil {
			t.Errorf("tool %q has a nil handler", name)
		}
		if st.Tool.Name != name {
			t.Errorf("tool registered under key %q has mismatched Tool.Name %q", name, st.Tool.Name)
		}
	}
}

// TestRegisteredHandlersDispatch routes a CallToolRequest through the server's
// own dispatch for every registered tool and asserts each handler runs and
// returns a result. With an empty token every handler short-circuits to the
// noAuth() error, which proves the name→handler binding is live (the request
// reached a real handler) without any stdio, network, or backend. This covers
// the ~19 handlers that previously had no test at all.
func TestRegisteredHandlersDispatch(t *testing.T) {
	c := newClient(config{URL: "http://localhost", Token: ""})
	s := buildServer(c)

	for name, st := range s.ListTools() {
		t.Run(name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Name = name
			req.Params.Arguments = map[string]any{}

			result, err := st.Handler(context.Background(), req)
			if err != nil {
				t.Fatalf("handler for %q returned transport error: %v", name, err)
			}
			if result == nil {
				t.Fatalf("handler for %q returned nil result", name)
			}
			// Unauthenticated: every handler must reject before doing any work.
			if !result.IsError {
				t.Fatalf("handler for %q did not error on missing auth", name)
			}
			if !strings.Contains(text(t, result), "not authenticated") {
				t.Errorf("handler for %q: expected noAuth error, got %q", name, text(t, result))
			}
		})
	}
}
