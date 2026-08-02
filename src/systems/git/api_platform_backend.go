package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// handleInternalPlatformBackend registers an in-cluster git host as a platform-owned
// backend, over the wire.
//
// The platform's own git host no longer needs this: git-factory is a core, Helm-deployed
// service, so git_connector seeds that row itself from GIT_FACTORY_URL at startup (see
// platform_backend.go) and the link establishes with no caller at all. The endpoint stays
// for the other ways a platform backend can arrive — a builder that has not yet been
// upgraded and still POSTs here on every reconcile pass, and any future platform-deployed
// git host whose address is not known at deploy time. Both paths share
// registerPlatformBackend, so they cannot drift apart in what they write.
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
	b, err := registerPlatformBackend(ctx, req.Name, req.BaseURL)
	if err != nil {
		var badInput platformBackendInputError
		if errors.As(err, &badInput) {
			http.Error(w, badInput.Error(), http.StatusBadRequest)
			return
		}
		slog.ErrorContext(ctx, "register platform backend", "name", req.Name, "base_url", req.BaseURL, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "platform git backend registered", "backend_id", b.ID, "host", b.Host)
	writeJSON(w, http.StatusOK, b.view())
}
