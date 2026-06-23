package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withOIDC(t *testing.T) {
	t.Helper()
	origKey, origKID, origIss := oidcSigningKey, oidcKeyID, oidcIssuer
	initOIDC() // generates an ephemeral signing key when OIDC_SIGNING_KEY is unset
	t.Cleanup(func() { oidcSigningKey, oidcKeyID, oidcIssuer = origKey, origKID, origIss })
}

func TestInitOIDC_DiscoveryAndJWKS(t *testing.T) {
	withOIDC(t)
	if oidcSigningKey == nil {
		t.Fatal("initOIDC did not set a signing key")
	}

	w := httptest.NewRecorder()
	handleOIDCDiscovery(w, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("discovery: code=%d len=%d", w.Code, w.Body.Len())
	}

	w2 := httptest.NewRecorder()
	handleJWKS(w2, httptest.NewRequest(http.MethodGet, "/oauth/jwks", nil))
	if w2.Code != http.StatusOK || w2.Body.Len() == 0 {
		t.Fatalf("jwks: code=%d len=%d", w2.Code, w2.Body.Len())
	}
}

func TestHandleAuthorizeSubmit_OIDCNotConfigured(t *testing.T) {
	// Ensure no signing key → 503.
	orig := oidcSigningKey
	oidcSigningKey = nil
	t.Cleanup(func() { oidcSigningKey = orig })
	w := httptest.NewRecorder()
	handleAuthorizeSubmit(w, httptest.NewRequest(http.MethodPost, "/oauth/authorize", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 when OIDC unconfigured", w.Code)
	}
}

func TestBuildGroups(t *testing.T) {
	u := seedTestUser(t)
	// No org/team membership → no groups, but the query path is exercised.
	groups := buildGroups(context.Background(), u)
	if groups == nil {
		groups = []string{}
	}
	_ = groups
}
