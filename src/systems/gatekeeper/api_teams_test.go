package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCreateTeam_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "createTeam", "gatekeeper/teams")

	b, _ := json.Marshal(teamRequest{TeamName: "test-team"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.TeamName != "test-team" {
		t.Fatalf("unexpected team_name: %s", resp.TeamName)
	}
}

func TestCreateTeam_MissingName(t *testing.T) {
	actor := createAuthorizedUser(t, "createTeam", "gatekeeper/teams")

	b, _ := json.Marshal(teamRequest{})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateTeam_Forbidden(t *testing.T) {
	actor := createTestUser(t)

	b, _ := json.Marshal(teamRequest{TeamName: "test-team"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCreateTeam_SetsOwnerTeamId(t *testing.T) {
	actor := createAuthorizedUser(t, "createTeam", "gatekeeper/teams")

	b, _ := json.Marshal(teamRequest{TeamName: "owner-team"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })

	row, err := (User{UserID: actor.UserID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := row.(User)
	if owner.TeamID == nil || *owner.TeamID != resp.TeamID {
		t.Fatalf("expected actor team_id=%s, got %v", resp.TeamID, owner.TeamID)
	}
}

func TestCreateTeam_WithExplicitRoleID(t *testing.T) {
	actor := createAuthorizedUser(t, "createTeam", "gatekeeper/teams")

	existingRole := Role{RoleID: uuid.New().String(), PermissionsIDs: []string{}}
	if err := existingRole.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { existingRole.Remove(context.Background()) })

	b, _ := json.Marshal(teamRequest{TeamName: "role-team", RoleID: &existingRole.RoleID})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.RoleID == nil || *resp.RoleID != existingRole.RoleID {
		t.Fatalf("expected role_id %s, got %v", existingRole.RoleID, resp.RoleID)
	}
}

func TestCreateTeam_OwnerGetsPermissions(t *testing.T) {
	actor := createAuthorizedUser(t, "createTeam", "gatekeeper/teams")

	b, _ := json.Marshal(teamRequest{TeamName: "perm-team"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var team Team
	json.Unmarshal(w.Body.Bytes(), &team)
	t.Cleanup(func() { team.Remove(context.Background()) })

	// Clean up the extra permission appended to the actor's role during team creation.
	t.Cleanup(func() {
		if actor.RoleID == nil {
			return
		}
		row, err := (Role{RoleID: *actor.RoleID}).Get(context.Background())
		if err != nil {
			return
		}
		for _, pid := range row.(Role).PermissionsIDs {
			(Permissions{PermissionsID: pid}).Remove(context.Background())
		}
	})

	ok, err := checkPermissions(context.Background(), actor.UserID, "gatekeeper", "getTeam", "gatekeeper/teams/"+team.TeamID)
	if err != nil {
		t.Fatalf("checkPermissions error: %v", err)
	}
	if !ok {
		t.Fatal("expected owner to have getTeam permission on created team")
	}
}

func TestCreateTeam_OwnerGetsPermissionsViaTeamRole(t *testing.T) {
	// createAuthorizedUserViaTeam gives the actor permission via a team role with no direct
	// RoleID, so handleCreateTeam must create a new direct role for the owner.
	actor := createAuthorizedUserViaTeam(t, "createTeam", "gatekeeper/teams")

	b, _ := json.Marshal(teamRequest{TeamName: "nil-role-team"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateTeam(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var team Team
	json.Unmarshal(w.Body.Bytes(), &team)
	t.Cleanup(func() { team.Remove(context.Background()) })

	// handleCreateTeam created a new direct role for the owner; clean it up.
	t.Cleanup(func() {
		row, err := (User{UserID: actor.UserID}).Get(context.Background())
		if err != nil {
			return
		}
		owner := row.(User)
		if owner.RoleID == nil {
			return
		}
		roleRow, err := (Role{RoleID: *owner.RoleID}).Get(context.Background())
		if err != nil {
			return
		}
		for _, pid := range roleRow.(Role).PermissionsIDs {
			(Permissions{PermissionsID: pid}).Remove(context.Background())
		}
		(Role{RoleID: *owner.RoleID}).Remove(context.Background())
	})

	ok, err := checkPermissions(context.Background(), actor.UserID, "gatekeeper", "getTeam", "gatekeeper/teams/"+team.TeamID)
	if err != nil {
		t.Fatalf("checkPermissions error: %v", err)
	}
	if !ok {
		t.Fatal("expected owner to have permission on created team even when starting with no direct role")
	}
}

func TestGetTeam_Success(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "test-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "getTeam", "gatekeeper/teams/"+team.TeamID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams/"+team.TeamID, nil), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleGetTeam(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TeamID != team.TeamID {
		t.Fatalf("got team_id %s, want %s", resp.TeamID, team.TeamID)
	}
}

func TestGetTeam_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "getTeam", "gatekeeper/teams/"+id)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetTeam(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetTeam_Forbidden(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "test-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams/"+team.TeamID, nil), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleGetTeam(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestUpdateTeam_Success(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "original"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updateTeam", "gatekeeper/teams/"+team.TeamID)

	b, _ := json.Marshal(teamRequest{TeamName: "updated"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/teams/"+team.TeamID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleUpdateTeam(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TeamName != "updated" {
		t.Fatalf("expected team_name updated, got %s", resp.TeamName)
	}
}

func TestUpdateTeam_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "updateTeam", "gatekeeper/teams/"+id)

	b, _ := json.Marshal(teamRequest{TeamName: "name"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/teams/"+id, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleUpdateTeam(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateTeam_Forbidden(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "original"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })
	actor := createTestUser(t)

	b, _ := json.Marshal(teamRequest{TeamName: "updated"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/teams/"+team.TeamID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleUpdateTeam(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeleteTeam_Success(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "to-delete"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "deleteTeam", "gatekeeper/teams/"+team.TeamID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/teams/"+team.TeamID, nil), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleDeleteTeam(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (Team{TeamID: team.TeamID}).Get(context.Background()); err == nil {
		t.Fatal("expected team to be inaccessible after delete")
	}
}

func TestDeleteTeam_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "deleteTeam", "gatekeeper/teams/"+id)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/teams/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteTeam(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteTeam_Forbidden(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "test"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/teams/"+team.TeamID, nil), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleDeleteTeam(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestListTeams_Success(t *testing.T) {
	ownerID := uuid.New().String()
	tm1 := Team{TeamID: uuid.New().String(), TeamName: "list-team-1", OwnerID: ownerID}
	tm2 := Team{TeamID: uuid.New().String(), TeamName: "list-team-2", OwnerID: ownerID}
	if err := tm1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm1.Remove(context.Background()) })
	if err := tm2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm2.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listTeam", "gatekeeper/teams")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams?owner_id="+ownerID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListTeams(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Fatalf("expected 2 teams, got %d", len(resp))
	}
}

func TestListTeams_ExcludesDeleted(t *testing.T) {
	ownerID := uuid.New().String()
	tm1 := Team{TeamID: uuid.New().String(), TeamName: "list-del-team-1", OwnerID: ownerID}
	tm2 := Team{TeamID: uuid.New().String(), TeamName: "list-del-team-2", OwnerID: ownerID}
	if err := tm1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm1.Remove(context.Background()) })
	if err := tm2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm2.Remove(context.Background()) })
	tm1.Remove(context.Background())

	actor := createAuthorizedUser(t, "listTeam", "gatekeeper/teams")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams?owner_id="+ownerID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListTeams(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 team after delete, got %d", len(resp))
	}
	if resp[0].TeamID != tm2.TeamID {
		t.Fatalf("expected team %s, got %s", tm2.TeamID, resp[0].TeamID)
	}
}

func TestListTeams_FilterByTeamID(t *testing.T) {
	tm := Team{TeamID: uuid.New().String(), TeamName: "list-by-id-team"}
	if err := tm.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listTeam", "gatekeeper/teams")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams?team_id="+tm.TeamID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListTeams(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Team
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 team, got %d", len(resp))
	}
	if resp[0].TeamID != tm.TeamID {
		t.Fatalf("expected team_id %s, got %s", tm.TeamID, resp[0].TeamID)
	}
}

func TestListTeams_Forbidden(t *testing.T) {
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListTeams(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestListTeams_InvalidLimit(t *testing.T) {
	actor := createAuthorizedUser(t, "listTeam", "gatekeeper/teams")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/teams?limit=notanumber", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListTeams(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDeleteTeam_ClearsMembership(t *testing.T) {
	team := Team{TeamID: uuid.New().String(), TeamName: "membership-team"}
	if err := team.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { team.Remove(context.Background()) })

	member := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "member-" + uuid.New().String(),
		HashedPassword: "hash",
		TeamID:         &team.TeamID,
	}
	if err := member.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { member.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "deleteTeam", "gatekeeper/teams/"+team.TeamID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/teams/"+team.TeamID, nil), actor.UserID)
	r.SetPathValue("id", team.TeamID)
	w := httptest.NewRecorder()
	handleDeleteTeam(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	row, err := (User{UserID: member.UserID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row.(User).TeamID != nil {
		t.Fatal("expected team_id to be cleared after team deletion")
	}
}
