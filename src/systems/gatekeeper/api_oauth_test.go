package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── handleOIDCDiscovery ───────────────────────────────────────────────────────

func TestHandleOIDCDiscovery_ContainsRequiredFields(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)
	w := httptest.NewRecorder()
	handleOIDCDiscovery(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var disc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &disc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, field := range []string{
		"issuer", "authorization_endpoint", "token_endpoint",
		"userinfo_endpoint", "jwks_uri", "response_types_supported",
	} {
		if _, ok := disc[field]; !ok {
			t.Errorf("missing required field %q in discovery document", field)
		}
	}
}

func TestHandleOIDCDiscovery_ContentType(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)
	w := httptest.NewRecorder()
	handleOIDCDiscovery(w, r)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ── handleJWKS ────────────────────────────────────────────────────────────────

func TestHandleJWKS_NotConfigured(t *testing.T) {
	orig := oidcSigningKey
	oidcSigningKey = nil
	defer func() { oidcSigningKey = orig }()

	r := httptest.NewRequest(http.MethodGet, "/oauth/jwks", nil)
	w := httptest.NewRecorder()
	handleJWKS(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when OIDC not configured, got %d", w.Code)
	}
}

func TestHandleJWKS_ReturnsECKey(t *testing.T) {
	initOIDC() // ensures an ephemeral signing key is generated

	r := httptest.NewRequest(http.MethodGet, "/oauth/jwks", nil)
	w := httptest.NewRecorder()
	handleJWKS(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var jwks map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	keys, ok := jwks["keys"].([]any)
	if !ok || len(keys) == 0 {
		t.Fatal("expected non-empty keys array in JWKS response")
	}
	key0, _ := keys[0].(map[string]any)
	if key0["kty"] != "EC" {
		t.Errorf("kty = %q, want EC", key0["kty"])
	}
	if key0["alg"] != "ES256" {
		t.Errorf("alg = %q, want ES256", key0["alg"])
	}
}

// ── handleAuthorize — parameter validation ────────────────────────────────────

func TestHandleAuthorize_MissingClientID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/authorize?redirect_uri=https://example.com/cb", nil)
	w := httptest.NewRecorder()
	handleAuthorize(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing client_id, got %d", w.Code)
	}
}

func TestHandleAuthorize_MissingRedirectURI(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/authorize?client_id=my-client", nil)
	w := httptest.NewRecorder()
	handleAuthorize(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing redirect_uri, got %d", w.Code)
	}
}

// ── handleToken — grant_type validation ──────────────────────────────────────

func TestHandleToken_UnsupportedGrantType(t *testing.T) {
	initOIDC()

	body := strings.NewReader("grant_type=implicit&client_id=c&client_secret=s")
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleToken(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported grant_type, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unsupported_grant_type") {
		t.Errorf("expected error code 'unsupported_grant_type' in body, got %q", w.Body.String())
	}
}

func TestHandleToken_MissingGrantType(t *testing.T) {
	initOIDC()

	body := strings.NewReader("client_id=c&client_secret=s")
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleToken(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing grant_type, got %d", w.Code)
	}
}

// ── OAuth client management — service auth ────────────────────────────────────

func TestHandleCreateOAuthClient_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/oauth/clients", nil)
	w := httptest.NewRecorder()
	handleCreateOAuthClient(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleListOAuthClients_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/clients", nil)
	w := httptest.NewRecorder()
	handleListOAuthClients(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleDeleteOAuthClient_NoServiceKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/oauth/clients/some-id", nil)
	r.SetPathValue("id", "some-id")
	w := httptest.NewRecorder()
	handleDeleteOAuthClient(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
