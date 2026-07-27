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

// projectsInNamespaces lists every active project across the namespaces the caller
// owns (their username and, when in one, their org). Used by GET /projects.
func projectsInNamespaces(ctx context.Context, namespaces []string) ([]Project, error) {
	var rows []Project
	if len(namespaces) == 0 {
		return []Project{}, nil
	}
	err := connectRead().WithContext(ctx).
		Where("active = ? AND namespace IN ?", true, namespaces).
		Order("created_at DESC").Find(&rows).Error
	if rows == nil {
		rows = []Project{}
	}
	return rows, err
}
