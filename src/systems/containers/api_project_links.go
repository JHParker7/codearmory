package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// projectLinkInput is the body of a set-project request. An empty/omitted project
// unlinks the repository (or namespace).
type projectLinkInput struct {
	Project string `json:"project"`
}

// handleSetRepoProject stamps a single repository (namespace/image) into a
// gatekeeper project — the persisted association that lets a project member reach a
// repository they do not own the namespace of.
//
// PUT /repositories/{namespace}/{image}/project
func handleSetRepoProject(w http.ResponseWriter, r *http.Request) {
	setProjectLink(w, r, r.PathValue("namespace"), r.PathValue("image"))
}

// handleSetNamespaceProject stamps an ENTIRE namespace into a project (image ""),
// so every repository under it is reachable by the project's members.
//
// PUT /repositories/{namespace}/project
func handleSetNamespaceProject(w http.ResponseWriter, r *http.Request) {
	setProjectLink(w, r, r.PathValue("namespace"), "")
}

// setProjectLink is the shared body of the two set-project handlers. It requires,
// in order: authentication + push authority over the repository (the coarse
// gatekeeper check), ownership of the namespace (only the owner may file their own
// repositories), and — for a non-empty project — write access to the target
// project. An empty project unlinks. The pushImage action is used as the coarse
// guard because filing a repository into a project is a management act and every
// namespace owner already holds push over their own repositories; it avoids
// introducing a new gatekeeper action for a check namespaceAllowed re-enforces.
func setProjectLink(w http.ResponseWriter, r *http.Request, namespace, image string) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleSetProjectLink")
	defer span.End()

	resource := "containers/repositories/" + namespace
	if image != "" {
		resource += "/" + image
	}
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "pushImage", resource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("repo.namespace", namespace),
		attribute.String("repo.image", image),
	)

	// Only the namespace owner may file its repositories into a project — the same
	// tenant boundary every read/delete handler enforces. Without this a project
	// developer could annex another tenant's namespace by linking it.
	if !namespaceAllowed(ctx, r, userID, orgID, namespace) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "set project link: namespace not owned by caller", "user_id", userID, "namespace", namespace)
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	var in projectLinkInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Empty project → unlink.
	if in.Project == "" {
		if err := deleteProjectLink(ctx, namespace, image); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db error")
			slog.ErrorContext(ctx, "unlink project: db error", "namespace", namespace, "image", image, "error", err)
			http.Error(w, "failed to unlink repository", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "repository unlinked from project", "user_id", userID, "namespace", namespace, "image", image)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// The slug must name a project the caller can actually reach; an unresolved slug
	// is useless here (no project id to widen on) so it is rejected rather than
	// stored as a dangling label.
	bearer := bearerHeader(r)
	p := resolveProjectSlug(ctx, bearer, in.Project)
	if p == nil {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "unknown or inaccessible project: "+in.Project, http.StatusBadRequest)
		return
	}
	// Filing into a project requires write access to it — mirrors forge/tickets
	// (createExecution/createBoard). A viewer cannot annex repositories into a
	// project they may only read.
	if !checkProjectPermission(ctx, bearer, "createRepository", "repositories", p.Slug, "") {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "you cannot file repositories into project "+p.Slug, http.StatusForbidden)
		return
	}

	now := time.Now().UTC()
	link := ProjectLink{
		ID:               newID(),
		Namespace:        namespace,
		Image:            image,
		Project:          p.Slug,
		ProjectID:        p.ProjectID,
		ProjectNamespace: p.Namespace,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := upsertProjectLink(ctx, link); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "set project link: db error", "namespace", namespace, "image", image, "error", err)
		http.Error(w, "failed to link repository", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("project.slug", p.Slug), attribute.String("project.id", p.ProjectID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repository linked to project", "user_id", userID, "namespace", namespace, "image", image, "project", p.Slug)
	writeJSON(w, http.StatusOK, link)
}
