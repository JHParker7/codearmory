package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

type client struct {
	cfg  config
	http *http.Client
}

func newClient(cfg config) *client {
	return &client{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// call makes an authenticated request to the conductor and returns the raw
// response body, HTTP status code, and any transport-level error.
func (c *client) call(ctx context.Context, method, path string, body any) (json.RawMessage, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		r = bytes.NewReader(b)
	}

	url := strings.TrimRight(c.cfg.URL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	return raw, resp.StatusCode, nil
}

func (c *client) get(ctx context.Context, path string) (json.RawMessage, int, error) {
	return c.call(ctx, http.MethodGet, path, nil)
}

func (c *client) post(ctx context.Context, path string, body any) (json.RawMessage, int, error) {
	return c.call(ctx, http.MethodPost, path, body)
}

func (c *client) put(ctx context.Context, path string, body any) (json.RawMessage, int, error) {
	return c.call(ctx, http.MethodPut, path, body)
}

func (c *client) del(ctx context.Context, path string) (json.RawMessage, int, error) {
	return c.call(ctx, http.MethodDelete, path, nil)
}

// ok returns a formatted text result from a successful JSON response body.
func ok(data json.RawMessage) *mcp.CallToolResult {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return mcp.NewToolResultText(string(data))
	}
	return mcp.NewToolResultText(pretty.String())
}

// apiErr returns a tool error result that includes the HTTP status and body.
func apiErr(status int, body json.RawMessage) *mcp.CallToolResult {
	return mcp.NewToolResultErrorf("HTTP %d: %s", status, strings.TrimSpace(string(body)))
}

// argStr safely extracts a string argument from a tool call request.
func argStr(req mcp.CallToolRequest, key string) string {
	args := req.GetArguments()
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

// argFloat safely extracts a float64 (number) argument.
func argFloat(req mcp.CallToolRequest, key string) float64 {
	args := req.GetArguments()
	if args == nil {
		return 0
	}
	v, _ := args[key].(float64)
	return v
}

// noAuth returns a standardised error when no token is configured.
func noAuth() *mcp.CallToolResult {
	return mcp.NewToolResultError("not authenticated — set CODEARMORY_TOKEN or run `armory auth login`")
}
