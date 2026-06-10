package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// ── Lookup helpers ────────────────────────────────────────────────────────────

// getUserByEmail returns the active user with the given email address.
func getUserByEmail(ctx context.Context, email string) (User, error) {
	var user User
	if err := connectRead().WithContext(ctx).Where("email = ? AND active = ?", email, true).First(&user).Error; err != nil {
		return User{}, err
	}
	return user, nil
}

// ── Misc write helpers ────────────────────────────────────────────────────────

// syncServiceAccountBootstrapKey updates the hashed_key column to match the
// bootstrap hash after the bootstrap key fallback path succeeds in requireServiceAuth.
func syncServiceAccountBootstrapKey(ctx context.Context, name, hash string) error {
	return connect().WithContext(ctx).Model(&ServiceAccount{}).
		Where("service_name = ?", name).
		Update("hashed_key", hash).Error
}

// clearOrgMembership clears the org_id field on all users that belong to orgID.
func clearOrgMembership(ctx context.Context, orgID string) error {
	return connect().WithContext(ctx).Model(&User{}).Where("org_id = ?", orgID).Update("org_id", nil).Error
}

// getUserIDsByTeam returns the user_id of all users in a team.
func getUserIDsByTeam(ctx context.Context, teamID string) ([]string, error) {
	var ids []string
	if err := connect().WithContext(ctx).Model(&User{}).Where("team_id = ?", teamID).Pluck("user_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// clearTeamMembership clears the team_id field on all users that belong to teamID.
func clearTeamMembership(ctx context.Context, teamID string) error {
	return connect().WithContext(ctx).Model(&User{}).Where("team_id = ?", teamID).Update("team_id", nil).Error
}

// countUsersByRole returns the number of active users assigned to roleID.
func countUsersByRole(ctx context.Context, roleID string) (int64, error) {
	var count int64
	if err := connect().WithContext(ctx).Model(&User{}).Where("role_id = ? AND active = ?", roleID, true).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// countTeamsByRole returns the number of active teams assigned to roleID.
func countTeamsByRole(ctx context.Context, roleID string) (int64, error) {
	var count int64
	if err := connect().WithContext(ctx).Model(&Team{}).Where("role_id = ? AND active = ?", roleID, true).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// deactivateUserSessions sets active=false on all sessions for a user.
func deactivateUserSessions(ctx context.Context, userID string) error {
	return connect().WithContext(ctx).Model(&Session{}).Where("user_id = ?", userID).Update("active", false).Error
}

// applyGrantsForResource grants service permissions to userID using the write connection.
// This is a non-transactional convenience wrapper; use grantServicePermissions directly
// when inside a *gorm.DB transaction.
func applyGrantsForResource(ctx context.Context, service, userID, name string, actions []string, resource string) error {
	return grantServicePermissions(ctx, connect().WithContext(ctx), service, userID, name, actions, resource)
}

// upsertServiceAccountDB upserts a gatekeeper service account by name.
// If the account already exists only HashedBootstrapKey is refreshed;
// HashedKey is preserved so keys rotated at runtime survive restarts.
func upsertServiceAccountDB(ctx context.Context, name, hash string) {
	var existing ServiceAccount
	err := connect().WithContext(ctx).Where("service_name = ?", name).First(&existing).Error
	if err == nil {
		if err2 := connect().WithContext(ctx).Model(&ServiceAccount{}).Where("service_name = ?", name).
			Update("hashed_bootstrap_key", hash).Error; err2 != nil {
			slog.Error("seedServiceAccounts: update bootstrap key failed", "name", name, "error", err2)
		} else {
			slog.Debug("seedServiceAccounts: account exists, bootstrap key refreshed", "name", name)
		}
		return
	}
	svc := ServiceAccount{
		ServiceAccountID:   uuid.New().String(),
		ServiceName:        name,
		HashedKey:          hash,
		HashedBootstrapKey: hash,
		Active:             true,
	}
	if err := connect().WithContext(ctx).Create(&svc).Error; err != nil {
		slog.Error("seedServiceAccounts: create failed", "name", name, "error", err)
	} else {
		slog.Info("seedServiceAccounts: created", "name", name)
	}
}
