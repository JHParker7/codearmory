package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Add inserts the invite.
func (invite Invite) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.add")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	invite.Active = true
	if err := connect().WithContext(ctx).Create(&invite).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all invite fields.
func (invite Invite) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.update")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	invite.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&invite).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the invite by setting active = false.
func (invite Invite) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.remove")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	if err := connect().WithContext(ctx).Model(&Invite{}).Where("invite_id = ?", invite.InviteID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active invite by InviteID.
func (invite Invite) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.get")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	var newInvite Invite
	if err := connectRead().WithContext(ctx).First(&newInvite, "invite_id = ? AND active = ?", invite.InviteID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return newInvite, nil
}

// List retrieves all active invites matching the non-zero fields of the receiver.
func (invite Invite) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.invite.list")
	defer span.End()
	span.SetAttributes(attribute.String("invite.id", invite.InviteID))
	var invites []Invite
	invite.Active = true
	q := connectRead().WithContext(ctx).Where(invite)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&invites).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(invites))
	for i, inv := range invites {
		result[i] = inv
	}
	return result, nil
}

// listInvitesForCaller returns active invites where callerID is the inviter or
// callerEmail is the invitee. Optional filters narrow the result further.
func listInvitesForCaller(ctx context.Context, callerID, callerEmail, inviteID, resourceType, resourceID, status string, limit, offset int) ([]db, error) {
	q := connectRead().WithContext(ctx).
		Where("active = ? AND (inviter_id = ? OR invitee_email = ?)", true, callerID, callerEmail)
	if inviteID != "" {
		q = q.Where("invite_id = ?", inviteID)
	}
	if resourceType != "" {
		q = q.Where("resource_type = ?", resourceType)
	}
	if resourceID != "" {
		q = q.Where("resource_id = ?", resourceID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	var invites []Invite
	if err := q.Find(&invites).Error; err != nil {
		return nil, err
	}
	result := make([]db, len(invites))
	for i, inv := range invites {
		result[i] = inv
	}
	return result, nil
}
