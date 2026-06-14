package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// ── Secret ────────────────────────────────────────────────────────────────────

func (s Secret) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.add")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	if err := connect().WithContext(ctx).Create(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Secret) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.update")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	if err := connect().WithContext(ctx).Save(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Secret) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.remove")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	if err := connect().WithContext(ctx).Model(&s).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Secret) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.get")
	defer span.End()
	span.SetAttributes(attribute.String("secret.id", s.SecretID))
	var result Secret
	if err := connectRead().WithContext(ctx).Where("secret_id = ? AND active = true", s.SecretID).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (s Secret) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.secret.list")
	defer span.End()
	var secrets []Secret
	q := connectRead().WithContext(ctx).Where(s)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&secrets).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(secrets))
	for i, sec := range secrets {
		result[i] = sec
	}
	return result, nil
}

// getSecretByID returns an active secret by ID.
func getSecretByID(ctx context.Context, id string) (Secret, error) {
	row, err := (Secret{SecretID: id}).Get(ctx)
	if err != nil {
		return Secret{}, err
	}
	return row.(Secret), nil
}

// ── OrgSecretProvider ─────────────────────────────────────────────────────────

func (p OrgSecretProvider) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org_secret_provider.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p OrgSecretProvider) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org_secret_provider.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&p).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p OrgSecretProvider) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.org_secret_provider.remove")
	defer span.End()
	if err := connect().WithContext(ctx).Delete(&OrgSecretProvider{}, "org_id = ?", p.OrgID).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p OrgSecretProvider) Get(ctx context.Context) (db, error) { return nil, nil }
func (p OrgSecretProvider) List(ctx context.Context, limit, offset int) ([]db, error) {
	return nil, nil
}
