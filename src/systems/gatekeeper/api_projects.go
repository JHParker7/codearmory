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

// projectServices are the services whose resources a project scopes. A project role
// carries one wildcard resource pattern per service (matchPermission's trailing "/*"
// is a LITERAL prefix, so a single "<ns>/*/projects/<slug>/*" would not match — each
// service needs its own "<ns>/<service>/projects/<slug>/*" entry, all held under one
// permission with Service "*").
var projectServices = []string{"workflows", "tickets", "forge", "codearmory_git_factory"}

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
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"` // optional; defaults to the caller's username
}

type projectMemberRequest struct {
	UserID string `json:"user_id"`
	Tier   string `json:"tier"` // viewer | developer | admin
}

// projectResourcePatterns returns the per-service wildcard resource strings that scope
// a project within a namespace, e.g. "admin/workflows/projects/core/*".
func projectResourcePatterns(namespace, slug string) []string {
	pats := make([]string, len(projectServices))
	for i, svc := range projectServices {
		pats[i] = namespace + "/" + svc + "/projects/" + slug + "/*"
	}
	return pats
}

// provisionTierRole creates one Permission (Service "*", the tier's action wildcards,
// every per-service project resource pattern) and a Role holding it, owned by the
// project owner. Confinement is guaranteed by construction — every pattern begins with
// the owner's namespace — so this establishes roles over a brand-new scope the owner
// just created without needing the attenuation the general namespace-role path uses.
func provisionTierRole(ctx context.Context, ownerID, namespace, slug, tier string) (string, error) {
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Name:          "project:" + namespace + ":" + slug + ":" + tier,
		Service:       "*",
		Actions:       projectTiers[tier],
		Resources:     projectResourcePatterns(namespace, slug),
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
	username, orgNS := callerNamespaces(r, callerID)
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
	// Namespace defaults to the caller's username; an explicit namespace must be one
	// the caller owns (their username or their org).
	ns := req.Namespace
	if ns == "" {
		ns = username
	}
	if ns != username && ns != orgNS {
		http.Error(w, "namespace "+ns+" is not yours", http.StatusForbidden)
		return
	}
	// Gate over the caller's own namespace, mirroring createNamespaceRole.
	if !requirePermission(w, r, "createProject", username+"/gatekeeper/projects") {
		return
	}

	p := Project{
		ProjectID: uuid.New().String(),
		Slug:      req.Slug,
		Namespace: ns,
		Name:      req.Name,
		OwnerID:   callerID,
		CreatedAt: time.Now().UTC(),
		Active:    true,
	}
	var err error
	if p.ViewerRoleID, err = provisionTierRole(ctx, callerID, ns, req.Slug, "viewer"); err != nil {
		internalError(w, ctx, "provision viewer role", err)
		return
	}
	if p.DeveloperRoleID, err = provisionTierRole(ctx, callerID, ns, req.Slug, "developer"); err != nil {
		internalError(w, ctx, "provision developer role", err)
		return
	}
	if p.AdminRoleID, err = provisionTierRole(ctx, callerID, ns, req.Slug, "admin"); err != nil {
		internalError(w, ctx, "provision admin role", err)
		return
	}
	if err = p.Add(ctx); err != nil {
		internalError(w, ctx, "create project", err)
		return
	}
	// The creator is the project's first admin.
	_ = assignMembership(ctx, p.AdminRoleID, callerID, callerID)

	slog.InfoContext(ctx, "project created", "project_id", p.ProjectID, "namespace", ns, "slug", req.Slug, "owner", callerID)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, p)
}

func handleListProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerID, _ := ctx.Value(userIDKey).(string)
	username, orgNS := callerNamespaces(r, callerID)
	if username == "" {
		http.Error(w, "unknown caller", http.StatusUnauthorized)
		return
	}
	namespaces := []string{username}
	if orgNS != "" {
		namespaces = append(namespaces, orgNS)
	}
	projects, err := projectsInNamespaces(ctx, namespaces)
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
