package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// instanceUserCount returns the total number of user accounts on the instance,
// regardless of active status. It is the single source of truth for "has this
// instance been bootstrapped": both the public setup-status endpoint and the
// bootstrap-admin determination use it, so deactivating every user can never reset
// the initialized state (which would otherwise hand bootstrap admin to the next
// signup). It deliberately counts inactive accounts — only a full account wipe
// resets it. The caller passes the db handle so the bootstrap path can count inside
// its transaction (read-your-writes) while the public endpoint uses the primary.
func instanceUserCount(db *gorm.DB) (int64, error) {
	var count int64
	if err := db.Model(&User{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

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

// upgradeServiceKeyHash rewrites a legacy bcrypt hash in the cheap format, after
// the key has already been verified.
//
// Both columns are written, because either can be the one that authenticated and
// leaving the other in the old format would keep paying for it. Nothing here
// changes what the key IS — only how it is stored — so a service that is
// authenticating successfully keeps doing so.
func upgradeServiceKeyHash(ctx context.Context, name, key string) error {
	hash := hashServiceKey(key)
	return connect().WithContext(ctx).Model(&ServiceAccount{}).
		Where("service_name = ? AND hashed_key NOT LIKE ?", name, serviceKeyScheme+"%").
		Updates(map[string]any{"hashed_key": hash}).Error
}

// getUserIDsByOrg returns the user_id of all users in an org.
func getUserIDsByOrg(ctx context.Context, orgID string) ([]string, error) {
	var ids []string
	if err := connect().WithContext(ctx).Model(&User{}).Where("org_id = ?", orgID).Pluck("user_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
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
// If the account already exists HashedBootstrapKey is refreshed and the row is
// reactivated; HashedKey is preserved so keys rotated at runtime survive restarts.
//
// active is restored deliberately: registering is how a torn-down service comes
// BACK, and the lookup here is the only one that sees inactive rows (ServiceAccount.Get
// filters active=true). Without it a deregistered account would be updated in place,
// stay invisible to every caller, and never revive — not on re-registration and not on
// a gatekeeper restart, since seeding takes this same path.
func upsertServiceAccountDB(ctx context.Context, name, hash string) {
	var existing ServiceAccount
	err := connect().WithContext(ctx).Where("service_name = ?", name).First(&existing).Error
	if err == nil {
		if err2 := connect().WithContext(ctx).Model(&ServiceAccount{}).Where("service_name = ?", name).
			Updates(map[string]any{"hashed_bootstrap_key": hash, "active": true}).Error; err2 != nil {
			slog.ErrorContext(ctx, "seedServiceAccounts: update bootstrap key failed", "name", name, "error", err2)
		} else {
			slog.DebugContext(ctx, "seedServiceAccounts: account exists, bootstrap key refreshed", "name", name)
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
		slog.ErrorContext(ctx, "seedServiceAccounts: create failed", "name", name, "error", err)
	} else {
		slog.InfoContext(ctx, "seedServiceAccounts: created", "name", name)
	}
}
