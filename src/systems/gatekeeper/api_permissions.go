package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// permittedServices is a startup-seeded set of known service names, populated
// from PERMITTED_SERVICES (comma-separated). It is a fast path only — any
// service not listed here is checked live against the ServiceAccount table, so
// services deployed after Gatekeeper starts are automatically permitted once
// they register via key rotation.
var permittedServices map[string]bool

func initPermittedServices() {
	raw := os.Getenv("PERMITTED_SERVICES")
	if raw == "" {
		raw = "gatekeeper,blueprints,forge,workflows,tickets,events"
	}
	permittedServices = make(map[string]bool)
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			permittedServices[s] = true
		}
	}
}

// isServicePermitted returns true if name is in the startup-seeded set or has
// an active ServiceAccount record (registered via SDK key rotation).
func isServicePermitted(ctx context.Context, name string) bool {
	if permittedServices[name] {
		return true
	}
	_, err := (ServiceAccount{ServiceName: name}).Get(ctx)
	return err == nil
}

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
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "create permissions request", "caller_id", callerID)

	if !requirePermission(w, r, "createPermissions", "gatekeeper/permissions") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req permissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "create permissions: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Service == "" {
		span.SetStatus(codes.Error, "missing service")
		slog.WarnContext(ctx, "create permissions: missing service", "caller_id", callerID)
		http.Error(w, "service is required", http.StatusBadRequest)
		return
	}
	if !isServicePermitted(ctx, req.Service) {
		span.SetStatus(codes.Error, "unknown service")
		slog.WarnContext(ctx, "create permissions: service not permitted", "caller_id", callerID, "service", req.Service)
		http.Error(w, "service not permitted", http.StatusBadRequest)
		return
	}
	for _, a := range req.Actions {
		if a == "" {
			span.SetStatus(codes.Error, "empty action")
			slog.WarnContext(ctx, "create permissions: empty string in actions", "caller_id", callerID)
			http.Error(w, "actions must not contain empty strings", http.StatusBadRequest)
			return
		}
	}
	for _, r := range req.Resources {
		if r == "" {
			span.SetStatus(codes.Error, "empty resource")
			slog.WarnContext(ctx, "create permissions: empty string in resources", "caller_id", callerID)
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
		slog.ErrorContext(ctx, "create permissions: db error", "caller_id", callerID, "service", req.Service, "error", err)
		http.Error(w, "failed to create permissions", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("permissions.id", p.PermissionsID),
		attribute.String("permissions.service", p.Service),
	))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create permissions: success", "caller_id", callerID, "permissions_id", p.PermissionsID, "service", p.Service, "actions", p.Actions, "resources", p.Resources)
	writeAudit(ctx, callerID, "user", "permission.create", p.PermissionsID, p.Service)
	row, _ := p.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(row.(Permissions))
}

func handleListPermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListPermissions")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "list permissions request", "caller_id", callerID)

	if !requirePermission(w, r, "listPermission", "gatekeeper/permissions") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	// Scope to the caller's org (matching list roles/users).
	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	q := r.URL.Query()
	var filter Permissions
	filter.OrgID = callerOrgID
	if v := q.Get("permissions_id"); v != "" {
		filter.PermissionsID = v
	}
	if v := q.Get("name"); v != "" {
		filter.Name = v
	}
	if v := q.Get("service"); v != "" {
		filter.Service = v
	}
	if v := q.Get("owner_id"); v != "" {
		filter.OwnerID = v
	}

	rows, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list permissions failed")
		slog.WarnContext(ctx, "list permissions: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list permissions", http.StatusInternalServerError)
		return
	}
	perms := make([]Permissions, len(rows))
	for i, row := range rows {
		perms[i] = row.(Permissions)
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list permissions: success", "caller_id", callerID, "count", len(perms))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(perms) //nolint:errcheck
}

func handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetPermissions")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("user.id", callerID),
		attribute.String("permissions.id", id),
	)
	slog.InfoContext(ctx, "get permissions request", "caller_id", callerID, "permissions_id", id)

	if !requirePermission(w, r, "getPermissions", "gatekeeper/permissions/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Permissions{PermissionsID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "permissions not found")
		slog.WarnContext(ctx, "get permissions: not found", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "permissions not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("permissions.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get permissions: success", "caller_id", callerID, "permissions_id", id)
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
		attribute.String("user.id", callerID),
		attribute.String("permissions.id", id),
	)
	slog.InfoContext(ctx, "update permissions request", "caller_id", callerID, "permissions_id", id)

	if !requirePermission(w, r, "updatePermissions", "gatekeeper/permissions/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req permissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "update permissions: invalid request body", "caller_id", callerID, "permissions_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Service == "" {
		span.SetStatus(codes.Error, "missing service")
		slog.WarnContext(ctx, "update permissions: missing service", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "service is required", http.StatusBadRequest)
		return
	}
	if !isServicePermitted(ctx, req.Service) {
		span.SetStatus(codes.Error, "unknown service")
		slog.WarnContext(ctx, "update permissions: service not permitted", "caller_id", callerID, "permissions_id", id, "service", req.Service)
		http.Error(w, "service not permitted", http.StatusBadRequest)
		return
	}
	for _, a := range req.Actions {
		if a == "" {
			span.SetStatus(codes.Error, "empty action")
			slog.WarnContext(ctx, "update permissions: empty string in actions", "caller_id", callerID, "permissions_id", id)
			http.Error(w, "actions must not contain empty strings", http.StatusBadRequest)
			return
		}
	}
	for _, r := range req.Resources {
		if r == "" {
			span.SetStatus(codes.Error, "empty resource")
			slog.WarnContext(ctx, "update permissions: empty string in resources", "caller_id", callerID, "permissions_id", id)
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
		slog.WarnContext(ctx, "update permissions: not found", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "permissions not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("permissions.id", id)))

	p := row.(Permissions)

	// Permissions owned by the system belong to a user's managed default role and
	// are rebuilt on login; editing them through the API would be silently reverted.
	if p.OwnerID == systemRoleOwner {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "update permissions: refusing to modify system-managed permission", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "system-managed permission cannot be modified", http.StatusForbidden)
		return
	}

	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}
	if p.OrgID != nil && (callerOrgID == nil || *p.OrgID != *callerOrgID) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "update permissions: cross-org attempt", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	p.Name = req.Name
	p.Service = req.Service
	p.Actions = req.Actions
	p.Resources = req.Resources
	if err := p.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update permissions: db error", "caller_id", callerID, "permissions_id", id, "error", err)
		http.Error(w, "failed to update permissions", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("permissions.id", id),
		attribute.String("permissions.service", req.Service),
	))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "update permissions: success", "caller_id", callerID, "permissions_id", id, "new_service", req.Service, "new_actions", req.Actions, "new_resources", req.Resources)
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
		attribute.String("user.id", callerID),
		attribute.String("permissions.id", id),
	)
	slog.InfoContext(ctx, "delete permissions request", "caller_id", callerID, "permissions_id", id)

	if !requirePermission(w, r, "deletePermissions", "gatekeeper/permissions/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Permissions{PermissionsID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "permissions not found")
		slog.WarnContext(ctx, "delete permissions: not found", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "permissions not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("permissions.id", id)))

	p := row.(Permissions)
	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}
	if p.OrgID != nil && (callerOrgID == nil || *p.OrgID != *callerOrgID) {
		span.SetStatus(codes.Ok, "")
		slog.WarnContext(ctx, "delete permissions: cross-org attempt", "caller_id", callerID, "permissions_id", id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := p.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete permissions: db error", "caller_id", callerID, "permissions_id", id, "error", err)
		http.Error(w, "failed to delete permissions", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("permissions.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete permissions: success", "caller_id", callerID, "permissions_id", id)
	writeAudit(ctx, callerID, "user", "permission.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}
