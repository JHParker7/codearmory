package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// resolveAndMint is the shared body of the user-facing and internal endpoints:
// resolve repoURL's host to one of owner's backends and mint a credential.
func resolveAndMint(ctx context.Context, owner, repoURL string) (credential, error) {
	host, err := deriveHost(repoURL)
	if err != nil {
		return credential{}, err
	}
	b, err := getBackendByHost(ctx, owner, host)
	if err != nil {
		return credential{}, err
	}
	cred, updated, err := mintForBackend(ctx, b, repoURL)
	if err != nil {
		return credential{}, err
	}
	persistRotatedAuth(ctx, b, updated)
	if meterCredentialsMinted != nil {
		meterCredentialsMinted.Add(ctx, 1, metric.WithAttributes(attribute.String("backend.type", b.Type)))
	}
	return cred, nil
}

func handleMintCredential(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleMintCredential")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "mintCredential", "git_connector/credentials")
	if !ok {
		return
	}
	var req mintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RepoURL == "" {
		http.Error(w, "repo_url is required", http.StatusBadRequest)
		return
	}
	cred, err := resolveAndMint(ctx, userID, req.RepoURL)
	if errors.Is(err, errBackendNotFound) {
		http.Error(w, "no git backend linked for that repository host", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "mint credential", "user_id", userID, "error", err)
		http.Error(w, "failed to mint credential: "+err.Error(), http.StatusBadGateway)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "credential minted", "user_id", userID, "backend", cred.Backend, "type", cred.BackendType)
	writeJSON(w, http.StatusOK, cred)
}

// handleInternalCloneToken serves forge/workflows. It authenticates with the
// shared GIT_INTERNAL_KEY HMAC and mints a clone URL for the named user. This
// replaces gitea_integration's /internal/clone-token with a backend-agnostic one.
func handleInternalCloneToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleInternalCloneToken")
	defer span.End()

	if internalKey == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(internalKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req internalMintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" || req.RepoURL == "" {
		http.Error(w, "user_id and repo_url are required", http.StatusBadRequest)
		return
	}
	cred, err := resolveAndMint(ctx, req.UserID, req.RepoURL)
	if errors.Is(err, errBackendNotFound) {
		http.Error(w, "no git backend linked for that repository host", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "internal clone-token", "user_id", req.UserID, "error", err)
		http.Error(w, "failed to mint credential", http.StatusBadGateway)
		return
	}
	if cred.CloneURL == "" {
		http.Error(w, "could not build clone url", http.StatusBadGateway)
		return
	}
	// Opt-in pull-through cache: for a PreferMirror backend, hand back git-factory's
	// warm in-cluster URL instead of upstream's. Best-effort — an empty result means the
	// mirror is off/unreachable and we keep the upstream URL, so clones never break.
	if mirrorURL, mirrorExp := maybeMirrorCloneURL(ctx, req.UserID, req.RepoURL, cred); mirrorURL != "" {
		cred.CloneURL = mirrorURL
		cred.ExpiresAt = mirrorExp
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"clone_url":  cred.CloneURL,
		"expires_at": cred.ExpiresAt,
	})
}
