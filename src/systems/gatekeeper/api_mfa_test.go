package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

// createTOTPUser creates a user with a bcrypt-hashed password and a confirmed
// TOTP credential. Returns the user and the plaintext TOTP secret so tests can
// call validTOTPCode to produce valid codes.
func createTOTPUser(t *testing.T, password string) (User, string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("createTOTPUser bcrypt: %v", err)
	}
	u := User{
		UserID:         uuid.New().String(),
		Email:          uuid.New().String() + "@test.com",
		Username:       "user-" + uuid.New().String(),
		HashedPassword: string(hash),
	}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("createTOTPUser Add: %v", err)
	}
	t.Cleanup(func() { u.Remove(context.Background()) }) //nolint:errcheck

	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: u.Email})
	if err != nil {
		t.Fatalf("createTOTPUser generate: %v", err)
	}
	encSecret, err := encryptSecret(key.Secret())
	if err != nil {
		t.Fatalf("createTOTPUser encryptSecret: %v", err)
	}
	cred := TOTPCredential{
		CredentialID: uuid.New().String(),
		UserID:       u.UserID,
		EncSecret:    encSecret,
		Confirmed:    true,
		Active:       true,
	}
	if err := cred.Add(context.Background()); err != nil {
		t.Fatalf("createTOTPUser cred Add: %v", err)
	}
	t.Cleanup(func() {
		connect().Where("user_id = ?", u.UserID).Delete(&TOTPCredential{}) //nolint:errcheck
	})
	return u, key.Secret()
}

// insertUnconfirmedTOTP inserts an unconfirmed TOTP credential for userID and
// returns the plaintext secret.
func insertUnconfirmedTOTP(t *testing.T, userID, email string) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: email})
	if err != nil {
		t.Fatalf("insertUnconfirmedTOTP generate: %v", err)
	}
	encSecret, err := encryptSecret(key.Secret())
	if err != nil {
		t.Fatalf("insertUnconfirmedTOTP encryptSecret: %v", err)
	}
	cred := TOTPCredential{
		CredentialID: uuid.New().String(),
		UserID:       userID,
		EncSecret:    encSecret,
		Confirmed:    false,
		Active:       true,
	}
	if err := cred.Add(context.Background()); err != nil {
		t.Fatalf("insertUnconfirmedTOTP Add: %v", err)
	}
	t.Cleanup(func() {
		connect().Where("user_id = ?", userID).Delete(&TOTPCredential{}) //nolint:errcheck
	})
	return key.Secret()
}

// validTOTPCode generates a valid TOTP code for secret at the current time.
func validTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("validTOTPCode: %v", err)
	}
	return code
}

// createTestOAuthClient inserts an OAuthClient and returns it along with the
// plaintext secret.
func createTestOAuthClient(t *testing.T) (OAuthClient, string) {
	t.Helper()
	rawSecret := uuid.New().String()
	hash, err := bcrypt.GenerateFromPassword([]byte(rawSecret), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("createTestOAuthClient bcrypt: %v", err)
	}
	client := OAuthClient{
		ClientID:     uuid.New().String(),
		Name:         "Test App",
		SecretHash:   string(hash),
		RedirectURIs: []string{"https://example.com/callback"},
		Active:       true,
		CreatedAt:    time.Now().UTC(),
	}
	if err := connect().Create(&client).Error; err != nil {
		t.Fatalf("createTestOAuthClient create: %v", err)
	}
	t.Cleanup(func() { connect().Delete(&client) }) //nolint:errcheck
	return client, rawSecret
}

// createOAuthMFAPending inserts a fresh MFAPending with the given OAuth params.
func createOAuthMFAPending(t *testing.T, userID, clientID, redirectURI, state, scope string) MFAPending {
	t.Helper()
	p := MFAPending{
		Token:            uuid.New().String(),
		UserID:           userID,
		ExpiresAt:        time.Now().Add(2 * time.Minute).UTC(),
		CreatedAt:        time.Now().UTC(),
		OAuthClientID:    clientID,
		OAuthRedirectURI: redirectURI,
		OAuthState:       state,
		OAuthScope:       scope,
	}
	if err := connect().Create(&p).Error; err != nil {
		t.Fatalf("createOAuthMFAPending: %v", err)
	}
	t.Cleanup(func() { connect().Where("token = ?", p.Token).Delete(&MFAPending{}) }) //nolint:errcheck
	return p
}

// ── handleTOTPEnroll ──────────────────────────────────────────────────────────

func TestHandleTOTPEnroll_Success(t *testing.T) {
	u := createTestUser(t)
	t.Cleanup(func() { connect().Where("user_id = ?", u.UserID).Delete(&TOTPCredential{}) }) //nolint:errcheck

	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/enroll", nil), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPEnroll(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["uri"] == "" || resp["secret"] == "" {
		t.Fatalf("expected uri and secret in response, got %+v", resp)
	}
	if !strings.HasPrefix(resp["uri"], "otpauth://") {
		t.Errorf("expected otpauth:// URI, got %q", resp["uri"])
	}
}

func TestHandleTOTPEnroll_AlreadyEnabled(t *testing.T) {
	u, _ := createTOTPUser(t, "password")

	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/enroll", nil), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPEnroll(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 when TOTP already enabled, got %d", w.Code)
	}
}

func TestHandleTOTPEnroll_ReplacesUnconfirmed(t *testing.T) {
	u := createTestUser(t)
	t.Cleanup(func() { connect().Where("user_id = ?", u.UserID).Delete(&TOTPCredential{}) }) //nolint:errcheck
	insertUnconfirmedTOTP(t, u.UserID, u.Email)

	// Second enroll should succeed and replace the first unconfirmed credential.
	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/enroll", nil), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPEnroll(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 re-enrolling with unconfirmed pending, got %d: %s", w.Code, w.Body.String())
	}

	// Only one active credential should remain.
	var count int64
	connect().Model(&TOTPCredential{}).Where("user_id = ? AND active = ?", u.UserID, true).Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 active credential, got %d", count)
	}
}

// ── handleTOTPConfirm ─────────────────────────────────────────────────────────

func TestHandleTOTPConfirm_Success(t *testing.T) {
	u := createTestUser(t)
	secret := insertUnconfirmedTOTP(t, u.UserID, u.Email)

	code := validTOTPCode(t, secret)
	body, _ := json.Marshal(map[string]string{"code": code})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/confirm", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPConfirm(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	var cred TOTPCredential
	connect().Where("user_id = ? AND active = ?", u.UserID, true).First(&cred)
	if !cred.Confirmed {
		t.Fatal("expected credential to be marked confirmed")
	}
}

func TestHandleTOTPConfirm_InvalidCode(t *testing.T) {
	u := createTestUser(t)
	insertUnconfirmedTOTP(t, u.UserID, u.Email)

	body, _ := json.Marshal(map[string]string{"code": "000000"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/confirm", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPConfirm(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid code, got %d", w.Code)
	}
}

func TestHandleTOTPConfirm_NoPendingEnrollment(t *testing.T) {
	u := createTestUser(t)

	body, _ := json.Marshal(map[string]string{"code": "123456"})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/confirm", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPConfirm(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 with no pending enrollment, got %d", w.Code)
	}
}

func TestHandleTOTPConfirm_MissingCode(t *testing.T) {
	u := createTestUser(t)
	insertUnconfirmedTOTP(t, u.UserID, u.Email)

	body, _ := json.Marshal(map[string]string{})
	r := withUserID(httptest.NewRequest(http.MethodPost, "/mfa/totp/confirm", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPConfirm(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing code, got %d", w.Code)
	}
}

// ── handleTOTPStatus ──────────────────────────────────────────────────────────

func TestHandleTOTPStatus_Enabled(t *testing.T) {
	u, _ := createTOTPUser(t, "password")

	r := withUserID(httptest.NewRequest(http.MethodGet, "/mfa/totp/status", nil), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !resp["enabled"] {
		t.Fatal("expected enabled=true for user with confirmed TOTP")
	}
}

func TestHandleTOTPStatus_Disabled(t *testing.T) {
	u := createTestUser(t)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/mfa/totp/status", nil), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]bool
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["enabled"] {
		t.Fatal("expected enabled=false for user without TOTP")
	}
}

func TestHandleTOTPStatus_UnconfirmedNotEnabled(t *testing.T) {
	u := createTestUser(t)
	insertUnconfirmedTOTP(t, u.UserID, u.Email)

	r := withUserID(httptest.NewRequest(http.MethodGet, "/mfa/totp/status", nil), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPStatus(w, r)

	var resp map[string]bool
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["enabled"] {
		t.Fatal("unconfirmed credential must not count as enabled")
	}
}

// ── handleTOTPDisable ─────────────────────────────────────────────────────────

func TestHandleTOTPDisable_Success(t *testing.T) {
	const password = "disablepassword"
	u, _ := createTOTPUser(t, password)

	body, _ := json.Marshal(map[string]string{"password": password})
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/mfa/totp", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPDisable(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if userHasTOTP(context.Background(), u.UserID) {
		t.Fatal("expected TOTP to be disabled after successful disable")
	}
}

func TestHandleTOTPDisable_WrongPassword(t *testing.T) {
	u, _ := createTOTPUser(t, "correctpassword")

	body, _ := json.Marshal(map[string]string{"password": "wrongpassword"})
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/mfa/totp", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPDisable(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", w.Code)
	}
	if !userHasTOTP(context.Background(), u.UserID) {
		t.Fatal("TOTP should still be enabled after failed disable attempt")
	}
}

func TestHandleTOTPDisable_NotEnabled(t *testing.T) {
	const password = "testpassword"
	u := createLoginUser(t, uuid.New().String()+"@test.com", password)

	body, _ := json.Marshal(map[string]string{"password": password})
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/mfa/totp", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPDisable(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when TOTP not enabled, got %d", w.Code)
	}
}

func TestHandleTOTPDisable_MissingPassword(t *testing.T) {
	u, _ := createTOTPUser(t, "password")

	body, _ := json.Marshal(map[string]string{})
	r := withUserID(httptest.NewRequest(http.MethodDelete, "/mfa/totp", bytes.NewReader(body)), u.UserID)
	w := httptest.NewRecorder()
	handleTOTPDisable(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing password, got %d", w.Code)
	}
}

// ── handleLogin — MFA interception ───────────────────────────────────────────

func TestHandleLogin_MFARequired(t *testing.T) {
	const password = "loginpassword"
	u, _ := createTOTPUser(t, password)
	t.Cleanup(func() { connect().Where("user_id = ?", u.UserID).Delete(&MFAPending{}) }) //nolint:errcheck

	body, _ := json.Marshal(loginRequest{Email: u.Email, Password: password})
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleLogin(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["mfa_required"] != true {
		t.Fatalf("expected mfa_required=true, got %+v", resp)
	}
	if resp["mfa_token"] == "" {
		t.Fatal("expected non-empty mfa_token")
	}
	if _, hasToken := resp["token"]; hasToken {
		t.Fatal("response must not include session token before MFA is completed")
	}
}

func TestHandleLogin_NoMFAWhenNotEnrolled(t *testing.T) {
	const password = "loginpassword"
	u := createLoginUser(t, uuid.New().String()+"@test.com", password)
	t.Cleanup(func() { connect().Model(&Session{}).Where("user_id = ?", u.UserID).Update("active", false) })

	body, _ := json.Marshal(loginRequest{Email: u.Email, Password: password})
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleLogin(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["token"] == "" {
		t.Fatal("expected direct session token for user without TOTP")
	}
	if resp["mfa_required"] != nil {
		t.Fatal("must not include mfa_required for users without TOTP")
	}
}

// ── handleMFAVerify ───────────────────────────────────────────────────────────

func TestHandleMFAVerify_Success(t *testing.T) {
	u, secret := createTOTPUser(t, "password")
	t.Cleanup(func() { connect().Model(&Session{}).Where("user_id = ?", u.UserID).Update("active", false) })

	pending, err := newMFAPending(context.Background(), u.UserID, "", "", "", "")
	if err != nil {
		t.Fatalf("newMFAPending: %v", err)
	}
	t.Cleanup(func() { connect().Where("token = ?", pending.Token).Delete(&MFAPending{}) }) //nolint:errcheck

	code := validTOTPCode(t, secret)
	body, _ := json.Marshal(map[string]string{"mfa_token": pending.Token, "code": code})
	r := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleMFAVerify(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["token"] == "" {
		t.Fatal("expected non-empty session token in response")
	}
}

func TestHandleMFAVerify_InvalidCode(t *testing.T) {
	u, _ := createTOTPUser(t, "password")

	pending, err := newMFAPending(context.Background(), u.UserID, "", "", "", "")
	if err != nil {
		t.Fatalf("newMFAPending: %v", err)
	}
	t.Cleanup(func() { connect().Where("token = ?", pending.Token).Delete(&MFAPending{}) }) //nolint:errcheck

	body, _ := json.Marshal(map[string]string{"mfa_token": pending.Token, "code": "000000"})
	r := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleMFAVerify(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid code, got %d", w.Code)
	}
}

func TestHandleMFAVerify_InvalidToken(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"mfa_token": "nonexistent-token", "code": "123456"})
	r := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleMFAVerify(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d", w.Code)
	}
}

func TestHandleMFAVerify_ExpiredToken(t *testing.T) {
	u, _ := createTOTPUser(t, "password")

	expired := MFAPending{
		Token:     uuid.New().String(),
		UserID:    u.UserID,
		ExpiresAt: time.Now().Add(-1 * time.Minute).UTC(),
		CreatedAt: time.Now().UTC(),
	}
	connect().Create(&expired)
	t.Cleanup(func() { connect().Where("token = ?", expired.Token).Delete(&MFAPending{}) }) //nolint:errcheck

	body, _ := json.Marshal(map[string]string{"mfa_token": expired.Token, "code": "123456"})
	r := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleMFAVerify(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d", w.Code)
	}
}

func TestHandleMFAVerify_TokenUsedOnce(t *testing.T) {
	u, secret := createTOTPUser(t, "password")
	t.Cleanup(func() { connect().Model(&Session{}).Where("user_id = ?", u.UserID).Update("active", false) })

	pending, err := newMFAPending(context.Background(), u.UserID, "", "", "", "")
	if err != nil {
		t.Fatalf("newMFAPending: %v", err)
	}
	t.Cleanup(func() { connect().Where("token = ?", pending.Token).Delete(&MFAPending{}) }) //nolint:errcheck

	code := validTOTPCode(t, secret)
	marshal := func() []byte { b, _ := json.Marshal(map[string]string{"mfa_token": pending.Token, "code": code}); return b }

	// First use should succeed.
	r1 := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(marshal()))
	w1 := httptest.NewRecorder()
	handleMFAVerify(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first use expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	// Second use of the same token must be rejected.
	r2 := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(marshal()))
	w2 := httptest.NewRecorder()
	handleMFAVerify(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("second use expected 401, got %d", w2.Code)
	}
}

func TestHandleMFAVerify_OAuthTokenRejected(t *testing.T) {
	u, _ := createTOTPUser(t, "password")
	client, _ := createTestOAuthClient(t)

	pending := createOAuthMFAPending(t, u.UserID, client.ClientID,
		"https://example.com/callback", "state", "openid")

	body, _ := json.Marshal(map[string]string{"mfa_token": pending.Token, "code": "123456"})
	r := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleMFAVerify(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when OAuth token sent to JSON endpoint, got %d", w.Code)
	}
}

func TestHandleMFAVerify_MissingFields(t *testing.T) {
	cases := []map[string]string{
		{"mfa_token": "tok"},
		{"code": "123456"},
		{},
	}
	for _, c := range cases {
		body, _ := json.Marshal(c)
		r := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleMFAVerify(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %+v, got %d", c, w.Code)
		}
	}
}

// ── handleOAuthMFAGet ─────────────────────────────────────────────────────────

func TestHandleOAuthMFAGet_RendersForm(t *testing.T) {
	u, _ := createTOTPUser(t, "password")
	client, _ := createTestOAuthClient(t)
	pending := createOAuthMFAPending(t, u.UserID, client.ClientID,
		"https://example.com/callback", "state", "openid")

	r := httptest.NewRequest(http.MethodGet, "/oauth/mfa?token="+pending.Token, nil)
	w := httptest.NewRecorder()
	handleOAuthMFAGet(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("expected text/html, got %q", w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, pending.Token) {
		t.Error("form must embed the MFA token as a hidden field")
	}
	if !strings.Contains(body, client.Name) {
		t.Error("form must display the OAuth client name")
	}
}

func TestHandleOAuthMFAGet_MissingToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/mfa", nil)
	w := httptest.NewRecorder()
	handleOAuthMFAGet(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing token, got %d", w.Code)
	}
}

func TestHandleOAuthMFAGet_InvalidToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/mfa?token=does-not-exist", nil)
	w := httptest.NewRecorder()
	handleOAuthMFAGet(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d", w.Code)
	}
}

func TestHandleOAuthMFAGet_ExpiredToken(t *testing.T) {
	u := createTestUser(t)
	expired := MFAPending{
		Token:            uuid.New().String(),
		UserID:           u.UserID,
		ExpiresAt:        time.Now().Add(-1 * time.Minute).UTC(),
		OAuthClientID:    "some-client",
		OAuthRedirectURI: "https://example.com/callback",
	}
	connect().Create(&expired)
	t.Cleanup(func() { connect().Where("token = ?", expired.Token).Delete(&MFAPending{}) }) //nolint:errcheck

	r := httptest.NewRequest(http.MethodGet, "/oauth/mfa?token="+expired.Token, nil)
	w := httptest.NewRecorder()
	handleOAuthMFAGet(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d", w.Code)
	}
}

// ── handleOAuthMFAPost ────────────────────────────────────────────────────────

func TestHandleOAuthMFAPost_Success(t *testing.T) {
	initOIDC()
	client, _ := createTestOAuthClient(t)
	u, secret := createTOTPUser(t, "password")
	pending := createOAuthMFAPending(t, u.UserID, client.ClientID,
		"https://example.com/callback", "mystate", "openid email")

	form := url.Values{}
	form.Set("token", pending.Token)
	form.Set("code", validTOTPCode(t, secret))

	r := httptest.NewRequest(http.MethodPost, "/oauth/mfa", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleOAuthMFAPost(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("invalid Location header %q: %v", loc, err)
	}
	if parsed.Query().Get("code") == "" {
		t.Error("expected code param in redirect URI")
	}
	if parsed.Query().Get("state") != "mystate" {
		t.Errorf("expected state=mystate, got %q", parsed.Query().Get("state"))
	}
}

func TestHandleOAuthMFAPost_InvalidCode(t *testing.T) {
	client, _ := createTestOAuthClient(t)
	u, _ := createTOTPUser(t, "password")
	pending := createOAuthMFAPending(t, u.UserID, client.ClientID,
		"https://example.com/callback", "state", "openid")

	form := url.Values{}
	form.Set("token", pending.Token)
	form.Set("code", "000000")

	r := httptest.NewRequest(http.MethodPost, "/oauth/mfa", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleOAuthMFAPost(w, r)

	// Should re-render the form with an error (not redirect).
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid code, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Error("expected HTML form on invalid code, not a redirect")
	}
}

func TestHandleOAuthMFAPost_InvalidToken(t *testing.T) {
	form := url.Values{}
	form.Set("token", "no-such-token")
	form.Set("code", "123456")

	r := httptest.NewRequest(http.MethodPost, "/oauth/mfa", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleOAuthMFAPost(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d", w.Code)
	}
}

func TestHandleOAuthMFAPost_MissingFields(t *testing.T) {
	cases := []url.Values{
		{"token": []string{"tok"}},
		{"code": []string{"123456"}},
		{},
	}
	for _, form := range cases {
		r := httptest.NewRequest(http.MethodPost, "/oauth/mfa", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handleOAuthMFAPost(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %v, got %d", form, w.Code)
		}
	}
}

// ── End-to-end: full TOTP login flow ─────────────────────────────────────────

func TestTOTPLoginFlow_EndToEnd(t *testing.T) {
	const password = "e2epassword"
	u, secret := createTOTPUser(t, password)
	t.Cleanup(func() { connect().Model(&Session{}).Where("user_id = ?", u.UserID).Update("active", false) })
	t.Cleanup(func() { connect().Where("user_id = ?", u.UserID).Delete(&MFAPending{}) }) //nolint:errcheck

	// Step 1: login returns MFA challenge.
	body, _ := json.Marshal(loginRequest{Email: u.Email, Password: password})
	r1 := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	w1 := httptest.NewRecorder()
	handleLogin(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("login step: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var loginResp map[string]any
	json.Unmarshal(w1.Body.Bytes(), &loginResp) //nolint:errcheck
	mfaToken, _ := loginResp["mfa_token"].(string)
	if mfaToken == "" {
		t.Fatalf("login step: expected mfa_token, got %+v", loginResp)
	}

	// Step 2: submit TOTP code to get session token.
	code := validTOTPCode(t, secret)
	body2, _ := json.Marshal(map[string]string{"mfa_token": mfaToken, "code": code})
	r2 := httptest.NewRequest(http.MethodPost, "/mfa/verify", bytes.NewReader(body2))
	w2 := httptest.NewRecorder()
	handleMFAVerify(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("verify step: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var verifyResp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &verifyResp) //nolint:errcheck
	if verifyResp["token"] == "" {
		t.Fatal("verify step: expected session token in response")
	}
}
