package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// clearsSessionCookie reports whether the response expires the armory_session cookie.
func clearsSessionCookie(res *http.Response) bool {
	for _, c := range res.Cookies() {
		if c.Name == "armory_session" && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// Logout must REVOKE the session, not merely clear the cookie. Clearing the cookie
// only drops the browser's copy; the session stays valid, so any other copy of the
// bearer keeps authenticating as a user who believes they logged out.
func TestHandleLogout_RevokesSession(t *testing.T) {
	user := createTestUser(t)
	token, session := makeSession(t, user.UserID, time.Now().Add(time.Hour))

	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handleLogout(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if !clearsSessionCookie(rec.Result()) {
		t.Error("logout did not expire the armory_session cookie")
	}
	if _, err := (Session{SessionID: session.SessionID}).Get(context.Background()); err == nil {
		t.Fatal("session still resolves after logout — it was not revoked")
	}
}

// The endpoint is unauthenticated, so the session id arrives from the caller. Acting on
// it without verifying the signature would make logout an unauthenticated "revoke any
// session by id" — a free denial of service against any user whose session id leaked.
func TestHandleLogout_ForgedTokenCannotRevokeAnotherSession(t *testing.T) {
	victim := createTestUser(t)
	_, victimSession := makeSession(t, victim.UserID, time.Now().Add(time.Hour))

	// A well-formed token naming the victim's session, signed with an attacker key.
	attackerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	forged, err := jwtlib.NewWithClaims(jwtlib.SigningMethodES256, authClaims{
		RegisteredClaims: jwtlib.RegisteredClaims{
			Issuer:    "gatekeeper",
			Subject:   victim.UserID,
			ID:        victimSession.SessionID,
			ExpiresAt: jwtlib.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwtlib.NewNumericDate(time.Now()),
		},
	}).SignedString(attackerKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	r.Header.Set("Authorization", "Bearer "+forged)
	rec := httptest.NewRecorder()
	handleLogout(rec, r)

	// The caller still gets their own cookie cleared — that is harmless and always safe.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, err := (Session{SessionID: victimSession.SessionID}).Get(context.Background()); err != nil {
		t.Fatal("a forged token revoked someone else's session")
	}
}

// A caller with no credential — or one too broken to verify — must still end up
// without a cookie. Answering 401 would strand exactly the callers most in need of
// shedding a bad credential.
func TestHandleLogout_NoCredentialStillClearsCookie(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth string
	}{
		{"no credential", ""},
		{"unparseable token", "Bearer not-a-jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/logout", nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			handleLogout(rec, r)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			if !clearsSessionCookie(rec.Result()) {
				t.Error("logout did not expire the armory_session cookie")
			}
		})
	}
}

// The cookie is a credential too, so presenting it must revoke the same as a bearer.
func TestHandleLogout_RevokesViaCookie(t *testing.T) {
	user := createTestUser(t)
	token, session := makeSession(t, user.UserID, time.Now().Add(time.Hour))

	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	r.AddCookie(&http.Cookie{Name: "armory_session", Value: token})
	rec := httptest.NewRecorder()
	handleLogout(rec, r)

	if _, err := (Session{SessionID: session.SessionID}).Get(context.Background()); err == nil {
		t.Fatal("session still resolves after cookie logout — it was not revoked")
	}
}
