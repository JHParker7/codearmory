package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type permissionsRequest struct {
	Name      string   `json:"name"`
	Service   string   `json:"service"`
	Actions   []string `json:"actions"`
	Resources []string `json:"resources"`
}

func handleCreatePermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreatePermissions")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("create permissions request", "caller_id", callerID)

	if !requirePermission(w, r, "createPermissions", "gatekeeper/permissions") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req permissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("create permissions: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Service == "" {
		span.SetStatus(codes.Error, "missing service")
		slog.Warn("create permissions: missing service", "caller_id", callerID)
		http.Error(w, "service is required", http.StatusBadRequest)
		return
	}
	for _, a := range req.Actions {
		if a == "" {
			span.SetStatus(codes.Error, "empty action")
			slog.Warn("create permissions: empty string in actions", "caller_id", callerID)
			http.Error(w, "actions must not contain empty strings", http.StatusBadRequest)
			return
		}
	}
	for _, r := range req.Resources {
		if r == "" {
			span.SetStatus(codes.Error, "empty resource")
			slog.Warn("create permissions: empty string in resources", "caller_id", callerID)
			http.Error(w, "resources must not contain empty strings", http.StatusBadRequest)
			return
		}
	}
	span.SetAttributes(
		attribute.String("permissions.service", req.Service),
		attribute.Int("permissions.actions_count", len(req.Actions)),
		attribute.Int("permissions.resources_count", len(req.Resources)),
	)

	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	p := Permissions{
		PermissionsID: uuid.New().String(),
		Name:          req.Name, Service: req.Service,
		Actions: req.Actions, Resources: req.Resources,
		OwnerID: callerID, OrgID: callerOrgID,
	}
	if err := p.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create permissions: db error", "caller_id", callerID, "service", req.Service, "error", err)
		http.Error(w, "failed to create permissions", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("permissions.id", p.PermissionsID),
		attribute.String("permissions.service", p.Service),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("create permissions: success", "caller_id", callerID, "permissions_id", p.PermissionsID, "service", p.Service, "actions", p.Actions, "resources", p.Resources)
	writeAudit(ctx, callerID, "user", "permission.create", p.PermissionsID, p.Service)
	row, _ := p.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(row.(Permissions))
}

func handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetPermissions")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("permissions.id", id),
	)
	slog.Info("get permissions request", "caller_id", callerID, "permissions_id", id)

	if !requirePermission(w, r, "getPermissions", "gatekeeper/permissions/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Permissions{PermissionsID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "permissions not found")
		slog.Warn("get permissions: not found", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "permissions not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("permissions.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("get permissions: success", "caller_id", callerID, "permissions_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Permissions))
}

func handleUpdatePermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleUpdatePermissions")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("permissions.id", id),
	)
	slog.Info("update permissions request", "caller_id", callerID, "permissions_id", id)

	if !requirePermission(w, r, "updatePermissions", "gatekeeper/permissions/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req permissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("update permissions: invalid request body", "caller_id", callerID, "permissions_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Service == "" {
		span.SetStatus(codes.Error, "missing service")
		slog.Warn("update permissions: missing service", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "service is required", http.StatusBadRequest)
		return
	}
	for _, a := range req.Actions {
		if a == "" {
			span.SetStatus(codes.Error, "empty action")
			slog.Warn("update permissions: empty string in actions", "caller_id", callerID, "permissions_id", id)
			http.Error(w, "actions must not contain empty strings", http.StatusBadRequest)
			return
		}
	}
	for _, r := range req.Resources {
		if r == "" {
			span.SetStatus(codes.Error, "empty resource")
			slog.Warn("update permissions: empty string in resources", "caller_id", callerID, "permissions_id", id)
			http.Error(w, "resources must not contain empty strings", http.StatusBadRequest)
			return
		}
	}
	span.SetAttributes(
		attribute.String("new.service", req.Service),
		attribute.Int("new.actions_count", len(req.Actions)),
		attribute.Int("new.resources_count", len(req.Resources)),
	)

	row, err := (Permissions{PermissionsID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "permissions not found")
		slog.Warn("update permissions: not found", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "permissions not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("permissions.id", id)))

	p := row.(Permissions)
	p.Name = req.Name
	p.Service = req.Service
	p.Actions = req.Actions
	p.Resources = req.Resources
	if err := p.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update permissions: db error", "caller_id", callerID, "permissions_id", id, "error", err)
		http.Error(w, "failed to update permissions", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("permissions.id", id),
		attribute.String("permissions.service", req.Service),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("update permissions: success", "caller_id", callerID, "permissions_id", id, "new_service", req.Service, "new_actions", req.Actions, "new_resources", req.Resources)
	writeAudit(ctx, callerID, "user", "permission.update", id, req.Service)
	row, _ = p.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Permissions))
}

func handleDeletePermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeletePermissions")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("permissions.id", id),
	)
	slog.Info("delete permissions request", "caller_id", callerID, "permissions_id", id)

	if !requirePermission(w, r, "deletePermissions", "gatekeeper/permissions/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Permissions{PermissionsID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "permissions not found")
		slog.Warn("delete permissions: not found", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "permissions not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("permissions.id", id)))

	if err := row.(Permissions).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete permissions: db error", "caller_id", callerID, "permissions_id", id, "error", err)
		http.Error(w, "failed to delete permissions", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("permissions.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("delete permissions: success", "caller_id", callerID, "permissions_id", id)
	writeAudit(ctx, callerID, "user", "permission.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}
