package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMain(m *testing.M) {
	conn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		log.Fatal(err)
	}
	conn.AutoMigrate(&User{}, &Org{}, &Team{}, &Role{}, &Session{}, &Permissions{}, &Invite{}, &ServiceAccount{}, &ServicePermissionRequest{}, &AuditLog{}, &PermissionsCheck{}, &Secret{}, &OrgSecretProvider{}, &OAuthClient{}, &OAuthCode{}, &TOTPCredential{}, &MFAPending{})
	gormDB = conn
	initMetrics()
	os.Setenv("PERMITTED_SERVICES", "gatekeeper,blueprints,forge,svc,my-service,other-service,team-service,direct-service,test-service,workflows")
	initPermittedServices()

	// Initialise AES-256-GCM with a fixed test key so secret handler tests can encrypt/decrypt.
	os.Setenv("GATEKEEPER_SECRETS_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	initSecretsEncryption()

	// Seed gatekeeper's own default grants so org/team/user creation tests can grant owner permissions.
	// Resources use {username}/ prefix so the scoping in checkPermissions (which prepends
	// "<username>/" to unscoped resources) matches the stored permission strings.
	defaultGrantsMu.Lock()
	cachedGrants = []DefaultGrant{
		{ServiceName: "gatekeeper", GrantOn: "user",
			Actions:   []string{"getUser", "updateUser", "deleteUser", "createOrg", "createTeam"},
			Resources: []string{"{username}/gatekeeper/users/{user_id}", "{username}/gatekeeper/orgs", "{username}/gatekeeper/teams"}},
		{ServiceName: "gatekeeper", GrantOn: "org",
			Actions:   []string{"getOrg", "updateOrg", "deleteOrg", "inviteUser"},
			Resources: []string{"{username}/gatekeeper/orgs/{org_id}"}},
		{ServiceName: "gatekeeper", GrantOn: "team",
			Actions:   []string{"getTeam", "updateTeam", "deleteTeam", "inviteUser"},
			Resources: []string{"{username}/gatekeeper/teams/{team_id}"}},
	}
	defaultGrantsMu.Unlock()

	os.Exit(m.Run())
}

func strPtr(s string) *string { return &s }

func createTestUser(t *testing.T) User {
	t.Helper()
	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       uuid.New().String(),
		HashedPassword: "sffssfasaf",
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("createTestUser: %v", err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })
	return u
}

func TestPermissionsCreate(t *testing.T) {
	testP := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "test-service",
		Actions:       []string{"read", "write"},
		Resources:     []string{"resource-a"},
		Active:        true,
	}
	t.Cleanup(func() { testP.Remove(context.Background()) })
	if err := testP.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testP.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newP := row.(Permissions)
	testP.UpdatedAt = row.(Permissions).UpdatedAt
	testP.CreatedAt = row.(Permissions).CreatedAt
	if !reflect.DeepEqual(newP, testP) {
		t.Fatalf("got permissions %v, want %v", newP, testP)
	}
}

func TestPermissionsDelete(t *testing.T) {
	testP := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "test-service",
		Actions:       []string{"read"},
		Resources:     []string{"resource-a"},
	}
	t.Cleanup(func() { testP.Remove(context.Background()) })
	if err := testP.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := testP.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testP.Get(context.Background()); err == nil {
		t.Fatal("expected Get() to fail after Remove()")
	}
}

func TestPermissionsList(t *testing.T) {
	svc := uuid.New().String()
	p1 := Permissions{PermissionsID: uuid.New().String(), Service: svc, Actions: []string{"read"}, Resources: []string{"r-1"}}
	p2 := Permissions{PermissionsID: uuid.New().String(), Service: svc, Actions: []string{"write"}, Resources: []string{"r-2"}}
	t.Cleanup(func() { p1.Remove(context.Background()); p2.Remove(context.Background()) })
	if err := p1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (Permissions{Service: svc}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 permissions, got %d", len(rows))
	}

	if err := p1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (Permissions{Service: svc}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 permissions after remove, got %d", len(rows))
	}
	if rows[0].(Permissions).PermissionsID != p2.PermissionsID {
		t.Fatalf("expected permissions %s, got %s", p2.PermissionsID, rows[0].(Permissions).PermissionsID)
	}
}

func TestPermissionsUpdate(t *testing.T) {
	testP := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "test-service",
		Actions:       []string{"read"},
		Resources:     []string{"resource-a"},
		Active:        true,
	}
	t.Cleanup(func() { testP.Remove(context.Background()) })
	if err := testP.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testP.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	testP.UpdatedAt = row.(Permissions).UpdatedAt
	testP.CreatedAt = row.(Permissions).CreatedAt
	if !reflect.DeepEqual(row.(Permissions), testP) {
		t.Fatalf("got permissions %v, want %v", row, testP)
	}
	testP.Actions = []string{"read", "write", "delete"}
	if err := testP.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = testP.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Time.Equal(testP.UpdatedAt, row.(Permissions).UpdatedAt) {
		t.Fatalf("updated permissions updated at matches, %v, should not match %v", row.(Permissions).UpdatedAt, testP.UpdatedAt)
	}
	testP.UpdatedAt = row.(Permissions).UpdatedAt
	if !reflect.DeepEqual(row.(Permissions), testP) {
		t.Fatalf("updated permissions %v, want %v", row, testP)
	}
}

func TestOrgCreate(t *testing.T) {
	testOrg := Org{OrgID: uuid.New().String(), OrgName: "test-org", Active: true}
	t.Cleanup(func() { testOrg.Remove(context.Background()) })
	if err := testOrg.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testOrg.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newOrg := row.(Org)
	testOrg.CreatedAt = newOrg.CreatedAt
	testOrg.UpdatedAt = newOrg.UpdatedAt
	if newOrg != testOrg {
		t.Fatalf("got org %v, want %v", newOrg, testOrg)
	}
}

func TestOrgDelete(t *testing.T) {
	testOrg := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	t.Cleanup(func() { testOrg.Remove(context.Background()) })
	if err := testOrg.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := testOrg.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testOrg.Get(context.Background()); err == nil {
		t.Fatal("expected Get() to fail after Remove()")
	}
}

func TestOrgList(t *testing.T) {
	o1 := Org{OrgID: uuid.New().String(), OrgName: "list-org-1"}
	o2 := Org{OrgID: uuid.New().String(), OrgName: "list-org-2"}
	t.Cleanup(func() { o1.Remove(context.Background()); o2.Remove(context.Background()) })
	if err := o1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := o2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (Org{OrgID: o1.OrgID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 org, got %d", len(rows))
	}
	if rows[0].(Org).OrgID != o1.OrgID {
		t.Fatalf("expected org %s, got %s", o1.OrgID, rows[0].(Org).OrgID)
	}

	if err := o1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (Org{OrgID: o1.OrgID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 orgs after remove, got %d", len(rows))
	}
}

func TestOrgUpdate(t *testing.T) {
	testOrg := Org{OrgID: uuid.New().String(), OrgName: "test-org", Active: true}
	t.Cleanup(func() { testOrg.Remove(context.Background()) })
	if err := testOrg.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testOrg.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newOrg := row.(Org)
	testOrg.CreatedAt = newOrg.CreatedAt
	testOrg.UpdatedAt = newOrg.UpdatedAt
	if newOrg != testOrg {
		t.Fatalf("got org %v, want %v", newOrg, testOrg)
	}
	testOrg.OrgName = "updated-org"
	if err := testOrg.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = testOrg.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newOrg = row.(Org)
	if time.Time.Equal(testOrg.UpdatedAt, newOrg.UpdatedAt) {
		slog.Info("does match", "testOrg", testOrg.UpdatedAt, "newOrg", newOrg.UpdatedAt)
		t.Fatal("updated at not changed")
	}
	newOrg.UpdatedAt = testOrg.UpdatedAt
	if newOrg != testOrg {
		t.Fatalf("updated org %v, want %v", newOrg, testOrg)
	}
}

func TestRoleCreate(t *testing.T) {
	testRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-1"}, OrgID: strPtr("o-1"), Active: true}
	t.Cleanup(func() { testRole.Remove(context.Background()) })
	if err := testRole.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testRole.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newRole := row.(Role)
	testRole.CreatedAt = newRole.CreatedAt
	testRole.UpdatedAt = newRole.UpdatedAt
	if !reflect.DeepEqual(newRole, testRole) {
		t.Fatalf("got role %v, want %v", newRole, testRole)
	}
}

func TestRoleDelete(t *testing.T) {
	testRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-1"}, OrgID: strPtr("o-1")}
	t.Cleanup(func() { testRole.Remove(context.Background()) })
	if err := testRole.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := testRole.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testRole.Get(context.Background()); err == nil {
		t.Fatal("expected Get() to fail after Remove()")
	}
}

func TestRoleList(t *testing.T) {
	orgID := uuid.New().String()
	r1 := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-1"}, OrgID: &orgID}
	r2 := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-2"}, OrgID: &orgID}
	t.Cleanup(func() { r1.Remove(context.Background()); r2.Remove(context.Background()) })
	if err := r1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (Role{OrgID: &orgID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(rows))
	}

	if err := r1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (Role{OrgID: &orgID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 role after remove, got %d", len(rows))
	}
	if rows[0].(Role).RoleID != r2.RoleID {
		t.Fatalf("expected role %s, got %s", r2.RoleID, rows[0].(Role).RoleID)
	}
}

func TestRoleUpdate(t *testing.T) {
	testRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{"p-1"}, OrgID: strPtr("o-1"), Active: true}
	t.Cleanup(func() { testRole.Remove(context.Background()) })
	if err := testRole.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testRole.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newRole := row.(Role)
	testRole.CreatedAt = newRole.CreatedAt
	testRole.UpdatedAt = newRole.UpdatedAt
	if !reflect.DeepEqual(newRole, testRole) {
		t.Fatalf("got role %v, want %v", newRole, testRole)
	}
	testRole.OrgID = strPtr("o-2")
	if err := testRole.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = testRole.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newRole = row.(Role)
	if time.Time.Equal(testRole.UpdatedAt, newRole.UpdatedAt) {
		slog.Info("does match", "testRole", testRole.UpdatedAt, "newRole", newRole.UpdatedAt)
		t.Fatal("updated at not changed")
	}
	newRole.UpdatedAt = testRole.UpdatedAt
	if !reflect.DeepEqual(newRole, testRole) {
		t.Fatalf("updated role %v, want %v", newRole, testRole)
	}
}

func TestTeamCreate(t *testing.T) {
	testTeam := Team{TeamID: uuid.New().String(), TeamName: "test-team", RoleID: strPtr("r-1"), Active: true}
	t.Cleanup(func() { testTeam.Remove(context.Background()) })
	if err := testTeam.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testTeam.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newTeam := row.(Team)
	testTeam.CreatedAt = newTeam.CreatedAt
	testTeam.UpdatedAt = newTeam.UpdatedAt
	if !reflect.DeepEqual(newTeam, testTeam) {
		t.Fatalf("got team %v, want %v", newTeam, testTeam)
	}
}

func TestTeamDelete(t *testing.T) {
	testTeam := Team{TeamID: uuid.New().String(), TeamName: "test-team", RoleID: strPtr("r-1")}
	t.Cleanup(func() { testTeam.Remove(context.Background()) })
	if err := testTeam.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := testTeam.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testTeam.Get(context.Background()); err == nil {
		t.Fatal("expected Get() to fail after Remove()")
	}
}

func TestTeamList(t *testing.T) {
	roleID := uuid.New().String()
	tm1 := Team{TeamID: uuid.New().String(), TeamName: "list-team-1", RoleID: &roleID}
	tm2 := Team{TeamID: uuid.New().String(), TeamName: "list-team-2", RoleID: &roleID}
	t.Cleanup(func() { tm1.Remove(context.Background()); tm2.Remove(context.Background()) })
	if err := tm1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tm2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (Team{RoleID: &roleID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 teams, got %d", len(rows))
	}

	if err := tm1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (Team{RoleID: &roleID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 team after remove, got %d", len(rows))
	}
	if rows[0].(Team).TeamID != tm2.TeamID {
		t.Fatalf("expected team %s, got %s", tm2.TeamID, rows[0].(Team).TeamID)
	}
}

func TestTeamUpdate(t *testing.T) {
	testTeam := Team{TeamID: uuid.New().String(), TeamName: "test-team", RoleID: strPtr("r-1"), Active: true}
	t.Cleanup(func() { testTeam.Remove(context.Background()) })
	if err := testTeam.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testTeam.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newTeam := row.(Team)
	testTeam.CreatedAt = newTeam.CreatedAt
	testTeam.UpdatedAt = newTeam.UpdatedAt
	if !reflect.DeepEqual(newTeam, testTeam) {
		t.Fatalf("got team %v, want %v", newTeam, testTeam)
	}
	testTeam.TeamName = "updated-team"
	if err := testTeam.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = testTeam.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newTeam = row.(Team)
	if time.Time.Equal(testTeam.UpdatedAt, newTeam.UpdatedAt) {
		slog.Info("does match", "testTeam", testTeam.UpdatedAt, "newTeam", newTeam.UpdatedAt)
		t.Fatal("updated at not changed")
	}
	newTeam.UpdatedAt = testTeam.UpdatedAt
	if !reflect.DeepEqual(newTeam, testTeam) {
		t.Fatalf("updated team %v, want %v", newTeam, testTeam)
	}
}

func TestSessionCreate(t *testing.T) {
	testUser := createTestUser(t)
	testSession := Session{
		SessionID: uuid.New().String(),
		UserID:    testUser.UserID,
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second),
		PubKey:    "test-pub-key",
		Active:    true,
	}
	t.Cleanup(func() { testSession.Remove(context.Background()) })
	if err := testSession.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testSession.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newSession := row.(Session)
	testSession.CreatedAt = newSession.CreatedAt
	testSession.UpdatedAt = newSession.UpdatedAt
	if newSession != testSession {
		t.Fatalf("got session %v, want %v", newSession, testSession)
	}
}

func TestSessionDelete(t *testing.T) {
	testUser := createTestUser(t)
	testSession := Session{
		SessionID: uuid.New().String(),
		UserID:    testUser.UserID,
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second),
		PubKey:    "test-pub-key",
	}
	t.Cleanup(func() { testSession.Remove(context.Background()) })
	if err := testSession.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := testSession.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testSession.Get(context.Background()); err == nil {
		t.Fatal("expected Get() to fail after Remove()")
	}
}

func TestSessionList(t *testing.T) {
	testUser := createTestUser(t)
	s1 := Session{
		SessionID: uuid.New().String(), UserID: testUser.UserID,
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second), PubKey: "key-1",
	}
	s2 := Session{
		SessionID: uuid.New().String(), UserID: testUser.UserID,
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second), PubKey: "key-2",
	}
	t.Cleanup(func() { s1.Remove(context.Background()); s2.Remove(context.Background()) })
	if err := s1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (Session{UserID: testUser.UserID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(rows))
	}

	if err := s1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (Session{UserID: testUser.UserID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session after remove, got %d", len(rows))
	}
	if rows[0].(Session).SessionID != s2.SessionID {
		t.Fatalf("expected session %s, got %s", s2.SessionID, rows[0].(Session).SessionID)
	}
}

func TestSessionUpdate(t *testing.T) {
	testUser := createTestUser(t)
	testSession := Session{
		SessionID: uuid.New().String(),
		UserID:    testUser.UserID,
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second),
		PubKey:    "test-pub-key",
		Active:    true,
	}
	t.Cleanup(func() { testSession.Remove(context.Background()) })
	if err := testSession.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testSession.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newSession := row.(Session)
	testSession.CreatedAt = newSession.CreatedAt
	testSession.UpdatedAt = newSession.UpdatedAt
	if newSession != testSession {
		t.Fatalf("got session %v, want %v", newSession, testSession)
	}
	testSession.PubKey = "updated-pub-key"
	if err := testSession.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = testSession.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newSession = row.(Session)
	if time.Time.Equal(testSession.UpdatedAt, newSession.UpdatedAt) {
		slog.Info("does match", "testSession", testSession.UpdatedAt, "newSession", newSession.UpdatedAt)
		t.Fatal("updated at not changed")
	}
	newSession.UpdatedAt = testSession.UpdatedAt
	if newSession != testSession {
		t.Fatalf("updated session %v, want %v", newSession, testSession)
	}
}

func TestUserCreate(t *testing.T) {
	testUser := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "create-" + uuid.New().String(),
		Firstname:      "john",
		Lastname:       "doe",
		HashedPassword: "sffssfasaf",
		Active:         true,
	}
	t.Cleanup(func() { testUser.Remove(context.Background()) })
	if err := testUser.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testUser.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newUser := row.(User)
	testUser.CreatedAt = newUser.CreatedAt
	testUser.UpdatedAt = newUser.UpdatedAt
	testUser.RoleID = newUser.RoleID
	testUser.OrgID = newUser.OrgID
	testUser.TeamID = newUser.TeamID
	if newUser != testUser {
		t.Fatalf("got user %v, want %v", newUser, testUser)
	}
}

func TestUserGetUUID(t *testing.T) {
	uuidValue := uuid.New().String()
	testUser := User{
		UserID:         uuidValue,
		Email:          uuid.New().String() + "@test.com",
		Username:       "getuuid-" + uuid.New().String(),
		Firstname:      "john",
		Lastname:       "doe",
		HashedPassword: "sffssfasaf",
		Active:         true,
	}
	t.Cleanup(func() { testUser.Remove(context.Background()) })
	if err := testUser.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := (User{UserID: uuidValue}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newUser := row.(User)
	testUser.CreatedAt = newUser.CreatedAt
	testUser.UpdatedAt = newUser.UpdatedAt
	testUser.RoleID = newUser.RoleID
	testUser.OrgID = newUser.OrgID
	testUser.TeamID = newUser.TeamID
	if newUser != testUser {
		t.Fatalf("got user %v, want %v", newUser, testUser)
	}
}

func TestUserGetUsername(t *testing.T) {
	testUser := User{
		UserID:         uuid.New().String(),
		Email:          "getusername@test.com",
		Username:       "testuser-" + uuid.New().String(),
		Firstname:      "john",
		Lastname:       "doe",
		HashedPassword: "sffssfasaf",
		Active:         true,
	}
	t.Cleanup(func() { testUser.Remove(context.Background()) })
	if err := testUser.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := (User{Username: testUser.Username}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newUser := row.(User)
	testUser.CreatedAt = newUser.CreatedAt
	testUser.UpdatedAt = newUser.UpdatedAt
	testUser.RoleID = newUser.RoleID
	testUser.OrgID = newUser.OrgID
	testUser.TeamID = newUser.TeamID
	if newUser != testUser {
		t.Fatalf("got user %v, want %v", newUser, testUser)
	}
}

func TestUserDelete(t *testing.T) {
	testUser := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "delete-" + uuid.New().String(),
		Firstname:      "john",
		Lastname:       "doe",
		HashedPassword: "sffssfasaf",
		Active:         true,
	}
	t.Cleanup(func() { testUser.Remove(context.Background()) })
	if err := testUser.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := testUser.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testUser.Get(context.Background()); err == nil {
		t.Fatal("expected Get() to fail after Remove()")
	}
}

func TestUserList(t *testing.T) {
	orgID := uuid.New().String()
	u1 := User{
		UserID: uuid.New().String(), Email: uuid.New().String() + "@test.com",
		Username: uuid.New().String(), HashedPassword: "hash", OrgID: &orgID,
	}
	u2 := User{
		UserID: uuid.New().String(), Email: uuid.New().String() + "@test.com",
		Username: uuid.New().String(), HashedPassword: "hash", OrgID: &orgID,
	}
	t.Cleanup(func() { u1.Remove(context.Background()); u2.Remove(context.Background()) })
	if err := u1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (User{OrgID: &orgID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 users, got %d", len(rows))
	}

	if err := u1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (User{OrgID: &orgID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 user after remove, got %d", len(rows))
	}
	if rows[0].(User).UserID != u2.UserID {
		t.Fatalf("expected user %s, got %s", u2.UserID, rows[0].(User).UserID)
	}
}

func TestUserUpdate(t *testing.T) {
	testUser := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Firstname:      "john",
		Lastname:       "doe",
		HashedPassword: "sffssfasaf",
		Username:       "update-" + uuid.New().String(),
		Active:         true,
	}
	t.Cleanup(func() { testUser.Remove(context.Background()) })
	if err := testUser.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := testUser.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newUser := row.(User)
	testUser.CreatedAt = newUser.CreatedAt
	testUser.UpdatedAt = newUser.UpdatedAt
	testUser.RoleID = newUser.RoleID
	testUser.OrgID = newUser.OrgID
	testUser.TeamID = newUser.TeamID
	if newUser != testUser {
		t.Fatalf("got user %v, want %v", newUser, testUser)
	}
	testUser.Firstname = "jane"
	if err := testUser.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = testUser.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newUser = row.(User)
	if time.Time.Equal(testUser.UpdatedAt, newUser.UpdatedAt) {
		slog.Info("does match", "testUser", testUser.UpdatedAt, "newUser", newUser.UpdatedAt)
		t.Fatal("updated at not changed")
	}
	newUser.UpdatedAt = testUser.UpdatedAt
	if newUser != testUser {
		t.Fatalf("updated user %v, want %v", newUser, testUser)
	}
}

func TestInviteList(t *testing.T) {
	inviterID := uuid.New().String()
	expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	inv1 := Invite{
		InviteID: uuid.New().String(), InviterID: inviterID,
		InviteeEmail: "a@test.com", ResourceType: "org", ResourceID: uuid.New().String(),
		Status: "pending", ExpiresAt: expires,
	}
	inv2 := Invite{
		InviteID: uuid.New().String(), InviterID: inviterID,
		InviteeEmail: "b@test.com", ResourceType: "org", ResourceID: uuid.New().String(),
		Status: "pending", ExpiresAt: expires,
	}
	t.Cleanup(func() { inv1.Remove(context.Background()); inv2.Remove(context.Background()) })
	if err := inv1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := inv2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := (Invite{InviterID: inviterID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 invites, got %d", len(rows))
	}

	if err := inv1.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = (Invite{InviterID: inviterID}).List(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 invite after remove, got %d", len(rows))
	}
	if rows[0].(Invite).InviteID != inv2.InviteID {
		t.Fatalf("expected invite %s, got %s", inv2.InviteID, rows[0].(Invite).InviteID)
	}
}

func TestSeedServiceAccounts_CreatesNew(t *testing.T) {
	name := "seed-test-" + t.Name()
	t.Cleanup(func() {
		gormDB.Unscoped().Where("service_name = ?", name).Delete(&ServiceAccount{}) //nolint:errcheck
	})

	t.Setenv("GATEKEEPER_SERVICES", name+"=bootstrapkey")
	seedServiceAccounts(context.Background())

	var svc ServiceAccount
	if err := gormDB.Where("service_name = ?", name).First(&svc).Error; err != nil {
		t.Fatalf("expected service account to be created: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(svc.HashedKey), []byte("bootstrapkey")) != nil {
		t.Fatal("HashedKey does not match bootstrap key")
	}
	if bcrypt.CompareHashAndPassword([]byte(svc.HashedBootstrapKey), []byte("bootstrapkey")) != nil {
		t.Fatal("HashedBootstrapKey does not match bootstrap key")
	}
}

func TestSeedServiceAccounts_PreservesRotatedKey(t *testing.T) {
	name := "seed-rotate-" + t.Name()
	t.Cleanup(func() {
		gormDB.Unscoped().Where("service_name = ?", name).Delete(&ServiceAccount{}) //nolint:errcheck
	})

	// First seed: create the account with the bootstrap key.
	t.Setenv("GATEKEEPER_SERVICES", name+"=bootstrapkey")
	seedServiceAccounts(context.Background())

	// Simulate runtime key rotation: overwrite HashedKey with the rotated key's hash.
	rotatedHash, err := bcrypt.GenerateFromPassword([]byte("rotatedkey"), 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := gormDB.Model(&ServiceAccount{}).Where("service_name = ?", name).
		Update("hashed_key", string(rotatedHash)).Error; err != nil {
		t.Fatalf("failed to simulate rotation: %v", err)
	}

	// Second seed: simulates Gatekeeper restarting. HashedKey must be preserved;
	// HashedBootstrapKey must be refreshed to the current bootstrap key.
	seedServiceAccounts(context.Background())

	var svc ServiceAccount
	if err := gormDB.Where("service_name = ?", name).First(&svc).Error; err != nil {
		t.Fatalf("expected service account to exist: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(svc.HashedKey), []byte("rotatedkey")) != nil {
		t.Fatal("rotated key was overwritten by seedServiceAccounts on restart")
	}
	if bcrypt.CompareHashAndPassword([]byte(svc.HashedBootstrapKey), []byte("bootstrapkey")) != nil {
		t.Fatal("HashedBootstrapKey was not refreshed to bootstrap key on restart")
	}
}
