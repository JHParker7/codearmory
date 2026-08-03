package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// challengeFor computes the S256 challenge for a verifier, as a client would.
func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeWithPKCE runs the authorization step and returns the issued code.
func authorizeWithPKCE(t *testing.T, clientID, redirect, email, password, challenge, method string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	form := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid"}, "email": {email}, "password": {password},
	}
	if challenge != "" {
		form.Set("code_challenge", challenge)
	}
	if method != "" {
		form.Set("code_challenge_method", method)
	}
	r := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleAuthorizeSubmit(w, r)
	if w.Code != http.StatusFound {
		return "", w
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	return loc.Query().Get("code"), w
}

// exchange redeems a code at the token endpoint, optionally presenting a verifier.
func exchange(t *testing.T, clientID, clientSecret, code, redirect, verifier string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(clientID, clientSecret)
	w := httptest.NewRecorder()
	handleToken(w, r)
	return w
}

// A code minted with a challenge may only be redeemed by presenting the matching
// verifier — the binding that makes an intercepted code useless to whoever holds it.
func TestPKCE_RoundTrip(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, clientSecret := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	const verifier = "a-high-entropy-verifier-value-0123456789"
	code, aw := authorizeWithPKCE(t, clientID, redirect, u.Email, "password123", challengeFor(verifier), pkceMethodS256)
	if code == "" {
		t.Fatalf("no code issued: %d %s", aw.Code, aw.Body.String())
	}
	if w := exchange(t, clientID, clientSecret, code, redirect, verifier); w.Code != http.StatusOK {
		t.Fatalf("exchange with correct verifier got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// The attack PKCE exists to stop: an intercepted code, redeemed by a party that does
// not hold the verifier. It must fail even though the client secret is presented.
func TestPKCE_WrongVerifierRejected(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, clientSecret := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	const verifier = "a-high-entropy-verifier-value-0123456789"
	code, aw := authorizeWithPKCE(t, clientID, redirect, u.Email, "password123", challengeFor(verifier), pkceMethodS256)
	if code == "" {
		t.Fatalf("no code issued: %d %s", aw.Code, aw.Body.String())
	}
	if w := exchange(t, clientID, clientSecret, code, redirect, "the-wrong-verifier"); w.Code == http.StatusOK {
		t.Fatal("exchange succeeded with the wrong verifier")
	}
	// The code must survive a failed attempt: a wrong verifier must not let an
	// attacker burn a code out from under the legitimate client.
	if w := exchange(t, clientID, clientSecret, code, redirect, verifier); w.Code != http.StatusOK {
		t.Fatalf("correct verifier rejected after a failed attempt (%d) — the bad guess consumed the code: %s", w.Code, w.Body.String())
	}
}

// Omitting the verifier entirely must fail too. Treating "absent" as "nothing to
// check" would make the whole extension opt-out for an attacker.
func TestPKCE_MissingVerifierRejected(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, clientSecret := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	const verifier = "a-high-entropy-verifier-value-0123456789"
	code, _ := authorizeWithPKCE(t, clientID, redirect, u.Email, "password123", challengeFor(verifier), pkceMethodS256)
	if code == "" {
		t.Fatal("no code issued")
	}
	if w := exchange(t, clientID, clientSecret, code, redirect, ""); w.Code == http.StatusOK {
		t.Fatal("exchange succeeded with no code_verifier against a PKCE-bound code")
	}
}

// PKCE is per-request: a client that never sent a challenge must keep working
// unchanged, with or without a stray verifier.
func TestPKCE_NotUsedRemainsBackwardCompatible(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, clientSecret := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	code, aw := authorizeWithPKCE(t, clientID, redirect, u.Email, "password123", "", "")
	if code == "" {
		t.Fatalf("no code issued: %d %s", aw.Code, aw.Body.String())
	}
	if w := exchange(t, clientID, clientSecret, code, redirect, ""); w.Code != http.StatusOK {
		t.Fatalf("non-PKCE exchange got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// "plain" is refused rather than silently accepted, and so is a method with no
// challenge. A downgrade a client can ask for is a downgrade an attacker can ask for.
func TestPKCE_RejectsPlainAndStrayMethod(t *testing.T) {
	withOIDC(t)
	redirect := "https://app.example/cb"
	clientID, _ := seedOAuthClient(t, redirect)
	u := seedUserWithPassword(t, "password123")

	for _, tc := range []struct {
		name      string
		challenge string
		method    string
	}{
		{"plain method", "some-challenge-value", "plain"},
		{"challenge without method", "some-challenge-value", ""},
		{"method without challenge", "", pkceMethodS256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, w := authorizeWithPKCE(t, clientID, redirect, u.Email, "password123", tc.challenge, tc.method)
			if code != "" {
				t.Fatalf("a code was issued for %s; want the request refused", tc.name)
			}
			// Refusal is an OAuth error redirect (302 carrying ?error=) or a 4xx —
			// either way, no code.
			if loc := w.Header().Get("Location"); loc != "" {
				u, _ := url.Parse(loc)
				if u.Query().Get("error") == "" {
					t.Fatalf("redirect carried no error parameter: %s", loc)
				}
			}
		})
	}
}

// verifyPKCE's contract, exercised directly.
func TestVerifyPKCE(t *testing.T) {
	const verifier = "verifier-abc"
	cases := []struct {
		name      string
		challenge string
		verifier  string
		want      bool
	}{
		{"no challenge needs no verifier", "", "", true},
		{"no challenge ignores a stray verifier", "", "anything", true},
		{"matching verifier", challengeFor(verifier), verifier, true},
		{"wrong verifier", challengeFor(verifier), "nope", false},
		{"missing verifier against a bound code", challengeFor(verifier), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := verifyPKCE(OAuthCode{CodeChallenge: c.challenge}, c.verifier)
			if got != c.want {
				t.Fatalf("verifyPKCE = %v, want %v", got, c.want)
			}
		})
	}
}
