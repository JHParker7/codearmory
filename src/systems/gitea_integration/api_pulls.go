package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var validPRStates = []string{"open", "closed", "all"}

func handleListPulls(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleListPulls")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listPull", "gitea_integration/repos/"+owner+"/"+name+"/pulls")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
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
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	if !slices.Contains(validPRStates, state) {
		http.Error(w, "state must be one of: open, closed, all", http.StatusBadRequest)
		return
	}

	prs, err := gitea.listPulls(ctx, owner, name, state, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("list pulls: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to list pull requests", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(prs) //nolint:errcheck
}

func handleCreatePull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleCreatePull")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createPull", "gitea_integration/repos/"+owner+"/"+name+"/pulls")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
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
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	for _, field := range []string{"title", "head", "base"} {
		if _, ok := payload[field]; !ok {
			http.Error(w, field+" is required", http.StatusBadRequest)
			return
		}
	}

	pr, err := gitea.createPull(ctx, owner, name, callerUsername, payload)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("create pull: gitea error", "user_id", userID, "owner", owner, "name", name, "error", err)
		http.Error(w, "failed to create pull request", http.StatusBadGateway)
		return
	}

	span.SetAttributes(attribute.Int64("pr.number", pr.Number))
	span.SetStatus(codes.Ok, "")
	slog.Info("pull request created", "user_id", userID, "owner", owner, "name", name, "pr", pr.Number)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(pr) //nolint:errcheck
}

func handleGetPull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleGetPull")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")
	index := r.PathValue("index")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getPull",
		"gitea_integration/repos/"+owner+"/"+name+"/pulls/"+index)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
		attribute.String("pull.index", index),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	pr, err := gitea.getPull(ctx, owner, name, index, callerUsername)
	if err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "pull request not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("get pull: gitea error", "user_id", userID, "owner", owner, "name", name, "index", index, "error", err)
		http.Error(w, "failed to get pull request", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pr) //nolint:errcheck
}

func handleMergePull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleMergePull")
	defer span.End()

	owner := r.PathValue("owner")
	name := r.PathValue("name")
	index := r.PathValue("index")

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "mergePull",
		"gitea_integration/repos/"+owner+"/"+name+"/pulls/"+index)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.String("repo.name", owner+"/"+name),
		attribute.String("pull.index", index),
	)

	callerUsername, ok := sudoFor(ctx, w, userID)
	if !ok {
		return
	}
	if !ownerAllowed(owner, callerUsername, resolveOrgName(ctx, r, orgID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if _, ok := payload["Do"]; !ok {
		payload["Do"] = "merge"
	}

	if err := gitea.mergePull(ctx, owner, name, index, callerUsername, payload); err != nil {
		if isNotFoundErr(err) {
			http.Error(w, "pull request not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea error")
		slog.Error("merge pull: gitea error", "user_id", userID, "owner", owner, "name", name, "index", index, "error", err)
		http.Error(w, "failed to merge pull request", http.StatusBadGateway)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("pull request merged", "user_id", userID, "owner", owner, "name", name, "index", index)
	w.WriteHeader(http.StatusNoContent)
}
