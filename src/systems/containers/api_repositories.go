package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

func handleListRepositories(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleListRepositories")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRepository", "containers/repositories")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))

	reg, ok := registryForRead(ctx, w, r)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	repos, err := reg.listRepositories(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "registry error")
		slog.ErrorContext(ctx, "list repositories: registry error", "error", err)
		http.Error(w, "failed to list repositories", http.StatusBadGateway)
		return
	}

	result := make([]Repository, len(repos))
	for i, name := range repos {
		result[i] = Repository{Name: name}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

func handleListTags(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleListTags")
	defer span.End()

	namespace := r.PathValue("namespace")
	image := r.PathValue("image")
	name := namespace + "/" + image

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTag",
		"containers/repositories/"+namespace+"/"+image)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	if !namespaceAllowed(ctx, r, userID, orgID, namespace) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "list tags: namespace not owned by caller", "user_id", userID, "namespace", namespace)
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("repo.name", name),
	)

	reg, ok := registryForRead(ctx, w, r)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	tl, err := reg.listTags(ctx, name)
	if err != nil {
		if isRegistryNotFound(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "registry error")
		slog.ErrorContext(ctx, "list tags: registry error", "name", name, "user_id", userID, "error", err)
		http.Error(w, "failed to list tags", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tl) //nolint:errcheck
}

func handleGetManifest(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleGetManifest")
	defer span.End()

	namespace := r.PathValue("namespace")
	image := r.PathValue("image")
	reference := r.PathValue("reference")
	name := namespace + "/" + image

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getManifest",
		"containers/repositories/"+namespace+"/"+image)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	if !namespaceAllowed(ctx, r, userID, orgID, namespace) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "get manifest: namespace not owned by caller", "user_id", userID, "namespace", namespace)
		http.Error(w, "manifest not found", http.StatusNotFound)
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("repo.name", name),
		attribute.String("manifest.reference", reference),
	)

	reg, ok := registryForRead(ctx, w, r)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	manifest, err := reg.getManifest(ctx, name, reference)
	if err != nil {
		if isRegistryNotFound(err) {
			http.Error(w, "manifest not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "registry error")
		slog.ErrorContext(ctx, "get manifest: registry error", "name", name, "reference", reference, "error", err)
		http.Error(w, "failed to get manifest", http.StatusBadGateway)
		return
	}

	span.SetAttributes(attribute.String("manifest.digest", manifest.Digest))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest) //nolint:errcheck
}

func handleDeleteManifest(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("containers").Start(r.Context(), "handleDeleteManifest")
	defer span.End()

	namespace := r.PathValue("namespace")
	image := r.PathValue("image")
	digest := r.PathValue("digest")
	name := namespace + "/" + image

	// Only allow digest-based deletes (sha256:...) to prevent accidental tag deletion.
	if !strings.HasPrefix(digest, "sha256:") {
		http.Error(w, "digest must be a sha256 digest (sha256:...)", http.StatusBadRequest)
		return
	}

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteManifest",
		"containers/repositories/"+namespace+"/"+image)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	if !namespaceAllowed(ctx, r, userID, orgID, namespace) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "delete manifest: namespace not owned by caller", "user_id", userID, "namespace", namespace)
		http.Error(w, "manifest not found", http.StatusNotFound)
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("repo.name", name),
		attribute.String("manifest.digest", digest),
	)

	reg, ok := registryForRead(ctx, w, r)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	if err := reg.deleteManifest(ctx, name, digest); err != nil {
		if isRegistryNotFound(err) {
			http.Error(w, "manifest not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "registry error")
		slog.ErrorContext(ctx, "delete manifest: registry error", "user_id", userID, "name", name, "digest", digest, "error", err)
		http.Error(w, "failed to delete manifest", http.StatusBadGateway)
		return
	}

	meterManifestsDeleted.Add(ctx, 1,
		metric.WithAttributes(attribute.String("repo.namespace", namespace)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "manifest deleted", "user_id", userID, "name", name, "digest", digest)
	w.WriteHeader(http.StatusNoContent)
}
