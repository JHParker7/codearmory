package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// giteaInternalKey authenticates trusted service-to-service callers (forge) on
// the /internal/clone-token endpoint. Empty disables the endpoint.
var giteaInternalKey = secret("GITEA_INTERNAL_KEY")

// internalKeyOK constant-time compares the X-Internal-Key header to the
// configured shared key. Returns false when the key is unset.
func internalKeyOK(r *http.Request) bool {
	if giteaInternalKey == "" {
		return false
	}
	got := r.Header.Get("X-Internal-Key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(giteaInternalKey)) == 1
}

// authedCloneURL builds an HTTPS clone URL with embedded basic-auth credentials,
// e.g. https://user:token@gitea-host/owner/repo.git.
func authedCloneURL(baseURL, username, token, owner, repo string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(username, token)
	u.Path = "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + ".git"
	return u.String(), nil
}

type registryTokenResponse struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

type cloneTokenResponse struct {
	Username string `json:"username"`
	Token    string `json:"token"`
	CloneURL string `json:"clone_url"`
}

// handleInternalCloneToken issues a short-lived repository-read Gitea token for a
// user's linked account and returns an authenticated HTTPS clone URL for the
// requested repo. Intended to be called by the forge worker over the internal
// network, authenticated by the shared GITEA_INTERNAL_KEY — not through Conductor.
func handleInternalCloneToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleInternalCloneToken")
	defer span.End()

	if !internalKeyOK(r) {
		slog.WarnContext(ctx, "clone token: unauthorized internal call")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		UserID string `json:"user_id"`
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.UserID == "" || req.Owner == "" || req.Repo == "" {
		http.Error(w, "user_id, owner, and repo are required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(
		attribute.String("user.id", req.UserID),
		attribute.String("repo", req.Owner+"/"+req.Repo),
	)

	accountRow, err := (GiteaAccount{UserID: req.UserID}).Get(ctx)
	if err != nil {
		if isDbNotFound(err) {
			http.Error(w, "no gitea account linked", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "clone token: get account", "user_id", req.UserID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	username := accountRow.(GiteaAccount).GiteaUsername

	// Gitea PATs cannot be given an expiry, so clone tokens are pruned to bound
	// accumulation. Mint the new token FIRST, then prune older ones keeping a small
	// recent window — deleting before creating would race a concurrent execution
	// and revoke its in-flight token out from under a running clone.
	tokenName := fmt.Sprintf("%s%s", cloneTokenPrefix, randHex(8))
	token, err := gitea.createCloneToken(ctx, username, tokenName)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "token creation failed")
		slog.ErrorContext(ctx, "clone token: create failed", "username", username, "error", err)
		http.Error(w, "failed to create clone token", http.StatusInternalServerError)
		return
	}
	if err := gitea.pruneCloneTokens(ctx, username, cloneTokenRetain); err != nil {
		// Non-fatal: old tokens may linger but the new one still works.
		slog.WarnContext(ctx, "clone token: prune failed", "username", username, "error", err)
	}

	cloneURL, err := authedCloneURL(gitea.baseURL, username, token, req.Owner, req.Repo)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "build clone url")
		slog.ErrorContext(ctx, "clone token: build url", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "clone token issued", "user_id", req.UserID, "gitea_username", username, "repo", req.Owner+"/"+req.Repo)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cloneTokenResponse{Username: username, Token: token, CloneURL: cloneURL}) //nolint:errcheck
}

// handleInternalRegistryToken issues a fresh Gitea API token scoped to
// package read/write for the calling user's linked Gitea account.
// Intended to be called by the containers service, not through Conductor.
// Auth is verified directly with Gatekeeper using the caller's Bearer token.
func handleInternalRegistryToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleInternalRegistryToken")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRegistryToken", "gitea_integration/registry-token")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	accountRow, err := (GiteaAccount{UserID: userID}).Get(ctx)
	if err != nil {
		if isDbNotFound(err) {
			http.Error(w, "no gitea account linked", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "registry token: get account", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	account := accountRow.(GiteaAccount)

	username := account.GiteaUsername

	if err := gitea.cleanRegistryTokens(ctx, username); err != nil {
		// Non-fatal: old tokens may linger but the new one will still work.
		slog.WarnContext(ctx, "registry token: cleanup failed", "username", username, "error", err)
	}

	tokenName := fmt.Sprintf("%s%s", registryTokenPrefix, randHex(8))
	token, err := gitea.createRegistryToken(ctx, username, tokenName)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "token creation failed")
		slog.ErrorContext(ctx, "registry token: create failed", "username", username, "error", err)
		http.Error(w, "failed to create registry token", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "registry token issued", "user_id", userID, "gitea_username", username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(registryTokenResponse{Username: username, Token: token}) //nolint:errcheck
}

func randHex(n int) string {
	const chars = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
