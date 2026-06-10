package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Add inserts the service account.
func (svc ServiceAccount) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.add")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.name", svc.ServiceName))
	svc.Active = true
	if err := connect().WithContext(ctx).Create(&svc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all service account fields.
func (svc ServiceAccount) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.update")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.name", svc.ServiceName))
	svc.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&svc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the service account.
func (svc ServiceAccount) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.remove")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.id", svc.ServiceAccountID))
	if err := connect().WithContext(ctx).Model(&ServiceAccount{}).Where("service_account_id = ?", svc.ServiceAccountID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active service account by ServiceAccountID or ServiceName.
func (svc ServiceAccount) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.get")
	defer span.End()
	span.SetAttributes(attribute.String("service_account.name", svc.ServiceName))
	var result ServiceAccount
	if err := connectRead().WithContext(ctx).
		Where("(service_account_id = ? OR service_name = ?) AND active = ?", svc.ServiceAccountID, svc.ServiceName, true).
		First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves all active service accounts.
func (svc ServiceAccount) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_account.list")
	defer span.End()
	var rows []ServiceAccount
	q := connectRead().WithContext(ctx).Where("active = ?", true)
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

// Add inserts the service permission request.
func (req ServicePermissionRequest) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.add")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	req.Active = true
	if err := connect().WithContext(ctx).Create(&req).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all service permission request fields.
func (req ServicePermissionRequest) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.update")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	req.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&req).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the service permission request.
func (req ServicePermissionRequest) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.remove")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	if err := connect().WithContext(ctx).Model(&ServicePermissionRequest{}).Where("request_id = ?", req.RequestID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active service permission request by RequestID.
func (req ServicePermissionRequest) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.get")
	defer span.End()
	span.SetAttributes(attribute.String("request.id", req.RequestID))
	var result ServicePermissionRequest
	if err := connectRead().WithContext(ctx).First(&result, "request_id = ? AND active = ?", req.RequestID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves active service permission requests matching the non-zero fields of the receiver.
func (req ServicePermissionRequest) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.service_permission_request.list")
	defer span.End()
	var rows []ServicePermissionRequest
	req.Active = true
	q := connectRead().WithContext(ctx).Where(req).Order("created_at DESC")
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
