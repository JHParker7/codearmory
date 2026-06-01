package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerHookTools(s *server.MCPServer, c *client) {
	s.AddTool(mcp.NewTool("list_rules",
		mcp.WithDescription("List webhook pipeline rules for the authenticated user's org."),
	), handleListRules(c))

	s.AddTool(mcp.NewTool("get_rule",
		mcp.WithDescription("Get a webhook pipeline rule by ID."),
		mcp.WithString("rule_id", mcp.Required(), mcp.Description("UUID of the rule")),
	), handleGetRule(c))

	s.AddTool(mcp.NewTool("create_rule",
		mcp.WithDescription(`Create a pipeline rule that triggers a workflow when a matching webhook arrives.
Events example: ["push","pull_request"]. Input mapping maps workflow input keys to webhook payload fields.`),
		mcp.WithString("name", mcp.Required(), mcp.Description("Rule name")),
		mcp.WithString("repo", mcp.Required(), mcp.Description("Repository identifier, e.g. myorg/myrepo")),
		mcp.WithString("events", mcp.Required(), mcp.Description(`JSON array of event types, e.g. ["push","pull_request"]`)),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("UUID of the workflow to trigger")),
		mcp.WithString("secret", mcp.Required(), mcp.Description("HMAC-SHA256 webhook secret for signature verification")),
		mcp.WithString("ref_filter", mcp.Description(`Optional ref filter, e.g. "refs/heads/main" or "refs/heads/*"`)),
		mcp.WithString("input_mapping", mcp.Description(`Optional JSON object mapping workflow input keys to webhook payload fields, e.g. {"BRANCH":"ref","SHA":"commit"}`)),
	), handleCreateRule(c))

	s.AddTool(mcp.NewTool("update_rule",
		mcp.WithDescription("Update a pipeline rule. Omit secret to leave it unchanged; provide a new non-empty value to rotate it."),
		mcp.WithString("rule_id", mcp.Required(), mcp.Description("UUID of the rule to update")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Rule name")),
		mcp.WithString("repo", mcp.Required(), mcp.Description("Repository identifier")),
		mcp.WithString("events", mcp.Required(), mcp.Description(`JSON array of event types, e.g. ["push"]`)),
		mcp.WithString("workflow_id", mcp.Required(), mcp.Description("UUID of the workflow to trigger")),
		mcp.WithString("secret", mcp.Description("New HMAC secret — omit to leave unchanged")),
		mcp.WithString("ref_filter", mcp.Description("Ref filter pattern")),
		mcp.WithString("input_mapping", mcp.Description("JSON object mapping workflow keys to payload fields")),
	), handleUpdateRule(c))

	s.AddTool(mcp.NewTool("delete_rule",
		mcp.WithDescription("Delete a pipeline rule."),
		mcp.WithString("rule_id", mcp.Required(), mcp.Description("UUID of the rule to delete")),
	), handleDeleteRule(c))

	s.AddTool(mcp.NewTool("list_events",
		mcp.WithDescription("List received webhook events, optionally filtered by repository."),
		mcp.WithString("repo", mcp.Description("Optional: filter events for a specific repo, e.g. myorg/myrepo")),
	), handleListEvents(c))

	s.AddTool(mcp.NewTool("get_event",
		mcp.WithDescription("Get a webhook event including which rules it matched and whether runs were created."),
		mcp.WithString("event_id", mcp.Required(), mcp.Description("UUID of the event")),
	), handleGetEvent(c))
}

func handleListRules(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		data, status, err := c.get(ctx, "/rules")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleGetRule(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "rule_id")
		data, status, err := c.get(ctx, "/rules/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleCreateRule(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}

		eventsJSON := argStr(req, "events")
		var events []string
		if err := json.Unmarshal([]byte(eventsJSON), &events); err != nil {
			return mcp.NewToolResultErrorf("events is not a valid JSON array: %v", err), nil
		}

		secret := argStr(req, "secret")
		body := map[string]any{
			"name":       argStr(req, "name"),
			"repo":       argStr(req, "repo"),
			"events":     events,
			"workflow_id": argStr(req, "workflow_id"),
			"secret":     secret,
			"ref_filter": argStr(req, "ref_filter"),
		}

		if mappingJSON := argStr(req, "input_mapping"); mappingJSON != "" {
			var mapping map[string]string
			if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
				return mcp.NewToolResultErrorf("input_mapping is not a valid JSON object: %v", err), nil
			}
			body["input_mapping"] = mapping
		}

		data, status, err := c.post(ctx, "/rules", body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusCreated {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleUpdateRule(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}

		eventsJSON := argStr(req, "events")
		var events []string
		if err := json.Unmarshal([]byte(eventsJSON), &events); err != nil {
			return mcp.NewToolResultErrorf("events is not a valid JSON array: %v", err), nil
		}

		body := map[string]any{
			"name":        argStr(req, "name"),
			"repo":        argStr(req, "repo"),
			"events":      events,
			"workflow_id": argStr(req, "workflow_id"),
			"ref_filter":  argStr(req, "ref_filter"),
		}

		// secret: omit entirely (JSON null) to leave unchanged
		if s := argStr(req, "secret"); s != "" {
			body["secret"] = s
		}

		if mappingJSON := argStr(req, "input_mapping"); mappingJSON != "" {
			var mapping map[string]string
			if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
				return mcp.NewToolResultErrorf("input_mapping is not a valid JSON object: %v", err), nil
			}
			body["input_mapping"] = mapping
		}

		id := argStr(req, "rule_id")
		data, status, err := c.put(ctx, "/rules/"+id, body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleDeleteRule(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "rule_id")
		data, status, err := c.del(ctx, "/rules/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("rule %s deleted", id)), nil
	}
}

func handleListEvents(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		path := "/events"
		if repo := argStr(req, "repo"); repo != "" {
			path += "?repo=" + url.QueryEscape(repo)
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

func handleGetEvent(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "event_id")
		data, status, err := c.get(ctx, "/events/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}
