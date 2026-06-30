package main

import (
	"context"
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

// teamNameTaken reports whether an active team with the given name already
// exists in the same scope (the org when set, otherwise the owner for org-less
// teams), excluding the team identified by excludeID. It gives the uq_teams_*
// unique indexes a friendly 409 instead of surfacing a raw constraint error.
// On query error it fails open, leaving the DB index as the backstop.
func teamNameTaken(ctx context.Context, orgID *string, ownerID, name, excludeID string) bool {
	q := connectRead().WithContext(ctx).Model(&Team{}).Where("team_name = ? AND active = true", name)
	if orgID != nil {
		q = q.Where("org_id = ?", *orgID)
	} else {
		q = q.Where("org_id IS NULL AND owner_id = ?", ownerID)
	}
	if excludeID != "" {
		q = q.Where("team_id <> ?", excludeID)
	}
	var count int64
	if err := q.Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

func handleListTeams(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListTeams")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "list teams request", "caller_id", callerID)

	if !requirePermission(w, r, "listTeam", "gatekeeper/teams") {
		span.SetStatus(codes.Ok, "")
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
		slog.WarnContext(ctx, "list teams: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list teams", http.StatusInternalServerError)
		return
	}
	teams := make([]Team, len(rows))
	for i, row := range rows {
		teams[i] = row.(Team)
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list teams: success", "caller_id", callerID, "count", len(teams))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(teams)
}

func handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateTeam")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "create team request", "caller_id", callerID)

	if !requirePermission(w, r, "createTeam", "gatekeeper/teams") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req teamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "create team: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TeamName == "" {
		span.SetStatus(codes.Error, "missing team_name")
		slog.WarnContext(ctx, "create team: missing team_name", "caller_id", callerID)
		http.Error(w, "team_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("team.name", req.TeamName))

	// Resolve caller's org so the team and its role are scoped to it.
	var callerOrgID *string
	if callerRow, err := (User{UserID: callerID}).Get(ctx); err == nil {
		callerOrgID = callerRow.(User).OrgID
	}

	// Team names must be unique within their scope so a team can be referenced
	// by name rather than by its UUID (enforced by the uq_teams_* indexes).
	if teamNameTaken(ctx, callerOrgID, callerID, req.TeamName, "") {
		span.SetStatus(codes.Error, "team name taken")
		slog.WarnContext(ctx, "create team: name already exists", "caller_id", callerID, "team_name", req.TeamName)
		http.Error(w, "team with that name already exists", http.StatusConflict)
		return
	}

	roleID := req.RoleID

	if roleID != nil {
		// Verify the supplied role exists and is owned by the caller to prevent
		// a user from inheriting permissions from a role they do not control.
		roleRow, err := (Role{RoleID: *roleID}).Get(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "role not found")
			slog.WarnContext(ctx, "create team: supplied role_id not found", "caller_id", callerID, "role_id", *roleID)
			http.Error(w, "role not found", http.StatusNotFound)
			return
		}
		existingRole := roleRow.(Role)
		if existingRole.OwnerID != callerID {
			span.SetStatus(codes.Error, "role not owned by caller")
			slog.WarnContext(ctx, "create team: caller does not own the supplied role", "caller_id", callerID, "role_id", *roleID, "role_owner_id", existingRole.OwnerID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	if roleID == nil {
		teamRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{}, OrgID: callerOrgID, OwnerID: callerID}
		if err := teamRole.Add(ctx); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db insert failed")
			slog.ErrorContext(ctx, "create team role: db error", "caller_id", callerID, "team_name", req.TeamName, "error", err)
			http.Error(w, "failed to create team role", http.StatusInternalServerError)
			return
		}
		roleID = &teamRole.RoleID
	}

	team := Team{TeamID: uuid.New().String(), TeamName: req.TeamName, OwnerID: callerID, RoleID: roleID, OrgID: callerOrgID}
	if err := team.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create team: db error", "caller_id", callerID, "team_name", req.TeamName, "error", err)
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
	slog.InfoContext(ctx, "team created, assigning owner to team", "caller_id", callerID, "team_id", team.TeamID, "owner_id", callerID)
	ownerRow, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to load owner")
		slog.ErrorContext(ctx, "create team: failed to load owner user", "caller_id", callerID, "team_id", team.TeamID, "error", err)
		http.Error(w, "failed to put user in team", http.StatusInternalServerError)
		return
	}
	owner := ownerRow.(User)
	owner.TeamID = &team.TeamID
	if err := owner.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to assign owner to team")
		slog.ErrorContext(ctx, "create team: failed to assign owner to team", "caller_id", callerID, "team_id", team.TeamID, "error", err)
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
	teamGrants := defaultGrantsFor("team")
	if len(teamGrants) == 0 {
		slog.ErrorContext(ctx, "create team: no default grants for 'team' — owner will have no permissions; check that the registry is reachable and has default_grants seeded", "team_id", team.TeamID, "caller_id", callerID)
		http.Error(w, "service configuration error: permissions not available", http.StatusServiceUnavailable)
		return
	}
	for _, grant := range teamGrants {
		permName := fmt.Sprintf("%s-%s %s permissions", owner.Username, team.TeamName, grant.ServiceName)
		resources := applyGrantTemplates(grant.Resources, templateVars)
		for _, resource := range resources {
			if err := applyGrantsForResource(ctx, grant.ServiceName, callerID, permName, grant.Actions, resource); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "failed to grant owner permissions")
				slog.ErrorContext(ctx, "create team: failed to grant owner permissions", "caller_id", callerID, "team_id", team.TeamID, "service", grant.ServiceName, "error", err)
				http.Error(w, "failed to give owner permissions", http.StatusInternalServerError)
				return
			}
		}
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create team: success", "caller_id", callerID, "team_id", team.TeamID, "team_name", team.TeamName, "owner_id", callerID)
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
		attribute.String("user.id", callerID),
		attribute.String("team.id", id),
	)
	slog.InfoContext(ctx, "get team request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "getTeam", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Team{TeamID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.WarnContext(ctx, "get team: not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("team.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get team: success", "caller_id", callerID, "team_id", id)
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
		attribute.String("user.id", callerID),
		attribute.String("team.id", id),
	)
	slog.InfoContext(ctx, "update team request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "updateTeam", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req teamUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "update team: invalid request body", "caller_id", callerID, "team_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TeamName == "" {
		span.SetStatus(codes.Error, "missing team_name")
		slog.WarnContext(ctx, "update team: missing team_name", "caller_id", callerID, "team_id", id)
		http.Error(w, "team_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("new.team_name", req.TeamName))

	row, err := (Team{TeamID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.WarnContext(ctx, "update team: not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("team.id", id)))

	team := row.(Team)
	if req.TeamName != team.TeamName && teamNameTaken(ctx, team.OrgID, team.OwnerID, req.TeamName, team.TeamID) {
		span.SetStatus(codes.Error, "team name taken")
		slog.WarnContext(ctx, "update team: name already exists", "caller_id", callerID, "team_id", id, "team_name", req.TeamName)
		http.Error(w, "team with that name already exists", http.StatusConflict)
		return
	}
	team.TeamName = req.TeamName
	if err := team.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update team: db error", "caller_id", callerID, "team_id", id, "error", err)
		http.Error(w, "failed to update team", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("team.id", id),
		attribute.String("team.name", req.TeamName),
	))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "update team: success", "caller_id", callerID, "team_id", id, "new_name", req.TeamName)
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
		attribute.String("user.id", callerID),
		attribute.String("team.id", id),
	)
	slog.InfoContext(ctx, "delete team request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "deleteTeam", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Team{TeamID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.WarnContext(ctx, "delete team: not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("team.id", id)))

	if err := row.(Team).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete team: db error", "caller_id", callerID, "team_id", id, "error", err)
		http.Error(w, "failed to delete team", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("team.id", id)))

	// Soft delete does not cascade; clear team_id on members so checkPermissions
	// doesn't attempt to load the now-inactive team and deny access.
	memberIDs, memberErr := getUserIDsByTeam(ctx, id)
	if memberErr != nil {
		slog.ErrorContext(ctx, "delete team: failed to load member IDs for cache invalidation", "caller_id", callerID, "team_id", id, "error", memberErr)
	}
	if err := clearTeamMembership(ctx, id); err != nil {
		slog.ErrorContext(ctx, "delete team: failed to clear team membership", "caller_id", callerID, "team_id", id, "error", err)
	} else {
		for _, uid := range memberIDs {
			cacheDel(ctx, "gk:user:"+uid)
		}
		slog.InfoContext(ctx, "delete team: cleared team membership", "caller_id", callerID, "team_id", id)
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete team: success", "caller_id", callerID, "team_id", id)
	w.WriteHeader(http.StatusNoContent)
}
