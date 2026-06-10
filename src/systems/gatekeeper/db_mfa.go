package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Add inserts the TOTP credential.
func (c TOTPCredential) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.add")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all TOTP credential fields.
func (c TOTPCredential) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.update")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	c.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the TOTP credential.
func (c TOTPCredential) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.remove")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	if err := connect().WithContext(ctx).Model(&TOTPCredential{}).Where("credential_id = ?", c.CredentialID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active TOTP credential by CredentialID.
func (c TOTPCredential) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.get")
	defer span.End()
	span.SetAttributes(attribute.String("credential.id", c.CredentialID))
	var result TOTPCredential
	if err := connectRead().WithContext(ctx).First(&result, "credential_id = ? AND active = ?", c.CredentialID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// List retrieves active TOTP credentials matching the non-zero fields of the receiver.
func (c TOTPCredential) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.totp_credential.list")
	defer span.End()
	var rows []TOTPCredential
	c.Active = true
	q := connectRead().WithContext(ctx).Where(c)
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

func (m MFAPending) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.mfa_pending.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&m).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (m MFAPending) Update(ctx context.Context) error { return nil }
func (m MFAPending) Remove(ctx context.Context) error { return nil }
func (m MFAPending) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.mfa_pending.get")
	defer span.End()
	var result MFAPending
	if err := connectRead().WithContext(ctx).Where("token = ?", m.Token).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}
func (m MFAPending) List(ctx context.Context, limit, offset int) ([]db, error) { return nil, nil }

// redeemMFAPending atomically marks an unused MFA pending token as used.
// Returns the number of rows affected.
func redeemMFAPending(ctx context.Context, token string) (int64, error) {
	result := connect().WithContext(ctx).
		Model(&MFAPending{}).
		Where("token = ? AND used = ?", token, false).
		Update("used", true)
	return result.RowsAffected, result.Error
}

// deleteUnconfirmedTOTP removes any pending (unconfirmed) TOTP enrollment for a user.
func deleteUnconfirmedTOTP(ctx context.Context, userID string) error {
	return connect().WithContext(ctx).
		Where("user_id = ? AND confirmed = ?", userID, false).
		Delete(&TOTPCredential{}).Error
}

// deactivateTOTP deactivates all active TOTP credentials for a user.
// Returns the number of rows affected.
func deactivateTOTP(ctx context.Context, userID string) (int64, error) {
	result := connect().WithContext(ctx).
		Model(&TOTPCredential{}).
		Where("user_id = ? AND active = ?", userID, true).
		Update("active", false)
	return result.RowsAffected, result.Error
}
