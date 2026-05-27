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

func TestCreateOrg_Success(t *testing.T) {
	actor := createAuthorizedUser(t, "createOrg", "gatekeeper/orgs")

	b, _ := json.Marshal(orgRequest{OrgName: "test-org"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.OrgName != "test-org" {
		t.Fatalf("unexpected org_name: %s", resp.OrgName)
	}
}

func TestCreateOrg_SuccessViaTeamRole(t *testing.T) {
	actor := createAuthorizedUserViaTeam(t, "createOrg", "gatekeeper/orgs")

	b, _ := json.Marshal(orgRequest{OrgName: "team-org"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })
	if resp.OrgName != "team-org" {
		t.Fatalf("unexpected org_name: %s", resp.OrgName)
	}
}

func TestCreateOrg_MissingName(t *testing.T) {
	actor := createAuthorizedUser(t, "createOrg", "gatekeeper/orgs")

	b, _ := json.Marshal(orgRequest{})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateOrg_Forbidden(t *testing.T) {
	actor := createTestUser(t)

	b, _ := json.Marshal(orgRequest{OrgName: "test-org"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCreateOrg_SetsOwnerOrgID(t *testing.T) {
	actor := createAuthorizedUser(t, "createOrg", "gatekeeper/orgs")

	b, _ := json.Marshal(orgRequest{OrgName: "owner-org"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	t.Cleanup(func() { resp.Remove(context.Background()) })

	row, err := (User{UserID: actor.UserID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := row.(User)
	if owner.OrgID == nil || *owner.OrgID != resp.OrgID {
		t.Fatalf("expected actor org_id=%s, got %v", resp.OrgID, owner.OrgID)
	}
}

func TestCreateOrg_OwnerGetsPermissions(t *testing.T) {
	actor := createAuthorizedUser(t, "createOrg", "gatekeeper/orgs")

	b, _ := json.Marshal(orgRequest{OrgName: "perm-org"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader(b)), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var org Org
	json.Unmarshal(w.Body.Bytes(), &org)
	t.Cleanup(func() { org.Remove(context.Background()) })

	// Clean up permissions appended to the actor's role during org creation.
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

	ok, err := checkPermissions(context.Background(), actor.UserID, "gatekeeper", "deleteOrg", "gatekeeper/orgs/"+org.OrgID)
	if err != nil {
		t.Fatalf("checkPermissions error: %v", err)
	}
	if !ok {
		t.Fatal("expected owner to have wildcard permission on created org")
	}
}

func TestGetOrg_Success(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "getOrg", "gatekeeper/orgs/"+org.OrgID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs/"+org.OrgID, nil), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleGetOrg(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OrgID != org.OrgID {
		t.Fatalf("got org_id %s, want %s", resp.OrgID, org.OrgID)
	}
}

func TestGetOrg_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "getOrg", "gatekeeper/orgs/"+id)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetOrg(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetOrg_Forbidden(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "test-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs/"+org.OrgID, nil), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleGetOrg(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestUpdateOrg_Success(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "original"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updateOrg", "gatekeeper/orgs/"+org.OrgID)

	b, _ := json.Marshal(orgRequest{OrgName: "updated"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/orgs/"+org.OrgID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleUpdateOrg(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OrgName != "updated" {
		t.Fatalf("expected org_name updated, got %s", resp.OrgName)
	}
}

func TestUpdateOrg_MissingName(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "original"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updateOrg", "gatekeeper/orgs/"+org.OrgID)

	b, _ := json.Marshal(orgRequest{})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/orgs/"+org.OrgID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleUpdateOrg(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateOrg_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "updateOrg", "gatekeeper/orgs/"+id)

	b, _ := json.Marshal(orgRequest{OrgName: "name"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/orgs/"+id, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleUpdateOrg(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateOrg_Forbidden(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "original"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createTestUser(t)

	b, _ := json.Marshal(orgRequest{OrgName: "updated"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/orgs/"+org.OrgID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleUpdateOrg(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeleteOrg_Success(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "to-delete"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "deleteOrg", "gatekeeper/orgs/"+org.OrgID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/orgs/"+org.OrgID, nil), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleDeleteOrg(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (Org{OrgID: org.OrgID}).Get(context.Background()); err == nil {
		t.Fatal("expected org to be inaccessible after delete")
	}
}

func TestDeleteOrg_SuccessViaTeamRole(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "team-delete-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createAuthorizedUserViaTeam(t, "deleteOrg", "gatekeeper/orgs/"+org.OrgID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/orgs/"+org.OrgID, nil), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleDeleteOrg(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (Org{OrgID: org.OrgID}).Get(context.Background()); err == nil {
		t.Fatal("expected org to be inaccessible after delete")
	}
}

func TestDeleteOrg_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "deleteOrg", "gatekeeper/orgs/"+id)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/orgs/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteOrg(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteOrg_Forbidden(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "test"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/orgs/"+org.OrgID, nil), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleDeleteOrg(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestListOrgs_Success(t *testing.T) {
	ownerID := uuid.New().String()
	o1 := Org{OrgID: uuid.New().String(), OrgName: "list-org-1", OwnerID: ownerID}
	o2 := Org{OrgID: uuid.New().String(), OrgName: "list-org-2", OwnerID: ownerID}
	if err := o1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o1.Remove(context.Background()) })
	if err := o2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o2.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listOrg", "gatekeeper/orgs")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs?owner_id="+ownerID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListOrgs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Fatalf("expected 2 orgs, got %d", len(resp))
	}
}

func TestListOrgs_ExcludesDeleted(t *testing.T) {
	ownerID := uuid.New().String()
	o1 := Org{OrgID: uuid.New().String(), OrgName: "list-del-org-1", OwnerID: ownerID}
	o2 := Org{OrgID: uuid.New().String(), OrgName: "list-del-org-2", OwnerID: ownerID}
	if err := o1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o1.Remove(context.Background()) })
	if err := o2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o2.Remove(context.Background()) })
	o1.Remove(context.Background())

	actor := createAuthorizedUser(t, "listOrg", "gatekeeper/orgs")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs?owner_id="+ownerID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListOrgs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 org after delete, got %d", len(resp))
	}
	if resp[0].OrgID != o2.OrgID {
		t.Fatalf("expected org %s, got %s", o2.OrgID, resp[0].OrgID)
	}
}

func TestListOrgs_FilterByOrgID(t *testing.T) {
	o := Org{OrgID: uuid.New().String(), OrgName: "list-by-id-org"}
	if err := o.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listOrg", "gatekeeper/orgs")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs?org_id="+o.OrgID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListOrgs(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []Org
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 org, got %d", len(resp))
	}
	if resp[0].OrgID != o.OrgID {
		t.Fatalf("expected org_id %s, got %s", o.OrgID, resp[0].OrgID)
	}
}

func TestListOrgs_Forbidden(t *testing.T) {
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListOrgs(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestListOrgs_InvalidLimit(t *testing.T) {
	actor := createAuthorizedUser(t, "listOrg", "gatekeeper/orgs")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/orgs?limit=notanumber", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListOrgs(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDeleteOrg_ClearsMembership(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "membership-org"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })

	member := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "member-" + uuid.New().String(),
		HashedPassword: "hash",
		OrgID:          &org.OrgID,
	}
	if err := member.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { member.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "deleteOrg", "gatekeeper/orgs/"+org.OrgID)
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/orgs/"+org.OrgID, nil), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleDeleteOrg(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	row, err := (User{UserID: member.UserID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row.(User).OrgID != nil {
		t.Fatal("expected org_id to be cleared after org deletion")
	}
}

func TestCreateOrg_InvalidBody(t *testing.T) {
	actor := createAuthorizedUser(t, "createOrg", "gatekeeper/orgs")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/orgs", bytes.NewReader([]byte("not json"))), actor.UserID)
	w := httptest.NewRecorder()
	handleCreateOrg(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateOrg_InvalidBody(t *testing.T) {
	org := Org{OrgID: uuid.New().String(), OrgName: "original"}
	if err := org.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { org.Remove(context.Background()) })
	actor := createAuthorizedUser(t, "updateOrg", "gatekeeper/orgs/"+org.OrgID)

	r := withUserID(httptest.NewRequest(http.MethodPut, "/orgs/"+org.OrgID, bytes.NewReader([]byte("not json"))), actor.UserID)
	r.SetPathValue("id", org.OrgID)
	w := httptest.NewRecorder()
	handleUpdateOrg(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
