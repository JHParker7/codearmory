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
	if orgID == nil {
		callerRow, err := (User{UserID: callerID}).Get(ctx)
		if err == nil {
			orgID = callerRow.(User).OrgID
		}
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
	role.PermissionsIDs = req.PermissionsIDs
	role.OrgID = req.OrgID
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
