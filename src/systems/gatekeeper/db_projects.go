package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Project storage follows the same db-interface pattern as the other gatekeeper
// entities (Add/Update/Remove/Get/List), soft-deleting via active=false. Projects are
// not cached: they are read on the project-management path, not the hot
// check-permissions path (that path only touches roles and permissions).

func (p Project) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.project.add")
	defer span.End()
	span.SetAttributes(attribute.String("project.id", p.ProjectID))
	p.Active = true
	if err := connect().WithContext(ctx).Create(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p Project) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.project.update")
	defer span.End()
	span.SetAttributes(attribute.String("project.id", p.ProjectID))
	if err := connect().WithContext(ctx).Save(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p Project) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.project.remove")
	defer span.End()
	span.SetAttributes(attribute.String("project.id", p.ProjectID))
	if err := connect().WithContext(ctx).Model(&Project{}).Where("project_id = ?", p.ProjectID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p Project) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.project.get")
	defer span.End()
	span.SetAttributes(attribute.String("project.id", p.ProjectID))
	var out Project
	if err := connectRead().WithContext(ctx).First(&out, "project_id = ? AND active = ?", p.ProjectID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

// List returns active projects matching the non-zero fields of the receiver — in
// practice the caller sets Namespace to list a namespace's projects.
func (p Project) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.project.list")
	defer span.End()
	var rows []Project
	p.Active = true
	q := connectRead().WithContext(ctx).Where(p).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&rows).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(rows))
	for i, r := range rows {
		result[i] = r
	}
	return result, nil
}

// projectsOwnedBy lists the active projects a user administers (created). Used by
// GET /projects — the "mine to manage" list.
func projectsOwnedBy(ctx context.Context, ownerID string) ([]Project, error) {
	var rows []Project
	err := connectRead().WithContext(ctx).
		Where("active = ? AND owner_id = ?", true, ownerID).
		Order("created_at DESC").Find(&rows).Error
	if rows == nil {
		rows = []Project{}
	}
	return rows, err
}

// getProjectBySlug looks up a project by its global slug (slugs are unique across the
// platform now that a project is its own top-level namespace). It deliberately spans
// INACTIVE rows: the unique index on slug covers every row, so a soft-deleted project
// still holds its slug, and an active-only lookup would report a name as free that the
// insert then rejects. Callers read Active to tell "taken" from "taken by a deleted
// project".
func getProjectBySlug(ctx context.Context, slug string) (Project, error) {
	var p Project
	err := connectRead().WithContext(ctx).First(&p, "slug = ?", slug).Error
	return p, err
}

// accessibleProject pairs a project with the caller's highest tier in it, so a service
// can widen list queries (by ProjectID) and the portal can show role. Owners report
// tier "owner".
type accessibleProject struct {
	Project
	Tier string `json:"tier"`
}

// accessibleProjects returns every project the caller can reach: those they own, plus
// those they hold any tier role in (via RoleMembership). This is what services call to
// widen list views to include another namespace's project-shared resources.
func accessibleProjects(ctx context.Context, callerID string) ([]accessibleProject, error) {
	rd := connectRead().WithContext(ctx)

	var owned []Project
	if err := rd.Where("active = ? AND owner_id = ?", true, callerID).Find(&owned).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]accessibleProject, 0, len(owned))
	for _, p := range owned {
		seen[p.ProjectID] = true
		out = append(out, accessibleProject{Project: p, Tier: "owner"})
	}

	// Member projects: the caller's role memberships → tier roles → projects.
	var roleIDs []string
	if err := rd.Model(&RoleMembership{}).Where("user_id = ?", callerID).Pluck("role_id", &roleIDs).Error; err != nil {
		return nil, err
	}
	if len(roleIDs) == 0 {
		return out, nil
	}
	var member []Project
	if err := rd.Where("active = ? AND (viewer_role_id IN ? OR developer_role_id IN ? OR admin_role_id IN ?)",
		true, roleIDs, roleIDs, roleIDs).Find(&member).Error; err != nil {
		return nil, err
	}
	rset := map[string]bool{}
	for _, id := range roleIDs {
		rset[id] = true
	}
	for _, p := range member {
		if seen[p.ProjectID] {
			continue
		}
		seen[p.ProjectID] = true
		out = append(out, accessibleProject{Project: p, Tier: tierFor(p, rset)})
	}
	return out, nil
}

// tierFor reports the caller's strongest tier in a project given the set of role ids
// they hold (admin ⊃ developer ⊃ viewer).
func tierFor(p Project, held map[string]bool) string {
	switch {
	case p.AdminRoleID != "" && held[p.AdminRoleID]:
		return "admin"
	case p.DeveloperRoleID != "" && held[p.DeveloperRoleID]:
		return "developer"
	case p.ViewerRoleID != "" && held[p.ViewerRoleID]:
		return "viewer"
	}
	return ""
}

// isProjectMember reports whether the caller can VIEW a project (owner or any tier).
func isProjectMember(ctx context.Context, callerID string, p Project) bool {
	if p.OwnerID == callerID {
		return true
	}
	var n int64
	connectRead().WithContext(ctx).Model(&RoleMembership{}).
		Where("user_id = ? AND role_id IN ?", callerID, []string{p.ViewerRoleID, p.DeveloperRoleID, p.AdminRoleID}).
		Count(&n)
	return n > 0
}
