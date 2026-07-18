package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// concurrencyLimitBody is the PUT payload for an org/user concurrency override.
// MaxConcurrent <= 0 means unlimited.
type concurrencyLimitBody struct {
	MaxConcurrent int `json:"max_concurrent"`
}

// concurrencyLimitsResponse is the GET /concurrency-limits shape: the global env
// defaults plus every per-scope override. The UI needs both to render what a scope
// with no override actually resolves to.
type concurrencyLimitsResponse struct {
	Defaults concurrencyDefaults `json:"defaults"`
	Limits   []ConcurrencyLimit  `json:"limits"`
}

type concurrencyDefaults struct {
	Org  int `json:"org"`
	User int `json:"user"`
}

// ── List ─────────────────────────────────────────────────────────────────────

func handleListConcurrencyLimits(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleListConcurrencyLimits")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listConcurrencyLimit", "forge/concurrency-limits")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")

	rows, err := (ConcurrencyLimit{}).List(ctx, 0, 0)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list concurrency limits: db error", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	limits := make([]ConcurrencyLimit, len(rows))
	for i, row := range rows {
		limits[i] = row.(ConcurrencyLimit)
	}

	resp := concurrencyLimitsResponse{
		Defaults: concurrencyDefaults{Org: defaultMaxConcurrentPerOrg, User: defaultMaxConcurrentPerUser},
		Limits:   limits,
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list concurrency limits: success", "user_id", userID, "count", len(limits))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// ── Get ──────────────────────────────────────────────────────────────────────

func handleGetConcurrencyLimit(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGetConcurrencyLimit")
	defer span.End()

	scope, scopeID := r.PathValue("scope"), r.PathValue("scope_id")
	userID, ok := checkGatekeeper(ctx, w, r, "getConcurrencyLimit", "forge/concurrency-limits/"+scope+"/"+scopeID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("concurrency_limit.scope", scope), attribute.String("concurrency_limit.scope_id", scopeID))
	span.AddEvent("permission.granted")

	row, err := (ConcurrencyLimit{Scope: scope, ScopeID: scopeID}).Get(ctx)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		span.SetStatus(codes.Error, "concurrency limit not found")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "get concurrency limit: db error", "user_id", userID, "scope", scope, "scope_id", scopeID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(ConcurrencyLimit)) //nolint:errcheck
}

// ── Set (upsert) ───────────────────────────────────────────────────────────────

func handleSetConcurrencyLimit(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleSetConcurrencyLimit")
	defer span.End()

	scope, scopeID := r.PathValue("scope"), r.PathValue("scope_id")
	userID, ok := checkGatekeeper(ctx, w, r, "setConcurrencyLimit", "forge/concurrency-limits/"+scope+"/"+scopeID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("concurrency_limit.scope", scope), attribute.String("concurrency_limit.scope_id", scopeID))
	span.AddEvent("permission.granted")

	if !validateConcurrencyScope(scope) {
		span.SetStatus(codes.Error, "invalid scope")
		http.Error(w, "scope must be 'org' or 'user'", http.StatusBadRequest)
		return
	}
	if scopeID == "" {
		span.SetStatus(codes.Error, "empty scope_id")
		http.Error(w, "scope_id is required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var b concurrencyLimitBody
	if err := json.Unmarshal(body, &b); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if b.MaxConcurrent < 0 {
		span.SetStatus(codes.Error, "negative max_concurrent")
		http.Error(w, "max_concurrent must not be negative (0 = unlimited)", http.StatusBadRequest)
		return
	}

	cl := ConcurrencyLimit{Scope: scope, ScopeID: scopeID, MaxConcurrent: b.MaxConcurrent}
	if err := cl.Save(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db upsert failed")
		slog.ErrorContext(ctx, "set concurrency limit: db error", "user_id", userID, "scope", scope, "scope_id", scopeID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Return the persisted row (with updated_at) so callers see the canonical state.
	row, err := (ConcurrencyLimit{Scope: scope, ScopeID: scopeID}).Get(ctx)
	if err != nil {
		// The write succeeded; a read-back failure is not fatal to the client.
		span.SetStatus(codes.Ok, "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cl) //nolint:errcheck
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "set concurrency limit: success", "user_id", userID, "scope", scope, "scope_id", scopeID, "max_concurrent", b.MaxConcurrent)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(ConcurrencyLimit)) //nolint:errcheck
}

// ── Delete ───────────────────────────────────────────────────────────────────

func handleDeleteConcurrencyLimit(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleDeleteConcurrencyLimit")
	defer span.End()

	scope, scopeID := r.PathValue("scope"), r.PathValue("scope_id")
	userID, ok := checkGatekeeper(ctx, w, r, "deleteConcurrencyLimit", "forge/concurrency-limits/"+scope+"/"+scopeID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("concurrency_limit.scope", scope), attribute.String("concurrency_limit.scope_id", scopeID))
	span.AddEvent("permission.granted")

	if err := (ConcurrencyLimit{Scope: scope, ScopeID: scopeID}).Remove(ctx); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "concurrency limit not found")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete concurrency limit: db error", "user_id", userID, "scope", scope, "scope_id", scopeID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete concurrency limit: success", "user_id", userID, "scope", scope, "scope_id", scopeID)
	w.WriteHeader(http.StatusNoContent)
}
