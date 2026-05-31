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

type teamRequest struct {
	TeamName string  `json:"team_name"`
	RoleID   *string `json:"role_id"`
}

type teamUpdateRequest struct {
	TeamName string `json:"team_name"`
}

func handleListTeams(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListTeams")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("list teams request", "caller_id", callerID)

	if !requirePermission(w, r, "listTeam", "gatekeeper/teams") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	// Always scope results to the caller's own org regardless of any ?org_id= param.
	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	q := r.URL.Query()
	var filter Team
	filter.OrgID = callerOrgID
	if v := q.Get("team_id"); v != "" {
		filter.TeamID = v
	}
	if v := q.Get("team_name"); v != "" {
		filter.TeamName = v
	}
	if v := q.Get("owner_id"); v != "" {
		filter.OwnerID = v
	}
	if v := q.Get("role_id"); v != "" {
		s := v
		filter.RoleID = &s
	}

	rows, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list teams failed")
		slog.Warn("list teams: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list teams", http.StatusInternalServerError)
		return
	}
	teams := make([]Team, len(rows))
	for i, row := range rows {
		teams[i] = row.(Team)
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.Info("list teams: success", "caller_id", callerID, "count", len(teams))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(teams)
}

func handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateTeam")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("create team request", "caller_id", callerID)

	if !requirePermission(w, r, "createTeam", "gatekeeper/teams") {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req teamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("create team: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TeamName == "" {
		span.SetStatus(codes.Error, "missing team_name")
		slog.Warn("create team: missing team_name", "caller_id", callerID)
		http.Error(w, "team_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("team.name", req.TeamName))

	// Resolve caller's org so the team and its role are scoped to it.
	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	roleID := req.RoleID

	if roleID != nil {
		// Verify the supplied role exists and is owned by the caller to prevent
		// a user from inheriting permissions from a role they do not control.
		roleRow, err := (Role{RoleID: *roleID}).Get(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "role not found")
			slog.Warn("create team: supplied role_id not found", "caller_id", callerID, "role_id", *roleID)
			http.Error(w, "role not found", http.StatusNotFound)
			return
		}
		existingRole := roleRow.(Role)
		if existingRole.OwnerID != callerID {
			span.SetStatus(codes.Error, "role not owned by caller")
			slog.Warn("create team: caller does not own the supplied role", "caller_id", callerID, "role_id", *roleID, "role_owner_id", existingRole.OwnerID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	if roleID == nil {
		teamRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{}, OrgID: callerOrgID, OwnerID: callerID}
		if err := teamRole.Add(ctx); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db insert failed")
			slog.Error("create team role: db error", "caller_id", callerID, "team_name", req.TeamName, "error", err)
			http.Error(w, "failed to create team role", http.StatusInternalServerError)
			return
		}
		roleID = &teamRole.RoleID
	}

	team := Team{TeamID: uuid.New().String(), TeamName: req.TeamName, OwnerID: callerID, RoleID: roleID, OrgID: callerOrgID}
	if err := team.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create team: db error", "caller_id", callerID, "team_name", req.TeamName, "error", err)
		http.Error(w, "failed to create team", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(
		attribute.String("team.id", team.TeamID),
		attribute.String("team.owner_id", callerID),
	)
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("team.id", team.TeamID),
		attribute.String("team.name", team.TeamName),
		attribute.String("team.owner_id", callerID),
	))

	// Get the full user before updating so Save() doesn't blank out other fields.
	slog.Info("team created, assigning owner to team", "caller_id", callerID, "team_id", team.TeamID, "owner_id", callerID)
	ownerRow, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to load owner")
		slog.Error("create team: failed to load owner user", "caller_id", callerID, "team_id", team.TeamID, "error", err)
		http.Error(w, "failed to put user in team", http.StatusInternalServerError)
		return
	}
	owner := ownerRow.(User)
	owner.TeamID = &team.TeamID
	if err := owner.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to assign owner to team")
		slog.Error("create team: failed to assign owner to team", "caller_id", callerID, "team_id", team.TeamID, "error", err)
		http.Error(w, "failed to put user in team", http.StatusInternalServerError)
		return
	}

	span.AddEvent("owner.assigned", trace.WithAttributes(
		attribute.String("team.id", team.TeamID),
		attribute.String("user.id", callerID),
	))

	templateVars := map[string]string{
		"team_id":  team.TeamID,
		"user_id":  callerID,
		"username": owner.Username,
	}
	db := connect().WithContext(ctx)
	for _, grant := range defaultGrantsFor("team") {
		permName := fmt.Sprintf("%s-%s %s permissions", owner.Username, team.TeamName, grant.ServiceName)
		resources := applyGrantTemplates(grant.Resources, templateVars)
		for _, resource := range resources {
			if err := grantServicePermissions(ctx, db, grant.ServiceName, callerID, permName, grant.Actions, resource); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "failed to grant owner permissions")
				slog.Error("create team: failed to grant owner permissions", "caller_id", callerID, "team_id", team.TeamID, "service", grant.ServiceName, "error", err)
				http.Error(w, "failed to give owner permissions", http.StatusInternalServerError)
				return
			}
		}
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("create team: success", "caller_id", callerID, "team_id", team.TeamID, "team_name", team.TeamName, "owner_id", callerID)
	row, _ := team.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(row.(Team))
}

func handleGetTeam(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetTeam")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("team.id", id),
	)
	slog.Info("get team request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "getTeam", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Team{TeamID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.Warn("get team: not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("team.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("get team: success", "caller_id", callerID, "team_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Team))
}

func handleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleUpdateTeam")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("team.id", id),
	)
	slog.Info("update team request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "updateTeam", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	var req teamUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("update team: invalid request body", "caller_id", callerID, "team_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TeamName == "" {
		span.SetStatus(codes.Error, "missing team_name")
		slog.Warn("update team: missing team_name", "caller_id", callerID, "team_id", id)
		http.Error(w, "team_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("new.team_name", req.TeamName))

	row, err := (Team{TeamID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.Warn("update team: not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("team.id", id)))

	team := row.(Team)
	team.TeamName = req.TeamName
	if err := team.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update team: db error", "caller_id", callerID, "team_id", id, "error", err)
		http.Error(w, "failed to update team", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("team.id", id),
		attribute.String("team.name", req.TeamName),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("update team: success", "caller_id", callerID, "team_id", id, "new_name", req.TeamName)
	row, _ = team.Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row.(Team))
}

func handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteTeam")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("team.id", id),
	)
	slog.Info("delete team request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "deleteTeam", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Team{TeamID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.Warn("delete team: not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("team.id", id)))

	if err := row.(Team).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete team: db error", "caller_id", callerID, "team_id", id, "error", err)
		http.Error(w, "failed to delete team", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("team.id", id)))

	// Soft delete does not cascade; clear team_id on members so checkPermissions
	// doesn't attempt to load the now-inactive team and deny access.
	var memberIDs []string
	if err := connect().WithContext(ctx).Model(&User{}).Where("team_id = ?", id).Pluck("user_id", &memberIDs).Error; err != nil {
		slog.Error("delete team: failed to load member IDs for cache invalidation", "caller_id", callerID, "team_id", id, "error", err)
	}
	if err := connect().WithContext(ctx).Model(&User{}).Where("team_id = ?", id).Update("team_id", nil).Error; err != nil {
		slog.Error("delete team: failed to clear team membership", "caller_id", callerID, "team_id", id, "error", err)
	} else {
		for _, uid := range memberIDs {
			cacheDel(ctx, "gk:user:"+uid)
		}
		slog.Info("delete team: cleared team membership", "caller_id", callerID, "team_id", id)
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("delete team: success", "caller_id", callerID, "team_id", id)
	w.WriteHeader(http.StatusNoContent)
}
