package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Add inserts the audit log entry.
func (a AuditLog) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.audit_log.add")
	defer span.End()
	span.SetAttributes(attribute.String("audit_log.id", a.AuditLogID))
	if err := connect().WithContext(ctx).Create(&a).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves an audit log entry by AuditLogID.
func (a AuditLog) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.audit_log.get")
	defer span.End()
	span.SetAttributes(attribute.String("audit_log.id", a.AuditLogID))
	var result AuditLog
	if err := connectRead().WithContext(ctx).First(&result, "audit_log_id = ?", a.AuditLogID).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves audit log entries matching the non-zero fields of the receiver.
func (a AuditLog) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.audit_log.list")
	defer span.End()
	var rows []AuditLog
	q := connectRead().WithContext(ctx).Where(a).Order("created_at DESC")
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

