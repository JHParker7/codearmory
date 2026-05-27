package main

import (
	"context"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func cleanupSignup(t *testing.T, userID string) {
	t.Helper()
	row, err := (User{UserID: userID}).Get(context.Background())
	if err != nil {
		return
	}
	u := row.(User)
	if u.RoleID != nil {
		rRow, err := (Role{RoleID: *u.RoleID}).Get(context.Background())
		if err == nil {
			role := rRow.(Role)
			for _, pid := range role.PermissionsIDs {
				(Permissions{PermissionsID: pid}).Remove(context.Background())
			}
			role.Remove(context.Background())
		}
	}
	u.Remove(context.Background())
}

// createLoginUser creates a user with a bcrypt-hashed password suitable for login tests.
func createLoginUser(t *testing.T, email, password string) User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("createLoginUser bcrypt: %v", err)
	}
	u := User{
		UserID:         uuid.New().String(),
		Email:          email,
		Username:       "user-" + uuid.New().String(),
		HashedPassword: string(hash),
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("createLoginUser Add: %v", err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })
	return u
}

// --- handleSignup ---

func TestHandleSignup_Success(t *testing.T) {
	req := signupRequest{
		Email:     uuid.New().String() + "@test.com",
		Username:  "user-" + uuid.New().String(),
		Password:  "password123",
		Firstname: "John",
		Lastname:  "Doe",
	}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp signupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Email != req.Email || resp.Username != req.Username {
		t.Fatalf("got %+v, want email=%s username=%s", resp, req.Email, req.Username)
	}
	t.Cleanup(func() { cleanupSignup(t, resp.UserID) })
}

func TestHandleSignup_MissingEmail(t *testing.T) {
	b, _ := json.Marshal(signupRequest{Username: "user", Password: "testpass1"})
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSignup_MissingUsername(t *testing.T) {
	b, _ := json.Marshal(signupRequest{Email: "x@test.com", Password: "testpass1"})
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSignup_MissingPassword(t *testing.T) {
	b, _ := json.Marshal(signupRequest{Email: "x@test.com", Username: "user"})
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSignup_InvalidBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	handleSignup(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSignup_DuplicateEmail(t *testing.T) {
	email := uuid.New().String() + "@test.com"

	b1, _ := json.Marshal(signupRequest{Email: email, Username: "user-" + uuid.New().String(), Password: "testpass1"})
	r1 := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b1))
	w1 := httptest.NewRecorder()
	handleSignup(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first signup expected 201, got %d", w1.Code)
	}
	var resp signupResponse
	json.Unmarshal(w1.Body.Bytes(), &resp)
	t.Cleanup(func() { cleanupSignup(t, resp.UserID) })

	b2, _ := json.Marshal(signupRequest{Email: email, Username: "user-" + uuid.New().String(), Password: "testpass1"})
	r2 := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b2))
	w2 := httptest.NewRecorder()
	handleSignup(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate signup expected 409, got %d", w2.Code)
	}
}

// --- handleLogin ---

func TestHandleLogin_Success(t *testing.T) {
	const password = "securepassword"
	u := createLoginUser(t, uuid.New().String()+"@test.com", password)
	t.Cleanup(func() { connect().Model(&Session{}).Where("user_id = ?", u.UserID).Update("active", false) })

	b, _ := json.Marshal(loginRequest{Email: u.Email, Password: password})
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleLogin(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["token"] == "" {
		t.Fatal("expected non-empty token in response")
	}
}

func TestHandleLogin_WrongPassword(t *testing.T) {
	u := createLoginUser(t, uuid.New().String()+"@test.com", "correctpassword")
	b, _ := json.Marshal(loginRequest{Email: u.Email, Password: "wrongpassword"})
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleLogin_UserNotFound(t *testing.T) {
	b, _ := json.Marshal(loginRequest{Email: "nobody@nowhere.com", Password: "whatever"})
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleLogin_MissingFields(t *testing.T) {
	cases := []loginRequest{
		{Email: "x@test.com"},
		{Password: "testpass1"},
		{},
	}
	for _, body := range cases {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b))
		w := httptest.NewRecorder()
		handleLogin(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %+v, got %d", body, w.Code)
		}
	}
}

func TestHandleLogin_InvalidBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	handleLogin(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- User CRUD ---

func TestGetUser_Success(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "getUser", "gatekeeper/users/"+target.UserID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleGetUser(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.UserID != target.UserID {
		t.Fatalf("got user_id %s, want %s", resp.UserID, target.UserID)
	}
}

func TestGetUser_SuccessViaTeamRole(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUserViaTeam(t, "getUser", "gatekeeper/users/"+target.UserID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleGetUser(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.UserID != target.UserID {
		t.Fatalf("got user_id %s, want %s", resp.UserID, target.UserID)
	}
}

func TestGetUser_ResponseOmitsPassword(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "getUser", "gatekeeper/users/"+target.UserID)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleGetUser(w, r)

	var raw map[string]any
	json.Unmarshal(w.Body.Bytes(), &raw)
	if _, ok := raw["hashed_password"]; ok {
		t.Fatal("response must not include hashed_password")
	}
}

func TestGetUser_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "getUser", "gatekeeper/users/"+id)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/users/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetUser(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetUser_Forbidden(t *testing.T) {
	target := createTestUser(t)
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleGetUser(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestUpdateUser_Success(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "updateUser", "gatekeeper/users/"+target.UserID)

	body := updateUserRequest{
		Email:     uuid.New().String() + "@updated.com",
		Username:  "updated-" + uuid.New().String(),
		Firstname: "Updated",
		Lastname:  "Name",
	}
	b, _ := json.Marshal(body)
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp userResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Username != body.Username {
		t.Fatalf("expected username %s, got %s", body.Username, resp.Username)
	}
}

func TestUpdateUser_SuccessViaTeamRole(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUserViaTeam(t, "updateUser", "gatekeeper/users/"+target.UserID)

	body := updateUserRequest{
		Email:     uuid.New().String() + "@updated.com",
		Username:  "updated-" + uuid.New().String(),
		Firstname: "TeamUpdated",
		Lastname:  "Name",
	}
	b, _ := json.Marshal(body)
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp userResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Username != body.Username {
		t.Fatalf("expected username %s, got %s", body.Username, resp.Username)
	}
}

func TestUpdateUser_MissingEmail(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "updateUser", "gatekeeper/users/"+target.UserID)

	b, _ := json.Marshal(updateUserRequest{Username: "user"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateUser_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "updateUser", "gatekeeper/users/"+id)

	b, _ := json.Marshal(updateUserRequest{Email: "x@test.com", Username: "user"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+id, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateUser_Forbidden(t *testing.T) {
	target := createTestUser(t)
	actor := createTestUser(t)

	b, _ := json.Marshal(updateUserRequest{Email: "x@test.com", Username: "user"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestDeleteUser_Success(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "deleteUser", "gatekeeper/users/"+target.UserID)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleDeleteUser(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := (User{UserID: target.UserID}).Get(context.Background()); err == nil {
		t.Fatal("expected user to be inaccessible after delete")
	}
}

func TestDeleteUser_NotFound(t *testing.T) {
	id := uuid.New().String()
	actor := createAuthorizedUser(t, "deleteUser", "gatekeeper/users/"+id)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/users/"+id, nil), actor.UserID)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteUser(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteUser_InvalidatesSessions(t *testing.T) {
	target := createAuthorizedUser(t, "deleteUser", "gatekeeper/users/"+uuid.New().String())
	_, session := makeSession(t, target.UserID, time.Now().Add(24*time.Hour))

	actor := createAuthorizedUser(t, "deleteUser", "gatekeeper/users/"+target.UserID)
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleDeleteUser(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := (Session{SessionID: session.SessionID}).Get(context.Background()); err == nil {
		t.Fatal("expected session to be inactive after user deletion")
	}
}

func TestDeleteUser_Forbidden(t *testing.T) {
	target := createTestUser(t)
	actor := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodDelete, "/users/"+target.UserID, nil), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleDeleteUser(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- handleUpdateUser additional edge cases ---

func TestUpdateUser_MissingUsername(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "updateUser", "gatekeeper/users/"+target.UserID)

	b, _ := json.Marshal(updateUserRequest{Email: "x@test.com"})
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing username, got %d", w.Code)
	}
}

func TestUpdateUser_InvalidBody(t *testing.T) {
	target := createTestUser(t)
	actor := createAuthorizedUser(t, "updateUser", "gatekeeper/users/"+target.UserID)

	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader([]byte("not json"))), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d", w.Code)
	}
}

func TestUpdateUser_PasswordChange(t *testing.T) {
	const oldPassword = "oldpassword"
	target := createLoginUser(t, uuid.New().String()+"@test.com", oldPassword)
	actor := createAuthorizedUser(t, "updateUser", "gatekeeper/users/"+target.UserID)

	const newPassword = "newpassword"
	body := updateUserRequest{
		Email:    target.Email,
		Username: target.Username,
		Password: newPassword,
	}
	b, _ := json.Marshal(body)
	r := withUserID(httptest.NewRequest(http.MethodPut, "/users/"+target.UserID, bytes.NewReader(b)), actor.UserID)
	r.SetPathValue("id", target.UserID)
	w := httptest.NewRecorder()
	handleUpdateUser(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the password was actually updated: the new hash should accept newPassword.
	row, err := (User{UserID: target.UserID}).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updated := row.(User)

	// New password should work.
	b2, _ := json.Marshal(loginRequest{Email: updated.Email, Password: newPassword})
	rLogin := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b2))
	wLogin := httptest.NewRecorder()
	handleLogin(wLogin, rLogin)
	if wLogin.Code != http.StatusOK {
		t.Fatalf("login with new password expected 200, got %d", wLogin.Code)
	}
	t.Cleanup(func() { connect().Model(&Session{}).Where("user_id = ?", target.UserID).Update("active", false) })
}

// --- handleListUsers ---

func TestListUsers_Success(t *testing.T) {
	orgID := uuid.New().String()
	u1 := User{
		UserID: uuid.New().String(), Email: uuid.New().String() + "@test.com",
		Username: uuid.New().String(), HashedPassword: "hash", OrgID: &orgID,
	}
	u2 := User{
		UserID: uuid.New().String(), Email: uuid.New().String() + "@test.com",
		Username: uuid.New().String(), HashedPassword: "hash", OrgID: &orgID,
	}
	if err := u1.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u1.Remove(context.Background()) })
	if err := u2.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u2.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listUser", "gatekeeper/users")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/users?org_id="+orgID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListUsers(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp))
	}
}

func TestListUsers_FilterByUsername(t *testing.T) {
	username := "filterbyusername-" + uuid.New().String()
	u := User{
		UserID: uuid.New().String(), Email: uuid.New().String() + "@test.com",
		Username: username, HashedPassword: "hash",
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) })

	actor := createAuthorizedUser(t, "listUser", "gatekeeper/users")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/users?username="+username, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListUsers(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []userResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("expected 1 user, got %d", len(resp))
	}
	if resp[0].Username != username {
		t.Fatalf("expected username %s, got %s", username, resp[0].Username)
	}
}

func TestListUsers_Forbidden(t *testing.T) {
	actor := createTestUser(t)
	r := withUserID(httptest.NewRequest(http.MethodGet, "/users", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListUsers(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestListUsers_InvalidLimit(t *testing.T) {
	actor := createAuthorizedUser(t, "listUser", "gatekeeper/users")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/users?limit=bad", nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListUsers(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestListUsers_ResponseOmitsPassword(t *testing.T) {
	u := createTestUser(t)
	actor := createAuthorizedUser(t, "listUser", "gatekeeper/users")
	r := withUserID(httptest.NewRequest(http.MethodGet, "/users?user_id="+u.UserID, nil), actor.UserID)
	w := httptest.NewRecorder()
	handleListUsers(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var raw []map[string]any
	json.Unmarshal(w.Body.Bytes(), &raw)
	if len(raw) == 0 {
		t.Fatal("expected at least one user in response")
	}
	if _, ok := raw[0]["hashed_password"]; ok {
		t.Fatal("response must not include hashed_password")
	}
}

func TestHandleSignup_DuplicateUsername(t *testing.T) {
	username := "user-" + uuid.New().String()

	b1, _ := json.Marshal(signupRequest{
		Email:    uuid.New().String() + "@test.com",
		Username: username,
		Password: "testpass1",
	})
	r1 := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b1))
	w1 := httptest.NewRecorder()
	handleSignup(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first signup expected 201, got %d", w1.Code)
	}
	var resp signupResponse
	json.Unmarshal(w1.Body.Bytes(), &resp)
	t.Cleanup(func() { cleanupSignup(t, resp.UserID) })

	b2, _ := json.Marshal(signupRequest{
		Email:    uuid.New().String() + "@test.com",
		Username: username,
		Password: "testpass1",
	})
	r2 := httptest.NewRequest(http.MethodPost, "/signup", bytes.NewReader(b2))
	w2 := httptest.NewRecorder()
	handleSignup(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate username signup expected 409, got %d", w2.Code)
	}
}
