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

type orgRequest struct {
	OrgName string `json:"org_name"`
}

func handleListOrgs(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListOrgs")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("list orgs request", "caller_id", callerID)

	if !requirePermission(w, r, "listOrg", "gatekeeper/orgs") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	q := r.URL.Query()
	var filter Org
	if v := q.Get("org_id"); v != "" {
		filter.OrgID = v
	}
	if v := q.Get("org_name"); v != "" {
		filter.OrgName = v
	}
	if v := q.Get("owner_id"); v != "" {
		filter.OwnerID = v
	}

	rows, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list orgs failed")
		slog.Warn("list orgs: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list orgs", http.StatusInternalServerError)
		return
	}
	orgs := make([]Org, len(rows))
	for i, row := range rows {
		orgs[i] = row.(Org)
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.Info("list orgs: success", "caller_id", callerID, "count", len(orgs))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orgs)
}

// handleCreateOrg creates an org, assigns the caller as its owner (setting their
// org_id), and grants them getOrg/updateOrg/deleteOrg/inviteUser on the new org.
func handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateOrg")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("create org request", "caller_id", callerID)

	if !requirePermission(w, r, "createOrg", "gatekeeper/orgs") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req orgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("create org: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OrgName == "" {
		span.SetStatus(codes.Error, "missing org_name")
		slog.Warn("create org: missing org_name", "caller_id", callerID)
		http.Error(w, "org_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("org.name", req.OrgName))
	userID, _ := r.Context().Value(userIDKey).(string)
	org := Org{OrgID: uuid.New().String(), OrgName: req.OrgName, OwnerID: userID}
	if err := org.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create org: db error", "caller_id", callerID, "org_name", req.OrgName, "error", err)
		http.Error(w, "failed to create org", http.StatusInternalServerError)
		return
	}

	slog.Info("org created, assigning owner to org", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID)
	ownerRow, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to load owner")
		slog.Error("create org: failed to load owner user", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID, "error", err)
		http.Error(w, "failed to put user in org", http.StatusInternalServerError)
		return
	}
	owner := ownerRow.(User)
	owner.OrgID = &org.OrgID
	if err := owner.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to assign owner to org")
		slog.Error("create org: failed to assign owner to org", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID, "error", err)
		http.Error(w, "failed to put user in org", http.StatusInternalServerError)
		return
	}

	span.AddEvent("owner.assigned", trace.WithAttributes(
		attribute.String("org.id", org.OrgID),
		attribute.String("user.id", userID),
	))

	permName := fmt.Sprintf("%s-%s-owners-permissions", owner.Username, org.OrgName)
	if err := grantPermissions(ctx, connect().WithContext(ctx), userID, permName,
		[]string{"getOrg", "updateOrg", "deleteOrg", "inviteUser"},
		fmt.Sprintf("gatekeeper/orgs/%s", org.OrgID)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to grant owner permissions")
		slog.Error("create org: failed to grant owner permissions", "caller_id", callerID, "org_id", org.OrgID, "error", err)
		http.Error(w, "failed to give owner permissions", http.StatusInternalServerError)
		return
	}

	bpPermName := fmt.Sprintf("%s-%s-blueprints-state", owner.Username, org.OrgName)
	if err := grantServicePermissions(ctx, connect().WithContext(ctx), "blueprints", userID, bpPermName,
		[]string{"getState", "updateState", "deleteState", "lockState", "unlockState"},
		fmt.Sprintf("blueprints/%s/states/*", org.OrgName)); err != nil {
		slog.Warn("create org: failed to grant blueprints state permission", "caller_id", callerID, "org_id", org.OrgID, "error", err)
	} else {
		slog.Info("create org: blueprints state permission granted", "caller_id", callerID, "org_id", org.OrgID)
	}

	span.SetAttributes(attribute.String("org.id", org.OrgID))
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("org.id", org.OrgID),
		attribute.String("org.name", org.OrgName),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("create org: success", "caller_id", callerID, "org_id", org.OrgID, "org_name", org.OrgName)
	row, _ := org.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(row.(Org))
}

func handleGetOrg(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetOrg")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("org.id", id),
	)
	slog.Info("get org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "getOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.Warn("get org: not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("org.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("get org: success", "caller_id", callerID, "org_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Org))
}

func handleUpdateOrg(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleUpdateOrg")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("org.id", id),
	)
	slog.Info("update org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "updateOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req orgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("update org: invalid request body", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OrgName == "" {
		span.SetStatus(codes.Error, "missing org_name")
		slog.Warn("update org: missing org_name", "caller_id", callerID, "org_id", id)
		http.Error(w, "org_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("new.org_name", req.OrgName))

	row, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.Warn("update org: not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("org.id", id)))

	org := row.(Org)
	org.OrgName = req.OrgName
	if err := org.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update org: db error", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to update org", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("org.id", id),
		attribute.String("org.name", req.OrgName),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("update org: success", "caller_id", callerID, "org_id", id, "new_name", req.OrgName)
	row, _ = org.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Org))
}

// handleDeleteOrg soft-deletes the org and clears org_id from all member users.
func handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteOrg")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("org.id", id),
	)
	slog.Info("delete org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "deleteOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.Warn("delete org: not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("org.id", id)))

	if err := row.(Org).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete org: db error", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to delete org", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("org.id", id)))

	if err := connect().WithContext(ctx).Model(&User{}).Where("org_id = ?", id).Update("org_id", nil).Error; err != nil {
		slog.Error("delete org: failed to clear org membership", "caller_id", callerID, "org_id", id, "error", err)
	} else {
		slog.Info("delete org: cleared org membership", "caller_id", callerID, "org_id", id)
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("delete org: success", "caller_id", callerID, "org_id", id)
	w.WriteHeader(http.StatusNoContent)
}
