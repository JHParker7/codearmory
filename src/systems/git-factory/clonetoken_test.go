package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func basicAuth(token string) string {
	// git puts the token in the password field; the username is a fixed placeholder.
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("git:"+token))
}

// postCloneToken drives handleMintCloneToken directly.
func postCloneToken(t *testing.T, body map[string]any, key string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/internal/clone-token", bytes.NewReader(raw))
	if key != "" {
		req.Header.Set("X-Internal-Key", key)
	}
	rec := httptest.NewRecorder()
	handleMintCloneToken(rec, req)
	return rec
}

// ensureMirror is a small helper: create+fetch a mirror and return its row.
func ensureMirror(t *testing.T, ns, name string) Repo {
	t.Helper()
	up, _ := makeUpstream(t)
	rec := postMirror(t, map[string]any{
		"upstream_url": up, "namespace": ns, "name": name, "owner": "ci-user",
	}, testInternalKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure mirror: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var re Repo
	if err := json.Unmarshal(rec.Body.Bytes(), &re); err != nil {
		t.Fatalf("decode mirror: %v", err)
	}
	return re
}

// The end-to-end goal of Part B: a runner holding ONLY a service-minted clone token
// (no gatekeeper stub, no JWT) can advertise/clone the mirror.
func TestCloneToken_MintAndClone(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "clone-signing-key")
	t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")

	re := ensureMirror(t, "acme", "widgets")

	rec := postCloneToken(t, map[string]any{"repo_id": re.ID}, testInternalKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got cloneTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(got.CloneURL, "https://git:cgfct1.") {
		t.Fatalf("clone_url = %q, want an authenticated git-factory URL", got.CloneURL)
	}
	if !got.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at %v is not in the future", got.ExpiresAt)
	}

	// Extract the token from the URL and clone with it — NO gatekeeper stub is set, so a
	// 200 proves the token alone authorized the read.
	token := got.CloneURL[len("https://git:"):strings.Index(got.CloneURL, "@")]
	wr := infoRefs(t, "/acme/widgets.git", string(svcUploadPack), basicAuth(token))
	if wr.Code != http.StatusOK {
		t.Fatalf("clone with token: status = %d, want 200; body=%s", wr.Code, wr.Body.String())
	}
}

// The token is read-only: it must never authorize a push.
func TestCloneToken_ReadOnlyRejectsPush(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "clone-signing-key")

	re := ensureMirror(t, "ci-user", "w")
	token, _ := mintCloneToken(re.ID, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/ci-user/w.git/"+string(svcReceivePack), strings.NewReader(""))
	req.Header.Set("Authorization", basicAuth(token))
	rec := httptest.NewRecorder()
	gitMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push with clone token: status = %d, want 403", rec.Code)
	}
}

// A token minted for one repo must not clone another (no cross-repo access, no
// enumeration).
func TestCloneToken_WrongRepoIs404(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "clone-signing-key")
	t.Setenv("GIT_HTTP_BASE_URL", "https://git.example.com")

	a := ensureMirror(t, "acme", "a")
	_ = ensureMirror(t, "acme", "b")
	tokenForA, _ := mintCloneToken(a.ID, time.Minute)

	rec := infoRefs(t, "/acme/b.git", string(svcUploadPack), basicAuth(tokenForA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-repo token: status = %d, want 404", rec.Code)
	}
}

// Minting is behind the internal key, and off entirely until the signing key is set.
func TestCloneToken_MintGuards(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	t.Setenv("GIT_FACTORY_INTERNAL_KEY", testInternalKey)
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "clone-signing-key")
	re := ensureMirror(t, "acme", "guarded")

	if rec := postCloneToken(t, map[string]any{"repo_id": re.ID}, "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong internal key: status = %d, want 401", rec.Code)
	}
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "")
	if rec := postCloneToken(t, map[string]any{"repo_id": re.ID}, testInternalKey); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no signing key: status = %d, want 503", rec.Code)
	}
}

// A tampered or expired token verifies as invalid.
func TestVerifyCloneToken_TamperAndExpiry(t *testing.T) {
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "clone-signing-key")

	good, _ := mintCloneToken("repo-1", time.Minute)
	if id, ok := verifyCloneToken(good); !ok || id != "repo-1" {
		t.Fatalf("valid token: id=%q ok=%v, want repo-1/true", id, ok)
	}
	if _, ok := verifyCloneToken(good + "x"); ok {
		t.Error("tampered signature verified")
	}
	if expired, _ := mintCloneToken("repo-1", -time.Second); func() bool { _, ok := verifyCloneToken(expired); return ok }() {
		t.Error("expired token verified")
	}
	// A different signing key must reject a token minted under the old one.
	t.Setenv("GIT_FACTORY_CLONE_TOKEN_KEY", "rotated-key")
	if _, ok := verifyCloneToken(good); ok {
		t.Error("token verified under a different key")
	}
}
