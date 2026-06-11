package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type inviteRequest struct {
	Email string `json:"email"`
}

// createInviteBody decodes the request body, creates and inserts an invite record,
// and writes the 201 response. It is called after the outer handler has verified
// permissions and confirmed the target resource exists.
func createInviteBody(w http.ResponseWriter, r *http.Request, callerID, resourceType, resourceID, logLabel string) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "createInviteBody")
	defer span.End()
	r = r.WithContext(ctx)

	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		slog.Warn("create "+logLabel+" invite: invalid request body", "caller_id", callerID, "resource_id", resourceID, "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Email == "" {
		span.SetStatus(codes.Error, "missing email")
		slog.Warn("create "+logLabel+" invite: missing email", "caller_id", callerID, "resource_id", resourceID)
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		span.SetStatus(codes.Error, "invalid email")
		slog.Warn("create "+logLabel+" invite: invalid email format", "caller_id", callerID, "resource_id", resourceID)
		http.Error(w, "invalid email address", http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("invitee.email", req.Email))

	invite := Invite{
		InviteID:     uuid.New().String(),
		InviterID:    callerID,
		InviteeEmail: req.Email,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Status:       "pending",
		ExpiresAt:    time.Now().Add(7 * 24 * time.Hour).UTC(),
	}
	if err := invite.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create "+logLabel+" invite: db error", "caller_id", callerID, "resource_id", resourceID, "error", err)
		http.Error(w, "failed to create invite", http.StatusInternalServerError)
		return
	}
	span.AddEvent("invite.created", trace.WithAttributes(
		attribute.String("invite.id", invite.InviteID),
		attribute.String("invitee.email", req.Email),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("create "+logLabel+" invite: success", "caller_id", callerID, "resource_id", resourceID, "invite_id", invite.InviteID, "invitee_email", req.Email)
	writeAudit(ctx, callerID, "user", "invite.create", invite.InviteID, resourceType+":"+resourceID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(invite)
}

func handleListInvites(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListInvites")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("caller.id", callerID))
	slog.Info("list invites request", "caller_id", callerID)

	if !requirePermission(w, r, "listInvite", "gatekeeper/invites") {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	limit, offset, ok := parsePagination(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid pagination")
		return
	}

	// Resolve the caller's email to scope the invite list to invites they sent
	// or received. This prevents any user with listInvite from enumerating all
	// invitee emails across the system.
	callerRow, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "caller not found")
		slog.Warn("list invites: failed to fetch caller", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list invites", http.StatusInternalServerError)
		return
	}
	callerEmail := callerRow.(User).Email

	q := r.URL.Query()
	// Honour optional query-param filters but always constrain to the caller's scope.
	inviteID := q.Get("invite_id")
	resourceType := q.Get("resource_type")
	resourceID := q.Get("resource_id")
	status := q.Get("status")

	rows, err := listInvitesForCaller(ctx, callerID, callerEmail, inviteID, resourceType, resourceID, status, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list invites failed")
		slog.Warn("list invites: db error", "caller_id", callerID, "error", err)
		http.Error(w, "failed to list invites", http.StatusInternalServerError)
		return
	}
	invites := make([]Invite, len(rows))
	for i, row := range rows {
		invites[i] = row.(Invite)
	}
	span.AddEvent("db.read")
	span.SetStatus(codes.Ok, "")
	slog.Info("list invites: success", "caller_id", callerID, "count", len(invites))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(invites)
}

func handleCreateOrgInvite(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateOrgInvite")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("org.id", id),
	)
	slog.Info("create org invite request", "caller_id", callerID, "org_id", id)

	if !requirePermission(w, r, "inviteUser", "gatekeeper/orgs/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	if _, err := (Org{OrgID: id}).Get(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "org not found")
		slog.Warn("create org invite: org not found", "caller_id", callerID, "org_id", id)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}

	createInviteBody(w, r, callerID, "org", id, "org")
}

func handleCreateTeamInvite(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateTeamInvite")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("team.id", id),
	)
	slog.Info("create team invite request", "caller_id", callerID, "team_id", id)

	if !requirePermission(w, r, "inviteUser", "gatekeeper/teams/"+id) {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	if _, err := (Team{TeamID: id}).Get(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "team not found")
		slog.Warn("create team invite: team not found", "caller_id", callerID, "team_id", id)
		http.Error(w, "team not found", http.StatusNotFound)
		return
	}

	createInviteBody(w, r, callerID, "team", id, "team")
}

func handleGetInvite(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetInvite")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("invite.id", id),
	)
	slog.Info("get invite request", "caller_id", callerID, "invite_id", id)

	row, err := (Invite{InviteID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invite not found")
		slog.Warn("get invite: not found", "caller_id", callerID, "invite_id", id)
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	invite := row.(Invite)
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("invite.id", id)))

	callerRow, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "caller not found")
		dbLog(err, "get invite: caller not found", "caller_id", callerID, "error", err)
		// Return 404 to avoid revealing whether the invite exists.
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	caller := callerRow.(User)
	if callerID != invite.InviterID && caller.Email != invite.InviteeEmail {
		span.SetStatus(codes.Error, "not participant")
		slog.Warn("get invite: caller is not inviter or invitee", "caller_id", callerID, "invite_id", id)
		// Return 404 instead of 403 to avoid revealing that the invite exists.
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	span.AddEvent("participant.verified", trace.WithAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("invite.id", id),
	))

	span.SetStatus(codes.Ok, "")
	slog.Info("get invite: success", "caller_id", callerID, "invite_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(invite)
}

// handleAcceptInvite accepts a pending invite as the invitee. The three writes
// (assign user to the org/team, mark invite accepted, grant read permission on
// the resource) are committed atomically in a single transaction so partial
// failures cannot leave the user in an inconsistent state.
func handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleAcceptInvite")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("invite.id", id),
	)
	slog.Info("accept invite request", "caller_id", callerID, "invite_id", id)

	row, err := (Invite{InviteID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invite not found")
		slog.Warn("accept invite: not found", "caller_id", callerID, "invite_id", id)
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	invite := row.(Invite)

	callerRow, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "caller not found")
		dbLog(err, "accept invite: caller not found", "caller_id", callerID, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	caller := callerRow.(User)
	if caller.Email != invite.InviteeEmail {
		span.SetStatus(codes.Ok, "")
		slog.Warn("accept invite: caller is not invitee", "caller_id", callerID, "invite_id", id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if invite.Status != "pending" {
		span.SetStatus(codes.Error, "invite not pending")
		slog.Warn("accept invite: invite not pending", "caller_id", callerID, "invite_id", id, "status", invite.Status)
		http.Error(w, "invite is not pending", http.StatusConflict)
		return
	}
	if time.Now().After(invite.ExpiresAt) {
		span.SetStatus(codes.Error, "invite expired")
		slog.Warn("accept invite: invite expired", "caller_id", callerID, "invite_id", id, "expires_at", invite.ExpiresAt)
		http.Error(w, "invite has expired", http.StatusGone)
		return
	}

	// Derive permission strings from the outer (non-locked) caller read; username
	// is immutable so this is safe. The definitive membership check runs inside
	// the transaction under SELECT FOR UPDATE on the user row.
	var memberAction, permResource, permName string
	switch invite.ResourceType {
	case "org":
		if caller.OrgID != nil && *caller.OrgID != invite.ResourceID {
			span.SetStatus(codes.Error, "already in org")
			slog.Warn("accept invite: caller already belongs to a different org", "caller_id", callerID, "invite_id", id)
			http.Error(w, "you already belong to an org; leave it before accepting this invite", http.StatusConflict)
			return
		}
		memberAction = "getOrg"
		permResource = fmt.Sprintf("%s/gatekeeper/orgs/%s", caller.Username, invite.ResourceID)
		permName = fmt.Sprintf("%s-org-member-read", caller.Username)
	case "team":
		if caller.TeamID != nil && *caller.TeamID != invite.ResourceID {
			span.SetStatus(codes.Error, "already in team")
			slog.Warn("accept invite: caller already belongs to a different team", "caller_id", callerID, "invite_id", id)
			http.Error(w, "you already belong to a team; leave it before accepting this invite", http.StatusConflict)
			return
		}
		memberAction = "getTeam"
		permResource = fmt.Sprintf("%s/gatekeeper/teams/%s", caller.Username, invite.ResourceID)
		permName = fmt.Sprintf("%s-team-member-read", caller.Username)
	default:
		span.SetStatus(codes.Error, "unknown resource type")
		slog.Error("accept invite: unknown resource type", "caller_id", callerID, "invite_id", id, "resource_type", invite.ResourceType)
		http.Error(w, "invalid invite", http.StatusInternalServerError)
		return
	}

	err = acceptInviteAtomic(ctx, id, callerID, invite, permName, memberAction, permResource)
	if err != nil {
		switch err.Error() {
		case "invite is not pending":
			span.SetStatus(codes.Error, "invite not pending")
			slog.Warn("accept invite: concurrent accept detected", "caller_id", callerID, "invite_id", id)
			http.Error(w, "invite is not pending", http.StatusConflict)
		case "already in org", "already in team":
			span.SetStatus(codes.Error, err.Error())
			slog.Warn("accept invite: membership conflict inside tx", "caller_id", callerID, "invite_id", id, "detail", err)
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			span.RecordError(err)
			span.SetStatus(codes.Error, "db update failed")
			slog.Error("accept invite: failed to accept invite atomically", "caller_id", callerID, "invite_id", id, "error", err)
			http.Error(w, "failed to accept invite", http.StatusInternalServerError)
		}
		return
	}
	// Evict the user cache after the transaction commits. grantPermissions also
	// evicts inside the transaction (before commit), which can create a brief
	// window where a concurrent read re-populates the cache with stale pre-commit
	// state. This post-commit eviction ensures the cache is correct.
	cacheDel(ctx, "gk:user:"+callerID)

	span.AddEvent("invite.accepted", trace.WithAttributes(
		attribute.String("invite.id", id),
		attribute.String("resource.type", invite.ResourceType),
		attribute.String("resource.id", invite.ResourceID),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("accept invite: success", "caller_id", callerID, "invite_id", id, "resource_type", invite.ResourceType, "resource_id", invite.ResourceID)
	writeAudit(ctx, callerID, "user", "invite.accept", id, invite.ResourceType+":"+invite.ResourceID)
	w.WriteHeader(http.StatusNoContent)
}

func handleDeclineInvite(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeclineInvite")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("invite.id", id),
	)
	slog.Info("decline invite request", "caller_id", callerID, "invite_id", id)

	row, err := (Invite{InviteID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invite not found")
		slog.Warn("decline invite: not found", "caller_id", callerID, "invite_id", id)
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	invite := row.(Invite)

	callerRow, err := (User{UserID: callerID}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "caller not found")
		dbLog(err, "decline invite: caller not found", "caller_id", callerID, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	caller := callerRow.(User)
	if caller.Email != invite.InviteeEmail {
		span.SetStatus(codes.Ok, "")
		slog.Warn("decline invite: caller is not invitee", "caller_id", callerID, "invite_id", id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if invite.Status != "pending" {
		span.SetStatus(codes.Error, "invite not pending")
		slog.Warn("decline invite: invite not pending", "caller_id", callerID, "invite_id", id, "status", invite.Status)
		http.Error(w, "invite is not pending", http.StatusConflict)
		return
	}

	invite.Status = "declined"
	if err := invite.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("decline invite: db error", "caller_id", callerID, "invite_id", id, "error", err)
		http.Error(w, "failed to decline invite", http.StatusInternalServerError)
		return
	}
	span.AddEvent("invite.declined", trace.WithAttributes(attribute.String("invite.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("decline invite: success", "caller_id", callerID, "invite_id", id)
	writeAudit(ctx, callerID, "user", "invite.decline", id, "")
	w.WriteHeader(http.StatusNoContent)
}

func handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteInvite")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("invite.id", id),
	)
	slog.Info("delete invite request", "caller_id", callerID, "invite_id", id)

	row, err := (Invite{InviteID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invite not found")
		slog.Warn("delete invite: not found", "caller_id", callerID, "invite_id", id)
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	invite := row.(Invite)

	if callerID != invite.InviterID {
		span.SetStatus(codes.Ok, "")
		slog.Warn("delete invite: caller is not inviter", "caller_id", callerID, "invite_id", id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := invite.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete invite: db error", "caller_id", callerID, "invite_id", id, "error", err)
		http.Error(w, "failed to cancel invite", http.StatusInternalServerError)
		return
	}
	span.AddEvent("invite.cancelled", trace.WithAttributes(attribute.String("invite.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("delete invite: success", "caller_id", callerID, "invite_id", id)
	writeAudit(ctx, callerID, "user", "invite.cancel", id, "")
	w.WriteHeader(http.StatusNoContent)
}
