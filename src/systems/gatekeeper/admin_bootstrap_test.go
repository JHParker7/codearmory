package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// useIsolatedDB swaps in a fresh in-memory database for the duration of the test so
// user-count- and seed-dependent behaviour is deterministic regardless of accounts
// created by other tests. connect()/connectRead() both fall back to gormDB when
// gormDBRead is nil.
func useIsolatedDB(t *testing.T) {
	t.Helper()
	oldDB, oldRead := gormDB, gormDBRead
	t.Cleanup(func() { gormDB, gormDBRead = oldDB, oldRead })
	conn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := conn.AutoMigrate(&User{}, &Org{}, &Team{}, &Role{}, &UserOrgMembership{}, &Session{}, &Permissions{}, &AuditLog{}, &PermissionsCheck{}, &SignupAllowlistEntry{}, &SignupPolicy{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gormDB, gormDBRead = conn, nil
}

func doSignup(t *testing.T, req signupRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	return w
}

func roleName(t *testing.T, roleID string) string {
	t.Helper()
	row, err := (Role{RoleID: roleID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get role %s: %v", roleID, err)
	}
	return row.(Role).Name
}

func TestNewAdminPermission_FullAccess(t *testing.T) {
	p := newAdminPermission("user-123", "alice")
	if p.Service != "*" {
		t.Fatalf("admin permission service = %q, want \"*\"", p.Service)
	}
	if len(p.Actions) != 1 || p.Actions[0] != "*" {
		t.Fatalf("admin permission actions = %v, want [\"*\"]", p.Actions)
	}
	if len(p.Resources) != 1 || p.Resources[0] != "*" {
		t.Fatalf("admin permission resources = %v, want [\"*\"]", p.Resources)
	}
	if p.OwnerID != "user-123" {
		t.Fatalf("admin permission owner = %q, want user-123", p.OwnerID)
	}
	// The grant must match an arbitrary (service, action, resource) triple.
	if !matchPermission(p, "forge", "createExecution", "someone/forge/executions") {
		t.Fatal("wildcard admin permission should match any request")
	}
}

func TestMatchPermission_WildcardService(t *testing.T) {
	wildcard := Permissions{Service: "*", Actions: []string{"*"}, Resources: []string{"*"}}
	if !matchPermission(wildcard, "anything", "doStuff", "any/resource") {
		t.Fatal("Service \"*\" should match any service")
	}
	// A non-wildcard service must still match exactly.
	scoped := Permissions{Service: "forge", Actions: []string{"*"}, Resources: []string{"*"}}
	if matchPermission(scoped, "workflows", "x", "y") {
		t.Fatal("non-wildcard service must not match a different service")
	}
	if !matchPermission(scoped, "forge", "x", "y") {
		t.Fatal("exact service should match")
	}
}

func TestHandleSignup_FirstUserBecomesAdmin(t *testing.T) {
	useIsolatedDB(t)

	// First signup → admin.
	w := doSignup(t, signupRequest{Email: "admin@test.com", Username: "admin-user", Password: "password123"})
	if w.Code != http.StatusCreated {
		t.Fatalf("first signup: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var first signupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first signup: %v", err)
	}

	row, err := (User{UserID: first.UserID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get first user: %v", err)
	}
	u := row.(User)
	if u.RoleID == nil {
		t.Fatal("first user has no personal role")
	}
	if got := roleName(t, *u.RoleID); got != adminRoleName {
		t.Fatalf("personal role name = %q, want %q", got, adminRoleName)
	}

	// End-to-end: the admin can perform any action on any service/resource.
	if ok, err := checkPermissions(context.Background(), first.UserID, "forge", "createExecution", "anyone/forge/executions"); err != nil || !ok {
		t.Fatalf("expected admin full access to forge (ok=%v err=%v)", ok, err)
	}
	if ok, err := checkPermissions(context.Background(), first.UserID, "workflows", "deleteRun", "x"); err != nil || !ok {
		t.Fatalf("expected admin wildcard on workflows (ok=%v err=%v)", ok, err)
	}

	// Second signup → ordinary user: no admin role, no full access.
	w2 := doSignup(t, signupRequest{Email: "member@test.com", Username: "member-user", Password: "password123"})
	if w2.Code != http.StatusCreated {
		t.Fatalf("second signup: expected 201, got %d: %s", w2.Code, w2.Body.String())
	}
	var second signupResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second signup: %v", err)
	}
	row2, err := (User{UserID: second.UserID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get second user: %v", err)
	}
	if got := roleName(t, *row2.(User).RoleID); got == adminRoleName {
		t.Fatal("second user must not receive the admin role")
	}
	if ok, _ := checkPermissions(context.Background(), second.UserID, "forge", "createExecution", "x"); ok {
		t.Fatal("second user must not have full forge access")
	}
}

func TestSeedAdminUser_CreatesAdminAndIsIdempotent(t *testing.T) {
	useIsolatedDB(t)
	t.Setenv("GATEKEEPER_ADMIN_EMAIL", "ops@acme.com")
	t.Setenv("GATEKEEPER_ADMIN_PASSWORD", "supersecret")

	seedAdminUser(context.Background())

	admin, err := getUserByEmail(context.Background(), "ops@acme.com")
	if err != nil {
		t.Fatalf("admin user not created: %v", err)
	}
	if admin.Username != "ops" {
		t.Fatalf("admin username = %q, want ops (derived from email)", admin.Username)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.HashedPassword), []byte("supersecret")); err != nil {
		t.Fatalf("admin password does not verify: %v", err)
	}
	if got := roleName(t, *admin.RoleID); got != adminRoleName {
		t.Fatalf("admin role name = %q, want %q", got, adminRoleName)
	}
	if ok, err := checkPermissions(context.Background(), admin.UserID, "forge", "createExecution", "x/forge/y"); err != nil || !ok {
		t.Fatalf("expected seeded admin to have full access (ok=%v err=%v)", ok, err)
	}

	// Re-running must not create a second admin or duplicate the wildcard permission.
	seedAdminUser(context.Background())
	var userCount int64
	gormDB.Model(&User{}).Where("email = ? AND active = ?", "ops@acme.com", true).Count(&userCount)
	if userCount != 1 {
		t.Fatalf("expected exactly 1 admin user after re-seed, got %d", userCount)
	}
	rRow, err := (Role{RoleID: *admin.RoleID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get admin role: %v", err)
	}
	wildcards := 0
	for _, pid := range rRow.(Role).PermissionsIDs {
		if pr, err := (Permissions{PermissionsID: pid}).Get(context.Background()); err == nil && pr.(Permissions).Service == "*" {
			wildcards++
		}
	}
	if wildcards != 1 {
		t.Fatalf("expected exactly 1 wildcard admin permission after re-seed, got %d", wildcards)
	}
}

func TestSeedAdminUser_DisabledWhenUnset(t *testing.T) {
	useIsolatedDB(t)
	t.Setenv("GATEKEEPER_ADMIN_EMAIL", "")
	t.Setenv("GATEKEEPER_ADMIN_PASSWORD", "")

	seedAdminUser(context.Background())

	var count int64
	gormDB.Model(&User{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected no users when admin env unset, got %d", count)
	}
}

func TestSeedAdminUser_GrantsExistingUserAdmin(t *testing.T) {
	useIsolatedDB(t)

	// Pre-existing ordinary user with a personal role and no admin permission.
	roleID := uuid.New().String()
	if err := (Role{RoleID: roleID, OwnerID: "u1", PermissionsIDs: []string{}}).Add(context.Background()); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	u := User{UserID: "u1", Email: "boss@acme.com", Username: "boss", HashedPassword: "x", RoleID: &roleID}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if ok, _ := checkPermissions(context.Background(), "u1", "forge", "createExecution", "x"); ok {
		t.Fatal("user should not have admin before seeding")
	}

	t.Setenv("GATEKEEPER_ADMIN_EMAIL", "boss@acme.com")
	t.Setenv("GATEKEEPER_ADMIN_PASSWORD", "supersecret")
	seedAdminUser(context.Background())

	if ok, err := checkPermissions(context.Background(), "u1", "forge", "createExecution", "x"); err != nil || !ok {
		t.Fatalf("expected existing user to gain admin (ok=%v err=%v)", ok, err)
	}
}

func TestAdminSeedConfigured_DisablesFirstUserAdmin(t *testing.T) {
	useIsolatedDB(t)
	t.Setenv("GATEKEEPER_ADMIN_EMAIL", "ops@acme.com")
	t.Setenv("GATEKEEPER_ADMIN_PASSWORD", "supersecret")

	// With the env-var admin configured, the first signup is an ordinary user.
	w := doSignup(t, signupRequest{Email: "first@acme.com", Username: "first-user", Password: "password123"})
	if w.Code != http.StatusCreated {
		t.Fatalf("signup: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp signupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode signup: %v", err)
	}

	row, err := (User{UserID: resp.UserID}).Get(context.Background())
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got := roleName(t, *row.(User).RoleID); got == adminRoleName {
		t.Fatal("first user must NOT be admin when GATEKEEPER_ADMIN_* is set")
	}
	if ok, _ := checkPermissions(context.Background(), resp.UserID, "forge", "createExecution", "x"); ok {
		t.Fatal("first user must not have full access when env-var admin is configured")
	}
}
