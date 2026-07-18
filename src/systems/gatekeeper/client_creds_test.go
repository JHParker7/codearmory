package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHandleClientCredentialsGrant(t *testing.T) {
	withOIDC(t)
	clientID, secret := seedOAuthClient(t, "https://app.example/cb")

	form := url.Values{"grant_type": {"client_credentials"}}
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(clientID, secret)
	w := httptest.NewRecorder()
	handleToken(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("client_credentials got %d, want 200: %s", w.Code, w.Body.String())
	}
	var tok map[string]any
	json.Unmarshal(w.Body.Bytes(), &tok) //nolint:errcheck
	if tok["access_token"] == nil || tok["token_type"] != "Bearer" {
		t.Errorf("bad client_credentials response: %v", tok)
	}
}

func TestHandleClientCredentialsGrant_BadSecret(t *testing.T) {
	clientID, _ := seedOAuthClient(t, "https://app.example/cb")
	form := url.Values{"grant_type": {"client_credentials"}}
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(clientID, "wrong-secret")
	w := httptest.NewRecorder()
	handleToken(w, r)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusBadRequest {
		t.Fatalf("bad secret got %d, want 4xx", w.Code)
	}
}

func TestHandleToken_MissingClientCreds(t *testing.T) {
	form := url.Values{"grant_type": {"client_credentials"}}
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleToken(w, r)
	if w.Code < 400 {
		t.Fatalf("missing client creds got %d, want 4xx", w.Code)
	}
}
