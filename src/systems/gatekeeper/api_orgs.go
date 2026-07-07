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
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "list orgs request", "caller_id", callerID)

	if !requirePermission(w, r, "listOrg", "gatekeeper/orgs") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	// Scope results to every org the caller is a member of. With multi-org
	// membership a user can belong to more than one org, so this returns the full
	// membership set (not just the active org). Optional org_id / org_name query
	// params narrow within that set; an org the caller is not a member of yields
	// nothing rather than leaking other orgs.
	q := r.URL.Query()
	orgs, err := listOrgsForUser(ctx, callerID, q.Get("org_id"), q.Get("org_name"), limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list orgs failed")
		slog.WarnContext(ctx, "list orgs: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list orgs", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list orgs: success", "caller_id", callerID, "count", len(orgs))
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
	span.SetAttributes(attribute.String("user.id", callerID))
	slog.InfoContext(ctx, "create org request", "caller_id", callerID)

	if !requirePermission(w, r, "createOrg", "gatekeeper/orgs") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req orgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "create org: invalid request body", "caller_id", callerID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OrgName == "" {
		span.SetStatus(codes.Error, "missing org_name")
		slog.WarnContext(ctx, "create org: missing org_name", "caller_id", callerID)
		http.Error(w, "org_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("org.name", req.OrgName))
	userID, _ := r.Context().Value(userIDKey).(string)
	org := Org{OrgID: uuid.New().String(), OrgName: req.OrgName, OwnerID: userID}
	if err := org.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create org: db error", "caller_id", callerID, "org_name", req.OrgName, "error", err)
		http.Error(w, "failed to create org", http.StatusInternalServerError)
		return
	}

	slog.InfoContext(ctx, "org created, assigning owner to org", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID)
	ownerRow, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to load owner")
		slog.ErrorContext(ctx, "create org: failed to load owner user", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID, "error", err)
		http.Error(w, "failed to put user in org", http.StatusInternalServerError)
		return
	}
	owner := ownerRow.(User)
	owner.OrgID = &org.OrgID
	if err := owner.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to assign owner to org")
		slog.ErrorContext(ctx, "create org: failed to assign owner to org", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID, "error", err)
		http.Error(w, "failed to put user in org", http.StatusInternalServerError)
		return
	}

	// Record the owner's membership so they appear in their own org list and the
	// active org (owner.OrgID) has a backing membership row like any other member.
	if err := addMembership(ctx, userID, org.OrgID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to record owner membership")
		slog.ErrorContext(ctx, "create org: failed to record owner membership", "caller_id", callerID, "org_id", org.OrgID, "owner_id", userID, "error", err)
		http.Error(w, "failed to put user in org", http.StatusInternalServerError)
		return
	}

	span.AddEvent("owner.assigned", trace.WithAttributes(
		attribute.String("org.id", org.OrgID),
		attribute.String("user.id", userID),
	))

	templateVars := map[string]string{
		"org_id":   org.OrgID,
		"user_id":  userID,
		"username": owner.Username,
	}
	orgGrants := defaultGrantsFor("org")
	if len(orgGrants) == 0 {
		slog.ErrorContext(ctx, "create org: no default grants for 'org' — owner will have no permissions; check that the registry is reachable and has default_grants seeded", "org_id", org.OrgID, "user_id", userID)
		http.Error(w, "service configuration error: permissions not available", http.StatusServiceUnavailable)
		return
	}
	for _, grant := range orgGrants {
		permName := fmt.Sprintf("%s-%s %s permissions", owner.Username, org.OrgName, grant.ServiceName)
		resources := applyGrantTemplates(grant.Resources, templateVars)
		for _, resource := range resources {
			if err := applyGrantsForResource(ctx, grant.ServiceName, userID, permName, grant.Actions, resource); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "failed to grant owner permissions")
				slog.ErrorContext(ctx, "create org: failed to grant owner permissions", "caller_id", callerID, "org_id", org.OrgID, "service", grant.ServiceName, "error", err)
				http.Error(w, "failed to give owner permissions", http.StatusInternalServerError)
				return
			}
		}
	}

	span.SetAttributes(attribute.String("org.id", org.OrgID))
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("org.id", org.OrgID),
		attribute.String("org.name", org.OrgName),
	))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create org: success", "caller_id", callerID, "org_id", org.OrgID, "org_name", org.OrgName)
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
		attribute.String("user.id", callerID),
		attribute.String("org.id", id),
	)
	slog.InfoContext(ctx, "get org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "getOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.WarnContext(ctx, "get org: not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("org.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get org: success", "caller_id", callerID, "org_id", id)
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
		attribute.String("user.id", callerID),
		attribute.String("org.id", id),
	)
	slog.InfoContext(ctx, "update org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "updateOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req orgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.WarnContext(ctx, "update org: invalid request body", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OrgName == "" {
		span.SetStatus(codes.Error, "missing org_name")
		slog.WarnContext(ctx, "update org: missing org_name", "caller_id", callerID, "org_id", id)
		http.Error(w, "org_name is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("new.org_name", req.OrgName))

	row, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.WarnContext(ctx, "update org: not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("org.id", id)))

	org := row.(Org)
	org.OrgName = req.OrgName
	if err := org.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update org: db error", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to update org", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.write", trace.WithAttributes(
		attribute.String("org.id", id),
		attribute.String("org.name", req.OrgName),
	))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "update org: success", "caller_id", callerID, "org_id", id, "new_name", req.OrgName)
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
		attribute.String("user.id", callerID),
		attribute.String("org.id", id),
	)
	slog.InfoContext(ctx, "delete org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "deleteOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.WarnContext(ctx, "delete org: not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("org.id", id)))

	if err := row.(Org).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete org: db error", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to delete org", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("org.id", id)))

	// Deactivate every membership in the deleted org and move each affected user's
	// active org to another org they still belong to (or clear it). Cached User
	// rows for those users are invalidated — otherwise checkPermissions keeps
	// reading the stale OrgID (and scoping resources/secrets under the deleted org)
	// for up to entityTTL.
	affected, cleanupErr := reassignActiveOrgAfterOrgDelete(ctx, id)
	if cleanupErr != nil {
		slog.ErrorContext(ctx, "delete org: failed to reassign active org / clear memberships", "caller_id", callerID, "org_id", id, "error", cleanupErr)
	}
	for _, uid := range affected {
		cacheDel(ctx, "gk:user:"+uid)
	}
	slog.InfoContext(ctx, "delete org: cleared org memberships", "caller_id", callerID, "org_id", id, "reassigned_users", len(affected))

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete org: success", "caller_id", callerID, "org_id", id)
	writeAudit(ctx, callerID, "user", "org.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleSwitchOrg changes the caller's active org (User.OrgID) to the org named in
// the path. The caller must be a member of that org — membership, not just the
// getOrg permission (which an admin holds on every org), is the real gate. The
// active org drives every per-request org scoping decision, so switching it
// changes which org's resources, secrets and list results the caller sees.
func handleSwitchOrg(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleSwitchOrg")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID), attribute.String("org.id", id))
	slog.InfoContext(ctx, "switch org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "getOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	member, err := userIsOrgMember(ctx, callerID, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "membership lookup failed")
		slog.ErrorContext(ctx, "switch org: membership lookup failed", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to switch org", http.StatusInternalServerError)
		return
	}
	if !member {
		span.SetStatus(codes.Ok, "not a member")
		slog.WarnContext(ctx, "switch org: caller is not a member", "caller_id", callerID, "org_id", id)
		http.Error(w, "you are not a member of this org", http.StatusForbidden)
		return
	}

	row, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "caller not found")
		slog.ErrorContext(ctx, "switch org: failed to load caller", "caller_id", callerID, "error", err)
		http.Error(w, "failed to switch org", http.StatusInternalServerError)
		return
	}
	u := row.(User)
	if u.OrgID == nil || *u.OrgID != id {
		orgID := id
		u.OrgID = &orgID
		if err := u.Update(ctx); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db update failed")
			slog.ErrorContext(ctx, "switch org: db update failed", "caller_id", callerID, "org_id", id, "error", err)
			http.Error(w, "failed to switch org", http.StatusInternalServerError)
			return
		}
		writeAudit(ctx, callerID, "user", "org.switch", id, "")
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "switch org: success", "caller_id", callerID, "org_id", id)
	row, _ = (User{UserID: callerID}).Get(ctx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toUserResponse(row.(User))) //nolint:errcheck
}

// handleLeaveOrg removes the caller's membership in the org named in the path. The
// org owner cannot leave their own org (they must delete or transfer it). If the
// org being left is the caller's active org, the active org is moved to another
// org they still belong to, or cleared when none remain.
func handleLeaveOrg(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleLeaveOrg")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID), attribute.String("org.id", id))
	slog.InfoContext(ctx, "leave org request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "getOrg", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	orgRow, err := (Org{OrgID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.WarnContext(ctx, "leave org: org not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	if orgRow.(Org).OwnerID == callerID {
		span.SetStatus(codes.Ok, "owner cannot leave")
		slog.WarnContext(ctx, "leave org: owner cannot leave own org", "caller_id", callerID, "org_id", id)
		http.Error(w, "the org owner cannot leave; delete the org instead", http.StatusConflict)
		return
	}

	member, err := userIsOrgMember(ctx, callerID, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "membership lookup failed")
		slog.ErrorContext(ctx, "leave org: membership lookup failed", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to leave org", http.StatusInternalServerError)
		return
	}
	if !member {
		span.SetStatus(codes.Ok, "not a member")
		slog.WarnContext(ctx, "leave org: caller is not a member", "caller_id", callerID, "org_id", id)
		http.Error(w, "you are not a member of this org", http.StatusConflict)
		return
	}

	if err := removeMembership(ctx, callerID, id); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to remove membership")
		slog.ErrorContext(ctx, "leave org: failed to remove membership", "caller_id", callerID, "org_id", id, "error", err)
		http.Error(w, "failed to leave org", http.StatusInternalServerError)
		return
	}

	// If the org just left was the active one, move the active org to another
	// membership (oldest first) or clear it. User.Update invalidates the cache.
	row, err := (User{UserID: callerID}).Get(ctx)
	if err == nil {
		u := row.(User)
		if u.OrgID != nil && *u.OrgID == id {
			remaining, remErr := membershipOrgIDsForUser(ctx, callerID)
			if remErr != nil {
				slog.ErrorContext(ctx, "leave org: failed to load remaining memberships", "caller_id", callerID, "org_id", id, "error", remErr)
			}
			if len(remaining) > 0 {
				next := remaining[0]
				u.OrgID = &next
			} else {
				u.OrgID = nil
			}
			if err := u.Update(ctx); err != nil {
				slog.ErrorContext(ctx, "leave org: failed to reassign active org", "caller_id", callerID, "org_id", id, "error", err)
			}
		} else {
			// Active org unchanged, but the membership set changed — drop any cached
			// User row defensively.
			cacheDel(ctx, "gk:user:"+callerID)
		}
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "leave org: success", "caller_id", callerID, "org_id", id)
	writeAudit(ctx, callerID, "user", "org.leave", id, "")
	w.WriteHeader(http.StatusNoContent)
}
