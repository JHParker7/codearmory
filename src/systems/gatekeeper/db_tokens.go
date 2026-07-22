package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// Storage for user-minted scoped tokens. Every read is owner-scoped: a token id is
// only ever resolvable by the user who created it, so one user's id cannot be probed
// by another (a denial and a miss are the same 404 to the caller).

// Add inserts the token record.
func (t PersonalToken) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.personal_token.add")
	defer span.End()
	span.SetAttributes(attribute.String("token.id", t.TokenID))
	if err := connect().WithContext(ctx).Create(&t).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get / Update / Remove satisfy the db interface the other entities implement. The
// handlers use the owner-scoped helpers below instead, so these stay minimal.
func (t PersonalToken) Get(ctx context.Context) (db, error) {
	var row PersonalToken
	if err := connectRead().WithContext(ctx).
		Where("token_id = ? AND active = ?", t.TokenID, true).
		First(&row).Error; err != nil {
		return nil, err
	}
	return row, nil
}

func (t PersonalToken) Update(ctx context.Context) error {
	return connect().WithContext(ctx).Model(&PersonalToken{}).
		Where("token_id = ?", t.TokenID).
		Updates(map[string]any{"name": t.Name, "expires_at": t.ExpiresAt}).Error
}

func (t PersonalToken) Remove(ctx context.Context) error { return deactivateToken(ctx, t.TokenID) }

// List returns the owner's tokens, matching the db interface the other entities
// implement. UserID must be set — a token listing is meaningless unscoped.
func (t PersonalToken) List(ctx context.Context, limit, offset int) ([]db, error) {
	q := connectRead().WithContext(ctx).
		Where("user_id = ? AND active = ?", t.UserID, true).
		Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	var rows []PersonalToken
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]db, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out, nil
}

// listTokensForUser returns a user's live tokens, newest first. Expired ones are kept
// and marked rather than hidden: "this token stopped working last week" is the answer
// someone is looking for, and a list that silently drops them cannot give it.
func listTokensForUser(ctx context.Context, userID string) ([]PersonalToken, error) {
	var rows []PersonalToken
	err := connectRead().WithContext(ctx).
		Where("user_id = ? AND active = ?", userID, true).
		Order("created_at DESC").
		Find(&rows).Error
	return rows, err
}

// getTokenForUser resolves one of the caller's own tokens.
func getTokenForUser(ctx context.Context, userID, tokenID string) (PersonalToken, error) {
	var row PersonalToken
	err := connectRead().WithContext(ctx).
		Where("token_id = ? AND user_id = ? AND active = ?", tokenID, userID, true).
		First(&row).Error
	return row, err
}

// tokenForSession finds the token bound to a session, for last-used tracking. Returns
// gorm.ErrRecordNotFound for an ordinary login session, which is the common case.
func tokenForSession(ctx context.Context, sessionID string) (PersonalToken, error) {
	var row PersonalToken
	err := connectRead().WithContext(ctx).
		Where("session_id = ? AND active = ?", sessionID, true).
		First(&row).Error
	return row, err
}

// touchTokenLastUsed records that a token was used, at most once per
// lastUsedResolution. The throttle is the point: this sits on the authentication path
// of every request a token makes, and a write per request would turn a read-mostly
// table into a hot one to answer a question nobody asks to the second.
const lastUsedResolution = 5 * time.Minute

func touchTokenLastUsed(ctx context.Context, sessionID string) {
	row, err := tokenForSession(ctx, sessionID)
	if err != nil {
		return // not a token session, or already revoked
	}
	now := time.Now().UTC()
	if row.LastUsedAt != nil && now.Sub(*row.LastUsedAt) < lastUsedResolution {
		return
	}
	if err := connect().WithContext(ctx).Model(&PersonalToken{}).
		Where("token_id = ?", row.TokenID).
		Update("last_used_at", now).Error; err != nil && err != gorm.ErrRecordNotFound {
		// Best-effort: a failure to record usage must never fail the request that
		// was otherwise authenticated.
		return
	}
}

// deactivateToken retires a token record. Deactivation rather than deletion, so a
// revoked token's name and dates survive in the owner's history.
func deactivateToken(ctx context.Context, tokenID string) error {
	return connect().WithContext(ctx).Model(&PersonalToken{}).
		Where("token_id = ?", tokenID).
		Update("active", false).Error
}

// deactivateRole retires the attenuated role behind a token, going through Role.Remove
// so the permission cache is invalidated with it.
func deactivateRole(ctx context.Context, roleID string) error {
	return Role{RoleID: roleID}.Remove(ctx)
}
