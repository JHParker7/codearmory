package main

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// db_memberships.go holds the persistence layer for UserOrgMembership — the
// user↔org join table that lets a user belong to more than one org at a time.
// It follows the same db-interface pattern as the other entities (Add / Update /
// Remove / Get / List) and adds a handful of targeted query helpers used by the
// org handlers and the startup backfill.

// Add inserts the membership as active.
func (m UserOrgMembership) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.membership.add")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", m.UserID), attribute.String("org.id", m.OrgID))
	m.Active = true
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if err := connect().WithContext(ctx).Create(&m).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all fields of the membership.
func (m UserOrgMembership) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.membership.update")
	defer span.End()
	span.SetAttributes(attribute.String("membership.id", m.MembershipID))
	m.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&m).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the membership by setting active = false.
func (m UserOrgMembership) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.membership.remove")
	defer span.End()
	span.SetAttributes(attribute.String("membership.id", m.MembershipID))
	if err := connect().WithContext(ctx).Model(&UserOrgMembership{}).
		Where("membership_id = ?", m.MembershipID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active membership by MembershipID.
func (m UserOrgMembership) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.membership.get")
	defer span.End()
	span.SetAttributes(attribute.String("membership.id", m.MembershipID))
	var found UserOrgMembership
	if err := connectRead().WithContext(ctx).
		First(&found, "membership_id = ? AND active = ?", m.MembershipID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return found, nil
}

// List retrieves all active memberships matching the non-zero fields of the receiver.
func (m UserOrgMembership) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.membership.list")
	defer span.End()
	m.Active = true
	var rows []UserOrgMembership
	q := connectRead().WithContext(ctx).Where(m)
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
	for i, row := range rows {
		result[i] = row
	}
	return result, nil
}

// membershipOrgIDsForUser returns the org IDs of every active org the user belongs
// to, in a stable order (oldest membership first).
func membershipOrgIDsForUser(ctx context.Context, userID string) ([]string, error) {
	var ids []string
	err := connectRead().WithContext(ctx).Model(&UserOrgMembership{}).
		Where("user_id = ? AND active = ?", userID, true).
		Order("created_at ASC").
		Pluck("org_id", &ids).Error
	return ids, err
}

// userIsOrgMember reports whether the user holds an active membership in orgID.
func userIsOrgMember(ctx context.Context, userID, orgID string) (bool, error) {
	var count int64
	err := connectRead().WithContext(ctx).Model(&UserOrgMembership{}).
		Where("user_id = ? AND org_id = ? AND active = ?", userID, orgID, true).
		Count(&count).Error
	return count > 0, err
}

// addMembership records that userID belongs to orgID. It is idempotent: an
// existing active membership is left untouched (returns nil).
func addMembership(ctx context.Context, userID, orgID string) error {
	member, err := userIsOrgMember(ctx, userID, orgID)
	if err != nil {
		return err
	}
	if member {
		return nil
	}
	return UserOrgMembership{
		MembershipID: uuid.New().String(),
		UserID:       userID,
		OrgID:        orgID,
	}.Add(ctx)
}

// listOrgsForUser returns the active orgs the user is a member of, optionally
// narrowed to a single org_id and/or an exact org_name. Returns an empty slice
// (never all orgs) when the user has no memberships or requests an org they do
// not belong to.
func listOrgsForUser(ctx context.Context, userID, orgIDFilter, orgNameFilter string, limit, offset int) ([]Org, error) {
	ids, err := membershipOrgIDsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Org{}, nil
	}
	if orgIDFilter != "" {
		if !slices.Contains(ids, orgIDFilter) {
			return []Org{}, nil
		}
		ids = []string{orgIDFilter}
	}
	q := connectRead().WithContext(ctx).Where("org_id IN ? AND active = ?", ids, true)
	if orgNameFilter != "" {
		q = q.Where("org_name = ?", orgNameFilter)
	}
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	var orgs []Org
	if err := q.Order("created_at ASC").Find(&orgs).Error; err != nil {
		return nil, err
	}
	return orgs, nil
}

// removeMembership soft-deletes the user's active membership in orgID (used when
// a user leaves an org). A no-op when no active membership exists.
func removeMembership(ctx context.Context, userID, orgID string) error {
	return connect().WithContext(ctx).Model(&UserOrgMembership{}).
		Where("user_id = ? AND org_id = ? AND active = ?", userID, orgID, true).
		Update("active", false).Error
}

// deactivateOrgMembershipsForOrg soft-deletes every membership row for orgID.
// Called when an org is deleted so no user is left holding a membership in it.
func deactivateOrgMembershipsForOrg(ctx context.Context, orgID string) error {
	return connect().WithContext(ctx).Model(&UserOrgMembership{}).
		Where("org_id = ? AND active = ?", orgID, true).
		Update("active", false).Error
}

// reassignActiveOrgAfterOrgDelete deactivates every membership in the deleted org
// and, for each user whose active org was that org, moves their active org to
// another org they still belong to (oldest membership first), or clears it when
// they have none left. Returns the IDs of users whose active org changed so the
// caller can invalidate their cached User row.
func reassignActiveOrgAfterOrgDelete(ctx context.Context, deletedOrgID string) ([]string, error) {
	affected, err := getUserIDsByOrg(ctx, deletedOrgID) // users whose active org == deleted org
	if err != nil {
		return nil, err
	}
	if err := deactivateOrgMembershipsForOrg(ctx, deletedOrgID); err != nil {
		return affected, err
	}
	for _, uid := range affected {
		remaining, err := membershipOrgIDsForUser(ctx, uid)
		if err != nil {
			return affected, err
		}
		row, err := (User{UserID: uid}).Get(ctx)
		if err != nil {
			continue
		}
		u := row.(User)
		if len(remaining) > 0 {
			next := remaining[0]
			u.OrgID = &next
		} else {
			u.OrgID = nil
		}
		if err := u.Update(ctx); err != nil {
			return affected, err
		}
	}
	return affected, nil
}

// backfillMemberships gives every user with a non-nil active org a matching
// membership row, so existing single-org accounts participate in the join table
// without a manual data migration. Idempotent — safe to run on every startup.
func backfillMemberships(ctx context.Context) error {
	var users []User
	if err := connect().WithContext(ctx).
		Where("org_id IS NOT NULL AND active = ?", true).
		Find(&users).Error; err != nil {
		return err
	}
	for _, u := range users {
		if u.OrgID == nil {
			continue
		}
		if err := addMembership(ctx, u.UserID, *u.OrgID); err != nil {
			return err
		}
	}
	return nil
}
