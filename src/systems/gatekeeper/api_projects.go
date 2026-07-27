package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// projectTiers maps a tier name to the action wildcards it grants over the project's
// resources. Ordered viewer ⊂ developer ⊂ admin. matchPermission supports the trailing
// "*" action wildcard, so "read*" covers readRepo/readBoard/etc.
var projectTiers = map[string][]string{
	"viewer":    {"read*", "list*", "get*"},
	"developer": {"read*", "list*", "get*", "create*", "update*", "write*", "run*"},
	"admin":     {"*"},
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type createProjectRequest struct {
	Slug  string  `json:"slug"`
	Name  string  `json:"name"`
	OrgID *string `json:"org_id"` // optional; when set the project is owned/managed by the org
}

type projectMemberRequest struct {
	UserID string `json:"user_id"`
	Tier   string `json:"tier"` // viewer | developer | admin
}

// projectResource is the single wildcard that scopes a whole project: "project/<slug>/*".
// A project is its OWN top-level namespace (scopeResource treats "project/" like "org/"),
// so one pattern covers every service's resources — "project/core/workflows/pipelines/42",
// "project/core/codearmory_git_factory/repos/x", etc. — because matchPermission's trailing
// "/*" is a literal prefix and there is no middle wildcard to expand.
func projectResource(slug string) string { return "project/" + slug + "/*" }

// provisionTierRole creates one Permission (Service "*", the tier's action wildcards, the
// single "project/<slug>/*" pattern) and a Role holding it, owned by the project's first
// admin. The project namespace is brand-new and belongs to no user, so this establishes
// the roles directly without the confinement/attenuation the user-namespace path needs.
func provisionTierRole(ctx context.Context, ownerID, slug, tier string) (string, error) {
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Name:          "project:" + slug + ":" + tier,
		Service:       "*",
		Actions:       projectTiers[tier],
		Resources:     []string{projectResource(slug)},
		OwnerID:       ownerID,
		Active:        true,
	}
	if err := perm.Add(ctx); err != nil {
		return "", err
	}
	role := Role{
		RoleID:         uuid.New().String(),
		Name:           "project/" + slug + "/" + tier,
		PermissionsIDs: []string{perm.PermissionsID},
		OwnerID:        ownerID,
		Active:         true,
	}
	if err := role.Add(ctx); err != nil {
		return "", err
	}
	return role.RoleID, nil
}

// handleCreateProject creates a project in the caller's namespace and provisions its
// viewer/developer/admin roles, then assigns the caller the admin role.
func handleCreateProject(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleCreateProject")
	defer span.End()
	r = r.WithContext(ctx)

	callerID, _ := ctx.Value(userIDKey).(string)
	username, _ := callerNamespaces(r, callerID)
	if username == "" {
		http.Error(w, "unknown caller", http.StatusUnauthorized)
		return
	}

	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Slug = strings.TrimSpace(req.Slug)
	if !slugRe.MatchString(req.Slug) {
		http.Error(w, "slug must be lowercase alphanumeric/dashes, 1–63 chars", http.StatusBadRequest)
		return
	}
	// A project is a top-level namespace of its own, not bound to a user; the slug is
	// global. An explicit org binds management to that org (its members can administer
	// it); otherwise the creator alone administers it. The gate is still over the
	// caller's own gatekeeper/projects, so creating a project is a self-service act.
	if !requirePermission(w, r, "createProject", username+"/gatekeeper/projects") {
		return
	}
	orgNS := "" // "org/<name>" when org-owned, else empty (unbound)
	if req.OrgID != nil && *req.OrgID != "" {
		if orgRow, err := (Org{OrgID: *req.OrgID}).Get(ctx); err == nil {
			orgNS = "org/" + orgRow.(Org).OrgName
		}
	}
	// Slug is global, so a collision is a real conflict, not an overwrite.
	if _, err := getProjectBySlug(ctx, req.Slug); err == nil {
		http.Error(w, "a project with slug "+req.Slug+" already exists", http.StatusConflict)
		return
	}

	p := Project{
		ProjectID: uuid.New().String(),
		Slug:      req.Slug,
		Namespace: orgNS,
		Name:      req.Name,
		OwnerID:   callerID,
		CreatedAt: time.Now().UTC(),
		Active:    true,
	}
	var err error
	if p.ViewerRoleID, err = provisionTierRole(ctx, callerID, req.Slug, "viewer"); err != nil {
		internalError(w, ctx, "provision viewer role", err)
		return
	}
	if p.DeveloperRoleID, err = provisionTierRole(ctx, callerID, req.Slug, "developer"); err != nil {
		internalError(w, ctx, "provision developer role", err)
		return
	}
	if p.AdminRoleID, err = provisionTierRole(ctx, callerID, req.Slug, "admin"); err != nil {
		internalError(w, ctx, "provision admin role", err)
		return
	}
	if err = p.Add(ctx); err != nil {
		internalError(w, ctx, "create project", err)
		return
	}
	// The creator is the project's first admin.
	_ = assignMembership(ctx, p.AdminRoleID, callerID, callerID)

	slog.InfoContext(ctx, "project created", "project_id", p.ProjectID, "slug", req.Slug, "org", orgNS, "owner", callerID)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, p)
}

// handleListProjects lists the projects the caller administers (owns). Shared-with-me
// projects come from /projects/accessible; this is the "mine to manage" list.
func handleListProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	projects, err := projectsOwnedBy(ctx, callerID)
	if err != nil {
		internalError(w, ctx, "list projects", err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

// ownedProject loads a project the caller owns, 404 for both "missing" and "not yours"
// so ids cannot be probed (mirrors ownedRole).
func ownedProject(w http.ResponseWriter, ctx context.Context, callerID, projectID string) (Project, bool) {
	row, err := (Project{ProjectID: projectID}).Get(ctx)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return Project{}, false
	}
	p := row.(Project)
	if p.OwnerID != callerID {
		http.Error(w, "project not found", http.StatusNotFound)
		return Project{}, false
	}
	return p, true
}

// handleListAccessibleProjects returns every project the caller can reach (owned +
// member), each tagged with the caller's tier. Services call this to widen list views;
// the portal uses it to show "projects shared with me".
func handleListAccessibleProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	projects, err := accessibleProjects(ctx, callerID)
	if err != nil {
		internalError(w, ctx, "list accessible projects", err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

// handleGetProject is viewable by owner OR any member — a member must be able to see
// the project they were added to (mutations below stay owner-only).
func handleGetProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	row, err := (Project{ProjectID: r.PathValue("id")}).Get(ctx)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	p := row.(Project)
	if !isProjectMember(ctx, callerID, p) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	p, ok := ownedProject(w, ctx, callerID, r.PathValue("id"))
	if !ok {
		return
	}
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		p.Name = req.Name
	}
	if err := p.Update(ctx); err != nil {
		internalError(w, ctx, "update project", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	p, ok := ownedProject(w, ctx, callerID, r.PathValue("id"))
	if !ok {
		return
	}
	// Soft-delete the project and its tier roles; membership rows are left to be
	// garbage: a role with active=false grants nothing, so they are inert.
	for _, rid := range []string{p.ViewerRoleID, p.DeveloperRoleID, p.AdminRoleID} {
		if rid != "" {
			_ = (Role{RoleID: rid}).Remove(ctx)
		}
	}
	if err := p.Remove(ctx); err != nil {
		internalError(w, ctx, "delete project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tierRoleID maps a tier name to the project's role id for that tier.
func (p Project) tierRoleID(tier string) string {
	switch tier {
	case "viewer":
		return p.ViewerRoleID
	case "developer":
		return p.DeveloperRoleID
	case "admin":
		return p.AdminRoleID
	}
	return ""
}

// handleAddProjectMember is sugar over role membership: it resolves tier → role id and
// assigns it, so callers add "Bob as a developer on project core" without role uuids.
// Only the project owner may manage membership (ownedProject enforces it).
func handleAddProjectMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	p, ok := ownedProject(w, ctx, callerID, r.PathValue("id"))
	if !ok {
		return
	}
	var req projectMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	roleID := p.tierRoleID(req.Tier)
	if roleID == "" {
		http.Error(w, "tier must be viewer, developer or admin", http.StatusBadRequest)
		return
	}
	if _, err := (User{UserID: req.UserID}).Get(ctx); err != nil {
		http.Error(w, "no such user", http.StatusNotFound)
		return
	}
	// A member holds exactly one tier: clear any other tier's role first so a
	// downgrade actually reduces access instead of layering.
	for _, t := range []string{"viewer", "developer", "admin"} {
		if t != req.Tier {
			if rid := p.tierRoleID(t); rid != "" {
				_ = revokeMembership(ctx, rid, req.UserID)
			}
		}
	}
	if err := assignMembership(ctx, roleID, req.UserID, callerID); err != nil {
		internalError(w, ctx, "assign project member", err)
		return
	}
	slog.InfoContext(ctx, "project member set", "project_id", p.ProjectID, "user_id", req.UserID, "tier", req.Tier, "by", callerID)
	writeJSON(w, http.StatusOK, map[string]string{"project_id": p.ProjectID, "user_id": req.UserID, "tier": req.Tier})
}

func handleRemoveProjectMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	p, ok := ownedProject(w, ctx, callerID, r.PathValue("id"))
	if !ok {
		return
	}
	targetID := r.PathValue("user_id")
	for _, rid := range []string{p.ViewerRoleID, p.DeveloperRoleID, p.AdminRoleID} {
		if rid != "" {
			_ = revokeMembership(ctx, rid, targetID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// assignMembership upserts a RoleMembership (idempotent on redelivery), the same
// operation handleAssignRole performs, factored out for reuse by the project sugar.
func assignMembership(ctx context.Context, roleID, userID, grantedBy string) error {
	m := RoleMembership{RoleID: roleID, UserID: userID, CreatedAt: time.Now().UTC(), GrantedBy: grantedBy}
	return connect().WithContext(ctx).
		Where("role_id = ? AND user_id = ?", roleID, userID).
		Assign(map[string]any{"granted_by": grantedBy}).
		FirstOrCreate(&m).Error
}

func revokeMembership(ctx context.Context, roleID, userID string) error {
	return connect().WithContext(ctx).
		Where("role_id = ? AND user_id = ?", roleID, userID).
		Delete(&RoleMembership{}).Error
}

// internalError logs and returns a 500 without leaking detail — a small helper the
// project handlers share.
func internalError(w http.ResponseWriter, ctx context.Context, msg string, err error) {
	slog.ErrorContext(ctx, msg, "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
