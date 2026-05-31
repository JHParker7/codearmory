package main

import (
	"log/slog"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

func main() {
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(jsonHandler))

	cfg := loadConfig()
	c := newClient(cfg)

	s := server.NewMCPServer(
		"CodeArmory",
		"1.0.0",
		server.WithInstructions(`CodeArmory DevOps platform. Provides tools for:
- Workflows: define and run CI/CD pipelines (list_workflows, get_workflow, create_workflow, trigger_run, list_runs, get_run, cancel_run)
- Tickets: task tracker with comments (list_tickets, create_ticket, get_ticket, update_ticket, add_ticket_comment)
- Forge: sandboxed code execution (run_forge, list_executions, get_execution)
- Hooks: webhook-triggered pipeline rules (list_rules, create_rule, list_events)

All tools require authentication. Set CODEARMORY_TOKEN or run ` + "`armory auth login`" + ` first.
Set CODEARMORY_URL to override the conductor base URL (default: http://localhost:8082).`),
		server.WithToolCapabilities(false),
	)

	registerWorkflowTools(s, c)
	registerTicketTools(s, c)
	registerForgeTools(s, c)
	registerHookTools(s, c)

	if err := server.ServeStdio(s); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
