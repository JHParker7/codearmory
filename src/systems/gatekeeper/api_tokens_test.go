package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Scoped tokens. The properties worth pinning are the security ones: a token can only
// carry permissions its owner already holds, only over their own namespace, and
// revoking it takes effect immediately rather than at expiry.

// tokenOwner creates a user holding exactly one permission — readRepo on their own
// namespace — plus the default-grant actions needed to manage tokens. Anything the
// tests then try to put in a token beyond readRepo must be refused.
func tokenOwner(t *testing.T) User {
	t.Helper()
	ctx := context.Background()
	owner := createTestUser(t)

	perms := []Permissions{
		{
			PermissionsID: uuid.New().String(), Name: "own-read-" + owner.UserID, Service: "codearmory_git_factory",
			Actions: []string{"readRepo"}, Resources: []string{owner.Username + "/codearmory_git_factory/repos/*"},
			OwnerID: owner.UserID, Active: true,
		},
		{
			PermissionsID: uuid.New().String(), Name: "own-tokens-" + owner.UserID, Service: "gatekeeper",
			Actions: []string{"createToken", "listToken", "deleteToken"}, Resources: []string{owner.Username + "/gatekeeper/tokens"},
			OwnerID: owner.UserID, Active: true,
		},
	}
	var ids []string
	for _, p := range perms {
		if err := p.Add(ctx); err != nil {
			t.Fatalf("add permission: %v", err)
		}
		t.Cleanup(func() { p.Remove(ctx) }) //nolint:errcheck
		ids = append(ids, p.PermissionsID)
	}
	role := Role{RoleID: uuid.New().String(), Name: "own-" + owner.UserID, PermissionsIDs: ids, OwnerID: owner.UserID, Active: true}
	if err := role.Add(ctx); err != nil {
		t.Fatalf("add role: %v", err)
	}
	t.Cleanup(func() { role.Remove(ctx) }) //nolint:errcheck

	row, err := (User{UserID: owner.UserID}).Get(ctx)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	u := row.(User)
	u.RoleID = &role.RoleID
	if err := u.Update(ctx); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	return owner
}

func createToken(t *testing.T, owner User, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := withUserID(httptest.NewRequest(http.MethodPost, "/tokens", bytes.NewReader(raw)), owner.UserID)
	w := httptest.NewRecorder()
	handleCreateToken(w, r)
	return w
}

func readPermission(owner User) map[string]any {
	return map[string]any{
		"service":  "codearmory_git_factory",
		"action":   "readRepo",
		"resource": owner.Username + "/codearmory_git_factory/repos/*",
	}
}

// The whole point: the token authenticates as its owner but evaluates only its own
// role, so it can do the one thing it was minted for and nothing else the owner can do.
func TestCreateToken_MintsACredentialScopedToItsRole(t *testing.T) {
	ctx := context.Background()
	owner := tokenOwner(t)

	w := createToken(t, owner, map[string]any{
		"name":            "ci",
		"permissions":     []map[string]any{readPermission(owner)},
		"expires_in_days": 30,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	var got struct {
		TokenID   string    `json:"token_id"`
		Token     string    `json:"token"`
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token == "" {
		t.Fatal("no token in the response — it is returned once and never again")
	}
	if got.Name != "ci" {
		t.Errorf("name = %q, want ci", got.Name)
	}
	if got.ExpiresAt.IsZero() || got.ExpiresAt.After(time.Now().Add(31*24*time.Hour)) {
		t.Errorf("expires_at = %v, want ~30 days out", got.ExpiresAt)
	}

	record, err := getTokenForUser(ctx, owner.UserID, got.TokenID)
	if err != nil {
		t.Fatalf("stored token not found: %v", err)
	}
	session, err := (Session{SessionID: record.SessionID}).Get(ctx)
	if err != nil {
		t.Fatalf("token session not found: %v", err)
	}
	scoped := session.(Session).ScopedRoleID
	if scoped == nil || *scoped != record.RoleID {
		t.Fatal("the token's session is not bound to its scoped role — it would carry the owner's full permissions")
	}

	// Evaluated as the token's own session would be: the scoped role in context is
	// what authMiddleware puts there, and evaluatePermissions then considers ONLY it.
	scopedCtx := context.WithValue(ctx, scopedRoleKey, *scoped)
	if ok, _, err := evaluatePermissions(scopedCtx, owner.UserID, "gatekeeper", "createToken", owner.Username+"/gatekeeper/tokens"); err != nil || ok {
		t.Error("the token can mint more tokens — a scoped credential must not inherit its owner's other permissions")
	}
	if ok, _, err := evaluatePermissions(scopedCtx, owner.UserID, "codearmory_git_factory", "readRepo", owner.Username+"/codearmory_git_factory/repos/abc"); err != nil || !ok {
		t.Errorf("the token cannot do the one thing it was minted for: %v", err)
	}
}

// Attenuation and confinement, rejected rather than silently dropped.
func TestCreateToken_RefusesWhatTheOwnerCannotGrant(t *testing.T) {
	owner := tokenOwner(t)

	cases := []struct {
		name string
		perm map[string]any
		want int
		why  string
	}{
		{
			"action the owner does not hold",
			map[string]any{"service": "codearmory_git_factory", "action": "deleteRepo", "resource": owner.Username + "/codearmory_git_factory/repos/*"},
			http.StatusForbidden,
			"a user holding only readRepo must not be able to mint a token that deletes",
		},
		{
			"someone else's namespace",
			map[string]any{"service": "codearmory_git_factory", "action": "readRepo", "resource": "someone-else/codearmory_git_factory/repos/*"},
			http.StatusForbidden,
			"confinement: a token must not reach outside its owner's namespace",
		},
		{
			"the escalation attempt",
			map[string]any{"service": "*", "action": "*", "resource": "*/*/*"},
			http.StatusForbidden,
			"a wildcard is not inside anyone's namespace",
		},
		{
			"incomplete spec",
			map[string]any{"service": "codearmory_git_factory", "action": "readRepo"},
			http.StatusBadRequest,
			"a permission with no resource cannot be evaluated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := createToken(t, owner, map[string]any{"name": "bad", "permissions": []map[string]any{tc.perm}})
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d — %s (%s)", w.Code, tc.want, tc.why, w.Body.String())
			}
		})
	}
}

func TestCreateToken_ValidatesTheRequest(t *testing.T) {
	owner := tokenOwner(t)
	past := time.Now().Add(-time.Hour)
	tooFar := time.Now().Add(400 * 24 * time.Hour)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no name", map[string]any{"permissions": []map[string]any{readPermission(owner)}}},
		{"no permissions", map[string]any{"name": "empty"}},
		{"expiry in the past", map[string]any{"name": "stale", "permissions": []map[string]any{readPermission(owner)}, "expires_at": past}},
		{"expiry beyond a year", map[string]any{"name": "forever", "permissions": []map[string]any{readPermission(owner)}, "expires_at": tooFar}},
		{"negative lifetime", map[string]any{"name": "neg", "permissions": []map[string]any{readPermission(owner)}, "expires_in_days": -3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := createToken(t, owner, tc.body); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestResolveTokenExpiry_DefaultsToNinetyDays(t *testing.T) {
	now := time.Now()
	got, problem := resolveTokenExpiry(createTokenRequest{}, now)
	if problem != "" {
		t.Fatalf("unexpected refusal: %s", problem)
	}
	if want := now.Add(defaultTokenLifetime).Truncate(time.Second); got.Sub(want).Abs() > time.Second {
		t.Errorf("default expiry = %v, want ~%v — a token must always expire", got, want)
	}
}

// Revocation has to kill the credential NOW. Deactivating the session is what does it:
// authMiddleware fails closed at the session lookup, before any permission is read.
func TestRevokeToken_KillsTheSessionImmediately(t *testing.T) {
	ctx := context.Background()
	owner := tokenOwner(t)

	w := createToken(t, owner, map[string]any{"name": "doomed", "permissions": []map[string]any{readPermission(owner)}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
	var created struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	record, err := getTokenForUser(ctx, owner.UserID, created.TokenID)
	if err != nil {
		t.Fatalf("stored token not found: %v", err)
	}
	if _, err := (Session{SessionID: record.SessionID}).Get(ctx); err != nil {
		t.Fatalf("session should be live before revocation: %v", err)
	}

	rr := withUserID(httptest.NewRequest(http.MethodDelete, "/tokens/"+created.TokenID, nil), owner.UserID)
	rr.SetPathValue("id", created.TokenID)
	rw := httptest.NewRecorder()
	handleRevokeToken(rw, rr)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want 204 (%s)", rw.Code, rw.Body.String())
	}

	if _, err := (Session{SessionID: record.SessionID}).Get(ctx); err == nil {
		t.Error("the session survived revocation — the token would keep working until it expired")
	}
	if _, err := getTokenForUser(ctx, owner.UserID, created.TokenID); err == nil {
		t.Error("the revoked token is still listed as live")
	}
}

// A token id belonging to someone else must read as not-found, never as forbidden —
// otherwise the endpoint confirms which ids exist.
func TestRevokeToken_OtherPeoplesTokensAreNotFound(t *testing.T) {
	owner := tokenOwner(t)
	stranger := tokenOwner(t)

	w := createToken(t, stranger, map[string]any{"name": "theirs", "permissions": []map[string]any{readPermission(stranger)}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
	var created struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rr := withUserID(httptest.NewRequest(http.MethodDelete, "/tokens/"+created.TokenID, nil), owner.UserID)
	rr.SetPathValue("id", created.TokenID)
	rw := httptest.NewRecorder()
	handleRevokeToken(rw, rr)
	if rw.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rw.Code)
	}
}

// The list is metadata only, and it is the caller's own.
func TestListTokens_ShowsMetadataNotCredentials(t *testing.T) {
	owner := tokenOwner(t)
	stranger := tokenOwner(t)

	if w := createToken(t, owner, map[string]any{"name": "mine", "permissions": []map[string]any{readPermission(owner)}}); w.Code != http.StatusCreated {
		t.Fatalf("create: %s", w.Body.String())
	}
	if w := createToken(t, stranger, map[string]any{"name": "theirs", "permissions": []map[string]any{readPermission(stranger)}}); w.Code != http.StatusCreated {
		t.Fatalf("create (stranger): %s", w.Body.String())
	}

	r := withUserID(httptest.NewRequest(http.MethodGet, "/tokens", nil), owner.UserID)
	w := httptest.NewRecorder()
	handleListTokens(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("\"token\"")) {
		t.Error("the listing carries a credential — tokens are shown once, at creation, and never again")
	}

	var views []tokenView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(views) != 1 || views[0].Name != "mine" {
		t.Fatalf("listing = %+v, want exactly the caller's own token", views)
	}
}
