package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Namespace roles: how an ordinary user shares what they own.
//
// A user owns a namespace — their username, and their org's "org/<name>" — and may
// create a role holding permissions over resources in it, then assign that role to
// other users. No platform admin is involved, which is the point: a repo owner should
// not need one to let a colleague clone their repository.
//
// Two rules make that safe, and they are enforced together because either alone is
// insufficient:
//
//  1. CONFINEMENT — every resource must be inside a namespace the caller owns.
//     Without it, a user writes a permission for someone else's namespace.
//  2. ATTENUATION — the caller must already hold each (service, action, resource)
//     they are granting. Without it, a user with permission to create permissions
//     writes themselves "*/*/*" and becomes an admin. This mirrors the workflow
//     scoped-role path, which skips any permission its owner does not hold.
//
// Confinement alone still allows escalation WITHIN your own namespace (granting
// deleteRepo when you only hold readRepo); attenuation alone would let you hand out
// permissions on resources you merely happen to be able to read. Both, always.

type namespaceRoleRequest struct {
	Name        string           `json:"name"`
	Permissions []permissionSpec `json:"permissions"`
}

// callerNamespaces resolves the namespaces a user owns: their own username, and their
// org's "org/<orgName>" when they belong to one.
func callerNamespaces(r *http.Request, userID string) (username, orgNS string) {
	row, err := (User{UserID: userID}).Get(r.Context())
	if err != nil {
		return "", ""
	}
	u := row.(User)
	if u.OrgID != nil {
		if orgRow, err := (Org{OrgID: *u.OrgID}).Get(r.Context()); err == nil {
			if name := orgRow.(Org).OrgName; name != "" {
				orgNS = "org/" + name
			}
		}
	}
	return u.Username, orgNS
}

// inOwnNamespace reports whether resource sits inside a namespace the caller owns.
func inOwnNamespace(resource, username, orgNS string) bool {
	if username != "" && strings.HasPrefix(resource, username+"/") {
		return true
	}
	return orgNS != "" && strings.HasPrefix(resource, orgNS+"/")
}

// inMemberProject reports whether resource sits inside the "project/<slug>/…"
// namespace of a project the caller belongs to. A project is a top-level namespace
// of its own, exactly like "org/<name>/…" — scopeResource already leaves such
// resources un-rescoped — so a member may grant within it, letting a project owner
// or member mint a scoped token confined to that project's resources.
//
// The membership check is what keeps this from widening confinement: a non-member
// gets false and the resource is refused just as any other namespace they do not
// own. isProjectMember admits owner and any tier (viewer/developer/admin) — the same
// "can VIEW" set the rest of the project surface uses; attenuation still bars
// granting any action the caller does not personally hold, so a viewer cannot pass
// on write.
func inMemberProject(ctx context.Context, callerID, resource string) bool {
	rest, ok := strings.CutPrefix(resource, "project/")
	if !ok {
		return false
	}
	// The slug must be a COMPLETE segment followed by a resource inside it, so
	// "project/core" (the bare namespace) and a slug that is merely a prefix of a
	// longer one are both refused — mirroring inOwnNamespace's segment guard.
	slug, tail, ok := strings.Cut(rest, "/")
	if !ok || slug == "" || tail == "" {
		return false
	}
	// getProjectBySlug spans inactive rows (its unique index covers soft-deleted
	// projects), so an explicit Active check is required — a deleted project's slug
	// must not grant confinement.
	p, err := getProjectBySlug(ctx, slug)
	if err != nil || !p.Active {
		return false
	}
	return isProjectMember(ctx, callerID, p)
}

// attenuationError is a refusal with the status the caller should see. Confinement and
// attenuation failures are 403 (you asked for something you may not have), a malformed
// spec is 400, and a failed lookup is 500 — kept distinct because "you cannot grant
// that" and "we could not tell" must not read the same to a caller deciding what to do.
type attenuationError struct {
	status  int
	message string
}

// attenuatedPermissions turns requested (service, action, resource) triples into
// stored Permissions rows, enforcing the two rules above. It is shared by namespace
// roles and scoped tokens deliberately: both hand out a subset of the caller's own
// authority, and the day one of them relaxes a rule by accident is the day the other
// silently inherits it. namePrefix identifies the rows in the permissions table.
//
// Permissions already created when a later one is refused are left behind rather than
// rolled back — they belong to no role, so they grant nothing, and a partial cleanup
// that itself failed would be a worse outcome than an orphan row.
func attenuatedPermissions(ctx context.Context, callerID, username, orgNS, namePrefix string, specs []permissionSpec) ([]string, *attenuationError) {
	var permIDs []string
	for _, p := range specs {
		if p.Service == "" || p.Action == "" || p.Resource == "" {
			return nil, &attenuationError{http.StatusBadRequest, "each permission needs service, action and resource"}
		}
		// CONFINEMENT: the resource must sit in a namespace the caller owns, or in the
		// "project/<slug>/…" namespace of a project they are a member of.
		if !inOwnNamespace(p.Resource, username, orgNS) && !inMemberProject(ctx, callerID, p.Resource) {
			return nil, &attenuationError{http.StatusForbidden, "resource " + p.Resource + " is outside your namespace"}
		}
		// ATTENUATION: you may only pass on what you hold.
		granted, err := checkPermissions(ctx, callerID, p.Service, p.Action, p.Resource)
		if err != nil {
			slog.ErrorContext(ctx, "attenuated permissions: permission check failed", "caller_id", callerID, "error", err)
			return nil, &attenuationError{http.StatusInternalServerError, "internal server error"}
		}
		if !granted {
			return nil, &attenuationError{http.StatusForbidden, "you do not hold " + p.Action + " on " + p.Resource}
		}

		perm := Permissions{
			PermissionsID: uuid.New().String(),
			Name:          namePrefix + ":" + p.Action,
			Service:       p.Service,
			Actions:       []string{p.Action},
			Resources:     []string{p.Resource},
			OwnerID:       callerID,
			Active:        true,
		}
		if err := perm.Add(ctx); err != nil {
			slog.ErrorContext(ctx, "attenuated permissions: create permission", "error", err)
			return nil, &attenuationError{http.StatusInternalServerError, "internal server error"}
		}
		permIDs = append(permIDs, perm.PermissionsID)
	}
	return permIDs, nil
}

// handleCreateNamespaceRole creates a role in the caller's namespace. Permissions that
// fail confinement or attenuation are REJECTED rather than silently dropped: a caller
// who asked to share write access must not be told "created" and later discover only
// read was granted.
func handleCreateNamespaceRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateNamespaceRole")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(attribute.String("user.id", callerID))

	username, orgNS := callerNamespaces(r, callerID)
	if username == "" {
		http.Error(w, "unknown caller", http.StatusUnauthorized)
		return
	}
	// The gate is over the caller's OWN namespace, so the default grant that carries
	// it ("{username}/gatekeeper/roles") cannot be used against anyone else's.
	if !requirePermission(w, r, "createNamespaceRole", username+"/gatekeeper/roles") {
		return
	}

	var req namespaceRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" || len(req.Permissions) == 0 {
		http.Error(w, "name and at least one permission are required", http.StatusBadRequest)
		return
	}

	permIDs, attErr := attenuatedPermissions(ctx, callerID, username, orgNS, "ns:"+username+":"+req.Name, req.Permissions)
	if attErr != nil {
		http.Error(w, attErr.message, attErr.status)
		return
	}

	role := Role{
		RoleID:         uuid.New().String(),
		Name:           req.Name,
		PermissionsIDs: permIDs,
		OwnerID:        callerID,
		Active:         true,
	}
	if err := role.Add(ctx); err != nil {
		slog.ErrorContext(ctx, "namespace role: create role", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "namespace role created", "caller_id", callerID, "role_id", role.RoleID, "permissions", len(permIDs))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(role)
}

// handleAssignRole grants a role to a user. Only the role's owner may assign it, which
// is what keeps this endpoint safe without a permission of its own: a role you do not
// own is not yours to hand out, however many roles you can create.
func handleAssignRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleAssignRole")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	roleID, targetID := r.PathValue("id"), r.PathValue("user_id")
	span.SetAttributes(attribute.String("role.id", roleID), attribute.String("target.user_id", targetID))

	role, ok := ownedRole(w, ctx, callerID, roleID)
	if !ok {
		return
	}
	if _, err := (User{UserID: targetID}).Get(ctx); err != nil {
		http.Error(w, "no such user", http.StatusNotFound)
		return
	}

	m := RoleMembership{RoleID: role.RoleID, UserID: targetID, CreatedAt: time.Now().UTC(), GrantedBy: callerID}
	if err := connect().WithContext(ctx).
		Where("role_id = ? AND user_id = ?", role.RoleID, targetID).
		Assign(map[string]any{"granted_by": callerID}).
		FirstOrCreate(&m).Error; err != nil {
		slog.ErrorContext(ctx, "assign role", "role_id", roleID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "role assigned", "role_id", roleID, "user_id", targetID, "by", callerID)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// handleListRoleMembers lists who holds a role. Owner-only, like assign and revoke:
// the membership list of a role is as sensitive as the role itself.
func handleListRoleMembers(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleListRoleMembers")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	role, ok := ownedRole(w, ctx, callerID, r.PathValue("id"))
	if !ok {
		return
	}
	var members []RoleMembership
	if err := connectRead().WithContext(ctx).Where("role_id = ?", role.RoleID).Order("created_at").Find(&members).Error; err != nil {
		slog.ErrorContext(ctx, "list role members", "role_id", role.RoleID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if members == nil {
		members = []RoleMembership{}
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(members)
}

func handleRevokeRole(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleRevokeRole")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	roleID, targetID := r.PathValue("id"), r.PathValue("user_id")

	role, ok := ownedRole(w, ctx, callerID, roleID)
	if !ok {
		return
	}
	if err := connect().WithContext(ctx).
		Where("role_id = ? AND user_id = ?", role.RoleID, targetID).
		Delete(&RoleMembership{}).Error; err != nil {
		slog.ErrorContext(ctx, "revoke role", "role_id", roleID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "role revoked", "role_id", roleID, "user_id", targetID, "by", callerID)
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// ownedRole loads a role the caller owns, answering 404 for both "missing" and "not
// yours" so role ids cannot be probed.
func ownedRole(w http.ResponseWriter, ctx context.Context, callerID, roleID string) (Role, bool) {
	row, err := (Role{RoleID: roleID}).Get(ctx)
	if err != nil {
		http.Error(w, "role not found", http.StatusNotFound)
		return Role{}, false
	}
	role := row.(Role)
	if role.OwnerID != callerID {
		http.Error(w, "role not found", http.StatusNotFound)
		return Role{}, false
	}
	return role, true
}
