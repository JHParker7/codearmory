package cmd

// The CodeArmory MCP server, rebuilt inside the monorepo CLI so it shares the
// CLI's authenticated client (doRequest → conductorURL()/bearerToken()) rather
// than duplicating API plumbing. `armory mcp` speaks the Model Context Protocol
// over stdio, exposing the platform's core resources as tools: workflows and
// runs, tickets and boards, git-factory repos and pull requests, and the
// blacksmith agent roles. Point an MCP client (Claude, an IDE) at
// `armory mcp` and it drives CodeArmory with the same token the CLI uses.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run a Model Context Protocol server over stdio",
	Long: `mcp starts an MCP server on stdio that exposes CodeArmory as tools for an
AI client (Claude Desktop/Code, an IDE). It reuses the CLI's authentication, so
run 'armory auth login' (or set CODEARMORY_TOKEN) first, and CODEARMORY_URL to
point at a conductor other than the default.

Tools: workflows (list/get/trigger/runs), tickets (list/get/create/update),
repos + pull requests (list/get/create/merge), and agent roles (list/get).
Moving a ticket to in_progress on the requests board starts the agent chain.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		s := server.NewMCPServer("CodeArmory", "1.0.0",
			server.WithInstructions(`CodeArmory DevOps platform. Tools:
- Workflows: list_workflows, get_workflow, trigger_run, list_runs, get_run
- Tickets: list_boards, list_tickets, get_ticket, create_ticket, update_ticket
- Repos: list_repos, get_repo, create_repo, list_pulls, create_pull, merge_pull
- Agents: list_roles, get_role
Authentication is inherited from the armory CLI (keychain / CODEARMORY_TOKEN).
The agent chain (architect → pm → backend → frontend → devops, each opening a PR)
starts when a ticket is created or moved to in_progress on the requests board.`),
			server.WithToolCapabilities(false),
		)
		registerMCPTools(s)
		return server.ServeStdio(s)
	},
}

func init() { rootCmd.AddCommand(mcpCmd) }

// ── helpers ──────────────────────────────────────────────────────────────────

func mcpArg(req mcp.CallToolRequest, key string) string {
	args := req.GetArguments()
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

// mcpJSON runs an authenticated request through the CLI client and returns the
// body as a pretty-printed tool result, or the HTTP error as a tool error.
func mcpJSON(method, path string, body []byte) (*mcp.CallToolResult, error) {
	data, err := doRequest(method, path, body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil && pretty.Len() > 0 {
		return mcp.NewToolResultText(pretty.String()), nil
	}
	if len(data) == 0 {
		return mcp.NewToolResultText("ok"), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// objBody marshals a map, dropping empty-string values so optional fields are omitted.
func objBody(fields map[string]string, extra map[string]any) []byte {
	m := map[string]any{}
	for k, v := range fields {
		if v != "" {
			m[k] = v
		}
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

// ── tool registration ────────────────────────────────────────────────────────

func registerMCPTools(s *server.MCPServer) {
	// Workflows ---------------------------------------------------------------
	s.AddTool(mcp.NewTool("list_workflows",
		mcp.WithDescription("List CI/CD workflow (pipeline) definitions.")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/workflows/pipelines", nil)
		})

	s.AddTool(mcp.NewTool("get_workflow",
		mcp.WithDescription("Get one workflow definition, including its steps."),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("workflow UUID"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/workflows/pipelines/"+mcpArg(req, "workflow_id"), nil)
		})

	s.AddTool(mcp.NewTool("trigger_run",
		mcp.WithDescription("Trigger a workflow run. Inputs fill the pipeline's declared inputs."),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("workflow UUID")),
		mcp.WithString("inputs", mcp.Description(`optional JSON object of inputs, e.g. {"project":"ops"}`))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			inputs := map[string]string{}
			if raw := mcpArg(req, "inputs"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
					return mcp.NewToolResultErrorf("inputs is not valid JSON: %v", err), nil
				}
			}
			body, _ := json.Marshal(map[string]any{"inputs": inputs})
			return mcpJSON(http.MethodPost, "/workflows/pipelines/"+mcpArg(req, "workflow_id")+"/runs", body)
		})

	s.AddTool(mcp.NewTool("list_runs",
		mcp.WithDescription("List recent workflow runs.")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/workflows/runs", nil)
		})

	s.AddTool(mcp.NewTool("get_run",
		mcp.WithDescription("Get one run with per-step status and output."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("run UUID"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/workflows/runs/"+mcpArg(req, "run_id"), nil)
		})

	// Tickets -----------------------------------------------------------------
	s.AddTool(mcp.NewTool("list_boards",
		mcp.WithDescription("List ticket boards (e.g. the requests board that starts the agent chain).")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/tickets/boards", nil)
		})

	s.AddTool(mcp.NewTool("list_tickets",
		mcp.WithDescription("List tickets, optionally scoped to a project."),
		mcp.WithString("project", mcp.Description("optional project slug filter"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path := "/tickets/tickets"
			if p := mcpArg(req, "project"); p != "" {
				path += "?project=" + p
			}
			return mcpJSON(http.MethodGet, path, nil)
		})

	s.AddTool(mcp.NewTool("get_ticket",
		mcp.WithDescription("Get one ticket with its comments."),
		mcp.WithString("id", mcp.Required(), mcp.Description("ticket UUID"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/tickets/tickets/"+mcpArg(req, "id"), nil)
		})

	s.AddTool(mcp.NewTool("create_ticket",
		mcp.WithDescription("Create a ticket. Creating one with status in_progress on the requests board starts the agent chain."),
		mcp.WithString("title", mcp.Required(), mcp.Description("ticket title")),
		mcp.WithString("board_id", mcp.Description("board UUID (the requests board triggers the chain)")),
		mcp.WithString("project", mcp.Description("project slug")),
		mcp.WithString("status", mcp.Description("initial status, e.g. open or in_progress")),
		mcp.WithString("description", mcp.Description("ticket body"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body := objBody(map[string]string{
				"title":       mcpArg(req, "title"),
				"board_id":    mcpArg(req, "board_id"),
				"project":     mcpArg(req, "project"),
				"status":      mcpArg(req, "status"),
				"description": mcpArg(req, "description"),
			}, nil)
			return mcpJSON(http.MethodPost, "/tickets/tickets", body)
		})

	s.AddTool(mcp.NewTool("update_ticket",
		mcp.WithDescription("Update a ticket's status/title/description. Moving to in_progress on the requests board starts the agent chain."),
		mcp.WithString("id", mcp.Required(), mcp.Description("ticket UUID")),
		mcp.WithString("status", mcp.Description("new status, e.g. in_progress")),
		mcp.WithString("title", mcp.Description("new title")),
		mcp.WithString("description", mcp.Description("new body"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body := objBody(map[string]string{
				"status":      mcpArg(req, "status"),
				"title":       mcpArg(req, "title"),
				"description": mcpArg(req, "description"),
			}, nil)
			return mcpJSON(http.MethodPut, "/tickets/tickets/"+mcpArg(req, "id"), body)
		})

	// Repos + pull requests ---------------------------------------------------
	const gf = "/codearmory_git_factory/repos"

	s.AddTool(mcp.NewTool("list_repos",
		mcp.WithDescription("List git-factory repositories.")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, gf, nil)
		})

	s.AddTool(mcp.NewTool("get_repo",
		mcp.WithDescription("Get one repository."),
		mcp.WithString("repo_id", mcp.Required(), mcp.Description("repo UUID"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, gf+"/"+mcpArg(req, "repo_id"), nil)
		})

	s.AddTool(mcp.NewTool("create_repo",
		mcp.WithDescription("Create a repository (namespace is the caller's). Returns id, namespace, name."),
		mcp.WithString("name", mcp.Required(), mcp.Description("repo name")),
		mcp.WithString("visibility", mcp.Description("private (default) or public"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body := objBody(map[string]string{
				"name":       mcpArg(req, "name"),
				"visibility": mcpArg(req, "visibility"),
			}, nil)
			return mcpJSON(http.MethodPost, gf, body)
		})

	s.AddTool(mcp.NewTool("list_pulls",
		mcp.WithDescription("List a repository's pull requests."),
		mcp.WithString("repo_id", mcp.Required(), mcp.Description("repo UUID"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, gf+"/"+mcpArg(req, "repo_id")+"/pulls", nil)
		})

	s.AddTool(mcp.NewTool("create_pull",
		mcp.WithDescription("Open a pull request."),
		mcp.WithString("repo_id", mcp.Required(), mcp.Description("repo UUID")),
		mcp.WithString("title", mcp.Required(), mcp.Description("PR title")),
		mcp.WithString("source_ref", mcp.Required(), mcp.Description("source branch")),
		mcp.WithString("target_ref", mcp.Required(), mcp.Description("target branch, e.g. dev")),
		mcp.WithString("body", mcp.Description("PR description"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			body := objBody(map[string]string{
				"title":      mcpArg(req, "title"),
				"source_ref": mcpArg(req, "source_ref"),
				"target_ref": mcpArg(req, "target_ref"),
				"body":       mcpArg(req, "body"),
			}, nil)
			return mcpJSON(http.MethodPost, gf+"/"+mcpArg(req, "repo_id")+"/pulls", body)
		})

	s.AddTool(mcp.NewTool("merge_pull",
		mcp.WithDescription("Merge a pull request by number."),
		mcp.WithString("repo_id", mcp.Required(), mcp.Description("repo UUID")),
		mcp.WithString("number", mcp.Required(), mcp.Description("PR number"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodPost, gf+"/"+mcpArg(req, "repo_id")+"/pulls/"+mcpArg(req, "number")+"/merge", nil)
		})

	// Agent roles -------------------------------------------------------------
	s.AddTool(mcp.NewTool("list_roles",
		mcp.WithDescription("List the blacksmith agent roles (architect, pm, backend, …).")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/blacksmith/roles", nil)
		})

	s.AddTool(mcp.NewTool("get_role",
		mcp.WithDescription("Get one agent role: its prompt, guard, tools and check."),
		mcp.WithString("name", mcp.Required(), mcp.Description("role name"))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcpJSON(http.MethodGet, "/blacksmith/roles/"+mcpArg(req, "name"), nil)
		})
}
