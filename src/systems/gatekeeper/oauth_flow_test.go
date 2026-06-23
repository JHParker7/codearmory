package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func seedOAuthClient(t *testing.T, redirect string) (clientID, secret string) {
	t.Helper()
	clientID = "client-" + uuid.NewString()[:8]
	secret = "secret-" + uuid.NewString()[:8]
	hash, _ := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	c := OAuthClient{ClientID: clientID, Name: "Test App", SecretHash: string(hash), RedirectURIs: []string{redirect}, Active: true, CreatedAt: time.Now().UTC()}
	if err := c.Add(context.Background()); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("client_id = ?", clientID).Delete(&OAuthClient{}) }) //nolint:errcheck
	return clientID, secret
}

func seedUserWithPassword(t *testing.T, password string) User {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	u := User{UserID: uuid.NewString(), Email: uuid.NewString() + "@x.com", Username: "u" + uuid.NewString()[:8], HashedPassword: string(hash)}
	if err := u.Add(context.Background()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { gormDB.Unscoped().Where("user_id = ?", u.UserID).Delete(&User{}) }) //nolint:errcheck
	return u
}

func TestOAuthAuthorizationCodeFlow(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, clientSecret := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	// 1) Authorization request with valid user credentials → 302 redirect with code.
	form := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid profile"}, "email": {u.Email}, "password": {"password123"},
	}
	ar := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	ar.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	aw := httptest.NewRecorder()
	handleAuthorizeSubmit(aw, ar)
	if aw.Code != http.StatusFound {
		t.Fatalf("authorize got %d, want 302: %s", aw.Code, aw.Body.String())
	}
	loc, _ := url.Parse(aw.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no authorization code in redirect %q", aw.Header().Get("Location"))
	}

	// 2) Exchange the code for tokens via the token endpoint (basic-auth client creds).
	tform := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}}
	tr := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tform.Encode()))
	tr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tr.SetBasicAuth(clientID, clientSecret)
	tw := httptest.NewRecorder()
	handleToken(tw, tr)
	if tw.Code != http.StatusOK {
		t.Fatalf("token got %d, want 200: %s", tw.Code, tw.Body.String())
	}
	var tok map[string]any
	json.Unmarshal(tw.Body.Bytes(), &tok) //nolint:errcheck
	if tok["access_token"] == nil || tok["id_token"] == nil {
		t.Fatalf("token response missing access/id token: %v", tok)
	}

	// 3) The code is single-use: a second exchange must fail.
	tr2 := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tform.Encode()))
	tr2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tr2.SetBasicAuth(clientID, clientSecret)
	tw2 := httptest.NewRecorder()
	handleToken(tw2, tr2)
	if tw2.Code == http.StatusOK {
		t.Error("authorization code should be single-use")
	}
}

func TestOAuthAuthorizeSubmit_BadPassword(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, _ := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "rightpass")
	form := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid"}, "email": {u.Email}, "password": {"wrongpass"},
	}
	r := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleAuthorizeSubmit(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad password got %d, want 401", w.Code)
	}
}

func TestHandleToken_UnsupportedGrant(t *testing.T) {
	form := url.Values{"grant_type": {"password"}}
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleToken(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported grant got %d, want 400", w.Code)
	}
}

func TestOAuthUserinfo(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, clientSecret := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	form := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid profile"}, "email": {u.Email}, "password": {"password123"},
	}
	ar := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	ar.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	aw := httptest.NewRecorder()
	handleAuthorizeSubmit(aw, ar)
	loc, _ := url.Parse(aw.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code: %s", aw.Header().Get("Location"))
	}

	tform := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}}
	tr := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tform.Encode()))
	tr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tr.SetBasicAuth(clientID, clientSecret)
	tw := httptest.NewRecorder()
	handleToken(tw, tr)
	var tok map[string]any
	json.Unmarshal(tw.Body.Bytes(), &tok) //nolint:errcheck
	access, _ := tok["access_token"].(string)
	if access == "" {
		t.Fatalf("no access token: %s", tw.Body.String())
	}

	// Call userinfo through the real mux so authMiddleware validates the bearer.
	h := buildMux()
	ur := httptest.NewRequest(http.MethodGet, "/oauth/userinfo", nil)
	ur.Header.Set("Authorization", "Bearer "+access)
	uw := httptest.NewRecorder()
	h.ServeHTTP(uw, ur)
	if uw.Code != http.StatusOK {
		t.Fatalf("userinfo got %d, want 200: %s", uw.Code, uw.Body.String())
	}
}
