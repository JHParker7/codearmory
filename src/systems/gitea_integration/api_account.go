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

type linkAccountRequest struct {
	GiteaUsername string `json:"gitea_username"`
	// GiteaToken is a personal access token for the claimed Gitea account.
	// It is used once to prove ownership and is never stored.
	GiteaToken string `json:"gitea_token"`
}

func handleGetAccount(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleGetAccount")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getAccount", "gitea/account")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	account, err := getAccount(ctx, userID)
	if err != nil {
		if isDbNotFound(err) {
			http.Error(w, "no gitea account linked", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get account: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to get account", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("account.gitea_username", account.GiteaUsername))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(account) //nolint:errcheck
}

func handleLinkAccount(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleLinkAccount")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "linkAccount", "gitea/account")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	var req linkAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.GiteaUsername == "" {
		http.Error(w, "gitea_username is required", http.StatusBadRequest)
		return
	}
	if req.GiteaToken == "" {
		http.Error(w, "gitea_token is required to verify account ownership", http.StatusBadRequest)
		return
	}

	// Verify the caller controls the claimed Gitea account by authenticating
	// with their own token (no admin Sudo) and checking the returned login.
	verifiedLogin, err := gitea.verifyUserToken(ctx, req.GiteaToken)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gitea token verification failed")
		slog.Warn("link account: gitea token invalid", "user_id", userID, "claimed_username", req.GiteaUsername, "error", err)
		http.Error(w, "gitea_token is invalid or the Gitea instance is unreachable", http.StatusUnprocessableEntity)
		return
	}
	if verifiedLogin != req.GiteaUsername {
		span.SetStatus(codes.Error, "gitea username mismatch")
		slog.Warn("link account: username mismatch", "user_id", userID, "claimed", req.GiteaUsername, "actual", verifiedLogin)
		http.Error(w, "gitea_token does not belong to the claimed gitea_username", http.StatusUnprocessableEntity)
		return
	}

	existing, err := getAccount(ctx, userID)
	now := time.Now().UTC()
	if isDbNotFound(err) {
		account := GiteaAccount{
			UserID:        userID,
			GiteaUsername: req.GiteaUsername,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := account.Add(ctx); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db insert failed")
			slog.Error("link account: db error", "user_id", userID, "error", err)
			http.Error(w, "failed to link account", http.StatusInternalServerError)
			return
		}
		span.SetAttributes(attribute.String("account.gitea_username", req.GiteaUsername))
		span.SetStatus(codes.Ok, "")
		slog.Info("gitea account linked", "user_id", userID, "gitea_username", req.GiteaUsername)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(account) //nolint:errcheck
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("link account: fetch error", "user_id", userID, "error", err)
		http.Error(w, "failed to link account", http.StatusInternalServerError)
		return
	}

	existing.GiteaUsername = req.GiteaUsername
	existing.UpdatedAt = now
	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("link account: update error", "user_id", userID, "error", err)
		http.Error(w, "failed to update account link", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("account.gitea_username", req.GiteaUsername))
	span.SetStatus(codes.Ok, "")
	slog.Info("gitea account updated", "user_id", userID, "gitea_username", req.GiteaUsername)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing) //nolint:errcheck
}

func handleUnlinkAccount(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleUnlinkAccount")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "unlinkAccount", "gitea/account")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	account, err := getAccount(ctx, userID)
	if err != nil {
		if isDbNotFound(err) {
			http.Error(w, "no gitea account linked", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("unlink account: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to unlink account", http.StatusInternalServerError)
		return
	}

	if err := account.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("unlink account: remove error", "user_id", userID, "error", err)
		http.Error(w, "failed to unlink account", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("gitea account unlinked", "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

