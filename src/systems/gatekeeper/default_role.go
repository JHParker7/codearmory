package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

const (
	// systemRoleOwner marks roles and permissions that gatekeeper manages itself.
	systemRoleOwner = "system"
	// defaultRoleName is the Name of the per-user system-managed default role.
	defaultRoleName = "default"
)

// isSystemRole reports whether a role is the gatekeeper-managed per-user default
// role and therefore must not be edited or deleted through the public role APIs.
// Only rebuildDefaultRole writes these.
func isSystemRole(role Role) bool {
	return role.Name == defaultRoleName || role.OwnerID == systemRoleOwner
}

// rebuildDefaultRole (re)builds the system-managed default role for a user from
// the current "user" default grants. It is the single code path that materialises
// default grants: signup calls it to create the role, and login calls it (gated by
// grantsVersion) to refresh it when the grant definitions change.
//
// Only the default role is touched — the user's personal role (RoleID) and team
// role are left alone, so org/team-ownership and custom grants survive a rebuild.
// The permission set is replaced wholesale, so grants removed from the manifest
// disappear just as new ones appear.
func rebuildDefaultRole(ctx context.Context, userID string) error {
	row, err := (User{UserID: userID}).Get(ctx)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	user := row.(User)

	grants := defaultGrantsFor("user")
	if len(grants) == 0 {
		// No grants available (registry unreachable / not seeded). Skip rather than
		// wipe an existing default role with an empty set.
		slog.WarnContext(ctx, "rebuildDefaultRole: no 'user' default grants available, skipping", "user_id", userID)
		return nil
	}

	templateVars := map[string]string{
		"user_id":  userID,
		"username": user.Username,
	}

	// The role ID is needed up front so the self-read permission can reference it.
	// Reuse the existing default role when present, otherwise mint a new one.
	roleID := uuid.New().String()
	creating := true
	if user.DefaultRoleID != nil && *user.DefaultRoleID != "" {
		roleID = *user.DefaultRoleID
		creating = false
	}

	var newPerms []Permissions
	for _, grant := range grants {
		newPerms = append(newPerms, Permissions{
			Name:          fmt.Sprintf("%s default permissions for %s", grant.ServiceName, user.Username),
			PermissionsID: uuid.New().String(),
			Service:       grant.ServiceName,
			Actions:       grant.Actions,
			Resources:     applyGrantTemplates(grant.Resources, templateVars),
			OwnerID:       systemRoleOwner,
		})
	}

	// Self-read permission: lets the user read their own role(s) and default
	// permission records. getRole covers the default role and (when present) the
	// personal role; getPermissions covers every default permission plus itself.
	selfReadID := uuid.New().String()
	selfResources := []string{"gatekeeper/roles/" + roleID}
	if user.RoleID != nil && *user.RoleID != "" {
		selfResources = append(selfResources, "gatekeeper/roles/"+*user.RoleID)
	}
	for _, p := range newPerms {
		selfResources = append(selfResources, "gatekeeper/permissions/"+p.PermissionsID)
	}
	selfResources = append(selfResources, "gatekeeper/permissions/"+selfReadID)
	newPerms = append(newPerms, Permissions{
		Name:          fmt.Sprintf("gatekeeper self-read permissions for %s", user.Username),
		PermissionsID: selfReadID,
		Service:       "gatekeeper",
		Actions:       []string{"getRole", "getPermissions"},
		Resources:     selfResources,
		OwnerID:       systemRoleOwner,
	})

	// Persist the new permissions. Roll back the ones we created on any failure.
	newIDs := make([]string, 0, len(newPerms))
	rollback := func() {
		for _, id := range newIDs {
			(Permissions{PermissionsID: id}).Remove(ctx) //nolint:errcheck
		}
	}
	for _, p := range newPerms {
		if err := p.Add(ctx); err != nil {
			rollback()
			return fmt.Errorf("create permission: %w", err)
		}
		newIDs = append(newIDs, p.PermissionsID)
	}

	// Point the role at the new permissions. Order matters: the new records are
	// active before the role references them, and the old ones are only removed
	// after the role no longer points at them, so a concurrent permission check
	// never resolves an inactive permission ID.
	var oldPermIDs []string
	if creating {
		role := Role{RoleID: roleID, Name: defaultRoleName, OwnerID: systemRoleOwner, PermissionsIDs: newIDs}
		if err := role.Add(ctx); err != nil {
			rollback()
			return fmt.Errorf("create default role: %w", err)
		}
	} else {
		rRow, err := (Role{RoleID: roleID}).Get(ctx)
		if err != nil {
			rollback()
			return fmt.Errorf("load default role: %w", err)
		}
		role := rRow.(Role)
		oldPermIDs = role.PermissionsIDs
		role.PermissionsIDs = newIDs
		if err := role.Update(ctx); err != nil {
			rollback()
			return fmt.Errorf("update default role: %w", err)
		}
	}

	// Stamp the user with the default role link (first build) and the grants
	// version so later logins skip the rebuild until grants change again.
	user.DefaultRoleID = &roleID
	user.DefaultGrantsVersion = grantsVersion()
	if err := user.Update(ctx); err != nil {
		return fmt.Errorf("stamp user: %w", err)
	}

	// Retire the superseded permissions now that nothing references them.
	for _, id := range oldPermIDs {
		(Permissions{PermissionsID: id}).Remove(ctx) //nolint:errcheck
	}

	slog.InfoContext(ctx, "default role rebuilt", "user_id", userID, "role_id", roleID, "permissions", len(newIDs), "created", creating)
	return nil
}
