package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSecretsServer stands in for gatekeeper's /internal/secrets/lookup endpoint
// (and any other gatekeeper internal path), overriding gatekeeperURL + the rotated
// service key accessor for the test duration so credential resolution can be
// exercised without a live gatekeeper.
func fakeSecretsServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	origURL := gatekeeperURL
	origKey := forgeServiceKey
	gatekeeperURL = srv.URL
	forgeServiceKey = func() string { return "live-key" }
	t.Cleanup(func() {
		gatekeeperURL = origURL
		forgeServiceKey = origKey
		srv.Close()
	})
}

// fakeGiteaServer stands in for gitea_integration's /internal/clone-token endpoint,
// overriding giteaInternalURL + giteaInternalKey for the test duration.
func fakeGiteaServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	origURL := giteaInternalURL
	origKey := giteaInternalKey
	giteaInternalURL = srv.URL
	giteaInternalKey = "internal-key"
	t.Cleanup(func() {
		giteaInternalURL = origURL
		giteaInternalKey = origKey
		srv.Close()
	})
}

// fakeGitBrokerServer stands in for the git credential-broker's
// /internal/clone-token endpoint, overriding gitInternalURL + gitInternalKey for
// the test duration.
func fakeGitBrokerServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	origURL := gitInternalURL
	origKey := gitInternalKey
	gitInternalURL = srv.URL
	gitInternalKey = "git-internal-key"
	t.Cleanup(func() {
		gitInternalURL = origURL
		gitInternalKey = origKey
		srv.Close()
	})
}

// ── lookupScopedSecret ──────────────────────────────────────────────────────────

func TestLookupOrgSecret_Success(t *testing.T) {
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/secrets/lookup" {
			t.Errorf("path = %q, want /internal/secrets/lookup", r.URL.Path)
		}
		if got := r.Header.Get("X-Service-Key"); got != "forge:live-key" {
			t.Errorf("X-Service-Key = %q, want forge:live-key (the live rotated key)", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":"s3cr3t"}`)) //nolint:errcheck
	})
	got, err := lookupScopedSecret(context.Background(), "org1", "", "deploy-key")
	if err != nil {
		t.Fatalf("lookupScopedSecret: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t", got)
	}
}

func TestLookupScopedSecret_NoScope(t *testing.T) {
	// Neither org nor user bound: must fail before any network call — there is no
	// ownership scope to resolve the secret under.
	if _, err := lookupScopedSecret(context.Background(), "", "", "name"); err == nil {
		t.Fatal("expected an error when neither org nor user is bound to the execution")
	}
}

func TestLookupScopedSecret_PersonalUser(t *testing.T) {
	// No org, but a user is bound: forge must send user_id so gatekeeper resolves
	// the caller's personal secret. This is the org-less CI path.
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OrgID  string `json:"org_id"`
			UserID string `json:"user_id"`
			Name   string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.OrgID != "" || body.UserID != "u-solo" {
			t.Errorf("lookup body = %+v, want org_id='' user_id='u-solo'", body)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":"personal"}`)) //nolint:errcheck
	})
	got, err := lookupScopedSecret(context.Background(), "", "u-solo", "registry-auth")
	if err != nil {
		t.Fatalf("lookupScopedSecret personal: %v", err)
	}
	if got != "personal" {
		t.Errorf("value = %q, want personal", got)
	}
}

func TestLookupOrgSecret_KeyNotInitialised(t *testing.T) {
	orig := forgeServiceKey
	forgeServiceKey = nil
	t.Cleanup(func() { forgeServiceKey = orig })
	if _, err := lookupScopedSecret(context.Background(), "org1", "", "name"); err == nil {
		t.Fatal("expected an error when the service key accessor is nil")
	}
}

func TestLookupOrgSecret_NotFound(t *testing.T) {
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := lookupScopedSecret(context.Background(), "org1", "", "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestLookupOrgSecret_ServerError(t *testing.T) {
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := lookupScopedSecret(context.Background(), "org1", "", "name"); err == nil {
		t.Fatal("expected an error for a non-200 lookup response")
	}
}

func TestLookupOrgSecret_BadJSON(t *testing.T) {
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json`)) //nolint:errcheck
	})
	if _, err := lookupScopedSecret(context.Background(), "org1", "", "name"); err == nil {
		t.Fatal("expected a decode error for a malformed lookup response")
	}
}

// ── mintGiteaCloneURL ───────────────────────────────────────────────────────────

func TestMintGiteaCloneURL_Success(t *testing.T) {
	fakeGiteaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/clone-token" {
			t.Errorf("path = %q, want /internal/clone-token", r.URL.Path)
		}
		if got := r.Header.Get("X-Internal-Key"); got != "internal-key" {
			t.Errorf("X-Internal-Key = %q, want internal-key", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"clone_url":"https://x:tok@gitea/acme/widgets.git"}`)) //nolint:errcheck
	})
	got, err := mintGiteaCloneURL(context.Background(), "user-1", "acme/widgets")
	if err != nil {
		t.Fatalf("mintGiteaCloneURL: %v", err)
	}
	if !strings.Contains(got, "acme/widgets.git") {
		t.Errorf("clone url = %q, want the minted URL", got)
	}
}

func TestMintGiteaCloneURL_NotConfigured(t *testing.T) {
	origURL := giteaInternalURL
	origKey := giteaInternalKey
	giteaInternalURL = ""
	giteaInternalKey = ""
	t.Cleanup(func() { giteaInternalURL = origURL; giteaInternalKey = origKey })
	if _, err := mintGiteaCloneURL(context.Background(), "user-1", "acme/widgets"); err == nil {
		t.Fatal("expected an error when gitea integration is not configured")
	}
}

func TestMintGiteaCloneURL_NoLinkedAccount(t *testing.T) {
	fakeGiteaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := mintGiteaCloneURL(context.Background(), "user-1", "acme/widgets")
	if err == nil || !strings.Contains(err.Error(), "no linked gitea account") {
		t.Fatalf("err = %v, want a no-linked-account error", err)
	}
}

func TestMintGiteaCloneURL_ServerError(t *testing.T) {
	fakeGiteaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, err := mintGiteaCloneURL(context.Background(), "user-1", "acme/widgets"); err == nil {
		t.Fatal("expected an error for a non-200 clone-token response")
	}
}

func TestMintGiteaCloneURL_EmptyCloneURL(t *testing.T) {
	fakeGiteaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"clone_url":""}`)) //nolint:errcheck
	})
	if _, err := mintGiteaCloneURL(context.Background(), "user-1", "acme/widgets"); err == nil {
		t.Fatal("expected an error when the response carries no clone_url")
	}
}

func TestMintGiteaCloneURL_BadJSON(t *testing.T) {
	fakeGiteaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{`)) //nolint:errcheck
	})
	if _, err := mintGiteaCloneURL(context.Background(), "user-1", "acme/widgets"); err == nil {
		t.Fatal("expected a decode error for a malformed clone-token response")
	}
}

// ── mintGitCloneURL ─────────────────────────────────────────────────────────────

func TestMintGitCloneURL_Success(t *testing.T) {
	fakeGitBrokerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/clone-token" {
			t.Errorf("path = %q, want /internal/clone-token", r.URL.Path)
		}
		if got := r.Header.Get("X-Internal-Key"); got != "git-internal-key" {
			t.Errorf("X-Internal-Key = %q, want git-internal-key", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"clone_url":"https://x-access-token:tok@github.com/acme/widgets.git"}`)) //nolint:errcheck
	})
	got, err := mintGitCloneURL(context.Background(), "user-1", "https://github.com/acme/widgets.git")
	if err != nil {
		t.Fatalf("mintGitCloneURL: %v", err)
	}
	if !strings.Contains(got, "github.com/acme/widgets.git") {
		t.Errorf("clone url = %q, want the minted URL", got)
	}
}

func TestMintGitCloneURL_NotConfigured(t *testing.T) {
	origURL := gitInternalURL
	origKey := gitInternalKey
	gitInternalURL = ""
	gitInternalKey = ""
	t.Cleanup(func() { gitInternalURL = origURL; gitInternalKey = origKey })
	if _, err := mintGitCloneURL(context.Background(), "user-1", "https://github.com/acme/widgets.git"); err == nil {
		t.Fatal("expected an error when the git broker is not configured")
	}
}

func TestMintGitCloneURL_NoLinkedBackend(t *testing.T) {
	fakeGitBrokerServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := mintGitCloneURL(context.Background(), "user-1", "https://github.com/acme/widgets.git")
	if err == nil || !strings.Contains(err.Error(), "no linked git backend") {
		t.Fatalf("err = %v, want a no-linked-backend error", err)
	}
}

func TestMintGitCloneURL_EmptyCloneURL(t *testing.T) {
	fakeGitBrokerServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"clone_url":""}`)) //nolint:errcheck
	})
	if _, err := mintGitCloneURL(context.Background(), "user-1", "https://github.com/acme/widgets.git"); err == nil {
		t.Fatal("expected an error when the response carries no clone_url")
	}
}

// ── resolveCredentials (orchestration over both schemes) ────────────────────────

func TestResolveCredentials_Empty(t *testing.T) {
	out, err := resolveCredentials(context.Background(), Execution{})
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil for an execution with no secret_refs", out)
	}
}

func TestResolveCredentials_SecretAndGitea(t *testing.T) {
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":"token-value"}`)) //nolint:errcheck
	})
	fakeGiteaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"clone_url":"https://clone"}`)) //nolint:errcheck
	})
	exec := Execution{
		UserID: "user-1", OrgID: "org1",
		SecretRefs: map[string]string{
			"GIT_TOKEN": "secret:deploy",
			"REPO_URL":  "gitea:acme/widgets",
		},
	}
	out, err := resolveCredentials(context.Background(), exec)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if out["GIT_TOKEN"] != "token-value" {
		t.Errorf("GIT_TOKEN = %q, want resolved secret value", out["GIT_TOKEN"])
	}
	if out["REPO_URL"] != "https://clone" {
		t.Errorf("REPO_URL = %q, want minted clone url", out["REPO_URL"])
	}
}

func TestResolveCredentials_MalformedRefFails(t *testing.T) {
	exec := Execution{SecretRefs: map[string]string{"X": "no-scheme"}}
	if _, err := resolveCredentials(context.Background(), exec); err == nil {
		t.Fatal("expected resolveCredentials to fail on a malformed reference")
	}
}

func TestResolveCredentials_SecretLookupFails(t *testing.T) {
	fakeSecretsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	exec := Execution{OrgID: "org1", SecretRefs: map[string]string{"X": "secret:missing"}}
	if _, err := resolveCredentials(context.Background(), exec); err == nil {
		t.Fatal("expected resolveCredentials to surface a failed secret lookup")
	}
}
