package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerWorkflowTools(s *server.MCPServer, c *client) {
	s.AddTool(mcp.NewTool("list_workflows",
		mcp.WithDescription("List all CI/CD workflows visible to the authenticated user."),
	), handleListWorkflows(c))

	s.AddTool(mcp.NewTool("get_workflow",
		mcp.WithDescription("Get a workflow definition including its step sequence."),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("UUID of the workflow")),
	), handleGetWorkflow(c))

	s.AddTool(mcp.NewTool("create_workflow",
		mcp.WithDescription(`Create a new workflow. The steps field is a JSON array of step objects.
Each step has: action (string, e.g. "http"), with (object with keys: service, method, path, body, headers, expected_status), timeout (int, seconds), name (string), description (string).
Example steps: [{"action":"http","name":"deploy","with":{"service":"forge","method":"POST","path":"/executions","body":{"image":"alpine:3.19","command":["sh","-c","echo hello"]}}}]`),
		mcp.WithString("name", mcp.Required(), mcp.Description("Workflow name")),
		mcp.WithString("steps", mcp.Required(), mcp.Description("JSON array of step objects")),
		mcp.WithString("description", mcp.Description("Optional description")),
	), handleCreateWorkflow(c))

	s.AddTool(mcp.NewTool("update_workflow",
		mcp.WithDescription("Replace a workflow's name, description, and step sequence."),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("UUID of the workflow to update")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Workflow name")),
		mcp.WithString("steps", mcp.Required(), mcp.Description("JSON array of step objects (replaces existing steps)")),
		mcp.WithString("description", mcp.Description("Optional description")),
	), handleUpdateWorkflow(c))

	s.AddTool(mcp.NewTool("delete_workflow",
		mcp.WithDescription("Soft-delete a workflow. Running runs are not affected."),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("UUID of the workflow to delete")),
	), handleDeleteWorkflow(c))

	s.AddTool(mcp.NewTool("trigger_run",
		mcp.WithDescription("Trigger a workflow run. Inputs are substituted into step ${KEY} placeholders."),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("UUID of the workflow to run")),
		mcp.WithString("inputs", mcp.Description(`Optional JSON object of input key/value pairs, e.g. {"ENV":"prod","TAG":"v1.2"}`)),
	), handleTriggerRun(c))

	s.AddTool(mcp.NewTool("list_runs",
		mcp.WithDescription("List workflow runs, optionally filtered to a single workflow."),
		mcp.WithString("workflow_id", mcp.Description("Optional: filter to runs for this workflow UUID")),
	), handleListRuns(c))

	s.AddTool(mcp.NewTool("get_run",
		mcp.WithDescription("Get a workflow run including per-step status and output."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("UUID of the run")),
	), handleGetRun(c))

	s.AddTool(mcp.NewTool("cancel_run",
		mcp.WithDescription("Cancel a pending or running workflow run."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("UUID of the run to cancel")),
	), handleCancelRun(c))
}

func handleListWorkflows(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		data, status, err := c.get(ctx, "/workflows/pipelines")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleGetWorkflow(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "workflow_id")
		data, status, err := c.get(ctx, "/workflows/pipelines/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleCreateWorkflow(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		stepsJSON := argStr(req, "steps")
		var steps json.RawMessage
		if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
			return mcp.NewToolResultErrorf("steps is not valid JSON: %v", err), nil
		}
		body := map[string]any{
			"name":        argStr(req, "name"),
			"description": argStr(req, "description"),
			"steps":       steps,
		}
		data, status, err := c.post(ctx, "/workflows/pipelines", body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusCreated {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleUpdateWorkflow(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		stepsJSON := argStr(req, "steps")
		var steps json.RawMessage
		if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
			return mcp.NewToolResultErrorf("steps is not valid JSON: %v", err), nil
		}
		body := map[string]any{
			"name":        argStr(req, "name"),
			"description": argStr(req, "description"),
			"steps":       steps,
		}
		id := argStr(req, "workflow_id")
		data, status, err := c.put(ctx, "/workflows/pipelines/"+id, body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleDeleteWorkflow(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "workflow_id")
		data, status, err := c.del(ctx, "/workflows/pipelines/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("workflow %s deleted", id)), nil
	}
}

func handleTriggerRun(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "workflow_id")
		body := map[string]any{"inputs": map[string]string{}}
		if raw := argStr(req, "inputs"); raw != "" {
			var inputs map[string]string
			if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
				return mcp.NewToolResultErrorf("inputs is not valid JSON: %v", err), nil
			}
			body["inputs"] = inputs
		}
		data, status, err := c.post(ctx, "/workflows/pipelines/"+id+"/runs", body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusAccepted {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleListRuns(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		path := "/workflows/runs"
		if wfID := argStr(req, "workflow_id"); wfID != "" {
			path += "?workflow_id=" + wfID
		}
		data, status, err := c.get(ctx, path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleGetRun(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "run_id")
		data, status, err := c.get(ctx, "/workflows/runs/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleCancelRun(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "run_id")
		data, status, err := c.del(ctx, "/workflows/runs/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("run %s cancelled", id)), nil
	}
}
