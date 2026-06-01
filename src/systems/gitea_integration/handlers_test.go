package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeGatekeeper spins up a test server that always returns the given status
// and body, overriding gatekeeperClient.URL for the test duration.
func fakeGatekeeper(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	orig := gatekeeperClient.URL
	gatekeeperURL = srv.URL
	gatekeeperClient.URL = srv.URL
	t.Cleanup(func() {
		gatekeeperClient.URL = orig
		gatekeeperURL = orig
		srv.Close()
	})
}

// fakeGitea spins up a test HTTP server for the Gitea API and wires gitea to
// point at it for the test duration.
func fakeGitea(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := gitea
	gitea = &giteaClient{baseURL: srv.URL, adminToken: "test-admin-token", http: httpClient}
	t.Cleanup(func() {
		gitea = orig
		srv.Close()
	})
	return srv
}

// insertTestAccount writes a GiteaAccount directly into the test DB and
// registers a cleanup that removes it.
func insertTestAccount(t *testing.T, userID, giteaUsername string) GiteaAccount {
	t.Helper()
	a := GiteaAccount{UserID: userID, GiteaUsername: giteaUsername}
	if err := connect().Create(&a).Error; err != nil {
		t.Fatalf("insertTestAccount: %v", err)
	}
	t.Cleanup(func() { connect().Where("user_id = ?", userID).Delete(&GiteaAccount{}) }) //nolint:errcheck
	return a
}

func TestMain(m *testing.M) {
	conn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		log.Fatal(err)
	}
	if err := conn.AutoMigrate(&GiteaAccount{}); err != nil {
		log.Fatal(err)
	}
	gormDB = conn

	initMetrics()
	httpClient = &http.Client{}
	gatekeeperClient = &gk.Client{URL: gatekeeperURL, Service: "gitea", HTTPClient: httpClient}
	gitea = &giteaClient{baseURL: "http://127.0.0.1:1", adminToken: "test", http: httpClient}

	os.Exit(m.Run())
}

// ── ownerAllowed ──────────────────────────────────────────────────────────────

func TestOwnerAllowed_PersonalRepo(t *testing.T) {
	if !ownerAllowed("alice", "alice", "") {
		t.Fatal("owner should be allowed on their own repos")
	}
}

func TestOwnerAllowed_OrgRepo(t *testing.T) {
	if !ownerAllowed("my-org", "alice", "my-org") {
		t.Fatal("org member should be allowed on org repos")
	}
}

func TestOwnerAllowed_DifferentUser(t *testing.T) {
	if ownerAllowed("alice", "bob", "") {
		t.Fatal("unrelated user should not be allowed")
	}
}

func TestOwnerAllowed_EmptyOrg(t *testing.T) {
	if ownerAllowed("my-org", "alice", "") {
		t.Fatal("empty org name should not grant org access")
	}
}

func TestOwnerAllowed_OrgMismatch(t *testing.T) {
	if ownerAllowed("other-org", "alice", "my-org") {
		t.Fatal("different org should not be allowed")
	}
}

// ── isGitProxyPath ────────────────────────────────────────────────────────────

func TestIsGitProxyPath_InfoRefs(t *testing.T) {
	if !isGitProxyPath("/owner/repo/info/refs") {
		t.Fatal("info/refs should be a git proxy path")
	}
}

func TestIsGitProxyPath_UploadPack(t *testing.T) {
	if !isGitProxyPath("/owner/repo/git-upload-pack") {
		t.Fatal("git-upload-pack should be a git proxy path")
	}
}

func TestIsGitProxyPath_ReceivePack(t *testing.T) {
	if !isGitProxyPath("/owner/repo/git-receive-pack") {
		t.Fatal("git-receive-pack should be a git proxy path")
	}
}

func TestIsGitProxyPath_Regular(t *testing.T) {
	if isGitProxyPath("/repos/owner/repo") {
		t.Fatal("regular API path should not be a git proxy path")
	}
}

// ── repoName ──────────────────────────────────────────────────────────────────

func TestRepoName_StripsDotGit(t *testing.T) {
	if got := repoName("myrepo.git"); got != "myrepo" {
		t.Fatalf("got %q, want myrepo", got)
	}
}

func TestRepoName_NoDotGit(t *testing.T) {
	if got := repoName("myrepo"); got != "myrepo" {
		t.Fatalf("got %q, want myrepo", got)
	}
}

// ── bearerToken ───────────────────────────────────────────────────────────────

func TestBearerToken_BearerHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer my-token")
	if got := bearerToken(r); got != "my-token" {
		t.Fatalf("got %q, want my-token", got)
	}
}

func TestBearerToken_BasicAuth(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	encoded := base64.StdEncoding.EncodeToString([]byte("git:my-password"))
	r.Header.Set("Authorization", "Basic "+encoded)
	if got := bearerToken(r); got != "my-password" {
		t.Fatalf("got %q, want my-password", got)
	}
}

func TestBearerToken_NoAuth(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := bearerToken(r); got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

// ── giteaError / isNotFoundErr ────────────────────────────────────────────────

func TestGiteaError_Error(t *testing.T) {
	e := &giteaError{Status: 404, Body: "not found"}
	want := "gitea 404: not found"
	if got := e.Error(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestIsNotFoundErr_404(t *testing.T) {
	if !isNotFoundErr(&giteaError{Status: http.StatusNotFound}) {
		t.Fatal("expected true for 404 giteaError")
	}
}

func TestIsNotFoundErr_OtherStatus(t *testing.T) {
	if isNotFoundErr(&giteaError{Status: http.StatusBadRequest}) {
		t.Fatal("expected false for non-404 giteaError")
	}
}

func TestIsNotFoundErr_NonGiteaError(t *testing.T) {
	if isNotFoundErr(fmt.Errorf("some other error")) {
		t.Fatal("expected false for non-giteaError")
	}
}

// ── handler auth (no token → 401) ────────────────────────────────────────────

func TestHandleGetAccount_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/account", nil)
	w := httptest.NewRecorder()
	handleGetAccount(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleLinkAccount_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/account", nil)
	w := httptest.NewRecorder()
	handleLinkAccount(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleUnlinkAccount_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/account", nil)
	w := httptest.NewRecorder()
	handleUnlinkAccount(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListRepos_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos", nil)
	w := httptest.NewRecorder()
	handleListRepos(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreateRepo_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/repos", nil)
	w := httptest.NewRecorder()
	handleCreateRepo(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetRepo_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleGetRepo(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleDeleteRepo_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/repos/owner/name", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleDeleteRepo(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListBranches_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name/branches", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleListBranches(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListTags_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name/tags", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleListTags(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListReleases_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name/releases", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleListReleases(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListCommits_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name/commits", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleListCommits(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleListPulls_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name/pulls", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleListPulls(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleCreatePull_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/repos/owner/name/pulls", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	w := httptest.NewRecorder()
	handleCreatePull(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleGetPull_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/owner/name/pulls/1", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	r.SetPathValue("index", "1")
	w := httptest.NewRecorder()
	handleGetPull(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestHandleMergePull_Unauthorized(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/repos/owner/name/pulls/1/merge", nil)
	r.SetPathValue("owner", "owner")
	r.SetPathValue("name", "name")
	r.SetPathValue("index", "1")
	w := httptest.NewRecorder()
	handleMergePull(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

// ── handleLinkAccount validation (no DB needed — fails before DB call) ────────

func TestHandleLinkAccount_MissingUsername(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	body, _ := json.Marshal(map[string]string{"gitea_token": "tok"})
	r := httptest.NewRequest(http.MethodPut, "/account", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleLinkAccount(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleLinkAccount_MissingToken(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	body, _ := json.Marshal(map[string]string{"gitea_username": "alice"})
	r := httptest.NewRequest(http.MethodPut, "/account", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleLinkAccount(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleLinkAccount_InvalidBody(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	r := httptest.NewRequest(http.MethodPut, "/account", bytes.NewBufferString("not-json"))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleLinkAccount(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleLinkAccount_GiteaTokenInvalid(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	fakeGitea(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"token invalid"}`)) //nolint:errcheck
	})
	body, _ := json.Marshal(map[string]string{"gitea_username": "alice", "gitea_token": "bad-tok"})
	r := httptest.NewRequest(http.MethodPut, "/account", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleLinkAccount(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", w.Code)
	}
}

func TestHandleLinkAccount_UsernameMismatch(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1","org_id":""}`)
	fakeGitea(t, func(w http.ResponseWriter, r *http.Request) {
		// Token belongs to "bob", but caller claims "alice".
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"login": "bob"}) //nolint:errcheck
	})
	body, _ := json.Marshal(map[string]string{"gitea_username": "alice", "gitea_token": "bobs-tok"})
	r := httptest.NewRequest(http.MethodPut, "/account", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleLinkAccount(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", w.Code)
	}
}

// ── handleGetAccount — DB (SQLite) ────────────────────────────────────────────

func TestHandleGetAccount_NotFound(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"no-such-user","org_id":""}`)
	r := httptest.NewRequest(http.MethodGet, "/account", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleGetAccount(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// ── handleUnlinkAccount — DB (SQLite) ─────────────────────────────────────────

func TestHandleUnlinkAccount_NotFound(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"no-account-user","org_id":""}`)
	r := httptest.NewRequest(http.MethodDelete, "/account", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleUnlinkAccount(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// ── handleListPulls state validation — DB (SQLite) ────────────────────────────

func TestHandleListPulls_InvalidState(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-pr","org_id":""}`)
	insertTestAccount(t, "user-pr", "alice")
	r := httptest.NewRequest(http.MethodGet, "/repos/alice/myrepo/pulls?state=invalid", nil)
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleListPulls(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── handleCreateRepo validation — DB (SQLite) ─────────────────────────────────

func TestHandleCreateRepo_MissingName(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-cr","org_id":""}`)
	insertTestAccount(t, "user-cr", "alice")
	body, _ := json.Marshal(map[string]string{"description": "no name here"})
	r := httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleCreateRepo(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── handleCreatePull validation — DB (SQLite) ─────────────────────────────────

func TestHandleCreatePull_MissingTitle(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-cp","org_id":""}`)
	insertTestAccount(t, "user-cp", "alice")
	body, _ := json.Marshal(map[string]string{"head": "feature", "base": "main"})
	r := httptest.NewRequest(http.MethodPost, "/repos/alice/myrepo/pulls", bytes.NewReader(body))
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleCreatePull(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreatePull_MissingHead(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-cp2","org_id":""}`)
	insertTestAccount(t, "user-cp2", "alice")
	body, _ := json.Marshal(map[string]string{"title": "My PR", "base": "main"})
	r := httptest.NewRequest(http.MethodPost, "/repos/alice/myrepo/pulls", bytes.NewReader(body))
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleCreatePull(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCreatePull_MissingBase(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"user-cp3","org_id":""}`)
	insertTestAccount(t, "user-cp3", "alice")
	body, _ := json.Marshal(map[string]string{"title": "My PR", "head": "feature"})
	r := httptest.NewRequest(http.MethodPost, "/repos/alice/myrepo/pulls", bytes.NewReader(body))
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleCreatePull(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── statusResponseWriter ──────────────────────────────────────────────────────

func TestStatusResponseWriter_WriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusNotFound {
		t.Fatalf("rw.status: got %d, want 404", rw.status)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder: got %d, want 404", w.Code)
	}
}
