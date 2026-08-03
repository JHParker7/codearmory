package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// createAuthorizedUser creates a user with a role granting action on resource within the
// "gatekeeper" service. Both the permission, role, and user are cleaned up after the test.
// The permission resource is stored as "<username>/<resource>" to match the scoping that
// checkPermissions applies (it prepends the username to unscoped resources).
func createAuthorizedUser(t *testing.T, action, resource string) User {
	t.Helper()
	username := "user-" + uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "gatekeeper",
		Actions:       []string{action},
		Resources:     []string{username + "/" + resource},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUser perm: %v", err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUser role: %v", err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUser user: %v", err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })
	return u
}

// createAuthorizedUserViaTeam creates a user whose permission comes through a team role rather
// than a directly-assigned role. The chain created is: permission → role → team (with that role)
// → user (with TeamID, no direct RoleID). All records are cleaned up after the test.
// The permission resource is stored as "<username>/<resource>" to match checkPermissions' scoping.
func createAuthorizedUserViaTeam(t *testing.T, action, resource string) User {
	t.Helper()
	username := "user-" + uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "gatekeeper",
		Actions:       []string{action},
		Resources:     []string{username + "/" + resource},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUserViaTeam perm: %v", err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUserViaTeam role: %v", err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	team := Team{TeamID: uuid.New().String(), TeamName: "team-" + uuid.New().String(), RoleID: &role.RoleID}
	if err := team.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUserViaTeam team: %v", err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	userRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{}}
	if err := userRole.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUserViaTeam role: %v", err)
	}

	t.Cleanup(func() { userRole.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		TeamID:         &team.TeamID,
		RoleID:         &userRole.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("createAuthorizedUserViaTeam user: %v", err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })
	return u
}

// makeSession generates an ECDSA key pair, signs a JWT, stores the session in the DB,
// and returns the token string and session. Cleans up the session on test completion.
func makeSession(t *testing.T, userID string, expiresAt time.Time) (string, Session) {
	t.Helper()
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("makeSession key: %v", err)
	}
	sessionID := uuid.New().String()
	tokenStr, err := jwtlib.NewWithClaims(jwtlib.SigningMethodES256, authClaims{
		RegisteredClaims: jwtlib.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   userID,
			ID:        sessionID,
			ExpiresAt: jwtlib.NewNumericDate(expiresAt),
			IssuedAt:  jwtlib.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)
	if err != nil {
		t.Fatalf("makeSession sign: %v", err)
	}
	pubBytes, _ := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}))

	s := Session{
		SessionID: sessionID,
		UserID:    userID,
		ExpiresAt: expiresAt.UTC().Truncate(time.Second),
		PubKey:    pubPEM,
	}
	if err := s.Add(context.Background()); err != nil {
		t.Fatalf("makeSession Add: %v", err)
	}
	t.Cleanup(func() { s.Remove(context.Background()) })
	return tokenStr, s
}

// withUserID injects a userID into the request context as authMiddleware would.
func withUserID(r *http.Request, userID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userIDKey, userID))
}

// --- requirePermission ---

func TestRequirePermission_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/", nil), actor.UserID)
	w := httptest.NewRecorder()
	if requirePermission(w, r, "getOrg", "gatekeeper/orgs/anything") {
		t.Fatal("expected false for user with no role")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestRequirePermission_Allowed(t *testing.T) {
	actor := createAuthorizedUser(t, "getOrg", "gatekeeper/orgs/test-id")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/", nil), actor.UserID)
	w := httptest.NewRecorder()
	if !requirePermission(w, r, "getOrg", "gatekeeper/orgs/test-id") {
		t.Fatal("expected true for authorized user")
	}
}

func TestRequirePermission_AllowedViaTeamRole(t *testing.T) {
	actor := createAuthorizedUserViaTeam(t, "getOrg", "gatekeeper/orgs/test-id")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/", nil), actor.UserID)
	w := httptest.NewRecorder()
	if !requirePermission(w, r, "getOrg", "gatekeeper/orgs/test-id") {
		t.Fatal("expected true for user authorized via team role")
	}
}

// --- authMiddleware ---

func TestAuthMiddleware_Valid(t *testing.T) {
	u := createTestUser(t)
	tokenStr, _ := makeSession(t, u.UserID, time.Now().Add(24*time.Hour))

	var capturedID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID, _ = r.Context().Value(userIDKey).(string)
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	authMiddleware(next).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedID != u.UserID {
		t.Fatalf("context user_id: got %q, want %q", capturedID, u.UserID)
	}
}

func TestAuthMiddleware_NoHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_MalformedHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Token sometoken")
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not.a.valid.jwt")
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_NoSession(t *testing.T) {
	// Valid JWT format but the session ID does not exist in the DB.
	privKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tokenStr, _ := jwtlib.NewWithClaims(jwtlib.SigningMethodES256, authClaims{
		RegisteredClaims: jwtlib.RegisteredClaims{
			Subject:   uuid.New().String(),
			ID:        uuid.New().String(),
			ExpiresAt: jwtlib.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwtlib.NewNumericDate(time.Now()),
		},
	}).SignedString(privKey)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ExpiredToken(t *testing.T) {
	u := createTestUser(t)
	tokenStr, _ := makeSession(t, u.UserID, time.Now().Add(-1*time.Hour))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not be called")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// --- checkPermissions (pure function) ---

func TestCheckPermissions_UnknownUser(t *testing.T) {
	ok, err := checkPermissions(context.Background(), uuid.New().String(), "svc", "act", "res")
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
	if ok {
		t.Fatal("expected false")
	}
}

func TestCheckPermissions_NoRoleReturnsFalse(t *testing.T) {
	u := createTestUser(t)
	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "act", "res")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false for user with no role")
	}
}

func TestCheckPermissions_Match(t *testing.T) {
	username := "user-" + uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "my-service",
		Actions:       []string{"read"},
		Resources:     []string{username + "/my-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	ok, err := checkPermissions(context.Background(), u.UserID, "my-service", "read", "my-resource")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true for matching permission")
	}
}

func TestCheckPermissions_NoMatch(t *testing.T) {
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "my-service",
		Actions:       []string{"read"},
		Resources:     []string{"my-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "user-" + uuid.New().String(),
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	ok, err := checkPermissions(context.Background(), u.UserID, "other-service", "write", "other-resource")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false for non-matching permission")
	}
}

func TestCheckPermissions_MatchViaTeamRole(t *testing.T) {
	username := "user-" + uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "team-service",
		Actions:       []string{"read"},
		Resources:     []string{username + "/team-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	team := Team{TeamID: uuid.New().String(), TeamName: "team-" + uuid.New().String(), RoleID: &role.RoleID}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		TeamID:         &team.TeamID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	ok, err := checkPermissions(context.Background(), u.UserID, "team-service", "read", "team-resource")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true for matching permission via team role")
	}
}

func TestCheckPermissions_NoMatchViaTeamRole(t *testing.T) {
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "team-service",
		Actions:       []string{"read"},
		Resources:     []string{"team-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	team := Team{TeamID: uuid.New().String(), TeamName: "team-" + uuid.New().String(), RoleID: &role.RoleID}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "user-" + uuid.New().String(),
		HashedPassword: "hash",
		TeamID:         &team.TeamID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	ok, err := checkPermissions(context.Background(), u.UserID, "other-service", "write", "other-resource")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false for non-matching permission via team role")
	}
}

func TestCheckPermissions_SoftDeletedTeamSkipped(t *testing.T) {
	// Permission via direct role; team is soft-deleted (stale team_id on user).
	// checkPermissions must skip the missing team and still grant access.
	username := "user-" + uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "my-service",
		Actions:       []string{"read"},
		Resources:     []string{username + "/my-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	deletedTeam := Team{TeamID: uuid.New().String(), TeamName: "deleted-team-" + uuid.New().String()}
	if err := deletedTeam.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := deletedTeam.Remove(context.Background()); err != nil { // soft-delete immediately
		t.Fatal(err)
	}

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
		TeamID:         &deletedTeam.TeamID, // stale reference
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	ok, err := checkPermissions(context.Background(), u.UserID, "my-service", "read", "my-resource")
	if err != nil {
		t.Fatalf("expected no error for stale team ref, got: %v", err)
	}
	if !ok {
		t.Fatal("expected true: direct role should grant access even with stale team_id")
	}
}

func TestCheckPermissions_TeamAndDirectRoleAccumulate(t *testing.T) {
	username := "user-" + uuid.New().String()
	directPerm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "direct-service",
		Actions:       []string{"write"},
		Resources:     []string{username + "/direct-resource"},
	}
	if err := directPerm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directPerm.Remove(context.Background()) })

	directRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{directPerm.PermissionsID}}
	if err := directRole.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directRole.Remove(context.Background()) })

	teamPerm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "team-service",
		Actions:       []string{"read"},
		Resources:     []string{username + "/team-resource"},
	}
	if err := teamPerm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { teamPerm.Remove(context.Background()) })

	teamRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{teamPerm.PermissionsID}}
	if err := teamRole.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { teamRole.Remove(context.Background()) })

	team := Team{TeamID: uuid.New().String(), TeamName: "team-" + uuid.New().String(), RoleID: &teamRole.RoleID}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &directRole.RoleID,
		TeamID:         &team.TeamID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	okDirect, err := checkPermissions(context.Background(), u.UserID, "direct-service", "write", "direct-resource")
	if err != nil {
		t.Fatal(err)
	}
	if !okDirect {
		t.Fatal("expected true for direct role permission")
	}

	okTeam, err := checkPermissions(context.Background(), u.UserID, "team-service", "read", "team-resource")
	if err != nil {
		t.Fatal(err)
	}
	if !okTeam {
		t.Fatal("expected true for team role permission")
	}

	okNeither, err := checkPermissions(context.Background(), u.UserID, "other-service", "delete", "other-resource")
	if err != nil {
		t.Fatal(err)
	}
	if okNeither {
		t.Fatal("expected false for permission held by neither role")
	}
}

// --- handleCheckPermissions ---

func TestHandleCheckPermissions_NoRole(t *testing.T) {
	u := createTestUser(t)
	b, _ := json.Marshal(checkPermissionsRequest{Service: "svc", Resource: "res", Action: "read"})
	r := withUserID(httptest.NewRequest(http.MethodGet, "/check_permissions", bytes.NewReader(b)), u.UserID)
	w := httptest.NewRecorder()
	handleCheckPermissions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["authorized"].(bool) {
		t.Fatalf("expected authorized:false for user with no role, got %v", resp)
	}
}

func TestHandleCheckPermissions_Authorized(t *testing.T) {
	username := "user-" + uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "my-service",
		Actions:       []string{"read"},
		Resources:     []string{username + "/my-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	b, _ := json.Marshal(checkPermissionsRequest{Service: "my-service", Resource: "my-resource", Action: "read"})
	r := withUserID(httptest.NewRequest(http.MethodGet, "/check_permissions", bytes.NewReader(b)), u.UserID)
	w := httptest.NewRecorder()
	handleCheckPermissions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !resp["authorized"].(bool) {
		t.Fatalf("expected authorized:true, got %v", resp)
	}
}

func TestHandleCheckPermissions_NotAuthorized(t *testing.T) {
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "other-service",
		Actions:       []string{"write"},
		Resources:     []string{"other-resource"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), Name: "denied-role", PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	username := "user-" + uuid.New().String()
	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	b, _ := json.Marshal(checkPermissionsRequest{Service: "my-service", Resource: "my-resource", Action: "read"})
	r := withUserID(httptest.NewRequest(http.MethodGet, "/check_permissions", bytes.NewReader(b)), u.UserID)
	w := httptest.NewRecorder()
	handleCheckPermissions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["authorized"].(bool) {
		t.Fatalf("expected authorized:false, got %v", resp)
	}
	// The denial response must carry the role, scoped resource, service, action,
	// and a human-readable reason so every interface can explain the 403.
	if got := resp["role"]; got != "denied-role" {
		t.Fatalf("expected role %q, got %v", "denied-role", got)
	}
	if got := resp["resource"]; got != username+"/my-resource" {
		t.Fatalf("expected scoped resource %q, got %v", username+"/my-resource", got)
	}
	if resp["service"] != "my-service" || resp["action"] != "read" {
		t.Fatalf("expected service/action my-service/read, got %v/%v", resp["service"], resp["action"])
	}
	reason, _ := resp["reason"].(string)
	if !strings.Contains(reason, "denied-role") || !strings.Contains(reason, "my-service") {
		t.Fatalf("expected reason to name the role and service, got %q", reason)
	}
}

func TestHandleCheckPermissions_InvalidBody(t *testing.T) {
	u := createTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/check_permissions", bytes.NewReader([]byte("not json"))), u.UserID)
	w := httptest.NewRecorder()
	handleCheckPermissions(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- validatePermissionIDs ---

func TestValidatePermissionIDs_OrgScopedRejectedForNoOrgCaller(t *testing.T) {
	orgID := uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "svc",
		Actions:       []string{"read"},
		Resources:     []string{"res"},
		OrgID:         &orgID,
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	// nil callerOrgID: caller has no org; should be rejected for an org-scoped permission
	if err := validatePermissionIDs(context.Background(), []string{perm.PermissionsID}, nil); err == nil {
		t.Fatal("expected error: org-scoped permission should be rejected for a no-org caller")
	}
}

func TestValidatePermissionIDs_NilOrgPermAllowedForNoOrgCaller(t *testing.T) {
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "svc",
		Actions:       []string{"read"},
		Resources:     []string{"res"},
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	if err := validatePermissionIDs(context.Background(), []string{perm.PermissionsID}, nil); err != nil {
		t.Fatalf("unexpected error: nil-org permission should be allowed for a no-org caller: %v", err)
	}
}

func TestValidatePermissionIDs_CrossOrgRejected(t *testing.T) {
	orgA := uuid.New().String()
	orgB := uuid.New().String()
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       "svc",
		Actions:       []string{"read"},
		Resources:     []string{"res"},
		OrgID:         &orgA,
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	if err := validatePermissionIDs(context.Background(), []string{perm.PermissionsID}, &orgB); err == nil {
		t.Fatal("expected error: permission from org A should be rejected for caller in org B")
	}
}

// --- parsePagination ---

func TestParsePagination_Defaults(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items", nil)
	w := httptest.NewRecorder()
	limit, offset, ok := parsePagination(w, r)
	if !ok {
		t.Fatal("expected ok=true for request with no pagination params")
	}
	if limit != 50 {
		t.Fatalf("expected default limit=50, got %d", limit)
	}
	if offset != 0 {
		t.Fatalf("expected default offset=0, got %d", offset)
	}
}

func TestParsePagination_CustomValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items?limit=10&offset=20", nil)
	w := httptest.NewRecorder()
	limit, offset, ok := parsePagination(w, r)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if limit != 10 {
		t.Fatalf("expected limit=10, got %d", limit)
	}
	if offset != 20 {
		t.Fatalf("expected offset=20, got %d", offset)
	}
}

func TestParsePagination_LimitCappedAt500(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items?limit=9999", nil)
	w := httptest.NewRecorder()
	limit, _, ok := parsePagination(w, r)
	if !ok {
		t.Fatal("expected ok=true for oversized limit")
	}
	if limit != 500 {
		t.Fatalf("expected limit capped at 500, got %d", limit)
	}
}

func TestParsePagination_InvalidLimit(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "0"} {
		r := httptest.NewRequest(http.MethodGet, "/items?limit="+bad, nil)
		w := httptest.NewRecorder()
		_, _, ok := parsePagination(w, r)
		if ok {
			t.Fatalf("expected ok=false for limit=%q", bad)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for limit=%q, got %d", bad, w.Code)
		}
	}
}

func TestParsePagination_InvalidOffset(t *testing.T) {
	for _, bad := range []string{"abc", "-5"} {
		r := httptest.NewRequest(http.MethodGet, "/items?offset="+bad, nil)
		w := httptest.NewRecorder()
		_, _, ok := parsePagination(w, r)
		if ok {
			t.Fatalf("expected ok=false for offset=%q", bad)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for offset=%q, got %d", bad, w.Code)
		}
	}
}

func TestParsePagination_ZeroOffset(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/items?offset=0", nil)
	w := httptest.NewRecorder()
	_, offset, ok := parsePagination(w, r)
	if !ok {
		t.Fatal("expected ok=true for offset=0")
	}
	if offset != 0 {
		t.Fatalf("expected offset=0, got %d", offset)
	}
}

// --- parseECPublicKey ---

func TestParseECPublicKey_Valid(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}))

	key, err := parseECPublicKey(pemStr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
}

func TestParseECPublicKey_EmptyString(t *testing.T) {
	_, err := parseECPublicKey("")
	if err == nil {
		t.Fatal("expected error for empty PEM string")
	}
}

func TestParseECPublicKey_InvalidPEM(t *testing.T) {
	_, err := parseECPublicKey("not-a-pem-block")
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestParseECPublicKey_RSAKeyRejected(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}))

	_, err = parseECPublicKey(pemStr)
	if err == nil {
		t.Fatal("expected error: RSA key should be rejected as non-ECDSA")
	}
}

// --- checkPermissions: wildcard and prefix matching ---

func makePermUser(t *testing.T, service string, actions []string, resources []string) User {
	t.Helper()
	username := "user-" + uuid.New().String()
	// Prefix each resource with the username so checkPermissions' scoping (which prepends
	// "<username>/" to unscoped resources) doesn't produce a mismatch.
	scoped := make([]string, len(resources))
	for i, r := range resources {
		scoped[i] = username + "/" + r
	}
	perm := Permissions{
		PermissionsID: uuid.New().String(),
		Service:       service,
		Actions:       actions,
		Resources:     scoped,
	}
	if err := perm.Add(context.Background()); err != nil {
		t.Fatalf("makePermUser perm: %v", err)
	}
	t.Cleanup(func() { perm.Remove(context.Background()) })

	role := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{perm.PermissionsID}}
	if err := role.Add(context.Background()); err != nil {
		t.Fatalf("makePermUser role: %v", err)
	}
	t.Cleanup(func() { role.Remove(context.Background()) })

	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       username,
		HashedPassword: "hash",
		RoleID:         &role.RoleID,
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("makePermUser user: %v", err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })
	return u
}

func TestCheckPermissions_GlobalWildcardResource(t *testing.T) {
	u := makePermUser(t, "svc", []string{"read"}, []string{"*"})
	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "anything/at/all")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true: resource wildcard '*' should match any path")
	}
}

func TestCheckPermissions_PrefixWildcardResource(t *testing.T) {
	u := makePermUser(t, "svc", []string{"read"}, []string{"blueprints/states/*"})
	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "blueprints/states/mystate")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true: prefix wildcard should match path under the prefix")
	}
}

func TestCheckPermissions_PrefixWildcardNoMatch(t *testing.T) {
	u := makePermUser(t, "svc", []string{"read"}, []string{"blueprints/states/*"})
	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "blueprints/other/mystate")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false: prefix wildcard should not match a different path prefix")
	}
}

func TestCheckPermissions_SegmentWildcardResource(t *testing.T) {
	// "foo/*/bar" should match "foo/anything/bar" but not "foo/anything/baz".
	u := makePermUser(t, "svc", []string{"read"}, []string{"foo/*/bar"})

	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "foo/xyz/bar")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true: per-segment wildcard should match")
	}

	ok2, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "foo/xyz/baz")
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Fatal("expected false: per-segment wildcard should not match different final segment")
	}
}

func TestCheckPermissions_GlobalWildcardAction(t *testing.T) {
	u := makePermUser(t, "svc", []string{"*"}, []string{"res"})
	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "deleteEverything", "res")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true: action wildcard '*' should match any action")
	}
}

func TestCheckPermissions_ActionPrefixWildcard(t *testing.T) {
	u := makePermUser(t, "svc", []string{"get*"}, []string{"res"})

	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "getUser", "res")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true: action prefix wildcard 'get*' should match 'getUser'")
	}

	ok2, err := checkPermissions(context.Background(), u.UserID, "svc", "deleteUser", "res")
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Fatal("expected false: action prefix wildcard 'get*' should not match 'deleteUser'")
	}
}

func TestCheckPermissions_PrefixWildcardNoSlashIsNotWildcard(t *testing.T) {
	// "blueprints/states*" must NOT be treated as a prefix wildcard — the "*" is
	// only meaningful when preceded by "/" to prevent overmatch against sibling paths.
	u := makePermUser(t, "svc", []string{"read"}, []string{"blueprints/states*"})

	// The exact stored value "blueprints/states*" should not match "blueprints/states/mystate"
	// via prefix logic; it can only match as an exact resource string.
	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "blueprints/states/mystate")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false: 'blueprints/states*' without '/' before '*' must not prefix-match 'blueprints/states/mystate'")
	}
}

func TestCheckPermissions_PrefixWildcardExactPrefixNoMatch(t *testing.T) {
	// "blueprints/states/*" should NOT match the bare prefix "blueprints/states" itself
	// (no trailing slash) — only paths strictly under it.
	u := makePermUser(t, "svc", []string{"read"}, []string{"blueprints/states/*"})

	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "blueprints/states")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false: 'blueprints/states/*' should not match bare 'blueprints/states'")
	}
}

func TestCheckPermissions_PrefixWildcardSiblingNoMatch(t *testing.T) {
	// "blueprints/states/*" must not match "blueprints/states_admin/foo" even though
	// the old (buggy) HasPrefix("blueprints/states/") logic would match it.
	u := makePermUser(t, "svc", []string{"read"}, []string{"blueprints/states/*"})

	ok, err := checkPermissions(context.Background(), u.UserID, "svc", "read", "blueprints/states_admin/foo")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false: 'blueprints/states/*' must not match sibling path 'blueprints/states_admin/foo'")
	}
}

// --- realClientIP and rate limiter proxy-awareness ---

func TestRealClientIP_NoTrustedProxies(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	trustedProxyNets = nil

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5678"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := realClientIP(r); got != "1.2.3.4" {
		t.Fatalf("expected RemoteAddr when no trusted proxies, got %q", got)
	}
}

func TestRealClientIP_TrustedProxyUsesXFF(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("127.0.0.1/32")
	trustedProxyNets = []*net.IPNet{proxyNet}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := realClientIP(r); got != "203.0.113.7" {
		t.Fatalf("expected XFF IP from trusted proxy, got %q", got)
	}
}

func TestRealClientIP_UntrustedPeerIgnoresXFF(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxyNets = []*net.IPNet{proxyNet}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5678" // not in trusted CIDR
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := realClientIP(r); got != "1.2.3.4" {
		t.Fatalf("expected RemoteAddr for untrusted peer, got %q", got)
	}
}

func TestRateLimitMiddleware_RespectsTrustedProxy(t *testing.T) {
	// Two distinct clients (different XFF IPs) behind the same trusted proxy
	// must be rate-limited independently. Without using realClientIP they would
	// share the same bucket (the proxy's RemoteAddr) and the second client's
	// first request would be rejected after the first client fills the bucket.
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("127.0.0.1/32")
	trustedProxyNets = []*net.IPNet{proxyNet}

	var limiterMap sync.Map
	h := rateLimitMiddleware("test", &limiterMap, 2, time.Minute,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	doReq := func(xff string) int {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}

	// Client A exhausts its bucket (2 requests).
	if c := doReq("10.0.0.1"); c != http.StatusOK {
		t.Fatalf("A req1: want 200, got %d", c)
	}
	if c := doReq("10.0.0.1"); c != http.StatusOK {
		t.Fatalf("A req2: want 200, got %d", c)
	}

	// Client B is a different IP — its bucket is untouched, so this must succeed.
	if c := doReq("10.0.0.2"); c != http.StatusOK {
		t.Fatalf("B req1: want 200 (separate bucket), got %d", c)
	}

	// Client A is now over-limit.
	if c := doReq("10.0.0.1"); c != http.StatusTooManyRequests {
		t.Fatalf("A req3: want 429, got %d", c)
	}
}

// TestAuthMiddleware_ExpiredTokenIsDistinguishable is the point of the RFC 6750
// challenge: an expired credential and an invalid one must not look identical. Before
// this, both returned a bare 401, so the natural reading was "permissions" — the one
// thing it never is — and telling them apart meant decoding the JWT by hand.
func TestAuthMiddleware_ExpiredTokenIsDistinguishable(t *testing.T) {
	u := createTestUser(t)
	expiredAt := time.Now().Add(-3 * time.Hour)
	tokenStr, _ := makeSession(t, u.UserID, expiredAt)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next must not be called for an expired token")
	})).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	challenge := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want an invalid_token error code", challenge)
	}
	if !strings.Contains(challenge, "expired") {
		t.Errorf("WWW-Authenticate = %q, must say the token expired — that is what tells a caller to log in again", challenge)
	}
	// The expiry instant is named so the caller does not have to decode the token.
	if !strings.Contains(challenge, expiredAt.UTC().Format(time.RFC3339)) {
		t.Errorf("WWW-Authenticate = %q, want it to name the expiry instant %s", challenge, expiredAt.UTC().Format(time.RFC3339))
	}
}

// TestAuthMiddleware_InvalidTokenIsNotReportedAsExpired is the other half: a garbage or
// revoked token must NOT claim expiry, or "log in again" becomes the wrong advice.
func TestAuthMiddleware_InvalidTokenIsNotReportedAsExpired(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not-a-jwt")
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next must not be called for a malformed token")
	})).ServeHTTP(w, r)

	challenge := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want an invalid_token error code", challenge)
	}
	if strings.Contains(challenge, "expired") {
		t.Errorf("WWW-Authenticate = %q must not claim expiry for a malformed token", challenge)
	}
}

// TestAuthMiddleware_NoCredentialIsInvalidRequest — presenting nothing is a different
// error from presenting something broken, and RFC 6750 gives it its own code.
func TestAuthMiddleware_NoCredentialIsInvalidRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next must not be called without a credential")
	})).ServeHTTP(w, r)

	if challenge := w.Header().Get("WWW-Authenticate"); !strings.Contains(challenge, `error="invalid_request"`) {
		t.Errorf("WWW-Authenticate = %q, want an invalid_request error code", challenge)
	}
}

// X-Forwarded-For is APPENDED to by each hop, so only the entries a trusted proxy
// added can be believed. Reading the leftmost entry returns whatever the CLIENT put
// there — which would let any caller reaching gatekeeper through the trusted proxy
// forge its source IP and walk straight through the service-account CIDR allowlist in
// requireServiceAuth, this function's only consumer.
func TestRealClientIP_SpoofedPrefixIsIgnored(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxyNets = []*net.IPNet{proxyNet}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:9999" // the trusted proxy
	// The client sent "198.51.100.9" itself; the proxy then appended what it saw.
	r.Header.Set("X-Forwarded-For", "198.51.100.9, 203.0.113.7")
	if got := realClientIP(r); got != "203.0.113.7" {
		t.Fatalf("got %q, want the proxy-observed 203.0.113.7 — a client-supplied prefix must never win", got)
	}
}

// A chain of trusted proxies is walked past, landing on the first address none of
// them vouch for.
func TestRealClientIP_SkipsTrustedHops(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxyNets = []*net.IPNet{proxyNet}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5, 10.0.0.6")
	if got := realClientIP(r); got != "203.0.113.7" {
		t.Fatalf("got %q, want 203.0.113.7", got)
	}
}

// When every entry is a trusted proxy there is no client address in the header, so
// the immediate peer is the honest answer.
func TestRealClientIP_AllTrustedFallsBackToPeer(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxyNets = []*net.IPNet{proxyNet}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "10.0.0.5, 10.0.0.6")
	if got := realClientIP(r); got != "10.0.0.1" {
		t.Fatalf("got %q, want the peer 10.0.0.1", got)
	}
}

// An unparseable entry is client-supplied garbage. The walk stops rather than
// stepping further left into values the trusted hop cannot vouch for.
func TestRealClientIP_GarbageEntryStopsTheWalk(t *testing.T) {
	orig := trustedProxyNets
	t.Cleanup(func() { trustedProxyNets = orig })
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	trustedProxyNets = []*net.IPNet{proxyNet}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, not-an-ip")
	if got := realClientIP(r); got != "10.0.0.1" {
		t.Fatalf("got %q, want the peer 10.0.0.1", got)
	}
}
