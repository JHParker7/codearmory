package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capturedRequest records the inbound request observed by a fake Gitea server:
// the method, the URL path (without the /api/v1 prefix being stripped — it is
// captured verbatim), the decoded JSON body, and a couple of headers that are
// part of how the gitea client constructs outgoing requests.
type capturedRequest struct {
	method   string
	path     string
	rawQuery string
	body     map[string]any
	sudo     string
	authz    string
}

// capturingGitea stands up a fake Gitea server that records the first inbound
// request into *got and replies with the provided status and JSON response
// body. It wires the global gitea client at the fake for the test duration via
// the existing fakeGitea seam.
func capturingGitea(t *testing.T, got *capturedRequest, status int, respJSON string) {
	t.Helper()
	fakeGitea(t, func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.rawQuery = r.URL.RawQuery
		got.sudo = r.Header.Get("Sudo")
		got.authz = r.Header.Get("Authorization")
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(respJSON)) //nolint:errcheck
	})
}

// ── create repo: outgoing POST /api/v1/user/repos ────────────────────────────

func TestHandleCreateRepo_SendsCreateRepoRequest(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"out-cr","org_id":""}`)
	insertTestAccount(t, "out-cr", "alice")

	var got capturedRequest
	capturingGitea(t, &got, http.StatusCreated, `{"id":7,"name":"newrepo","full_name":"alice/newrepo"}`)

	body, _ := json.Marshal(map[string]any{
		"name":        "newrepo",
		"description": "a brand new repo",
		"private":     true,
	})
	r := httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleCreateRepo(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("handler status: got %d, want 201", w.Code)
	}
	if got.method != http.MethodPost {
		t.Fatalf("outgoing method: got %q, want POST", got.method)
	}
	if got.path != "/api/v1/user/repos" {
		t.Fatalf("outgoing path: got %q, want /api/v1/user/repos", got.path)
	}
	if got.sudo != "alice" {
		t.Fatalf("outgoing Sudo header: got %q, want alice", got.sudo)
	}
	if got.authz != "token test-admin-token" {
		t.Fatalf("outgoing Authorization header: got %q, want token test-admin-token", got.authz)
	}
	if got.body["name"] != "newrepo" {
		t.Fatalf("outgoing body name: got %v, want newrepo", got.body["name"])
	}
	if got.body["description"] != "a brand new repo" {
		t.Fatalf("outgoing body description: got %v, want %q", got.body["description"], "a brand new repo")
	}
	if got.body["private"] != true {
		t.Fatalf("outgoing body private: got %v, want true", got.body["private"])
	}
}

// ── create pull: outgoing POST /api/v1/repos/{owner}/{name}/pulls ─────────────

func TestHandleCreatePull_SendsCreatePullRequest(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"out-cp","org_id":""}`)
	insertTestAccount(t, "out-cp", "alice")

	var got capturedRequest
	capturingGitea(t, &got, http.StatusCreated,
		`{"id":3,"number":11,"title":"My PR","state":"open"}`)

	body, _ := json.Marshal(map[string]any{
		"title": "My PR",
		"head":  "feature",
		"base":  "main",
		"body":  "please review",
	})
	r := httptest.NewRequest(http.MethodPost, "/repos/alice/myrepo/pulls", bytes.NewReader(body))
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleCreatePull(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("handler status: got %d, want 201", w.Code)
	}
	if got.method != http.MethodPost {
		t.Fatalf("outgoing method: got %q, want POST", got.method)
	}
	if got.path != "/api/v1/repos/alice/myrepo/pulls" {
		t.Fatalf("outgoing path: got %q, want /api/v1/repos/alice/myrepo/pulls", got.path)
	}
	if got.sudo != "alice" {
		t.Fatalf("outgoing Sudo header: got %q, want alice", got.sudo)
	}
	if got.body["title"] != "My PR" {
		t.Fatalf("outgoing body title: got %v, want %q", got.body["title"], "My PR")
	}
	if got.body["head"] != "feature" {
		t.Fatalf("outgoing body head: got %v, want feature", got.body["head"])
	}
	if got.body["base"] != "main" {
		t.Fatalf("outgoing body base: got %v, want main", got.body["base"])
	}
	if got.body["body"] != "please review" {
		t.Fatalf("outgoing body body: got %v, want %q", got.body["body"], "please review")
	}
}

// ── merge pull: outgoing POST /api/v1/repos/{owner}/{name}/pulls/{index}/merge ─
// Covers the default "Do" injection the handler performs before forwarding.

func TestHandleMergePull_SendsMergeRequest(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"out-mp","org_id":""}`)
	insertTestAccount(t, "out-mp", "alice")

	var got capturedRequest
	capturingGitea(t, &got, http.StatusNoContent, "")

	r := httptest.NewRequest(http.MethodPost, "/repos/alice/myrepo/pulls/4/merge",
		bytes.NewBufferString("{}"))
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo")
	r.SetPathValue("index", "4")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleMergePull(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("handler status: got %d, want 204", w.Code)
	}
	if got.method != http.MethodPost {
		t.Fatalf("outgoing method: got %q, want POST", got.method)
	}
	if got.path != "/api/v1/repos/alice/myrepo/pulls/4/merge" {
		t.Fatalf("outgoing path: got %q, want /api/v1/repos/alice/myrepo/pulls/4/merge", got.path)
	}
	if got.body["Do"] != "merge" {
		t.Fatalf("outgoing body Do: got %v, want merge (default injection)", got.body["Do"])
	}
}

// ── git proxy: outgoing request is streamed to Forgejo unchanged ──────────────
// Drives handleGitUploadPack through the real reverse proxy (initGitProxy)
// pointed at a capturing fake, and asserts the proxied method and path.

func TestHandleGitUploadPack_ProxiesRequest(t *testing.T) {
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"out-gp","org_id":""}`)

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("packdata")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	origGitea := gitea
	origProxy := gitProxy
	gitea = &giteaClient{baseURL: srv.URL, adminToken: "test-admin-token", http: httpClient}
	initGitProxy()
	t.Cleanup(func() {
		gitea = origGitea
		gitProxy = origProxy
	})

	r := httptest.NewRequest(http.MethodPost, "/alice/myrepo.git/git-upload-pack",
		bytes.NewBufferString("0000"))
	r.SetPathValue("owner", "alice")
	r.SetPathValue("name", "myrepo.git")
	r.Header.Set("Authorization", "Bearer sometoken")
	w := httptest.NewRecorder()
	handleGitUploadPack(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("handler status: got %d, want 200", w.Code)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("proxied method: got %q, want POST", gotMethod)
	}
	if gotPath != "/alice/myrepo.git/git-upload-pack" {
		t.Fatalf("proxied path: got %q, want /alice/myrepo.git/git-upload-pack", gotPath)
	}
}
