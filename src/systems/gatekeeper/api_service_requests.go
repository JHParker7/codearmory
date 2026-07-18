package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// handleListAuditLogs returns paginated audit log entries, newest first.
// Supports ?actor_id=, ?action=, ?resource_id= query filters.
func handleListAuditLogs(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListAuditLogs")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	if !requirePermission(w, r, "listAuditLog", "gatekeeper/audit-logs") {
		span.SetStatus(codes.Ok, "")
		return
	}

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	filter := AuditLog{}
	q := r.URL.Query()
	if v := q.Get("actor_id"); v != "" {
		filter.ActorID = v
	}
	if v := q.Get("action"); v != "" {
		filter.Action = v
	}
	if v := q.Get("resource_id"); v != "" {
		filter.ResourceID = v
	}

	rows, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list failed")
		slog.ErrorContext(ctx, "list audit logs: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list audit logs", http.StatusInternalServerError)
		return
	}

	result := make([]AuditLog, len(rows))
	for i, row := range rows {
		result[i] = row.(AuditLog)
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleListPermissionChecks lists the permission-evaluation audit trail (one row
// per access decision, granted or denied) so admins can review access patterns and
// hunt suspicious usage. Filterable by user_id/service/action/org_id (exact),
// resource (substring), and granted (true/false). Admin-gated by listPermissionCheck.
func handleListPermissionChecks(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListPermissionChecks")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	if !requirePermission(w, r, "listPermissionCheck", "gatekeeper/permission-checks") {
		span.SetStatus(codes.Ok, "")
		return
	}

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	q := r.URL.Query()
	filter := PermissionsCheck{}
	if v := q.Get("user_id"); v != "" {
		filter.UserID = v
	}
	if v := q.Get("service"); v != "" {
		filter.Service = v
	}
	if v := q.Get("action"); v != "" {
		filter.Action = v
	}
	if v := q.Get("org_id"); v != "" {
		filter.OrgID = &v
	}
	var granted *bool
	if v := q.Get("granted"); v != "" {
		b := v == "true"
		granted = &b
	}

	rows, err := filter.ListFiltered(ctx, granted, q.Get("resource"), limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list failed")
		slog.ErrorContext(ctx, "list permission checks: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list permission checks", http.StatusInternalServerError)
		return
	}

	result := make([]PermissionsCheck, len(rows))
	for i, row := range rows {
		result[i] = row.(PermissionsCheck)
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

type servicePermissionRequestBody struct {
	Name      string   `json:"name"`
	Service   string   `json:"service"`
	Actions   []string `json:"actions"`
	Resources []string `json:"resources"`
}

// handleCreateServicePermissionRequest accepts a request from a service to add a
// permission to its service role. Authentication is via X-Service-Key header.
func handleCreateServicePermissionRequest(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateServicePermissionRequest")
	defer span.End()
	r = r.WithContext(ctx)

	svc, ok := requireServiceAuth(w, r)
	if !ok {
		span.SetStatus(codes.Error, "service auth failed")
		return
	}
	span.SetAttributes(attribute.String("service.name", svc.ServiceName))
	slog.InfoContext(ctx, "create service permission request", "service_name", svc.ServiceName)

	var body servicePermissionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		span.SetStatus(codes.Error, "invalid body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.Service == "" || len(body.Actions) == 0 || len(body.Resources) == 0 {
		span.SetStatus(codes.Error, "missing fields")
		http.Error(w, "name, service, actions, and resources are required", http.StatusBadRequest)
		return
	}
	if slices.Contains(body.Actions, "") {
		http.Error(w, "actions must not contain empty strings", http.StatusBadRequest)
		return
	}
	if slices.Contains(body.Resources, "") {
		http.Error(w, "resources must not contain empty strings", http.StatusBadRequest)
		return
	}

	// Idempotency: reject if a pending or approved request with the same name already exists.
	var existing ServicePermissionRequest
	err := connectRead().WithContext(ctx).
		Where("service_name = ? AND name = ? AND status IN ? AND active = ?",
			svc.ServiceName, body.Name, []string{"pending", "approved"}, true).
		First(&existing).Error
	if err == nil {
		span.SetStatus(codes.Error, "duplicate request")
		slog.InfoContext(ctx, "create service permission request: already exists", "service_name", svc.ServiceName, "name", body.Name, "status", existing.Status)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(existing)
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "create service permission request: db check failed", "error", err)
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	req := ServicePermissionRequest{
		RequestID:   uuid.New().String(),
		ServiceName: svc.ServiceName,
		Name:        body.Name,
		Service:     svc.ServiceName,
		Actions:     body.Actions,
		Resources:   body.Resources,
		Status:      "pending",
	}
	if err := req.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create service permission request: insert failed", "service_name", svc.ServiceName, "error", err)
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create service permission request: success", "service_name", svc.ServiceName, "request_id", req.RequestID)
	writeAudit(ctx, svc.ServiceName, "service", "service_permission_request.create", req.RequestID, req.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(req)
}

// handleListServicePermissionRequests lists all service permission requests.
// Supports ?service_name=, ?status= query filters.
func handleListServicePermissionRequests(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListServicePermissionRequests")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "list service permission requests", "caller_id", callerID)

	if !requirePermission(w, r, "listServicePermissionRequest", "gatekeeper/service-permission-requests") {
		span.SetStatus(codes.Ok, "")
		return
	}

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	filter := ServicePermissionRequest{}
	q := r.URL.Query()
	if v := q.Get("service_name"); v != "" {
		filter.ServiceName = v
	}
	if v := q.Get("status"); v != "" {
		filter.Status = v
	}

	rows, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list failed")
		slog.ErrorContext(ctx, "list service permission requests: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list requests", http.StatusInternalServerError)
		return
	}

	result := make([]ServicePermissionRequest, len(rows))
	for i, row := range rows {
		result[i] = row.(ServicePermissionRequest)
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list service permission requests: success", "caller_id", callerID, "count", len(result))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleGetServicePermissionRequest retrieves a single service permission request by ID.
func handleGetServicePermissionRequest(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetServicePermissionRequest")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID), attribute.String("request.id", id))
	slog.InfoContext(ctx, "get service permission request", "caller_id", callerID, "request_id", id)

	if !requirePermission(w, r, "getServicePermissionRequest", "gatekeeper/service-permission-requests/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}

	row, err := (ServicePermissionRequest{RequestID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "not found")
		slog.WarnContext(ctx, "get service permission request: not found", "caller_id", callerID, "request_id", id)
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(ServicePermissionRequest))
}

// handleApproveServicePermissionRequest approves a pending request. In a single transaction it:
// creates the Permissions record, ensures the service has a role (creating one if absent),
// appends the permission to the role, and marks the request approved.
func handleApproveServicePermissionRequest(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleApproveServicePermissionRequest")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID), attribute.String("request.id", id))
	slog.InfoContext(ctx, "approve service permission request", "caller_id", callerID, "request_id", id)

	if !requirePermission(w, r, "approveServicePermissionRequest", "gatekeeper/service-permission-requests/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}

	// Quick existence check before acquiring a write lock.
	if _, err := (ServicePermissionRequest{RequestID: id}).Get(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "not found")
		slog.WarnContext(ctx, "approve service permission request: not found", "caller_id", callerID, "request_id", id)
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	spr, err := approveServicePermissionRequestAtomic(ctx, id, callerID)
	if err != nil {
		if err.Error() == "request is not pending" {
			span.SetStatus(codes.Error, "not pending")
			slog.WarnContext(ctx, "approve service permission request: not pending", "caller_id", callerID, "request_id", id)
			http.Error(w, "request is not pending", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "transaction failed")
		slog.ErrorContext(ctx, "approve service permission request: transaction failed", "caller_id", callerID, "request_id", id, "error", err)
		http.Error(w, "failed to approve request", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "approve service permission request: success", "caller_id", callerID, "request_id", id, "service_name", spr.ServiceName)
	writeAudit(ctx, callerID, "user", "service_permission_request.approve", id, spr.ServiceName+":"+spr.Name)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeclineServicePermissionRequest declines a pending request.
func handleDeclineServicePermissionRequest(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeclineServicePermissionRequest")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID), attribute.String("request.id", id))
	slog.InfoContext(ctx, "decline service permission request", "caller_id", callerID, "request_id", id)

	if !requirePermission(w, r, "declineServicePermissionRequest", "gatekeeper/service-permission-requests/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}

	row, err := (ServicePermissionRequest{RequestID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "not found")
		slog.WarnContext(ctx, "decline service permission request: not found", "caller_id", callerID, "request_id", id)
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}
	spr := row.(ServicePermissionRequest)

	if spr.Status != "pending" {
		span.SetStatus(codes.Error, "not pending")
		slog.WarnContext(ctx, "decline service permission request: not pending", "caller_id", callerID, "request_id", id, "status", spr.Status)
		http.Error(w, "request is not pending", http.StatusConflict)
		return
	}

	now := time.Now()
	spr.Status = "declined"
	spr.ResolvedBy = &callerID
	spr.ResolvedAt = &now
	if err := spr.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "decline service permission request: db error", "caller_id", callerID, "request_id", id, "error", err)
		http.Error(w, "failed to decline request", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "decline service permission request: success", "caller_id", callerID, "request_id", id, "service_name", spr.ServiceName)
	writeAudit(ctx, callerID, "user", "service_permission_request.decline", id, spr.ServiceName+":"+spr.Name)
	w.WriteHeader(http.StatusNoContent)
}
