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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRepo", "gitea_integration/repos")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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

	// When filtering by project the match can sit on any Gitea page, so fetch the
	// user's full repo set and filter/paginate in memory below. Without a filter,
	// proxy Gitea's own pagination directly.
	projectFilter := r.URL.Query().Get("project")
	var repos []Repo
	var err error
	if projectFilter != "" {
		const pageSize = 50
		const maxPages = 50
		for p := 1; p <= maxPages; p++ {
			var batch []Repo
			batch, err = gitea.listRepos(ctx, callerUsername, p, pageSize)
			if err != nil {
				break
			}
			repos = append(repos, batch...)
			if len(batch) < pageSize {
				break
			}
		}
	} else {
		repos, err = gitea.listRepos(ctx, callerUsername, page, limit)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "list repos: gitea error", "user_id", userID, "error", err)
		http.Error(w, "failed to list repositories", http.StatusBadGateway)
		return
	}

	// Stamp each repo with its project label from the local mapping table.
	names := make([]string, len(repos))
	for i := range repos {
		names[i] = repos[i].FullName
	}
	projects, perr := repoProjectsByFullName(ctx, names)
	if perr != nil {
		// When filtering by project, a failed label lookup would leave every
		// repo's Project empty and the filter below would silently drop them
		// all — making a transient DB error look like "no repos in this
		// project". Fail loudly instead. Without a filter the labels are
		// cosmetic, so degrade gracefully.
		if projectFilter != "" {
			span.RecordError(perr)
			span.SetStatus(codes.Error, "project lookup failed")
			slog.ErrorContext(ctx, "list repos: project lookup failed", "user_id", userID, "error", perr)
			http.Error(w, "failed to list repositories", http.StatusBadGateway)
			return
		}
		slog.WarnContext(ctx, "list repos: project lookup failed", "user_id", userID, "error", perr)
	} else {
		for i := range repos {
			repos[i].Project = projects[repos[i].FullName]
		}
	}
	if projectFilter != "" {
		filtered := make([]Repo, 0, len(repos))
		for _, rp := range repos {
			if rp.Project == projectFilter {
				filtered = append(filtered, rp)
			}
		}
		// Paginate the filtered set in memory so page/limit still apply.
		start := min((page-1)*limit, len(filtered))
		end := min(start+limit, len(filtered))
		repos = filtered[start:end]
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(repos) //nolint:errcheck
}

// handleSetRepoProject assigns (or, with an empty project, clears) the project
// label for a repo. The project is a view filter only; access is still gated by
// the repo permission check and ownerAllowed below.
func handleSetRepoProject(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleSetRepoProject")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "setRepoProject", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	fullName := owner + "/" + name
	rp := RepoProject{FullName: fullName, Project: req.Project}
	var err error
	if req.Project == "" {
		err = rp.Remove(ctx)
	} else {
		err = rp.Upsert(ctx)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "set repo project: db error", "user_id", userID, "repo", fullName, "error", err)
		http.Error(w, "failed to set project", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo project set", "user_id", userID, "repo", fullName, "project", req.Project)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rp) //nolint:errcheck
}

func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleCreateRepo")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createRepo", "gitea_integration/repos")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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
		slog.ErrorContext(ctx, "create repo: gitea error", "user_id", userID, "error", err)
		http.Error(w, "failed to create repository", http.StatusBadGateway)
		return
	}

	span.SetAttributes(attribute.String("repo.full_name", repo.FullName))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo created", "user_id", userID, "repo", repo.FullName)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(repo) //nolint:errcheck
}

func handleGetRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleGetRepo")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRepo", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	repo, err := gitea.getRepo(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "get repo: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to get repository", http.StatusBadGateway)
		return
	}

	if projects, perr := repoProjectsByFullName(ctx, []string{repo.FullName}); perr == nil {
		repo.Project = projects[repo.FullName]
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteRepo", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := gitea.deleteRepo(ctx, owner, name, callerUsername); err != nil {
		if isNotFoundErr(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "delete repo: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to delete repository", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo deleted", "user_id", userID, "owner", owner, "name", name)
	w.WriteHeader(http.StatusNoContent)
}

func handleListBranches(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListBranches")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listBranch", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	branches, err := gitea.listBranches(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "list branches: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTag", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	tags, err := gitea.listTags(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "list tags: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRelease", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	releases, err := gitea.listReleases(ctx, owner, name, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "list releases: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listCommit", "gitea_integration/repos/"+owner+"/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		span.SetStatus(codes.Ok, "")
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
			span.SetStatus(codes.Ok, "")
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.ErrorContext(ctx, "list commits: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to list commits", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(commits) //nolint:errcheck
}
