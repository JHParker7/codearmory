package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type roleRequest struct {
	PermissionsIDs []string `json:"permissions_ids"`
	OrgID          *string  `json:"org_id"`
}

func handleCreateRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateRole")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("create role request", "caller_id", callerID)

	if !requirePermission(w, r, "createRole", "gatekeeper/roles") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("create role: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.Int("role.permissions_count", len(req.PermissionsIDs)))

	orgID := req.OrgID
	callerRow, err := (User{UserID: callerID}).Get(ctx)
	if err == nil && orgID == nil {
		orgID = callerRow.(User).OrgID
	}
	callerOrgID := (*string)(nil)
	if err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	// Validate that every supplied permission ID exists and belongs to the caller's org.
	if err := validatePermissionIDs(ctx, req.PermissionsIDs, callerOrgID); err != nil {
		span.SetStatus(codes.Error, "invalid permission id")
		slog.Warn("create role: "+err.Error(), "caller_id", callerID)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: req.PermissionsIDs, OrgID: orgID, OwnerID: callerID}
	if err := role.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create role: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to create role", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("role.id", role.RoleID))
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("role.id", role.RoleID),
		attribute.Int("role.permissions_count", len(req.PermissionsIDs)),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("create role: success", "caller_id", callerID, "role_id", role.RoleID, "permissions_count", len(req.PermissionsIDs))
	writeAudit(ctx, callerID, "user", "role.create", role.RoleID, fmt.Sprintf("permissions_count=%d", len(req.PermissionsIDs)))
	row, _ := role.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(row.(Role))
}

func handleGetRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetRole")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("role.id", id),
	)
	slog.Info("get role request", "caller_id", callerID, "role_id", id)

	if !requirePermission(w, r, "getRole", "gatekeeper/roles/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Role{RoleID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "role not found")
		slog.Warn("get role: not found", "caller_id", callerID, "role_id", id)
		http.Error(w, "role not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("role.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("get role: success", "caller_id", callerID, "role_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Role))
}

func handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleUpdateRole")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("role.id", id),
	)
	slog.Info("update role request", "caller_id", callerID, "role_id", id)

	if !requirePermission(w, r, "updateRole", "gatekeeper/roles/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("update role: invalid request body", "caller_id", callerID, "role_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.Int("new.permissions_count", len(req.PermissionsIDs)))

	row, err := (Role{RoleID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "role not found")
		slog.Warn("update role: not found", "caller_id", callerID, "role_id", id)
		http.Error(w, "role not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("role.id", id)))

	role := row.(Role)

	// Validate permission IDs before updating — prevent cross-tenant role poisoning.
	callerRow, cerr := (User{UserID: callerID}).Get(ctx)
	var callerOrgID *string
	if cerr == nil {
		callerOrgID = callerRow.(User).OrgID
	}
	if err := validatePermissionIDs(ctx, req.PermissionsIDs, callerOrgID); err != nil {
		span.SetStatus(codes.Error, "invalid permission id")
		slog.Warn("update role: "+err.Error(), "caller_id", callerID, "role_id", id)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	role.PermissionsIDs = req.PermissionsIDs
	if req.OrgID != nil {
		if callerOrgID == nil || *req.OrgID != *callerOrgID {
			span.SetStatus(codes.Error, "forbidden")
			slog.Warn("update role: org_id does not match caller's org", "caller_id", callerID, "role_id", id)
			http.Error(w, "org_id must match caller's org", http.StatusForbidden)
			return
		}
		role.OrgID = req.OrgID
	}
	if err := role.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update role: db error", "caller_id", callerID, "role_id", id, "error", err)
		http.Error(w, "failed to update role", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("role.id", id),
		attribute.Int("role.permissions_count", len(req.PermissionsIDs)),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("update role: success", "caller_id", callerID, "role_id", id, "permissions_count", len(req.PermissionsIDs))
	writeAudit(ctx, callerID, "user", "role.update", id, fmt.Sprintf("permissions_count=%d", len(req.PermissionsIDs)))
	row, _ = role.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Role))
}

func handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteRole")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("role.id", id),
	)
	slog.Info("delete role request", "caller_id", callerID, "role_id", id)

	if !requirePermission(w, r, "deleteRole", "gatekeeper/roles/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Role{RoleID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "role not found")
		slog.Warn("delete role: not found", "caller_id", callerID, "role_id", id)
		http.Error(w, "role not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("role.id", id)))

	// Refuse deletion while users or teams still reference this role to prevent
	// access disruption (those users would lose all permissions on next auth check).
	var userCount, teamCount int64
	connect().WithContext(ctx).Model(&User{}).Where("role_id = ? AND active = ?", id, true).Count(&userCount)
	connect().WithContext(ctx).Model(&Team{}).Where("role_id = ? AND active = ?", id, true).Count(&teamCount)
	if userCount > 0 || teamCount > 0 {
		span.SetStatus(codes.Error, "role still in use")
		slog.Warn("delete role: role still referenced", "caller_id", callerID, "role_id", id, "users", userCount, "teams", teamCount)
		http.Error(w, "role is still assigned to users or teams", http.StatusConflict)
		return
	}

	if err := row.(Role).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete role: db error", "caller_id", callerID, "role_id", id, "error", err)
		http.Error(w, "failed to delete role", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("role.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("delete role: success", "caller_id", callerID, "role_id", id)
	writeAudit(ctx, callerID, "user", "role.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}
