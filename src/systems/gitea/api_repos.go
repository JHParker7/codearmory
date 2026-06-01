package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

func handleListRepos(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListRepos")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRepo", "gitea/repos")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 50 {
		limit = 20
	}

	repos, err := gitea.listRepos(ctx, callerUsername, page, limit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("list repos: gitea error", "user_id", userID, "error", err)
		http.Error(w, "failed to list repositories", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(repos) //nolint:errcheck
}

func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleCreateRepo")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createRepo", "gitea/repos")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if _, ok := payload["name"]; !ok {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	repo, err := gitea.createRepo(ctx, callerUsername, payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("create repo: gitea error", "user_id", userID, "error", err)
		http.Error(w, "failed to create repository", http.StatusBadGateway)
		return
	}

	span.SetAttributes(attribute.String("repo.full_name", repo.FullName))
	span.SetStatus(codes.Ok, "")
	slog.Info("repo created", "user_id", userID, "repo", repo.FullName)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(repo) //nolint:errcheck
}

func handleGetRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleGetRepo")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRepo", "gitea/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	repo, err := gitea.getRepo(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("get repo: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to get repository", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(repo) //nolint:errcheck
}

func handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleDeleteRepo")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteRepo", "gitea/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := gitea.deleteRepo(ctx, owner, name, callerUsername); err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("delete repo: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to delete repository", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("repo deleted", "user_id", userID, "owner", owner, "name", name)
	w.WriteHeader(http.StatusNoContent)
}

func handleListBranches(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListBranches")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listBranch", "gitea/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	branches, err := gitea.listBranches(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("list branches: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to list branches", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(branches) //nolint:errcheck
}

func handleListTags(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListTags")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTag", "gitea/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	tags, err := gitea.listTags(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("list tags: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to list tags", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tags) //nolint:errcheck
}

func handleListReleases(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListReleases")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRelease", "gitea/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	releases, err := gitea.listReleases(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("list releases: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to list releases", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(releases) //nolint:errcheck
}

func handleListCommits(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListCommits")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listCommit", "gitea/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 50 {
		limit = 20
	}

	commits, err := gitea.listCommits(ctx, owner, name, callerUsername, page, limit)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("list commits: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to list commits", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(commits) //nolint:errcheck
}
