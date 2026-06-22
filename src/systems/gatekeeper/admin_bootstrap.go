package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// errNoDefaultGrants is returned by createUserWithBootstrapAdmin when a non-admin
// signup is attempted while no user default grants are available — refusing to
// create a permission-less account.
var errNoDefaultGrants = errors.New("no default grants available")

// firstUserAdvisoryKey is the fixed key for the Postgres transaction advisory lock
// that serialises the first-user (bootstrap admin) determination, so two
// concurrent signups on an empty database cannot both be granted admin.
const firstUserAdvisoryKey int64 = 800108081

// createUserWithBootstrapAdmin inserts personalRole and user in a single
// transaction, deciding inside that transaction whether this is the very first
// account — and therefore the bootstrap admin. The count and the insert are atomic:
// on Postgres a transaction-scoped advisory lock serialises concurrent signups
// (closing the count-then-create TOCTOU); on SQLite (tests) write transactions are
// already serialised. When admin is granted, personalRole is named "admin" and a
// wildcard permission is created in the same transaction.
//
// adminCandidate is false when an env-var admin is configured (the first-user
// fallback is off), so the lock/count are skipped. When the user is not the admin
// and no default grants are available, the whole transaction is rolled back with
// errNoDefaultGrants so a permission-less account is never created.
func createUserWithBootstrapAdmin(ctx context.Context, user *User, personalRole *Role, adminCandidate, grantsAvailable bool) (bool, error) {
	grantedAdmin := false
	err := connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if adminCandidate {
			// Serialise the first-user determination on Postgres so concurrent
			// signups on an empty DB cannot both win the admin grant.
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", firstUserAdvisoryKey).Error; err != nil {
					return err
				}
			}
			// Count every account (not just active ones): once any user has
			// existed the instance is bootstrapped, so deactivating users can never
			// reopen the first-user admin grant. See instanceUserCount.
			count, err := instanceUserCount(tx)
			if err != nil {
				return err
			}
			grantedAdmin = count == 0
		}
		if !grantedAdmin && !grantsAvailable {
			return errNoDefaultGrants
		}
		if grantedAdmin {
			adminPerm := newAdminPermission(user.UserID, user.Username)
			adminPerm.Active = true
			if err := tx.Create(&adminPerm).Error; err != nil {
				return err
			}
			personalRole.Name = adminRoleName
			personalRole.PermissionsIDs = []string{adminPerm.PermissionsID}
		}
		personalRole.Active = true
		if err := tx.Create(personalRole).Error; err != nil {
			return err
		}
		user.Active = true
		if err := tx.Create(user).Error; err != nil {
			return err
		}
		return nil
	})
	return grantedAdmin, err
}

// adminRoleName is the Name given to the bootstrap admin's personal role so the
// account is easy to identify. Unlike the system-managed "default" role it stays
// owned by the user and remains editable through the public role API.
const adminRoleName = "admin"

// newAdminPermission builds a single permission that grants every action on every
// resource of every service. matchPermission treats a "*" Service, Action, or
// Resource as matching anything, so this one record confers full administrative
// access. It is owned by the user (not systemRoleOwner) so the admin can manage it
// later through the normal permission API.
func newAdminPermission(ownerID, username string) Permissions {
	return Permissions{
		Name:          fmt.Sprintf("full administrative access for %s (bootstrap admin)", username),
		PermissionsID: uuid.New().String(),
		Service:       "*",
		Actions:       []string{"*"},
		Resources:     []string{"*"},
		OwnerID:       ownerID,
	}
}

// adminSeedConfigured reports whether a dedicated admin account is provisioned via
// GATEKEEPER_ADMIN_EMAIL/GATEKEEPER_ADMIN_PASSWORD. When true, the
// first-user-becomes-admin fallback in signup is disabled — the env-var admin is
// the designated operator.
func adminSeedConfigured() bool {
	return secret("GATEKEEPER_ADMIN_EMAIL") != "" && secret("GATEKEEPER_ADMIN_PASSWORD") != ""
}

// defaultAdminUsername derives a username from the admin email's local part,
// falling back to "admin" when the email has no usable local part.
func defaultAdminUsername(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return "admin"
}

// seedAdminUser provisions the platform admin from GATEKEEPER_ADMIN_EMAIL and
// GATEKEEPER_ADMIN_PASSWORD (both read via secret(), so *_FILE volume mounts work).
// It is idempotent and safe to call on every startup:
//   - both vars unset           → no-op (feature disabled; first-user fallback applies)
//   - email exists already      → ensure the account holds the admin grant
//   - email does not exist yet  → create the account with the wildcard admin role
//
// Password is never logged. A misconfiguration (one var set, the other blank, or a
// too-short password) is logged and skipped rather than fatal, so gatekeeper still
// starts and serves traffic.
func seedAdminUser(ctx context.Context) {
	email := secret("GATEKEEPER_ADMIN_EMAIL")
	password := secret("GATEKEEPER_ADMIN_PASSWORD")
	if email == "" || password == "" {
		if email != "" || password != "" {
			slog.WarnContext(ctx, "seedAdminUser: GATEKEEPER_ADMIN_EMAIL and GATEKEEPER_ADMIN_PASSWORD must both be set — admin seeding skipped")
		}
		return
	}

	if existing, err := getUserByEmail(ctx, email); err == nil {
		if err := ensureUserAdmin(ctx, existing); err != nil {
			slog.ErrorContext(ctx, "seedAdminUser: failed to ensure admin grant on existing user", "user_id", existing.UserID, "error", err)
			return
		}
		slog.InfoContext(ctx, "seedAdminUser: existing user confirmed as admin", "user_id", existing.UserID)
		return
	}

	if len(password) < 8 {
		slog.ErrorContext(ctx, "seedAdminUser: GATEKEEPER_ADMIN_PASSWORD must be at least 8 characters — admin not created")
		return
	}
	username := secret("GATEKEEPER_ADMIN_USERNAME")
	if username == "" {
		username = defaultAdminUsername(email)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		slog.ErrorContext(ctx, "seedAdminUser: bcrypt failed", "error", err)
		return
	}

	userID := uuid.New().String()
	adminPerm := newAdminPermission(userID, username)
	if err := adminPerm.Add(ctx); err != nil {
		slog.ErrorContext(ctx, "seedAdminUser: failed to create admin permission", "error", err)
		return
	}
	personalRole := Role{RoleID: uuid.New().String(), Name: adminRoleName, OwnerID: userID, PermissionsIDs: []string{adminPerm.PermissionsID}}
	if err := personalRole.Add(ctx); err != nil {
		adminPerm.Remove(ctx) //nolint:errcheck
		slog.ErrorContext(ctx, "seedAdminUser: failed to create admin role", "error", err)
		return
	}
	user := User{
		UserID:         userID,
		Email:          email,
		Username:       username,
		HashedPassword: string(hash),
		RoleID:         &personalRole.RoleID,
	}
	if err := user.Add(ctx); err != nil {
		adminPerm.Remove(ctx)    //nolint:errcheck
		personalRole.Remove(ctx) //nolint:errcheck
		slog.ErrorContext(ctx, "seedAdminUser: failed to create admin user", "email", email, "error", err)
		return
	}

	// Best-effort: also materialise the standard default grants so the account is
	// shaped like any other user. The wildcard grant already covers everything, so a
	// failure here (e.g. registry not yet reachable) is non-fatal.
	if err := rebuildDefaultRole(ctx, userID); err != nil {
		slog.WarnContext(ctx, "seedAdminUser: default role build failed (admin still has full access)", "user_id", userID, "error", err)
	}
	slog.InfoContext(ctx, "seedAdminUser: admin user created", "user_id", userID, "username", username)
}

// ensureUserAdmin guarantees that user holds the wildcard admin grant on its
// personal role, creating the role and/or permission only when missing. Idempotent:
// a user that already has a "*"-service permission is left untouched.
func ensureUserAdmin(ctx context.Context, user User) error {
	var personalRole Role
	if user.RoleID != nil && *user.RoleID != "" {
		row, err := (Role{RoleID: *user.RoleID}).Get(ctx)
		if err != nil {
			return fmt.Errorf("load personal role: %w", err)
		}
		personalRole = row.(Role)
	} else {
		personalRole = Role{RoleID: uuid.New().String(), Name: adminRoleName, OwnerID: user.UserID, PermissionsIDs: []string{}}
		if err := personalRole.Add(ctx); err != nil {
			return fmt.Errorf("create personal role: %w", err)
		}
		user.RoleID = &personalRole.RoleID
		if err := user.Update(ctx); err != nil {
			return fmt.Errorf("link personal role: %w", err)
		}
	}

	// Already an admin? One query (instead of a Get per permission) to detect any
	// wildcard-service grant already attached to the role.
	if len(personalRole.PermissionsIDs) > 0 {
		var wildcard int64
		if err := connectRead().WithContext(ctx).Model(&Permissions{}).
			Where("permissions_id IN ? AND service = ? AND active = ?", personalRole.PermissionsIDs, "*", true).
			Count(&wildcard).Error; err != nil {
			return fmt.Errorf("check existing admin grant: %w", err)
		}
		if wildcard > 0 {
			return nil
		}
	}

	adminPerm := newAdminPermission(user.UserID, user.Username)
	if err := adminPerm.Add(ctx); err != nil {
		return fmt.Errorf("create admin permission: %w", err)
	}
	personalRole.PermissionsIDs = append(personalRole.PermissionsIDs, adminPerm.PermissionsID)
	// role.Update invalidates the Redis permission cache — db.Save would not.
	if err := personalRole.Update(ctx); err != nil {
		adminPerm.Remove(ctx) //nolint:errcheck
		return fmt.Errorf("attach admin permission: %w", err)
	}
	return nil
}
