package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withGrants temporarily replaces the cached default grants for the duration of a
// test and restores the original set on cleanup.
func withGrants(t *testing.T, grants []DefaultGrant) {
	t.Helper()
	defaultGrantsMu.Lock()
	prev := cachedGrants
	cachedGrants = grants
	defaultGrantsMu.Unlock()
	t.Cleanup(func() {
		defaultGrantsMu.Lock()
		cachedGrants = prev
		defaultGrantsMu.Unlock()
	})
}

func TestRebuildDefaultRole_CreatesAndEnforces(t *testing.T) {
	u := createTestUser(t)
	ctx := context.Background()

	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("rebuildDefaultRole: %v", err)
	}
	t.Cleanup(func() { cleanupSignup(t, u.UserID) })

	row, err := (User{UserID: u.UserID}).Get(ctx)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	got := row.(User)
	if got.DefaultRoleID == nil {
		t.Fatal("expected DefaultRoleID to be set after rebuild")
	}
	if got.DefaultGrantsVersion != grantsVersion() {
		t.Fatalf("DefaultGrantsVersion = %q, want %q", got.DefaultGrantsVersion, grantsVersion())
	}

	// A user-scoped default grant (createOrg on gatekeeper/orgs) is now enforced
	// even though the user's personal role (RoleID) is empty/nil.
	ok, err := checkPermissions(ctx, u.UserID, "gatekeeper", "createOrg", "gatekeeper/orgs")
	if err != nil {
		t.Fatalf("checkPermissions: %v", err)
	}
	if !ok {
		t.Fatal("expected createOrg to be granted via the default role")
	}
}

func TestRebuildDefaultRole_RefreshesOnGrantsChange(t *testing.T) {
	u := createTestUser(t)
	ctx := context.Background()
	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("initial rebuild: %v", err)
	}
	t.Cleanup(func() { cleanupSignup(t, u.UserID) })

	row, _ := (User{UserID: u.UserID}).Get(ctx)
	roleID := *row.(User).DefaultRoleID
	rRow, _ := (Role{RoleID: roleID}).Get(ctx)
	oldPermIDs := rRow.(Role).PermissionsIDs
	v1 := grantsVersion()

	// Introduce a grant change that conveys a brand-new action and resource.
	withGrants(t, []DefaultGrant{
		{ServiceName: "gatekeeper", GrantOn: "user",
			Actions:   []string{"getUser", "newAction"},
			Resources: []string{"{username}/gatekeeper/users/{user_id}", "{username}/gatekeeper/newthing"}},
	})
	if grantsVersion() == v1 {
		t.Fatal("expected grantsVersion to change after grant change")
	}

	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}

	row2, _ := (User{UserID: u.UserID}).Get(ctx)
	if *row2.(User).DefaultRoleID != roleID {
		t.Fatal("default role ID should be stable across rebuilds")
	}
	if row2.(User).DefaultGrantsVersion != grantsVersion() {
		t.Fatal("version not restamped after rebuild")
	}
	for _, pid := range oldPermIDs {
		if _, err := (Permissions{PermissionsID: pid}).Get(ctx); err == nil {
			t.Fatalf("old permission %s should be inactive after rebuild", pid)
		}
	}
	ok, err := checkPermissions(ctx, u.UserID, "gatekeeper", "newAction", "gatekeeper/newthing")
	if err != nil {
		t.Fatalf("checkPermissions: %v", err)
	}
	if !ok {
		t.Fatal("expected newAction to be granted after rebuild")
	}
}

func TestRebuildDefaultRole_NoGrantsSkips(t *testing.T) {
	u := createTestUser(t)
	ctx := context.Background()
	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("initial rebuild: %v", err)
	}
	t.Cleanup(func() { cleanupSignup(t, u.UserID) })
	row, _ := (User{UserID: u.UserID}).Get(ctx)
	roleID := *row.(User).DefaultRoleID

	// With no grants available a rebuild must be a no-op rather than wipe the role.
	withGrants(t, nil)
	if grantsVersion() != "" {
		t.Fatal("expected empty grantsVersion with no grants loaded")
	}
	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("rebuild with no grants: %v", err)
	}
	rRow, err := (Role{RoleID: roleID}).Get(ctx)
	if err != nil {
		t.Fatalf("default role should still exist: %v", err)
	}
	if len(rRow.(Role).PermissionsIDs) == 0 {
		t.Fatal("default role permissions should be preserved when no grants are available")
	}
}

func TestDefaultRole_UpdateAndDeleteRejected(t *testing.T) {
	u := createTestUser(t)
	ctx := context.Background()
	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Cleanup(func() { cleanupSignup(t, u.UserID) })
	row, _ := (User{UserID: u.UserID}).Get(ctx)
	roleID := *row.(User).DefaultRoleID

	// Update is rejected.
	upd := createAuthorizedUser(t, "updateRole", "gatekeeper/roles/"+roleID)
	body, _ := json.Marshal(roleRequest{PermissionsIDs: []string{}})
	ur := withUserID(httptest.NewRequest(http.MethodPut, "/roles/"+roleID, bytes.NewReader(body)), upd.UserID)
	ur.SetPathValue("id", roleID)
	uw := httptest.NewRecorder()
	handleUpdateRole(uw, ur)
	if uw.Code != http.StatusForbidden {
		t.Fatalf("expected 403 updating system role, got %d: %s", uw.Code, uw.Body.String())
	}

	// Delete is rejected.
	del := createAuthorizedUser(t, "deleteRole", "gatekeeper/roles/"+roleID)
	dr := withUserID(httptest.NewRequest(http.MethodDelete, "/roles/"+roleID, nil), del.UserID)
	dr.SetPathValue("id", roleID)
	dw := httptest.NewRecorder()
	handleDeleteRole(dw, dr)
	if dw.Code != http.StatusForbidden {
		t.Fatalf("expected 403 deleting system role, got %d: %s", dw.Code, dw.Body.String())
	}
}

func TestUpdatePermissions_SystemRejected(t *testing.T) {
	u := createTestUser(t)
	ctx := context.Background()
	if err := rebuildDefaultRole(ctx, u.UserID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Cleanup(func() { cleanupSignup(t, u.UserID) })
	row, _ := (User{UserID: u.UserID}).Get(ctx)
	rRow, _ := (Role{RoleID: *row.(User).DefaultRoleID}).Get(ctx)
	permID := rRow.(Role).PermissionsIDs[0]

	actor := createAuthorizedUser(t, "updatePermissions", "gatekeeper/permissions/"+permID)
	body, _ := json.Marshal(permissionsRequest{Service: "gatekeeper", Actions: []string{"getUser"}, Resources: []string{"x"}})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/permissions/"+permID, bytes.NewReader(body)), actor.UserID)
	r.SetPathValue("id", permID)
	w := httptest.NewRecorder()
	handleUpdatePermissions(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 updating system permission, got %d: %s", w.Code, w.Body.String())
	}
}
