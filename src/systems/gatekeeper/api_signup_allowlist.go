package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type signupAllowlistRequest struct {
	Email string `json:"email"`
	Note  string `json:"note"`
}

type signupPolicyRequest struct {
	InviteOnly bool `json:"invite_only"`
}

// handleCreateSignupAllowlist adds an email or @domain rule to the invite-only
// signup allowlist. Admin-gated (createSignupAllowlist) — there is no default
// grant, so only the platform admin (or an explicitly-granted user) can manage it.
func handleCreateSignupAllowlist(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateSignupAllowlist")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	if !requirePermission(w, r, "createSignupAllowlist", "gatekeeper/signup-allowlist") {
		span.SetStatus(codes.Ok, "")
		return
	}

	var req signupAllowlistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	value, err := normalizeAllowlistValue(req.Email)
	if err != nil {
		span.SetStatus(codes.Error, "invalid email")
		slog.WarnContext(ctx, "create signup allowlist: invalid value", "caller_id", callerID, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("allowlist.value", value))

	// Reject a duplicate active entry so the list stays clean and idempotent.
	var count int64
	if err := connectRead().WithContext(ctx).Model(&SignupAllowlistEntry{}).
		Where("active = ? AND lower(email) = ?", true, value).Count(&count).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "create signup allowlist: duplicate check failed", "caller_id", callerID, "error", err)
		http.Error(w, "failed to create allowlist entry", http.StatusInternalServerError)
		return
	}
	if count > 0 {
		span.SetStatus(codes.Error, "duplicate")
		http.Error(w, duplicateAllowlistErr(value).Error(), http.StatusConflict)
		return
	}

	entry := SignupAllowlistEntry{
		EntryID:   uuid.New().String(),
		Email:     value,
		Note:      req.Note,
		CreatedBy: callerID,
	}
	if err := entry.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create signup allowlist: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to create allowlist entry", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create signup allowlist: success", "caller_id", callerID, "entry_id", entry.EntryID, "value", value)
	writeAudit(ctx, callerID, "user", "signup_allowlist.create", entry.EntryID, value)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(entry)
}

// handleListSignupAllowlist lists the active signup allowlist entries. Admin-gated.
func handleListSignupAllowlist(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListSignupAllowlist")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	if !requirePermission(w, r, "listSignupAllowlist", "gatekeeper/signup-allowlist") {
		span.SetStatus(codes.Ok, "")
		return
	}

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	rows, err := (SignupAllowlistEntry{}).List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list failed")
		slog.ErrorContext(ctx, "list signup allowlist: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list allowlist", http.StatusInternalServerError)
		return
	}
	entries := make([]SignupAllowlistEntry, len(rows))
	for i, row := range rows {
		entries[i] = row.(SignupAllowlistEntry)
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// handleDeleteSignupAllowlist soft-deletes an allowlist entry. Admin-gated.
func handleDeleteSignupAllowlist(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteSignupAllowlist")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("entry.id", id),
	)

	if !requirePermission(w, r, "deleteSignupAllowlist", "gatekeeper/signup-allowlist/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}

	row, err := (SignupAllowlistEntry{EntryID: id}).Get(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "not found")
		slog.WarnContext(ctx, "delete signup allowlist: not found", "caller_id", callerID, "entry_id", id)
		http.Error(w, "allowlist entry not found", http.StatusNotFound)
		return
	}
	entry := row.(SignupAllowlistEntry)

	if err := entry.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete signup allowlist: db error", "caller_id", callerID, "entry_id", id, "error", err)
		http.Error(w, "failed to delete allowlist entry", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete signup allowlist: success", "caller_id", callerID, "entry_id", id)
	writeAudit(ctx, callerID, "user", "signup_allowlist.delete", id, entry.Email)
	w.WriteHeader(http.StatusNoContent)
}

// handleGetSignupPolicy returns the current instance-wide signup policy. Admin-gated.
func handleGetSignupPolicy(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetSignupPolicy")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	if !requirePermission(w, r, "getSignupPolicy", "gatekeeper/signup-policy") {
		span.SetStatus(codes.Ok, "")
		return
	}

	policy, err := getSignupPolicy(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "get signup policy: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to read signup policy", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(policy)
}

// handleUpdateSignupPolicy toggles invite-only registration on or off. Admin-gated.
func handleUpdateSignupPolicy(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleUpdateSignupPolicy")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	if !requirePermission(w, r, "updateSignupPolicy", "gatekeeper/signup-policy") {
		span.SetStatus(codes.Ok, "")
		return
	}

	var req signupPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := setSignupPolicy(ctx, req.InviteOnly, callerID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update signup policy: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to update signup policy", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.Bool("signup.invite_only", req.InviteOnly))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "update signup policy: success", "caller_id", callerID, "invite_only", req.InviteOnly)
	writeAudit(ctx, callerID, "user", "signup_policy.update", signupPolicySingletonID, boolToStr(req.InviteOnly))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(SignupPolicy{
		ID:         signupPolicySingletonID,
		InviteOnly: req.InviteOnly,
		UpdatedAt:  time.Now(),
		UpdatedBy:  callerID,
	})
}

func boolToStr(b bool) string {
	if b {
		return "invite_only=true"
	}
	return "invite_only=false"
}
