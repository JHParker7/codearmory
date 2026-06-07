package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

type registryTokenResponse struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// handleInternalRegistryToken issues a fresh Gitea API token scoped to
// package read/write for the calling user's linked Gitea account.
// Intended to be called by the containers service, not through Conductor.
// Auth is verified directly with Gatekeeper using the caller's Bearer token.
func handleInternalRegistryToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gitea").Start(r.Context(), "handleInternalRegistryToken")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRegistryToken", "gitea_integration/registry-token")
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
		slog.Error("registry token: get account", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	username := account.GiteaUsername

	if err := gitea.cleanRegistryTokens(ctx, username); err != nil {
		// Non-fatal: old tokens may linger but the new one will still work.
		slog.Warn("registry token: cleanup failed", "username", username, "error", err)
	}

	tokenName := fmt.Sprintf("codearmory-reg-%s", randHex(8))
	token, err := gitea.createRegistryToken(ctx, username, tokenName)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "token creation failed")
		slog.Error("registry token: create failed", "username", username, "error", err)
		http.Error(w, "failed to create registry token", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("registry token issued", "user_id", userID, "gitea_username", username)
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
