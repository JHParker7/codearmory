package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A namespace owner grants access by assigning one of their roles. The user has one
// direct role, one default role and one team — all single pointers — so the grant has
// to be additive, or accepting access to someone else's namespace would cost the
// recipient their own permissions.
func TestRoleMembership_GrantsWithoutReplacingOwnPermissions(t *testing.T) {
	ctx := context.Background()
	owner := createTestUser(t)  // "alice" — owns the namespace
	member := createTestUser(t) // "bob"  — being granted access

	// bob's OWN permission, on his own namespace, via his direct role.
	ownPerm := Permissions{
		PermissionsID: uuid.New().String(), Name: "bob-own", Service: "git",
		Actions: []string{"readRepo"}, Resources: []string{member.Username + "/git/repos/*"},
		OwnerID: member.UserID, Active: true,
	}
	if err := ownPerm.Add(ctx); err != nil {
		t.Fatalf("add own permission: %v", err)
	}
	t.Cleanup(func() { ownPerm.Remove(ctx) })
	ownRole := Role{RoleID: uuid.New().String(), Name: "bob-role", PermissionsIDs: []string{ownPerm.PermissionsID}, OwnerID: member.UserID, Active: true}
	if err := ownRole.Add(ctx); err != nil {
		t.Fatalf("add own role: %v", err)
	}
	t.Cleanup(func() { ownRole.Remove(ctx) })
	// Reload before mutating — good practice generally, and the shape every caller
	// should follow even though Update no longer punishes a partial struct.
	row, err := (User{UserID: member.UserID}).Get(ctx)
	if err != nil {
		t.Fatalf("reload member: %v", err)
	}
	member = row.(User)
	member.RoleID = &ownRole.RoleID
	if err := member.Update(ctx); err != nil {
		t.Fatalf("assign own role: %v", err)
	}

	// alice's namespace role, granting read on ONE of her repos.
	shared := "alice-repo-id"
	sharedPerm := Permissions{
		PermissionsID: uuid.New().String(), Name: "alice-share", Service: "git",
		Actions: []string{"readRepo"}, Resources: []string{owner.Username + "/git/repos/" + shared},
		OwnerID: owner.UserID, Active: true,
	}
	if err := sharedPerm.Add(ctx); err != nil {
		t.Fatalf("add shared permission: %v", err)
	}
	t.Cleanup(func() { sharedPerm.Remove(ctx) })
	nsRole := Role{RoleID: uuid.New().String(), Name: "alice-readers", PermissionsIDs: []string{sharedPerm.PermissionsID}, OwnerID: owner.UserID, Active: true}
	if err := nsRole.Add(ctx); err != nil {
		t.Fatalf("add namespace role: %v", err)
	}
	t.Cleanup(func() { nsRole.Remove(ctx) })

	ownResource := member.Username + "/git/repos/anything"
	sharedResource := owner.Username + "/git/repos/" + shared

	// Before the grant: bob has his own access, and none of alice's.
	if ok, _, err := evaluatePermissions(ctx, member.UserID, "git", "readRepo", ownResource); err != nil || !ok {
		t.Fatalf("bob should hold his own permission (ok=%v err=%v)", ok, err)
	}
	if ok, _, _ := evaluatePermissions(ctx, member.UserID, "git", "readRepo", sharedResource); ok {
		t.Fatal("bob must not reach alice's repo before being granted")
	}

	// The grant.
	m := RoleMembership{RoleID: nsRole.RoleID, UserID: member.UserID, GrantedBy: owner.UserID}
	if err := connect().WithContext(ctx).Create(&m).Error; err != nil {
		t.Fatalf("assign namespace role: %v", err)
	}
	t.Cleanup(func() { connect().WithContext(ctx).Delete(&m) })

	// After: alice's repo is reachable AND bob keeps his own permissions.
	if ok, _, err := evaluatePermissions(ctx, member.UserID, "git", "readRepo", sharedResource); err != nil || !ok {
		t.Errorf("assigned role did not grant access to %s (ok=%v err=%v)", sharedResource, ok, err)
	}
	if ok, _, err := evaluatePermissions(ctx, member.UserID, "git", "readRepo", ownResource); err != nil || !ok {
		t.Errorf("assigning a namespace role cost bob his own permissions (ok=%v err=%v)", ok, err)
	}
	// The grant is scoped: it must not spill onto alice's OTHER repos.
	if ok, _, _ := evaluatePermissions(ctx, member.UserID, "git", "readRepo", owner.Username+"/git/repos/other"); ok {
		t.Error("the grant leaked to another repo in alice's namespace")
	}

	// Revoking the membership removes the access again.
	if err := connect().WithContext(ctx).Where("role_id = ? AND user_id = ?", nsRole.RoleID, member.UserID).Delete(&RoleMembership{}).Error; err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok, _, _ := evaluatePermissions(ctx, member.UserID, "git", "readRepo", sharedResource); ok {
		t.Error("access survived revocation")
	}
}

// Update used to Save() the whole struct, so a caller who built a User literal instead
// of Get()ing one wrote active=false and the account vanished from every lookup — the
// row survived, but Get filters on active, so the user simply ceased to exist.
func TestUserUpdate_DoesNotDeactivateOrBlankIdentity(t *testing.T) {
	ctx := context.Background()
	u := createTestUser(t)
	roleID := uuid.New().String()

	// A partial struct, exactly as an unwary caller would build it.
	partial := User{UserID: u.UserID, Username: u.Username, Email: u.Email, RoleID: &roleID}
	if err := partial.Update(ctx); err != nil {
		t.Fatalf("update: %v", err)
	}

	row, err := (User{UserID: u.UserID}).Get(ctx)
	if err != nil {
		t.Fatalf("user disappeared after a partial update: %v", err)
	}
	got := row.(User)
	if !got.Active {
		t.Error("update deactivated the user")
	}
	if got.RoleID == nil || *got.RoleID != roleID {
		t.Errorf("role_id = %v, want %s", got.RoleID, roleID)
	}
	if got.Username != u.Username || got.Email != u.Email {
		t.Errorf("identity changed: username=%q email=%q", got.Username, got.Email)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at was zeroed")
	}

	// Clearing a pointer must still work — that is why the columns are named rather
	// than relying on GORM's skip-zero-values behaviour.
	got.RoleID = nil
	if err := got.Update(ctx); err != nil {
		t.Fatalf("clear role: %v", err)
	}
	row, _ = (User{UserID: u.UserID}).Get(ctx)
	if r := row.(User).RoleID; r != nil {
		t.Errorf("role_id = %v, want nil after clearing", *r)
	}

	// And a struct that would blank identity is refused rather than silently applied.
	if err := (User{UserID: u.UserID}).Update(ctx); err == nil {
		t.Error("update with empty username/email was accepted")
	}
}

// Namespace-role creation is safe only if BOTH guards hold. These test them directly,
// since between them they are the difference between "users can share their own work"
// and "any user can make themselves an admin".
func TestNamespaceRole_ConfinementAndAttenuation(t *testing.T) {
	const (
		me    = "alice"
		myOrg = "org/acme"
	)

	// Confinement: only resources inside a namespace the caller owns.
	confined := []struct {
		resource string
		want     bool
	}{
		{"alice/codearmory_git_factory/repos/x", true},
		{"org/acme/codearmory_git_factory/repos/x", true},
		{"bob/codearmory_git_factory/repos/x", false},       // someone else's namespace
		{"org/other/codearmory_git_factory/repos/x", false}, // someone else's org
		{"codearmory_git_factory/repos/x", false},           // unscoped — no owner named
		{"alicia/codearmory_git_factory/repos/x", false},    // prefix, not a segment
		{"*/*/*", false}, // the escalation attempt
	}
	for _, c := range confined {
		if got := inOwnNamespace(c.resource, me, myOrg); got != c.want {
			t.Errorf("inOwnNamespace(%q) = %v, want %v", c.resource, got, c.want)
		}
	}

	// A user with no org owns only their own name.
	if inOwnNamespace("org/acme/x/y", me, "") {
		t.Error("a user with no org must not own an org namespace")
	}
}

// Attenuation is enforced by checkPermissions against the caller: granting an action
// you do not hold must fail. Proven end to end through evaluatePermissions rather than
// by inspecting the handler, so the property is tested where it actually lives.
func TestNamespaceRole_CannotGrantWhatYouDoNotHold(t *testing.T) {
	ctx := context.Background()
	owner := createTestUser(t)

	// The owner holds readRepo — and nothing else — on their own namespace.
	perm := Permissions{
		PermissionsID: uuid.New().String(), Name: "own-read", Service: "git",
		Actions: []string{"readRepo"}, Resources: []string{owner.Username + "/git/repos/*"},
		OwnerID: owner.UserID, Active: true,
	}
	if err := perm.Add(ctx); err != nil {
		t.Fatalf("add permission: %v", err)
	}
	t.Cleanup(func() { perm.Remove(ctx) })
	role := Role{RoleID: uuid.New().String(), Name: "own", PermissionsIDs: []string{perm.PermissionsID}, OwnerID: owner.UserID, Active: true}
	if err := role.Add(ctx); err != nil {
		t.Fatalf("add role: %v", err)
	}
	t.Cleanup(func() { role.Remove(ctx) })
	row, err := (User{UserID: owner.UserID}).Get(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	u := row.(User)
	u.RoleID = &role.RoleID
	if err := u.Update(ctx); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	res := owner.Username + "/git/repos/abc"
	// What they hold, they may pass on.
	if ok, _ := checkPermissions(ctx, owner.UserID, "git", "readRepo", res); !ok {
		t.Error("owner should hold readRepo, so sharing it must be allowed")
	}
	// What they do not hold, they may not.
	for _, action := range []string{"writeRepo", "deleteRepo"} {
		if ok, _ := checkPermissions(ctx, owner.UserID, "git", action, res); ok {
			t.Errorf("owner appears to hold %s — attenuation would let them grant it", action)
		}
	}
}
