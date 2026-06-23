package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuditLog_Immutable(t *testing.T) {
	if err := (AuditLog{}).Update(context.Background()); err == nil {
		t.Error("AuditLog.Update should return an immutability error")
	}
	if err := (AuditLog{}).Remove(context.Background()); err == nil {
		t.Error("AuditLog.Remove should return an immutability error")
	}
}

func TestListAndDeactivateOAuthClient(t *testing.T) {
	clientID, _ := seedOAuthClient(t, "https://app.example/cb")
	list, err := listOAuthClients(context.Background())
	if err != nil {
		t.Fatalf("listOAuthClients: %v", err)
	}
	found := false
	for _, c := range list {
		if c.ClientID == clientID {
			found = true
		}
	}
	if !found {
		t.Error("seeded client not in listOAuthClients")
	}
	n, err := deactivateOAuthClient(context.Background(), clientID)
	if err != nil || n != 1 {
		t.Fatalf("deactivateOAuthClient = %d, %v, want 1", n, err)
	}
}

func TestStartDefaultGrantPoller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`)) //nolint:errcheck
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	startDefaultGrantPoller(ctx, srv.URL, "gatekeeper:key")
	time.Sleep(100 * time.Millisecond) // allow the immediate fetch to run
	cancel()
	time.Sleep(30 * time.Millisecond)
	// The poller applied empty grants to the shared cache; restore the seed other
	// tests rely on so this test doesn't corrupt global state.
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
}

func TestHandleCreateOrg_InvalidBody(t *testing.T) {
	actor := createAuthorizedUser(t, "createOrg", "gatekeeper/orgs")
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", strings.NewReader("{bad")), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateTeam_InvalidBody(t *testing.T) {
	actor := createAuthorizedUser(t, "createTeam", "gatekeeper/teams")
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", strings.NewReader("{bad")), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreateOrg_Unauthorized(t *testing.T) {
	actor := createTestUser(t) // no createOrg permission
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", strings.NewReader(`{"org_name":"x"}`)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)
	if w.Code == http.StatusCreated {
		t.Fatal("unauthorized caller created an org")
	}
}
