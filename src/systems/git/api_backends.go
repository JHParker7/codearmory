package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// shortID returns a short random identifier for minted-token names.
func shortID() string { return strings.ReplaceAll(uuid.New().String(), "-", "")[:12] }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func handleListBackends(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleListBackends")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listBackend", "git_connector/backends")
	if !ok {
		return
	}
	backends, err := listBackends(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "list backends", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	views := make([]backendView, 0, len(backends))
	for _, b := range backends {
		views = append(views, b.view())
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, views)
}

func handleCreateBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleCreateBackend")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "createBackend", "git_connector/backends")
	if !ok {
		return
	}
	var req createBackendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validBackendType(req.Type) {
		http.Error(w, "type must be one of github, gitlab, forgejo, generic", http.StatusBadRequest)
		return
	}
	if !validModeForType(req.Type, req.Auth.Mode) {
		http.Error(w, "auth.mode is not valid for the backend type", http.StatusBadRequest)
		return
	}
	host, err := deriveHost(req.BaseURL)
	if err != nil {
		http.Error(w, "base_url: "+err.Error(), http.StatusBadRequest)
		return
	}
	enc, err := sealAuth(req.Auth)
	if err != nil {
		slog.ErrorContext(ctx, "seal auth", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	b := GitBackend{
		ID:           uuid.New().String(),
		Owner:        userID,
		Name:         req.Name,
		Type:         req.Type,
		BaseURL:      strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"),
		Host:         host,
		AuthMode:     req.Auth.Mode,
		AuthEnc:      enc,
		PreferMirror: req.PreferMirror,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := b.Add(ctx); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "a backend with this name or host already exists", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "create backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "backend created", "user_id", userID, "backend_id", b.ID, "type", b.Type, "host", b.Host)
	writeJSON(w, http.StatusCreated, b.view())
}

func handleGetBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleGetBackend")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "getBackend", "git_connector/backends/"+id)
	if !ok {
		return
	}
	b, err := getBackendByID(ctx, userID, id)
	if errors.Is(err, errBackendNotFound) {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, b.view())
}

func handleUpdateBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleUpdateBackend")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "updateBackend", "git_connector/backends/"+id)
	if !ok {
		return
	}
	var req updateBackendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	b, err := getBackendByID(ctx, userID, id)
	if errors.Is(err, errBackendNotFound) {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if bu := strings.TrimSpace(req.BaseURL); bu != "" {
		host, err := deriveHost(bu)
		if err != nil {
			http.Error(w, "base_url: "+err.Error(), http.StatusBadRequest)
			return
		}
		b.BaseURL = strings.TrimRight(bu, "/")
		b.Host = host
	}
	if req.Auth != nil {
		if !validModeForType(b.Type, req.Auth.Mode) {
			http.Error(w, "auth.mode is not valid for the backend type", http.StatusBadRequest)
			return
		}
		enc, err := sealAuth(*req.Auth)
		if err != nil {
			slog.ErrorContext(ctx, "seal auth", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		b.AuthMode = req.Auth.Mode
		b.AuthEnc = enc
	}
	if req.PreferMirror != nil {
		b.PreferMirror = *req.PreferMirror
	}
	if err := b.Update(ctx); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "a backend with this host already exists", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "update backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, b.view())
}

func handleDeleteBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleDeleteBackend")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "deleteBackend", "git_connector/backends/"+id)
	if !ok {
		return
	}
	b := GitBackend{ID: id, Owner: userID}
	if err := b.Remove(ctx); err != nil {
		if errors.Is(err, errBackendNotFound) {
			http.Error(w, "backend not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "delete backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "backend deleted", "user_id", userID, "backend_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestBackend exercises the credential path (without a repo) so the caller
// can confirm an app/oauth/admin backend can actually mint before relying on it.
func handleTestBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleTestBackend")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "testBackend", "git_connector/backends/"+id)
	if !ok {
		return
	}
	b, err := getBackendByID(ctx, userID, id)
	if errors.Is(err, errBackendNotFound) {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	cred, updated, err := mintForBackend(ctx, b, "")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	persistRotatedAuth(ctx, b, updated)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           cred.Secret != "",
		"backend_type": b.Type,
		"auth_mode":    b.AuthMode,
		"expires_at":   cred.ExpiresAt,
	})
}

// isUniqueViolation reports whether err is a unique-constraint failure across the
// postgres and sqlite drivers (sqlite is used in tests).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

// persistRotatedAuth stores a rotated authConfig (e.g. GitLab refresh token) when
// minting produced one. Failures are logged, not fatal — the cred is still valid.
func persistRotatedAuth(ctx context.Context, b GitBackend, updated *authConfig) {
	if updated == nil {
		return
	}
	enc, err := sealAuth(*updated)
	if err != nil {
		slog.ErrorContext(ctx, "seal rotated auth", "backend_id", b.ID, "error", err)
		return
	}
	b.AuthEnc = enc
	b.AuthMode = updated.Mode
	if err := b.Update(ctx); err != nil {
		slog.ErrorContext(ctx, "persist rotated auth", "backend_id", b.ID, "error", err)
	}
}
