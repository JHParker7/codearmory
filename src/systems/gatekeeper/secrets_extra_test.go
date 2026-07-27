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

// authedUserWithOrg makes an authorized user that belongs to a (fresh) org.
func authedUserWithOrg(t *testing.T, action, resource string) (User, string) {
	t.Helper()
	org := Org{OrgID: uuid.NewString(), OrgName: "org-" + uuid.NewString()[:8], CreatedAt: time.Now().UTC()}
	if err := org.Add(context.Background()); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("org_id = ?", org.OrgID).Delete(&Org{}) }) //nolint:errcheck
	u := createAuthorizedUser(t, action, resource)
	if err := gormDB.Model(&User{}).Where("user_id = ?", u.UserID).Update("org_id", org.OrgID).Error; err != nil {
		t.Fatalf("set user org: %v", err)
	}
	return u, org.OrgID
}

func seedSecret(t *testing.T, orgID, name string) Secret {
	t.Helper()
	ct, err := encryptSecret("v-" + uuid.NewString())
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	s := Secret{SecretID: uuid.NewString(), OrgID: orgID, Name: name, Ciphertext: ct, CreatedBy: "u1", Active: true, CreatedAt: time.Now().UTC()}
	if err := s.Add(context.Background()); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("secret_id = ?", s.SecretID).Delete(&Secret{}) }) //nolint:errcheck
	return s
}

func TestHandleCreateSecret(t *testing.T) {
	u, _ := authedUserWithOrg(t, "createSecret", "gatekeeper/secrets")
	post := func(body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := withUserID(httptest.NewRequest(http.MethodPost, "/secrets", bytes.NewReader(b)), u.UserID)
		w := httptest.NewRecorder()
		handleCreateSecret(w, r)
		return w
	}
	if w := post(map[string]any{"name": "MY_SECRET", "value": "s3cr3t"}); w.Code != http.StatusCreated {
		t.Fatalf("create got %d, want 201: %s", w.Code, w.Body.String())
	}
	if w := post(map[string]any{"name": "MY_SECRET", "value": "again"}); w.Code != http.StatusConflict {
		t.Errorf("duplicate got %d, want 409", w.Code)
	}
	if w := post(map[string]any{"name": "bad name!", "value": "x"}); w.Code != http.StatusBadRequest {
		t.Errorf("bad name got %d, want 400", w.Code)
	}
	if w := post(map[string]any{"name": "X"}); w.Code != http.StatusBadRequest {
		t.Errorf("missing value got %d, want 400", w.Code)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("name = ?", "MY_SECRET").Delete(&Secret{}) }) //nolint:errcheck
}

// TestHandleCreateSecret_NoOrg verifies an org-less caller creates a *personal*
// secret (org_id = ”, created_by = caller) rather than being rejected — so a solo
// user can hold credentials without an org.
func TestHandleCreateSecret_NoOrg(t *testing.T) {
	u := createAuthorizedUser(t, "createSecret", "gatekeeper/secrets") // user has no org
	name := "PERSONAL_" + uuid.NewString()[:8]
	t.Cleanup(func() { gormDB.Unscoped().Where("name = ?", name).Delete(&Secret{}) }) //nolint:errcheck
	b, _ := json.Marshal(map[string]any{"name": name, "value": "y"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/secrets", bytes.NewReader(b)), u.UserID)
	w := httptest.NewRecorder()
	handleCreateSecret(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("no-org caller got %d, want 201: %s", w.Code, w.Body.String())
	}
	var got Secret
	if err := gormDB.Where("name = ? AND active = true", name).First(&got).Error; err != nil {
		t.Fatalf("personal secret not stored: %v", err)
	}
	if got.OrgID != "" || got.CreatedBy != u.UserID {
		t.Fatalf("personal secret scope wrong: org_id=%q created_by=%q (want org_id='' created_by=%q)", got.OrgID, got.CreatedBy, u.UserID)
	}
}

func TestHandleListSecrets(t *testing.T) {
	u, orgID := authedUserWithOrg(t, "listSecret", "gatekeeper/secrets")
	seedSecret(t, orgID, "LISTED")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/secrets", nil), u.UserID)
	w := httptest.NewRecorder()
	handleListSecrets(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list got %d, want 200", w.Code)
	}
}

func TestHandleUpdateAndDeleteSecret(t *testing.T) {
	_, orgID := authedUserWithOrg(t, "x", "y")
	s := seedSecret(t, orgID, "UPD")

	upd := createAuthorizedUser(t, "updateSecret", "gatekeeper/secrets/"+s.SecretID)
	gormDB.Model(&User{}).Where("user_id = ?", upd.UserID).Update("org_id", orgID) //nolint:errcheck
	b, _ := json.Marshal(map[string]any{"value": "new-value"})
	ur := withUserID(httptest.NewRequest(http.MethodPut, "/secrets/"+s.SecretID, bytes.NewReader(b)), upd.UserID)
	ur.SetPathValue("id", s.SecretID)
	uw := httptest.NewRecorder()
	handleUpdateSecret(uw, ur)
	if uw.Code != http.StatusOK && uw.Code != http.StatusNoContent {
		t.Fatalf("update got %d, want 2xx: %s", uw.Code, uw.Body.String())
	}

	del := createAuthorizedUser(t, "deleteSecret", "gatekeeper/secrets/"+s.SecretID)
	gormDB.Model(&User{}).Where("user_id = ?", del.UserID).Update("org_id", orgID) //nolint:errcheck
	dr := withUserID(httptest.NewRequest(http.MethodDelete, "/secrets/"+s.SecretID, nil), del.UserID)
	dr.SetPathValue("id", s.SecretID)
	dw := httptest.NewRecorder()
	handleDeleteSecret(dw, dr)
	if dw.Code != http.StatusNoContent && dw.Code != http.StatusOK {
		t.Fatalf("delete got %d, want 2xx", dw.Code)
	}
}
