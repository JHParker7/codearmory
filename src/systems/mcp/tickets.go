package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerTicketTools(s *server.MCPServer, c *client) {
	s.AddTool(mcp.NewTool("list_tickets",
		mcp.WithDescription("List tickets for the authenticated user's org. Optionally filter by status, priority, or assignee."),
		mcp.WithString("status", mcp.Description("Filter by status: open, in_progress, resolved, closed")),
		mcp.WithString("priority", mcp.Description("Filter by priority: low, medium, high, critical")),
		mcp.WithString("assignee_id", mcp.Description("Filter by assignee user UUID")),
	), handleListTickets(c))

	s.AddTool(mcp.NewTool("create_ticket",
		mcp.WithDescription("Create a new ticket."),
		mcp.WithString("title", mcp.Required(), mcp.Description("Ticket title")),
		mcp.WithString("description", mcp.Description("Optional description")),
		mcp.WithString("priority", mcp.Description("low, medium (default), high, or critical")),
		mcp.WithString("assignee_id", mcp.Description("Optional: UUID of the user to assign")),
		mcp.WithString("workflow_id", mcp.Description("Optional: link to a workflow UUID")),
		mcp.WithString("run_id", mcp.Description("Optional: link to a workflow run UUID")),
	), handleCreateTicket(c))

	s.AddTool(mcp.NewTool("get_ticket",
		mcp.WithDescription("Get a ticket including its full comment thread."),
		mcp.WithString("ticket_id", mcp.Required(), mcp.Description("UUID of the ticket")),
	), handleGetTicket(c))

	s.AddTool(mcp.NewTool("update_ticket",
		mcp.WithDescription("Update a ticket's title, status, priority, or assignee."),
		mcp.WithString("ticket_id", mcp.Required(), mcp.Description("UUID of the ticket")),
		mcp.WithString("title", mcp.Required(), mcp.Description("Ticket title")),
		mcp.WithString("description", mcp.Description("Updated description")),
		mcp.WithString("status", mcp.Description("open, in_progress, resolved, or closed")),
		mcp.WithString("priority", mcp.Description("low, medium, high, or critical")),
		mcp.WithString("assignee_id", mcp.Description("UUID of the assignee, or empty string to clear")),
		mcp.WithString("workflow_id", mcp.Description("Linked workflow UUID, or empty to clear")),
		mcp.WithString("run_id", mcp.Description("Linked run UUID, or empty to clear")),
	), handleUpdateTicket(c))

	s.AddTool(mcp.NewTool("delete_ticket",
		mcp.WithDescription("Soft-delete a ticket."),
		mcp.WithString("ticket_id", mcp.Required(), mcp.Description("UUID of the ticket to delete")),
	), handleDeleteTicket(c))

	s.AddTool(mcp.NewTool("add_ticket_comment",
		mcp.WithDescription("Add a comment to a ticket."),
		mcp.WithString("ticket_id", mcp.Required(), mcp.Description("UUID of the ticket")),
		mcp.WithString("body", mcp.Required(), mcp.Description("Comment text")),
	), handleAddTicketComment(c))

	s.AddTool(mcp.NewTool("delete_ticket_comment",
		mcp.WithDescription("Delete a comment from a ticket."),
		mcp.WithString("ticket_id", mcp.Required(), mcp.Description("UUID of the ticket")),
		mcp.WithString("comment_id", mcp.Required(), mcp.Description("UUID of the comment")),
	), handleDeleteTicketComment(c))
}

func handleListTickets(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		var params []string
		if v := argStr(req, "status"); v != "" {
			params = append(params, "status="+v)
		}
		if v := argStr(req, "priority"); v != "" {
			params = append(params, "priority="+v)
		}
		if v := argStr(req, "assignee_id"); v != "" {
			params = append(params, "assignee_id="+v)
		}
		path := "/tickets"
		if len(params) > 0 {
			path += "?" + strings.Join(params, "&")
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

func handleCreateTicket(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		body := map[string]any{
			"title":       argStr(req, "title"),
			"description": argStr(req, "description"),
		}
		if v := argStr(req, "priority"); v != "" {
			body["priority"] = v
		}
		if v := argStr(req, "assignee_id"); v != "" {
			body["assignee_id"] = v
		}
		if v := argStr(req, "workflow_id"); v != "" {
			body["workflow_id"] = v
		}
		if v := argStr(req, "run_id"); v != "" {
			body["run_id"] = v
		}
		data, status, err := c.post(ctx, "/tickets", body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusCreated {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleGetTicket(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "ticket_id")
		data, status, err := c.get(ctx, "/tickets/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleUpdateTicket(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		body := map[string]any{
			"title":       argStr(req, "title"),
			"description": argStr(req, "description"),
		}
		if v := argStr(req, "status"); v != "" {
			body["status"] = v
		}
		if v := argStr(req, "priority"); v != "" {
			body["priority"] = v
		}
		// nullable fields: send null to clear, value to set
		for _, key := range []string{"assignee_id", "workflow_id", "run_id"} {
			v := argStr(req, key)
			if v != "" {
				body[key] = v
			} else {
				body[key] = nil
			}
		}
		id := argStr(req, "ticket_id")
		data, status, err := c.put(ctx, "/tickets/"+id, body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleDeleteTicket(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "ticket_id")
		data, status, err := c.del(ctx, "/tickets/"+id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("ticket %s deleted", id)), nil
	}
}

func handleAddTicketComment(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		id := argStr(req, "ticket_id")
		body := map[string]any{"body": argStr(req, "body")}
		data, status, err := c.post(ctx, "/tickets/"+id+"/comments", body)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusCreated {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleDeleteTicketComment(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		tid := argStr(req, "ticket_id")
		cid := argStr(req, "comment_id")
		data, status, err := c.del(ctx, "/tickets/"+tid+"/comments/"+cid)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("comment %s deleted", cid)), nil
	}
}
