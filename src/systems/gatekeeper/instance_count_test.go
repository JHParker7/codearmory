package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The platform seeds its own `codearmory` account on every startup, before any human
// can sign up. instanceUserCount used to count it, and that single row broke three
// things at once — so these are regression tests for a bug with three faces, not three
// tests of one function.
//
// Each test cleans the users table itself: TestMain shares one in-memory database across
// the package, and "is the instance empty" is meaningless if another test left rows.

func clearUsers(t *testing.T) {
	t.Helper()
	if err := connect().Exec("DELETE FROM users").Error; err != nil {
		t.Fatalf("clear users: %v", err)
	}
}

func TestInstanceUserCount_IgnoresThePlatformAccount(t *testing.T) {
	clearUsers(t)
	defer clearUsers(t)
	ctx := context.Background()

	// A brand-new instance: nothing but the row the platform seeds for itself.
	seedPlatformAccount(ctx)

	n, err := instanceUserCount(connect())
	if err != nil {
		t.Fatalf("instanceUserCount: %v", err)
	}
	if n != 0 {
		t.Errorf("instanceUserCount = %d on an instance with no real users, want 0 "+
			"— the platform account is being counted, which is what stopped the first "+
			"signup from ever becoming admin", n)
	}
}

func TestInstanceUserCount_CountsRealUsers(t *testing.T) {
	clearUsers(t)
	defer clearUsers(t)
	ctx := context.Background()
	seedPlatformAccount(ctx)

	u := User{UserID: uuid.New().String(), Username: "alice",
		Email: "alice@example.com", HashedPassword: "x", Active: true}
	if err := u.Add(ctx); err != nil {
		t.Fatalf("add user: %v", err)
	}

	n, err := instanceUserCount(connect())
	if err != nil {
		t.Fatalf("instanceUserCount: %v", err)
	}
	if n != 1 {
		t.Errorf("instanceUserCount = %d with one real user, want 1", n)
	}
}

// The property the original comment is most emphatic about: deactivating everyone must
// not reset the instance to "uninitialized" and re-open the admin grant. Excluding the
// platform account must not weaken that.
func TestInstanceUserCount_StillCountsInactiveUsers(t *testing.T) {
	clearUsers(t)
	defer clearUsers(t)
	ctx := context.Background()
	seedPlatformAccount(ctx)

	u := User{UserID: uuid.New().String(), Username: "bob",
		Email: "bob@example.com", HashedPassword: "x", Active: true}
	if err := u.Add(ctx); err != nil {
		t.Fatalf("add user: %v", err)
	}
	if err := connect().Model(&User{}).Where("user_id = ?", u.UserID).
		Update("active", false).Error; err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	n, err := instanceUserCount(connect())
	if err != nil {
		t.Fatalf("instanceUserCount: %v", err)
	}
	if n != 1 {
		t.Errorf("instanceUserCount = %d after deactivating the only user, want 1 — "+
			"deactivation must never reopen the first-user admin grant", n)
	}
}

// Face 1 of the bug: the bootstrap-admin determination. With the platform account
// counted, grantedAdmin = (count == 0) was false for the very first human, and an
// instance with no env-var admin configured ended up with no administrator at all.
func TestBootstrapAdmin_FirstRealSignupBecomesAdmin(t *testing.T) {
	clearUsers(t)
	defer clearUsers(t)
	ctx := context.Background()
	seedPlatformAccount(ctx)

	u := &User{UserID: uuid.New().String(), Username: "firstie",
		Email: "firstie@example.com", HashedPassword: "x"}
	role := &Role{RoleID: uuid.New().String(), OwnerID: u.UserID}

	granted, err := createUserWithBootstrapAdmin(ctx, u, role, true, true)
	if err != nil {
		t.Fatalf("createUserWithBootstrapAdmin: %v", err)
	}
	if !granted {
		t.Fatal("the first real signup was NOT granted admin — the instance would have " +
			"no administrator, since the only wildcard holder cannot log in")
	}
	if role.Name != adminRoleName {
		t.Errorf("personal role name = %q, want %q", role.Name, adminRoleName)
	}

	// And the second signup must NOT be admin.
	u2 := &User{UserID: uuid.New().String(), Username: "secondie",
		Email: "secondie@example.com", HashedPassword: "x"}
	role2 := &Role{RoleID: uuid.New().String(), OwnerID: u2.UserID}
	granted2, err := createUserWithBootstrapAdmin(ctx, u2, role2, true, true)
	if err != nil {
		t.Fatalf("createUserWithBootstrapAdmin (second): %v", err)
	}
	if granted2 {
		t.Error("a second signup was also granted admin")
	}
}

// Face 2: /setup/status. A brand-new instance reported initialized=true because of the
// seeded row, so the setup flow was skipped before anyone had an account.
func TestSetupStatus_UninitialisedUntilARealUserExists(t *testing.T) {
	clearUsers(t)
	defer clearUsers(t)
	ctx := context.Background()
	seedPlatformAccount(ctx)

	n, err := instanceUserCount(connect().WithContext(ctx))
	if err != nil {
		t.Fatalf("instanceUserCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("instance reports %d users before anyone signed up — /setup/status "+
			"would say initialized and skip setup", n)
	}

	u := User{UserID: uuid.New().String(), Username: "carol",
		Email: "carol@example.com", HashedPassword: "x", Active: true}
	if err := u.Add(ctx); err != nil {
		t.Fatalf("add user: %v", err)
	}
	if n, _ = instanceUserCount(connect().WithContext(ctx)); n != 1 {
		t.Errorf("instance reports %d users after a real signup, want 1", n)
	}
}

// The exclusion is by ID, not by username, so a row that somehow held a reserved name
// could never hide from the count and re-open the admin grant. User.Add rejects such
// names, so this writes the row directly — which is precisely the case the id-based
// check is defending against.
func TestInstanceUserCount_ExcludesByIDNotUsername(t *testing.T) {
	clearUsers(t)
	defer clearUsers(t)
	ctx := context.Background()

	// No platform row here: usernames are UNIQUE, so the impostor can only hold the
	// reserved name if it is the sole bearer of it. That is the stronger version of the
	// case anyway — a lone row wearing the platform's name, with an id of its own.
	impostor := User{UserID: uuid.New().String(), Username: platformUsername,
		Email: "impostor@example.com", HashedPassword: "x", Active: true}
	if err := connect().WithContext(ctx).Create(&impostor).Error; err != nil {
		t.Fatalf("create impostor row: %v", err)
	}

	n, err := instanceUserCount(connect())
	if err != nil {
		t.Fatalf("instanceUserCount: %v", err)
	}
	if n != 1 {
		t.Errorf("instanceUserCount = %d, want 1 — a row merely NAMED %q was excluded, "+
			"which would let it hide from the bootstrap check and re-open the "+
			"first-user admin grant", n, platformUsername)
	}
}
