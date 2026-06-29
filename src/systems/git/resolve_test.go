package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeriveHost(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://github.com/acme/widgets.git", "github.com", false},
		{"https://GitLab.Example.com:443/g/p.git", "gitlab.example.com", false},
		{"http://git.internal/a/b", "git.internal", false},
		{"ssh://git@host/a/b", "", true},
		{"not a url", "", true},
		{"https:///nohost", "", true},
	}
	for _, c := range cases {
		got, err := deriveHost(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("deriveHost(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("deriveHost(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestInjectCloneCreds(t *testing.T) {
	got, err := injectCloneCreds("https://github.com/acme/widgets.git", "x-access-token", "tok/with+special")
	if err != nil {
		t.Fatalf("injectCloneCreds: %v", err)
	}
	if !strings.HasPrefix(got, "https://x-access-token:") || !strings.HasSuffix(got, "@github.com/acme/widgets.git") {
		t.Fatalf("unexpected clone url: %s", got)
	}
}

func TestValidModeForType(t *testing.T) {
	ok := map[string][]string{
		backendGitHub:  {modeApp, modePAT},
		backendGitLab:  {modeToken, modeOAuth},
		backendForgejo: {modeToken, modeAdmin},
		backendGeneric: {modeBasic},
	}
	for typ, modes := range ok {
		for _, m := range modes {
			if !validModeForType(typ, m) {
				t.Errorf("validModeForType(%q,%q)=false want true", typ, m)
			}
		}
	}
	if validModeForType(backendGitHub, modeBasic) {
		t.Error("github should reject basic mode")
	}
	if validModeForType("svn", modePAT) {
		t.Error("unknown type should be invalid")
	}
}

func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func TestGithubInstallationToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/42/access_tokens" || r.Method != http.MethodPost {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no jwt", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"token":      "ghs_installationtoken",
			"expires_at": "2099-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	tok, exp, err := githubInstallationToken(context.Background(), srv.URL, 7, 42, testRSAKeyPEM(t))
	if err != nil {
		t.Fatalf("githubInstallationToken: %v", err)
	}
	if tok != "ghs_installationtoken" {
		t.Fatalf("token=%q", tok)
	}
	if exp.IsZero() {
		t.Fatal("expiry not parsed")
	}
}

func TestMintForBackendPassthrough(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		auth     authConfig
		wantUser string
	}{
		{"github pat", backendGitHub, authConfig{Mode: modePAT, Token: "ghp_x"}, "x-access-token"},
		{"gitlab token", backendGitLab, authConfig{Mode: modeToken, Token: "glpat_x"}, "oauth2"},
		{"generic basic", backendGeneric, authConfig{Mode: modeBasic, Username: "alice", Password: "pw"}, "alice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc, err := sealAuth(c.auth)
			if err != nil {
				t.Fatal(err)
			}
			b := GitBackend{Type: c.typ, Host: "example.com", BaseURL: "https://example.com", AuthMode: c.auth.Mode, AuthEnc: enc}
			cred, updated, err := mintForBackend(context.Background(), b, "https://example.com/a/b.git")
			if err != nil {
				t.Fatalf("mintForBackend: %v", err)
			}
			if updated != nil {
				t.Fatal("passthrough should not rotate auth")
			}
			if cred.Username != c.wantUser {
				t.Fatalf("username=%q want %q", cred.Username, c.wantUser)
			}
			if cred.CloneURL == "" {
				t.Fatal("clone url not built")
			}
		})
	}
}

func TestMintForBackendGitlabOAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"access_token":  "glat_short",
			"refresh_token": "new_refresh",
			"expires_in":    7200,
		})
	}))
	defer srv.Close()

	auth := authConfig{Mode: modeOAuth, RefreshToken: "old_refresh", ClientID: "cid", ClientSecret: "csec"}
	enc, _ := sealAuth(auth)
	b := GitBackend{Type: backendGitLab, Host: "gl.example.com", BaseURL: srv.URL, AuthMode: modeOAuth, AuthEnc: enc}
	cred, updated, err := mintForBackend(context.Background(), b, "https://gl.example.com/g/p.git")
	if err != nil {
		t.Fatalf("mintForBackend: %v", err)
	}
	if cred.Secret != "glat_short" {
		t.Fatalf("secret=%q", cred.Secret)
	}
	if updated == nil || updated.RefreshToken != "new_refresh" {
		t.Fatalf("expected rotated refresh token, got %+v", updated)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("expiry not set")
	}
}

func TestForgejoMintToken(t *testing.T) {
	var deleted, created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode([]forgejoToken{{ID: 9, Name: forgejoTokenPrefix + "old"}}) //nolint:errcheck
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/tokens/9"):
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tokens"):
			created = true
			json.NewEncoder(w).Encode(forgejoToken{ID: 10, Name: "x", SHA1: "abc123"}) //nolint:errcheck
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tok, err := forgejoMintToken(context.Background(), srv.URL, "admintok", "alice")
	if err != nil {
		t.Fatalf("forgejoMintToken: %v", err)
	}
	if tok != "abc123" {
		t.Fatalf("token=%q", tok)
	}
	if !deleted {
		t.Error("prior broker token was not revoked")
	}
	if !created {
		t.Error("new token was not created")
	}
}
