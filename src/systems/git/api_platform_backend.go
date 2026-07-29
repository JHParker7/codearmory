package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// handleInternalPlatformBackend registers the in-cluster git host as a platform-owned
// backend. Builder calls it every reconcile pass after it deploys git-factory, which is
// what makes the git-factory ↔ git_connector link self-establishing: no operator has to
// create a backend or mint a durable token for one.
//
// Auth is the same shared GIT_INTERNAL_KEY that guards /internal/clone-token — the
// east-west key builder reads straight from git's own Secret. The endpoint is
// deliberately narrow: it can only write backendGitFactory/modeService rows, which carry
// no credential material, so even a leaked internal key cannot use it to plant a
// credential-bearing backend or to overwrite a user's own link (platform rows live under
// platformOwner, which no user can own).
func handleInternalPlatformBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleInternalPlatformBackend")
	defer span.End()

	if internalKey == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(internalKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.TrimSpace(req.BaseURL) == "" {
		http.Error(w, "name and base_url are required", http.StatusBadRequest)
		return
	}
	host, err := deriveHost(req.BaseURL)
	if err != nil {
		http.Error(w, "base_url: "+err.Error(), http.StatusBadRequest)
		return
	}
	// modeService carries no secret, but the row still goes through sealAuth so every
	// backend is stored in one shape and openAuth has something well-formed to read.
	enc, err := sealAuth(authConfig{Mode: modeService})
	if err != nil {
		slog.ErrorContext(ctx, "seal platform auth", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	b, err := upsertPlatformBackend(ctx, GitBackend{
		ID:        uuid.New().String(),
		Owner:     platformOwner,
		Name:      req.Name,
		Type:      backendGitFactory,
		BaseURL:   strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"),
		Host:      host,
		AuthMode:  modeService,
		AuthEnc:   enc,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		slog.ErrorContext(ctx, "upsert platform backend", "host", host, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "platform git backend registered", "backend_id", b.ID, "host", b.Host)
	writeJSON(w, http.StatusOK, b.view())
}
