package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerContainerTools(s *server.MCPServer, c *client) {
	s.AddTool(mcp.NewTool("list_repositories",
		mcp.WithDescription("List all container image repositories visible to the authenticated user."),
	), handleListRepositories(c))

	s.AddTool(mcp.NewTool("list_tags",
		mcp.WithDescription("List all tags for a container image repository."),
		mcp.WithString("image", mcp.Required(), mcp.Description("Image in namespace/image format, e.g. myorg/myapp")),
	), handleListTags(c))

	s.AddTool(mcp.NewTool("get_manifest",
		mcp.WithDescription("Get an image manifest by tag or digest. Returns schema version, layers, and config descriptor."),
		mcp.WithString("image", mcp.Required(), mcp.Description("Image in namespace/image format, e.g. myorg/myapp")),
		mcp.WithString("reference", mcp.Required(), mcp.Description("Tag (e.g. latest, v1.2.3) or digest (sha256:...)")),
	), handleGetManifest(c))

	s.AddTool(mcp.NewTool("delete_manifest",
		mcp.WithDescription("Delete an image manifest by digest. The digest must start with sha256:. This removes all tags pointing to that digest."),
		mcp.WithString("image", mcp.Required(), mcp.Description("Image in namespace/image format, e.g. myorg/myapp")),
		mcp.WithString("digest", mcp.Required(), mcp.Description("Content digest to delete, e.g. sha256:abc123...")),
	), handleDeleteManifest(c))
}

func splitImage(image string) (namespace, name string, err error) {
	ns, img, ok := strings.Cut(image, "/")
	if !ok || ns == "" || img == "" {
		return "", "", fmt.Errorf("image must be in namespace/image format, got %q", image)
	}
	return ns, img, nil
}

func handleListRepositories(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		data, status, err := c.get(ctx, "/containers/repositories")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleListTags(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		ns, img, err := splitImage(argStr(req, "image"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, status, apiE := c.get(ctx, "/containers/repositories/"+ns+"/"+img+"/tags")
		if apiE != nil {
			return mcp.NewToolResultError(apiE.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleGetManifest(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		ns, img, err := splitImage(argStr(req, "image"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ref := argStr(req, "reference")
		data, status, apiE := c.get(ctx, "/containers/repositories/"+ns+"/"+img+"/manifests/"+ref)
		if apiE != nil {
			return mcp.NewToolResultError(apiE.Error()), nil
		}
		if status != http.StatusOK {
			return apiErr(status, data), nil
		}
		return ok(data), nil
	}
}

func handleDeleteManifest(c *client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := c.cfg.validate(); err != nil {
			return noAuth(), nil
		}
		ns, img, err := splitImage(argStr(req, "image"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		digest := argStr(req, "digest")
		if !strings.HasPrefix(digest, "sha256:") {
			return mcp.NewToolResultError("digest must start with sha256:"), nil
		}
		data, status, apiE := c.del(ctx, "/containers/repositories/"+ns+"/"+img+"/manifests/"+digest)
		if apiE != nil {
			return mcp.NewToolResultError(apiE.Error()), nil
		}
		if status != http.StatusNoContent {
			return apiErr(status, data), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("manifest %s deleted from %s/%s", digest, ns, img)), nil
	}
}
