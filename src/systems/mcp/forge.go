package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerForgeTools(s *server.MCPServer, c *client) {
	s.AddTool(mcp.NewTool("run_forge",
		mcp.WithDescription(`Execute a sandboxed command via the Forge service.
The command runs in an isolated container. Allowed images are configured by the platform operator.
The execution is async — use get_execution to poll for results.
Example: image="alpine:3.19", command=["sh","-c","echo hello && ls /"]`),
		mcp.WithString("image", mcp.Required(), mcp.Description("Container image, e.g. alpine:3.19 or python:3.12-slim")),
		mcp.WithString("command", mcp.Required(), mcp.Description(`JSON array of command parts, e.g. ["bash","-c","echo hello"]`)),
		mcp.WithString("env", mcp.Description(`Optional JSON object of environment variables, e.g. {"FOO":"bar","DEBUG":"1"}`)),
		mcp.WithNumber("timeout", mcp.Description("Timeout in seconds (default 30, max 3600)")),
	), handleRunForge(c))

	s.AddTool(mcp.NewTool("list_executions",
		mcp.WithDescription("List Forge sandbox executions for the authenticated user."),
	), handleListExecutions(c))

	s.AddTool(mcp.NewTool("get_execution",
		mcp.WithDescription("Get a Forge execution including stdout and stderr output."),
		mcp.WithString("execution_id", mcp.Required(), mcp.Description("UUID of the execution")),
	), handleGetExecution(c))

	s.AddTool(mcp.NewTool("cancel_execution",
		mcp.WithDescription("Cancel a pending or running Forge execution."),
		mcp.WithString("execution_id", mcp.Required(), mcp.Description("UUID of the execution to cancel")),
	), handleCancelExecution(c))
}

func handleRunForge(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}

		commandJSON := argStr(req, "command")
		var command []string
		if err := json.Unmarshal([]byte(commandJSON), &command); err != nil {
			return mcp.NewToolResultErrorf("command is not a valid JSON array: %v", err), nil
		}
		if len(command) == 0 {
			return mcp.NewToolResultError("command must have at least one element"), nil
		}

		body := map[string]any{
			"image":   argStr(req, "image"),
			"command": command,
		}

		if envJSON := argStr(req, "env"); envJSON != "" {
			var env map[string]string
			if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
				return mcp.NewToolResultErrorf("env is not a valid JSON object: %v", err), nil
			}
			body["env"] = env
		}

		if t := argFloat(req, "timeout"); t > 0 {
			body["timeout"] = int(t)
		}

		data, status, err := c.post(ctx, "/executions", body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusAccepted {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleListExecutions(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		data, status, err := c.get(ctx, "/executions")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleGetExecution(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "execution_id")
		data, status, err := c.get(ctx, "/executions/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleCancelExecution(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "execution_id")
		data, status, err := c.del(ctx, "/executions/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("execution %s cancelled", id)), nil
	}
}
