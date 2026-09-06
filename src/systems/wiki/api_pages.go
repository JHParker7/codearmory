package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// The HTTP surface. Every handler gates on WIKI permissions (project-scoped) and then
// delegates to the Store, which reaches git as the bot. Users never see git-factory.

// handleWritePage upserts a page (create or new version) at {project}/{id}.
func handleWritePage(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("wiki").Start(r.Context(), "handleWritePage")
	defer span.End()

	project, id := r.PathValue("project"), r.PathValue("id")
	if !validSlug(project) || !validSlug(id) {
		http.Error(w, "invalid project or page id", http.StatusBadRequest)
		return
	}
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "writePage", pageResource(project, id))
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	var req putPageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !req.Type.valid() {
		http.Error(w, "type must be one of overview|architecture|contract|model|service|component|decision|ticket", http.StatusBadRequest)
		return
	}
	if !req.Stack.valid() {
		http.Error(w, "stack must be one of shared|frontend|backend|infra", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if req.Format == "" {
		req.Format = "md"
	}
	if req.Status == "" {
		req.Status = "draft"
	}
	path := req.Path
	if path == "" {
		path = defaultPath(id, req.Type, req.Format)
	}
	page := Page{
		PageMeta: PageMeta{
			ID: id, Path: path, Type: req.Type, Stack: req.Stack, Format: req.Format,
			Title: req.Title, Status: req.Status, Related: req.Related,
			Updated: time.Now().UTC(), UpdatedBy: userID,
		},
		Content: req.Content,
	}
	saved, err := store.PutPage(ctx, project, page, "docs(wiki): update "+id)
	if err != nil {
		slog.ErrorContext(ctx, "write page", "project", project, "id", id, "error", err)
		http.Error(w, "failed to write page", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, saved)
}

func handleGetPage(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("wiki").Start(r.Context(), "handleGetPage")
	defer span.End()

	project, id := r.PathValue("project"), r.PathValue("id")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getPage", pageResource(project, id))
	if !ok {
		return
	}
	_ = userID
	p, err := store.GetPage(ctx, project, id)
	if errors.Is(err, errNotFound) {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get page", "project", project, "id", id, "error", err)
		http.Error(w, "failed to get page", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleListPages returns the manifest — the machine index used for read-scoping.
func handleListPages(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("wiki").Start(r.Context(), "handleListPages")
	defer span.End()

	project := r.PathValue("project")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listPage", pagesResource(project)); !ok {
		return
	}
	m, err := store.GetManifest(ctx, project)
	if err != nil {
		slog.ErrorContext(ctx, "list pages", "project", project, "error", err)
		http.Error(w, "failed to list pages", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func handleDeletePage(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("wiki").Start(r.Context(), "handleDeletePage")
	defer span.End()

	project, id := r.PathValue("project"), r.PathValue("id")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deletePage", pageResource(project, id)); !ok {
		return
	}
	err := store.DeletePage(ctx, project, id, "docs(wiki): remove "+id)
	if errors.Is(err, errNotFound) {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "delete page", "project", project, "id", id, "error", err)
		http.Error(w, "failed to delete page", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handlePageHistory(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("wiki").Start(r.Context(), "handlePageHistory")
	defer span.End()

	project, id := r.PathValue("project"), r.PathValue("id")
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getPage", pageResource(project, id)); !ok {
		return
	}
	commits, err := store.History(ctx, project, id)
	if errors.Is(err, errNotFound) {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "page history", "project", project, "id", id, "error", err)
		http.Error(w, "failed to get history", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commits": commits})
}

// defaultPath places a page in the repo by its type when the caller doesn't pin a path.
// The extension follows the format so structured files (openapi/sql/ts) keep their tool
// affinity; everything else is markdown.
func defaultPath(id string, t PageType, format string) string {
	ext := "md"
	switch format {
	case "openapi", "yaml":
		ext = "yaml"
	case "sql":
		ext = "sql"
	case "ts":
		ext = "ts"
	}
	dir := map[PageType]string{
		TypeOverview: ".", TypeArchitecture: ".", TypeContract: "contracts", TypeModel: "models",
		TypeService: "services", TypeComponent: "components", TypeDecision: "decisions", TypeTicket: "tickets",
	}[t]
	if dir == "" || dir == "." {
		return id + "." + ext
	}
	return dir + "/" + id + "." + ext
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
