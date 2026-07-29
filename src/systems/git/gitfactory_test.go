package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A git_factory backend must produce a credential without ever storing one: the secret
// comes from gatekeeper, minted for the user the clone is for.
func TestMintForBackendGitFactory(t *testing.T) {
	var gotServiceKey, gotUserID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/run-tokens" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		gotServiceKey = r.Header.Get("X-Service-Key")
		var body struct {
			UserID string `json:"user_id"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		gotUserID = body.UserID
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"token": "gk_session_token"}) //nolint:errcheck
	}))
	defer srv.Close()

	prevURL, prevKey := gatekeeperURL, gatekeeperServiceKey
	gatekeeperURL = srv.URL
	gatekeeperServiceKey = func() string { return "svckey" }
	defer func() { gatekeeperURL, gatekeeperServiceKey = prevURL, prevKey }()

	enc, err := sealAuth(authConfig{Mode: modeService})
	if err != nil {
		t.Fatal(err)
	}
	b := GitBackend{
		Owner: platformOwner, Type: backendGitFactory, Host: "git-factory",
		BaseURL: "http://git-factory:9002", AuthMode: modeService, AuthEnc: enc,
	}
	cred, updated, err := mintForBackend(context.Background(), b, "user-123", "http://git-factory:9002/alice/repo.git")
	if err != nil {
		t.Fatalf("mintForBackend: %v", err)
	}
	if updated != nil {
		t.Fatal("git_factory has no stored auth to rotate")
	}
	// Minted as the requesting user, NOT as the platform row's owner — the whole point
	// of the asUser parameter.
	if gotUserID != "user-123" {
		t.Fatalf("minted for %q, want user-123", gotUserID)
	}
	if gotServiceKey != "git_connector:svckey" {
		t.Fatalf("service key header=%q", gotServiceKey)
	}
	if cred.Secret != "gk_session_token" {
		t.Fatalf("secret=%q", cred.Secret)
	}
	if cred.Username != "token" {
		t.Fatalf("username=%q want token", cred.Username)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("expiry not set — callers cache on it")
	}
	if !strings.Contains(cred.CloneURL, "gk_session_token") {
		t.Fatalf("clone url missing minted token: %q", cred.CloneURL)
	}
}

// Minting must fail closed rather than emit a credential-less clone URL when the
// service key is not available (startup, or a misconfigured deployment).
func TestMintGitFactoryTokenNoServiceKey(t *testing.T) {
	prev := gatekeeperServiceKey
	gatekeeperServiceKey = func() string { return "" }
	defer func() { gatekeeperServiceKey = prev }()

	if _, _, err := mintGitFactoryToken(context.Background(), "user-123"); err == nil {
		t.Fatal("expected an error when no service key is available")
	}
}

// The user must be carried into the mint call; falling back to the row's owner would
// mint as the literal platform sentinel.
func TestMintForBackendGitFactoryRequiresUser(t *testing.T) {
	enc, err := sealAuth(authConfig{Mode: modeService})
	if err != nil {
		t.Fatal(err)
	}
	b := GitBackend{
		Owner: platformOwner, Type: backendGitFactory, Host: "git-factory",
		BaseURL: "http://git-factory:9002", AuthMode: modeService, AuthEnc: enc,
	}
	if _, _, err := mintForBackend(context.Background(), b, "", "http://git-factory:9002/a/b.git"); err == nil {
		t.Fatal("expected an error when no user is supplied")
	}
}

// git_factory is a platform-only type: a user-created one would have a gatekeeper token
// minted for them and delivered to a base_url of their choosing.
func TestCreateBackendRejectsGitFactoryType(t *testing.T) {
	if validModeForType(backendGitFactory, modeBasic) {
		t.Fatal("git_factory must not accept basic auth")
	}
	if !validModeForType(backendGitFactory, modeService) {
		t.Fatal("git_factory must accept service mode")
	}
	if validModeForType(backendGeneric, modeService) {
		t.Fatal("service mode must not be reachable via the generic type")
	}
}
