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

// makeOrgInvite inserts a pending Invite for the given org and cleans up on test end.
func makeOrgInvite(t *testing.T, inviterID, inviteeEmail, orgID string, expiresAt time.Time, status string) Invite {
	t.Helper()
	invite := Invite{
		InviteID:     uuid.New().String(),
		InviterID:    inviterID,
		InviteeEmail: inviteeEmail,
		ResourceType: "org",
		ResourceID:   orgID,
		Status:       status,
		ExpiresAt:    expiresAt,
	}
	if err := invite.Add(context.Background()); err != nil {
		t.Fatalf("makeOrgInvite: %v", err)
	}
	t.Cleanup(func() { invite.Remove(context.Background()) })
	return invite
}

// makeTeamInvite inserts a pending Invite for the given team and cleans up on test end.
func makeTeamInvite(t *testing.T, inviterID, inviteeEmail, teamID string, expiresAt time.Time, status string) Invite {
	t.Helper()
	invite := Invite{
		InviteID:     uuid.New().String(),
		InviterID:    inviterID,
		InviteeEmail: inviteeEmail,
		ResourceType: "team",
		ResourceID:   teamID,
		Status:       status,
		ExpiresAt:    expiresAt,
	}
	if err := invite.Add(context.Background()); err != nil {
		t.Fatalf("makeTeamInvite: %v", err)
	}
	t.Cleanup(func() { invite.Remove(context.Background()) })
	return invite
}

// --- handleCreateOrgInvite ---

func TestCreateOrgInvite_Success(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "inviteUser", "gatekeeper/orgs/"+org.OrgID)

	b, _ := json.Marshal(inviteRequest{Email: "invitee@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs/"+org.OrgID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleCreateOrgInvite(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Invite
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	t.Cleanup(func() { resp.Remove(context.Background()) })

	if resp.InviteID == "" {
		t.Fatal("expected invite_id in response")
	}
	if resp.InviteeEmail != "invitee@example.com" {
		t.Fatalf("expected invitee_email=invitee@example.com, got %s", resp.InviteeEmail)
	}
	if resp.ResourceType != "org" {
		t.Fatalf("expected resource_type=org, got %s", resp.ResourceType)
	}
	if resp.ResourceID != org.OrgID {
		t.Fatalf("expected resource_id=%s, got %s", org.OrgID, resp.ResourceID)
	}
	if resp.Status != "pending" {
		t.Fatalf("expected status=pending, got %s", resp.Status)
	}
}

func TestCreateOrgInvite_Forbidden(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	actor := createTestUser(t)

	b, _ := json.Marshal(inviteRequest{Email: "invitee@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs/"+org.OrgID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleCreateOrgInvite(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCreateOrgInvite_MissingEmail(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "inviteUser", "gatekeeper/orgs/"+org.OrgID)

	b, _ := json.Marshal(inviteRequest{Email: ""})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs/"+org.OrgID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleCreateOrgInvite(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateOrgInvite_OrgNotFound(t *testing.T) {
	orgID := uuid.New().String()
	actor := createAuthorizedUser(t, "inviteUser", "gatekeeper/orgs/"+orgID)

	b, _ := json.Marshal(inviteRequest{Email: "invitee@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs/"+orgID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", orgID)
	w := httptest.NewRecorder()
	handleCreateOrgInvite(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- handleCreateTeamInvite ---

func TestCreateTeamInvite_Success(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "test-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "inviteUser", "gatekeeper/teams/"+team.TeamID)

	b, _ := json.Marshal(inviteRequest{Email: "invitee@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams/"+team.TeamID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleCreateTeamInvite(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Invite
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	t.Cleanup(func() { resp.Remove(context.Background()) })

	if resp.ResourceType != "team" {
		t.Fatalf("expected resource_type=team, got %s", resp.ResourceType)
	}
	if resp.ResourceID != team.TeamID {
		t.Fatalf("expected resource_id=%s, got %s", team.TeamID, resp.ResourceID)
	}
}

// --- handleGetInvite ---

func TestGetInvite_Success(t *testing.T) {
	inviter := createTestUser(t)
	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, "invitee@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites/"+invite.InviteID, nil), inviter.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleGetInvite(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp Invite
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.InviteID != invite.InviteID {
		t.Fatalf("expected invite_id=%s, got %s", invite.InviteID, resp.InviteID)
	}
}

func TestGetInvite_InviteeCanGet(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites/"+invite.InviteID, nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleGetInvite(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetInvite_NonParticipantGets404(t *testing.T) {
	inviter := createTestUser(t)
	stranger := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, "someone@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites/"+invite.InviteID, nil), stranger.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleGetInvite(w, r)

	// Must be 404, not 403, to avoid leaking invite existence to non-participants.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetInvite_NotFound(t *testing.T) {
	actor := createTestUser(t)
	unknownID := uuid.New().String()

	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites/"+unknownID, nil), actor.UserID)
	r.SetPathValue("id", unknownID)
	w := httptest.NewRecorder()
	handleGetInvite(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- handleAcceptInvite ---

func TestAcceptInvite_Success(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify user now has org_id set.
	row, err := (User{UserID: invitee.UserID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updated := row.(User)
	if updated.OrgID == nil || *updated.OrgID != org.OrgID {
		t.Fatalf("expected org_id=%s, got %v", org.OrgID, updated.OrgID)
	}

	// Verify invite status is accepted.
	inviteRow, err := (Invite{InviteID: invite.InviteID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inviteRow.(Invite).Status != "accepted" {
		t.Fatalf("expected invite status=accepted, got %s", inviteRow.(Invite).Status)
	}
}

func TestAcceptOrgInvite_GrantsReadPermission(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	ok, err := checkPermissions(context.Background(), invitee.UserID, "gatekeeper", "getOrg", "gatekeeper/orgs/"+org.OrgID)
	if err != nil {
		t.Fatalf("checkPermissions error: %v", err)
	}
	if !ok {
		t.Fatal("expected invitee to have getOrg permission after accepting org invite")
	}
}

func TestAcceptOrgInvite_NoRole_GrantsReadPermission(t *testing.T) {
	// Invitee starts with no direct role — grantPermissions must create one.
	inviter := createTestUser(t)
	invitee := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "norole-" + uuid.New().String(),
		HashedPassword: "hash",
	}
	if err := invitee.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { invitee.Remove(context.Background()) })

	org := Org{OrgID: uuid.New().String(), OrgName: "norole-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	ok, err := checkPermissions(context.Background(), invitee.UserID, "gatekeeper", "getOrg", "gatekeeper/orgs/"+org.OrgID)
	if err != nil {
		t.Fatalf("checkPermissions error: %v", err)
	}
	if !ok {
		t.Fatal("expected invitee (no prior role) to have getOrg permission after accepting org invite")
	}
}

func TestAcceptTeamInvite_GrantsReadPermission(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	team := Team{TeamID: uuid.New().String(), TeamName: "test-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	invite := makeTeamInvite(t, inviter.UserID, invitee.Email, team.TeamID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	ok, err := checkPermissions(context.Background(), invitee.UserID, "gatekeeper", "getTeam", "gatekeeper/teams/"+team.TeamID)
	if err != nil {
		t.Fatalf("checkPermissions error: %v", err)
	}
	if !ok {
		t.Fatal("expected invitee to have getTeam permission after accepting team invite")
	}
}

func TestAcceptInvite_NotInvitee(t *testing.T) {
	inviter := createTestUser(t)
	stranger := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, "someone@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), stranger.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestAcceptInvite_AlreadyAccepted(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "accepted")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestAcceptInvite_Expired(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(-1*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/accept", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", w.Code)
	}
}

// --- handleDeclineInvite ---

func TestDeclineInvite_Success(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/decline", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleDeclineInvite(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	inviteRow, err := (Invite{InviteID: invite.InviteID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inviteRow.(Invite).Status != "declined" {
		t.Fatalf("expected invite status=declined, got %s", inviteRow.(Invite).Status)
	}
}

func TestDeclineInvite_NotInvitee(t *testing.T) {
	inviter := createTestUser(t)
	stranger := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, "someone@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/decline", nil), stranger.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleDeclineInvite(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- handleDeleteInvite ---

func TestDeleteInvite_Success(t *testing.T) {
	inviter := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, "invitee@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/invites/"+invite.InviteID, nil), inviter.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleDeleteInvite(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify invite is soft-deleted (no longer retrievable).
	if _, err := (Invite{InviteID: invite.InviteID}).Get(context.Background()); err == nil {
		t.Fatal("expected invite to be inaccessible after delete")
	}
}

func TestDeleteInvite_NotInviter(t *testing.T) {
	inviter := createTestUser(t)
	stranger := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, "invitee@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/invites/"+invite.InviteID, nil), stranger.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleDeleteInvite(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeclineInvite_AlreadyDeclined(t *testing.T) {
	inviter := createTestUser(t)
	invitee := createTestUser(t)

	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	invite := makeOrgInvite(t, inviter.UserID, invitee.Email, org.OrgID, time.Now().Add(7*24*time.Hour), "declined")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+invite.InviteID+"/decline", nil), invitee.UserID)
	r.SetPathValue("id", invite.InviteID)
	w := httptest.NewRecorder()
	handleDeclineInvite(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestDeleteInvite_NotFound(t *testing.T) {
	inviter := createTestUser(t)
	id := uuid.New().String()

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/invites/"+id, nil), inviter.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteInvite(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestAcceptInvite_NotFound(t *testing.T) {
	actor := createTestUser(t)
	id := uuid.New().String()

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+id+"/accept", nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleAcceptInvite(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeclineInvite_NotFound(t *testing.T) {
	actor := createTestUser(t)
	id := uuid.New().String()

	r := withUserID(httptest.NewRequest(http.MethodPost, "/invites/"+id+"/decline", nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeclineInvite(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- handleListInvites ---

func TestListInvites_Success(t *testing.T) {
	// actor is the inviter so the caller-scoped query returns their own invites
	actor := createAuthorizedUser(t, "listInvite", "gatekeeper/invites")
	org := Org{OrgID: uuid.New().String(), OrgName: "list-inv-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	makeOrgInvite(t, actor.UserID, "a@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")
	makeOrgInvite(t, actor.UserID, "b@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListInvites(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Invite
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 invites, got %d", len(resp))
	}
}

func TestListInvites_FilterByStatus(t *testing.T) {
	actor := createAuthorizedUser(t, "listInvite", "gatekeeper/invites")
	org := Org{OrgID: uuid.New().String(), OrgName: "status-inv-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	makeOrgInvite(t, actor.UserID, "a@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "pending")
	makeOrgInvite(t, actor.UserID, "b@example.com", org.OrgID, time.Now().Add(7*24*time.Hour), "accepted")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites?status=pending", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListInvites(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Invite
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 invite with status=pending, got %d", len(resp))
	}
	if resp[0].Status != "pending" {
		t.Fatalf("expected status=pending, got %s", resp[0].Status)
	}
}

func TestListInvites_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListInvites(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestListInvites_InvalidLimit(t *testing.T) {
	actor := createAuthorizedUser(t, "listInvite", "gatekeeper/invites")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/invites?limit=bad", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListInvites(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleCreateTeamInvite additional cases ---

func TestCreateTeamInvite_Forbidden(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "test-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	actor := createTestUser(t)

	b, _ := json.Marshal(inviteRequest{Email: "invitee@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams/"+team.TeamID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleCreateTeamInvite(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCreateTeamInvite_TeamNotFound(t *testing.T) {
	teamID := uuid.New().String()
	actor := createAuthorizedUser(t, "inviteUser", "gatekeeper/teams/"+teamID)

	b, _ := json.Marshal(inviteRequest{Email: "invitee@example.com"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams/"+teamID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", teamID)
	w := httptest.NewRecorder()
	handleCreateTeamInvite(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCreateTeamInvite_MissingEmail(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "test-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "inviteUser", "gatekeeper/teams/"+team.TeamID)

	b, _ := json.Marshal(inviteRequest{Email: ""})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams/"+team.TeamID+"/invites", bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleCreateTeamInvite(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
