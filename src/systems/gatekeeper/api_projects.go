package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
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

// NOTE: project scoping is EXACT-PROJECT — a child project's members get NO access to an
// ancestor's namespace, and a parent's members get none over a child's (per the product
// decision that a parent's repos/resources must not be reachable from a child, nor the
// inverse). The project TREE still exists (Project.ParentID) and still governs which
// project a pipeline may TARGET (workflows pipelineMayTargetProject walks the ancestor
// chain) — that "a bootstrap/parent pipeline may run for a descendant" path is the one
// deliberate cross-project relationship kept, and the anchor for a future explicit
// bootstrap/template concept. What was removed is the automatic cross-namespace *grant*
// (the former projectUseTiers "use-only" inheritance).

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type createProjectRequest struct {
	Slug   string  `json:"slug"`
	Name   string  `json:"name"`
	OrgID  *string `json:"org_id"` // optional; when set the project is owned/managed by the org
	Parent *string `json:"parent"` // optional parent project SLUG; makes this a child in the project tree
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
		// The permission (a Service "*" wildcard over THIS project's namespace only) is
		// reachable only through the role that was meant to carry it; leaving it behind
		// accumulates an unattached grant per attempt.
		if e := (Permissions{PermissionsID: perm.PermissionsID}).Remove(ctx); e != nil {
			slog.ErrorContext(ctx, "provision tier role: orphaned permission left behind", "permission_id", perm.PermissionsID, "error", e)
		}
		return "", err
	}
	return role.RoleID, nil
}

// discardTierRoles deactivates the tier roles and their permissions provisioned for a
// project that is being abandoned mid-creation. A soft delete is enough to make the
// grants inert (every lookup filters active=true) and matches how the rest of gatekeeper
// retires RBAC objects.
func discardTierRoles(ctx context.Context, roleIDs ...string) {
	for _, rid := range roleIDs {
		if rid == "" {
			continue
		}
		row, err := (Role{RoleID: rid}).Get(ctx)
		if err == nil {
			for _, pid := range row.(Role).PermissionsIDs {
				if e := (Permissions{PermissionsID: pid}).Remove(ctx); e != nil {
					slog.ErrorContext(ctx, "discard tier roles: remove permission", "permission_id", pid, "error", e)
				}
			}
		}
		if e := (Role{RoleID: rid}).Remove(ctx); e != nil {
			slog.ErrorContext(ctx, "discard tier roles: remove role", "role_id", rid, "error", e)
		}
	}
}

// releaseProjectRow HARD-deletes a project row. It is only used to undo a creation that
// failed after the row claimed the slug: a soft delete there would burn the slug forever
// (getProjectBySlug counts inactive rows) over what may be a transient error, for a
// project the caller was told was never created.
func releaseProjectRow(ctx context.Context, projectID string) {
	if err := connect().WithContext(ctx).Where("project_id = ?", projectID).Delete(&Project{}).Error; err != nil {
		slog.ErrorContext(ctx, "release project row", "project_id", projectID, "error", err)
	}
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
	// Slug is global, so a collision is a real conflict, not an overwrite — and it is a
	// conflict against a DELETED project too. The slug names a permission namespace
	// ("project/<slug>/*") that resources in other services still reference by string,
	// so handing it to a new project would silently expose the old project's records to
	// the new one's members. It is retired, not recycled.
	if existing, err := getProjectBySlug(ctx, req.Slug); err == nil {
		msg := "a project with slug " + req.Slug + " already exists"
		if !existing.Active {
			msg = "slug " + req.Slug + " belonged to a deleted project and cannot be reused"
		}
		http.Error(w, msg, http.StatusConflict)
		return
	}

	// Optional parent: records this project's place in the tree (Project.ParentID). The
	// parent must exist and be active, and the caller must be able to create within the
	// parent's namespace (its admin/developer) — creating a child is administering the
	// parent. The tree governs pipeline TARGETING (a parent's pipeline may run for a
	// descendant), but grants NO resource access across the parent↔child boundary:
	// scoping is exact-project, so nothing here provisions cross-namespace grants.
	var parentID *string
	if req.Parent != nil && strings.TrimSpace(*req.Parent) != "" {
		parentSlug := strings.TrimSpace(*req.Parent)
		parent, perr := getProjectBySlug(ctx, parentSlug)
		if perr != nil || !parent.Active {
			http.Error(w, "unknown parent project "+parentSlug, http.StatusBadRequest)
			return
		}
		if !requirePermission(w, r, "createProject", "project/"+parent.Slug+"/gatekeeper/projects") {
			return
		}
		parentID = &parent.ProjectID
	}

	p := Project{
		ProjectID: uuid.New().String(),
		Slug:      req.Slug,
		Namespace: orgNS,
		Name:      req.Name,
		OwnerID:   callerID,
		ParentID:  parentID,
		CreatedAt: time.Now().UTC(),
		Active:    true,
	}
	// Claim the slug with the INSERT before provisioning anything. The unique index is
	// the only check that cannot race the read above (which also runs on the read
	// replica), and provisioning first meant a losing race created three Permissions and
	// three Roles — one of them Service "*" / Actions ["*"] over project/<slug>/* — and
	// only then hit the constraint, orphaning all six with no rollback on every retry.
	if err := p.Add(ctx); err != nil {
		// Resolve the cause against the PRIMARY: the row that won the race may not have
		// replicated yet, and reading the replica here would turn a plain conflict into
		// a 500.
		var clash Project
		if e := connect().WithContext(ctx).First(&clash, "slug = ?", req.Slug).Error; e == nil && clash.ProjectID != p.ProjectID {
			http.Error(w, "a project with slug "+req.Slug+" already exists", http.StatusConflict)
			return
		}
		internalError(w, ctx, "create project", err)
		return
	}
	// The namespace is ours now, so the tier roles can be built. Anything that fails from
	// here rolls the whole creation back: the caller is told the project was not created,
	// so no grant over its namespace — and no row holding its slug — may outlive the call.
	var err error
	if p.ViewerRoleID, err = provisionTierRole(ctx, callerID, req.Slug, "viewer"); err != nil {
		releaseProjectRow(ctx, p.ProjectID)
		internalError(w, ctx, "provision viewer role", err)
		return
	}
	if p.DeveloperRoleID, err = provisionTierRole(ctx, callerID, req.Slug, "developer"); err != nil {
		discardTierRoles(ctx, p.ViewerRoleID)
		releaseProjectRow(ctx, p.ProjectID)
		internalError(w, ctx, "provision developer role", err)
		return
	}
	if p.AdminRoleID, err = provisionTierRole(ctx, callerID, req.Slug, "admin"); err != nil {
		discardTierRoles(ctx, p.ViewerRoleID, p.DeveloperRoleID)
		releaseProjectRow(ctx, p.ProjectID)
		internalError(w, ctx, "provision admin role", err)
		return
	}
	// Attach the tier roles to the row that already holds the slug.
	if err = p.Update(ctx); err != nil {
		discardTierRoles(ctx, p.ViewerRoleID, p.DeveloperRoleID, p.AdminRoleID)
		releaseProjectRow(ctx, p.ProjectID)
		internalError(w, ctx, "attach project roles", err)
		return
	}
	// The creator is the project's first admin.
	_ = assignMembership(ctx, p.AdminRoleID, callerID, callerID)

	// The wiki bot needs developer membership on every project so it can
	// resolve the project through git-factory and list/write pages. Without
	// this, wiki list-pages 500s ("unknown project") on any freshly created
	// project. Env-gated so non-agent deployments don't grant a phantom user.
	if botID := os.Getenv("WIKI_BOT_USER_ID"); botID != "" && botID != callerID {
		if err := assignMembership(ctx, p.DeveloperRoleID, botID, callerID); err != nil {
			slog.WarnContext(ctx, "wiki bot grant failed", "project_id", p.ProjectID, "bot", botID, "err", err)
		}
	}

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

// handleGetProjectAncestors returns the project followed by its ancestor chain, root
// LAST — the effective-scope list for resource inheritance: a child's inherited
// resources are those of every project in this chain. Membership is checked only on
// the requested (child) project, not each ancestor: inheriting a parent's resources is
// exactly a child member seeing them WITHOUT being an ancestor member. The walk stops
// at a missing/inactive/deleted parent (a dangling link) and is cycle-guarded.
func handleGetProjectAncestors(w http.ResponseWriter, r *http.Request) {
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
	chain := []Project{p}
	seen := map[string]bool{p.ProjectID: true}
	cur := p
	for cur.ParentID != nil && *cur.ParentID != "" {
		if seen[*cur.ParentID] {
			break // cycle guard — should be impossible, but never loop
		}
		prow, perr := (Project{ProjectID: *cur.ParentID}).Get(ctx)
		if perr != nil {
			break // dangling parent (e.g. deleted) — stop the chain here
		}
		pp := prow.(Project)
		if !pp.Active {
			break
		}
		chain = append(chain, pp)
		seen[pp.ProjectID] = true
		cur = pp
	}
	writeJSON(w, http.StatusOK, chain)
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
