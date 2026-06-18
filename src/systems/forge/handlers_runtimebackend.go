package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// runtimeBackendBody is the create/update payload. Config and SecretRefs are
// optional maps; SecretRefs values are env var NAMES (never secret values), so
// the body and every response are safe to log and return.
type runtimeBackendBody struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Enabled    bool              `json:"enabled"`
	Config     map[string]string `json:"config"`
	SecretRefs map[string]string `json:"secret_refs"`
}

func validateRuntimeBackendBody(b runtimeBackendBody) error {
	if !validRuntimeTypes[b.Type] {
		return fmt.Errorf("type must be one of: docker, kubernetes, proxmox, kata")
	}
	switch b.Type {
	case "proxmox":
		return validateProxmoxBackend(b)
	case "kata":
		return validateKataBackend(b)
	}
	return nil
}

// ── List ─────────────────────────────────────────────────────────────────────

func handleListRuntimeBackends(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleListRuntimeBackends")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listRuntimeBackend", "forge/runtime-backends")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "list runtime backends request", "user_id", userID)

	rows, err := (RuntimeBackend{}).List(ctx, 0, 0)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list runtime backends: db error", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	backends := make([]RuntimeBackend, len(rows))
	for i, row := range rows {
		backends[i] = row.(RuntimeBackend)
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list runtime backends: success", "user_id", userID, "count", len(backends))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backends) //nolint:errcheck
}

// ── Get ──────────────────────────────────────────────────────────────────────

func handleGetRuntimeBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGetRuntimeBackend")
	defer span.End()

	name := r.PathValue("name")
	userID, ok := checkGatekeeper(ctx, w, r, "getRuntimeBackend", "forge/runtime-backends/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("runtime_backend.name", name),
	)
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "get runtime backend request", "user_id", userID, "name", name)

	row, err := (RuntimeBackend{Name: name}).Get(ctx)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		span.SetStatus(codes.Error, "runtime backend not found")
		slog.WarnContext(ctx, "get runtime backend: not found", "user_id", userID, "name", name)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "get runtime backend: db error", "user_id", userID, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	b := row.(RuntimeBackend)

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get runtime backend: success", "user_id", userID, "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(b) //nolint:errcheck
}

// ── Create ───────────────────────────────────────────────────────────────────

func handleCreateRuntimeBackend(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCreateRuntimeBackend")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "createRuntimeBackend", "forge/runtime-backends")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "create runtime backend request", "user_id", userID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var b runtimeBackendBody
	if err := json.Unmarshal(body, &b); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if b.Name == "" {
		span.SetStatus(codes.Error, "name required")
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := validateRuntimeBackendBody(b); err != nil {
		span.SetStatus(codes.Error, "validation failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("runtime_backend.name", b.Name))

	backend := RuntimeBackend{
		Name:       b.Name,
		Type:       b.Type,
		Enabled:    b.Enabled,
		Config:     orEmptyMap(b.Config),
		SecretRefs: orEmptyMap(b.SecretRefs),
	}
	if err := backend.Add(ctx); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			span.SetStatus(codes.Ok, "")
			slog.WarnContext(ctx, "create runtime backend: already exists", "user_id", userID, "name", b.Name)
			http.Error(w, "runtime backend already exists", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create runtime backend: db error", "user_id", userID, "name", b.Name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create runtime backend: success", "user_id", userID, "name", backend.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(backend) //nolint:errcheck
}

// ── Update ───────────────────────────────────────────────────────────────────

// handleUpdateRuntimeBackend evicts the registry cache on success so the changed
// config applies to subsequent jobs without a process restart.
func handleUpdateRuntimeBackend(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleUpdateRuntimeBackend")
		defer span.End()

		name := r.PathValue("name")
		userID, ok := checkGatekeeper(ctx, w, r, "updateRuntimeBackend", "forge/runtime-backends/"+name)
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("runtime_backend.name", name),
		)
		span.AddEvent("permission.granted")
		slog.InfoContext(ctx, "update runtime backend request", "user_id", userID, "name", name)

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil || !json.Valid(body) {
			span.SetStatus(codes.Error, "invalid request body")
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var b runtimeBackendBody
		if err := json.Unmarshal(body, &b); err != nil {
			span.SetStatus(codes.Error, "invalid request body")
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := validateRuntimeBackendBody(b); err != nil {
			span.SetStatus(codes.Error, "validation failed")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		backend := RuntimeBackend{
			Name:       name,
			Type:       b.Type,
			Enabled:    b.Enabled,
			Config:     orEmptyMap(b.Config),
			SecretRefs: orEmptyMap(b.SecretRefs),
		}
		if err := backend.Update(ctx); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				span.SetStatus(codes.Error, "runtime backend not found")
				slog.WarnContext(ctx, "update runtime backend: not found", "user_id", userID, "name", name)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "db update failed")
			slog.ErrorContext(ctx, "update runtime backend: db error", "user_id", userID, "name", name, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		reg.Evict(name)

		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "update runtime backend: success", "user_id", userID, "name", name)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(backend) //nolint:errcheck
	}
}

// ── Delete ───────────────────────────────────────────────────────────────────

func handleDeleteRuntimeBackend(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleDeleteRuntimeBackend")
		defer span.End()

		name := r.PathValue("name")
		userID, ok := checkGatekeeper(ctx, w, r, "deleteRuntimeBackend", "forge/runtime-backends/"+name)
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("runtime_backend.name", name),
		)
		span.AddEvent("permission.granted")
		slog.InfoContext(ctx, "delete runtime backend request", "user_id", userID, "name", name)

		// The "default" backend is resolved eagerly at startup and is the backfill
		// target for every runner class; deleting it would break new jobs. Refuse.
		if name == "default" {
			span.SetStatus(codes.Error, "cannot delete default backend")
			slog.WarnContext(ctx, "delete runtime backend: refused (default)", "user_id", userID)
			http.Error(w, "cannot delete the default runtime backend", http.StatusConflict)
			return
		}

		if err := (RuntimeBackend{Name: name}).Remove(ctx); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				span.SetStatus(codes.Error, "runtime backend not found")
				slog.WarnContext(ctx, "delete runtime backend: not found", "user_id", userID, "name", name)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "db delete failed")
			slog.ErrorContext(ctx, "delete runtime backend: db error", "user_id", userID, "name", name, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		reg.Evict(name)

		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "delete runtime backend: success", "user_id", userID, "name", name)
		w.WriteHeader(http.StatusNoContent)
	}
}

// orEmptyMap returns m, or a non-nil empty map when m is nil, so the column is
// stored as '{}' jsonb rather than SQL NULL.
func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
