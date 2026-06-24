package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Add inserts the permissions record.
func (p Permissions) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.add")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	p.Active = true
	if err := connect().WithContext(ctx).Create(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all permissions fields.
func (p Permissions) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.update")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	p.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:perm:"+p.PermissionsID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the permissions record by setting active = false.
func (p Permissions) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.remove")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	if err := connect().WithContext(ctx).Model(&Permissions{}).Where("permissions_id = ?", p.PermissionsID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:perm:"+p.PermissionsID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active permissions record by PermissionsID.
func (p Permissions) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.get")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	if cached, ok := cacheGet[Permissions](ctx, "gk:perm:"+p.PermissionsID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newP Permissions
	if err := connectRead().WithContext(ctx).First(&newP, "permissions_id = ? AND active = ?", p.PermissionsID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	cacheSet(ctx, "gk:perm:"+newP.PermissionsID, newP, entityTTL)
	span.SetStatus(codes.Ok, "")
	return newP, nil
}

// List retrieves all active permissions records matching the non-zero fields of the receiver.
func (p Permissions) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions.list")
	defer span.End()
	span.SetAttributes(attribute.String("permissions.id", p.PermissionsID))
	var perms []Permissions
	p.Active = true
	q := connectRead().WithContext(ctx).Where(p)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&perms).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(perms))
	for i, perm := range perms {
		result[i] = perm
	}
	return result, nil
}

// Add inserts the permissions check audit record.
func (pc PermissionsCheck) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.add")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	pc.Active = true
	if err := connect().WithContext(ctx).Create(&pc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// ListFiltered returns active permission-check audit rows matching the receiver's
// non-zero filter fields (UserID/Service/Action/OrgID), optionally narrowed by grant
// outcome and a resource substring, newest first. (Resource is matched as a substring
// rather than via the receiver so callers can search scoped paths.)
func (pc PermissionsCheck) ListFiltered(ctx context.Context, granted *bool, resourceLike string, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.list_filtered")
	defer span.End()
	var rows []PermissionsCheck
	q := connectRead().WithContext(ctx).Where("active = ?", true).Where(pc).Order("created_at DESC")
	if granted != nil {
		q = q.Where("granted = ?", *granted)
	}
	if resourceLike != "" {
		q = q.Where("resource LIKE ?", "%"+resourceLike+"%")
	}
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

// Update saves all permissions check fields.
func (pc PermissionsCheck) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.update")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	pc.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&pc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the permissions check record by setting active = false.
func (pc PermissionsCheck) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.remove")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	if err := connect().WithContext(ctx).Model(&PermissionsCheck{}).Where("permissions_check_id = ?", pc.PermissionsCheckID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active permissions check record by PermissionsCheckID.
func (pc PermissionsCheck) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.get")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	var newPC PermissionsCheck
	if err := connectRead().WithContext(ctx).First(&newPC, "permissions_check_id = ? AND active = ?", pc.PermissionsCheckID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return newPC, nil
}

// List retrieves all active permissions check records matching the non-zero fields of the receiver.
func (pc PermissionsCheck) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.permissions_check.list")
	defer span.End()
	span.SetAttributes(attribute.String("permissions_check.id", pc.PermissionsCheckID))
	var checks []PermissionsCheck
	pc.Active = true
	q := connectRead().WithContext(ctx).Where(pc)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&checks).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(checks))
	for i, c := range checks {
		result[i] = c
	}
	return result, nil
}
