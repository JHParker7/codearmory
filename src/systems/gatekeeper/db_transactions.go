package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ── Complex transactions ──────────────────────────────────────────────────────

// approveServicePermissionRequestAtomic approves a pending service permission
// request inside a serialised FOR UPDATE transaction.
// Returns the approved request (for audit logging) or an error.
// A "request is not pending" error means a concurrent approver won the race.
func approveServicePermissionRequestAtomic(ctx context.Context, requestID, callerID string) (ServicePermissionRequest, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.approve_permission_request")
	defer span.End()
	span.SetAttributes(
		attribute.String("request.id", requestID),
		attribute.String("user.id", callerID),
	)

	var spr ServicePermissionRequest
	now := time.Now()
	err := connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("request_id = ? AND active = ?", requestID, true).
			First(&spr).Error; err != nil {
			return err
		}
		if spr.Status != "pending" {
			return fmt.Errorf("request is not pending")
		}

		var svcAcct ServiceAccount
		if err := tx.Where("service_name = ? AND active = ?", spr.ServiceName, true).First(&svcAcct).Error; err != nil {
			return err
		}

		var role Role
		if svcAcct.RoleID == nil {
			role = Role{
				RoleID:         uuid.New().String(),
				OwnerID:        callerID,
				Active:         true,
				PermissionsIDs: []string{},
			}
			if err := tx.Create(&role).Error; err != nil {
				return err
			}
			svcAcct.RoleID = &role.RoleID
			svcAcct.UpdatedAt = now
			if err := tx.Save(&svcAcct).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Where("role_id = ? AND active = ?", *svcAcct.RoleID, true).First(&role).Error; err != nil {
				return err
			}
		}

		perm := Permissions{
			PermissionsID: uuid.New().String(),
			Name:          spr.Name,
			Service:       spr.Service,
			Actions:       spr.Actions,
			Resources:     spr.Resources,
			OwnerID:       callerID,
			Active:        true,
		}
		if err := tx.Create(&perm).Error; err != nil {
			return err
		}

		if role.PermissionsIDs == nil {
			role.PermissionsIDs = []string{}
		}
		role.PermissionsIDs = append(role.PermissionsIDs, perm.PermissionsID)
		role.UpdatedAt = now
		if err := tx.Save(&role).Error; err != nil {
			return err
		}
		cacheDel(ctx, "gk:role:"+role.RoleID)

		spr.Status = "approved"
		spr.ResolvedBy = &callerID
		spr.ResolvedAt = &now
		spr.UpdatedAt = now
		return tx.Save(&spr).Error
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "approve permission request failed", "request_id", requestID, "caller_id", callerID, "error", err)
		return spr, err
	}
	span.SetAttributes(
		attribute.String("service.name", spr.ServiceName),
		attribute.String("permission.name", spr.Name),
	)
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "permission request approved", "request_id", requestID, "service", spr.ServiceName, "caller_id", callerID)
	return spr, nil
}

// acceptInviteAtomic accepts an invite atomically: locks the invite and user
// rows, updates membership, marks the invite accepted, and grants the member
// permission — all in a single transaction.
// Returns sentinel errors "invite is not pending", "already in org", "already in team"
// for concurrency conflicts that map to HTTP 409.
func acceptInviteAtomic(ctx context.Context, inviteID, callerID string, invite Invite, permName, memberAction, permResource string) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.accept_invite")
	defer span.End()
	span.SetAttributes(
		attribute.String("invite.id", inviteID),
		attribute.String("user.id", callerID),
		attribute.String("resource.type", invite.ResourceType),
		attribute.String("resource.id", invite.ResourceID),
	)

	err := connect().Transaction(func(tx *gorm.DB) error {
		var fresh Invite
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("invite_id = ? AND active = ?", inviteID, true).
			First(&fresh).Error; err != nil {
			return err
		}
		if fresh.Status != "pending" {
			return errors.New("invite is not pending")
		}

		var freshCaller User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND active = ?", callerID, true).
			First(&freshCaller).Error; err != nil {
			return err
		}
		switch invite.ResourceType {
		case "org":
			// Multi-org: record a membership rather than overwriting the single org.
			// Reject only if the caller is already a member of this org (idempotency);
			// belonging to other orgs is fine. If the caller has no active org yet,
			// this org becomes their active one.
			var existing int64
			if err := tx.Model(&UserOrgMembership{}).
				Where("user_id = ? AND org_id = ? AND active = ?", callerID, invite.ResourceID, true).
				Count(&existing).Error; err != nil {
				return err
			}
			if existing > 0 {
				return errors.New("already in org")
			}
			now := time.Now()
			membership := UserOrgMembership{
				MembershipID: uuid.New().String(),
				UserID:       callerID,
				OrgID:        invite.ResourceID,
				Active:       true,
				CreatedAt:    now,
				UpdatedAt:    now,
			}
			if err := tx.Create(&membership).Error; err != nil {
				return err
			}
			if freshCaller.OrgID == nil {
				freshCaller.OrgID = &invite.ResourceID
			}
		case "team":
			if freshCaller.TeamID != nil && *freshCaller.TeamID != invite.ResourceID {
				return errors.New("already in team")
			}
			freshCaller.TeamID = &invite.ResourceID
		}
		freshCaller.UpdatedAt = time.Now()

		fresh.Status = "accepted"
		if err := tx.Save(&freshCaller).Error; err != nil {
			return err
		}
		if err := tx.Save(&fresh).Error; err != nil {
			return err
		}
		return grantPermissions(ctx, tx, callerID, permName, []string{memberAction}, permResource)
	})
	if err != nil {
		// Sentinel concurrency errors (invite not pending, already in org/team) are
		// expected 409 outcomes — record but don't log at Error level.
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "invite accepted", "invite_id", inviteID, "caller_id", callerID, "resource_type", invite.ResourceType, "resource_id", invite.ResourceID)
	return nil
}
