package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRepoNameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widgets.git":      "acme/widgets",
		"https://github.com/acme/widgets":          "acme/widgets",
		"https://gitlab.com/group/sub/project.git": "sub/project",
		"https://git.internal/solo":                "solo",
		"https://git.internal/":                    "git.internal",
	}
	for in, want := range cases {
		if got := repoNameFromURL(in); got != want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoManualCRUD(t *testing.T) {
	// frank links a generic backend (not enumerable) then pins a repo on it.
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "frank", createBackendRequest{
		Name: "internal", Type: backendGeneric, BaseURL: "https://git.internal",
		Auth: authConfig{Mode: modeBasic, Username: "frank", Password: "pw"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create backend: %d %s", rec.Code, rec.Body.String())
	}

	// Register a manual repo (no name → derived from URL).
	rec = httptest.NewRecorder()
	handleCreateRepo(rec, req("POST", "/repos", "frank", createRepoRequest{URL: "https://git.internal/team/app.git"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create repo: %d %s", rec.Code, rec.Body.String())
	}
	var created repoView
	json.Unmarshal(rec.Body.Bytes(), &created) //nolint:errcheck
	if created.ID == "" || created.Name != "team/app" || created.Source != repoSourceManual {
		t.Fatalf("unexpected repo: %+v", created)
	}
	if created.Backend != "internal" || created.BackendType != backendGeneric {
		t.Fatalf("backend not resolved from host: %+v", created)
	}

	// List shows it (generic backend contributes no enumerated repos).
	rec = httptest.NewRecorder()
	handleListRepos(rec, req("GET", "/repos", "frank", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// Another user must not see it.
	rec = httptest.NewRecorder()
	handleListRepos(rec, req("GET", "/repos", "grace", nil))
	if strings.Contains(rec.Body.String(), created.ID) {
		t.Fatal("cross-user leak: grace saw frank's repo")
	}

	// Duplicate URL → 409.
	rec = httptest.NewRecorder()
	handleCreateRepo(rec, req("POST", "/repos", "frank", createRepoRequest{URL: "https://git.internal/team/app.git"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d", rec.Code)
	}

	// Invalid URL → 400.
	rec = httptest.NewRecorder()
	handleCreateRepo(rec, req("POST", "/repos", "frank", createRepoRequest{URL: "not-a-url"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid url: expected 400, got %d", rec.Code)
	}

	// Delete (grace denied → 404; frank ok → 204).
	rec = httptest.NewRecorder()
	r := req("DELETE", "/repos/"+created.ID, "grace", nil)
	r.SetPathValue("id", created.ID)
	handleDeleteRepo(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("grace delete: expected 404, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r = req("DELETE", "/repos/"+created.ID, "frank", nil)
	r.SetPathValue("id", created.ID)
	handleDeleteRepo(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("frank delete: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRepoEnumerationForgejoMergesManual(t *testing.T) {
	// Mock Forgejo: /api/v1/user/repos returns one repo.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/repos" || r.Header.Get("Authorization") != "token fj-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{ //nolint:errcheck
			{"full_name": "heidi/site", "clone_url": srvCloneURL(r, "/heidi/site.git")},
		})
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "heidi", createBackendRequest{
		Name: "fj", Type: backendForgejo, BaseURL: srv.URL,
		Auth: authConfig{Mode: modeToken, Token: "fj-tok", Username: "heidi"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create backend: %d %s", rec.Code, rec.Body.String())
	}

	// A manual repo on the same host, plus a duplicate of the enumerated one.
	dup := srv.URL + "/heidi/site.git"
	for _, u := range []string{srv.URL + "/heidi/pinned.git", dup} {
		rec = httptest.NewRecorder()
		handleCreateRepo(rec, req("POST", "/repos", "heidi", createRepoRequest{URL: u}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create repo %s: %d %s", u, rec.Code, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	handleListRepos(rec, req("GET", "/repos", "heidi", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var repos []repoView
	json.Unmarshal(rec.Body.Bytes(), &repos) //nolint:errcheck

	var enumerated, manual, dupCount int
	for _, rp := range repos {
		switch rp.Source {
		case repoSourceEnumerated:
			enumerated++
		case repoSourceManual:
			manual++
		}
		if rp.URL == dup {
			dupCount++
		}
	}
	// pinned (manual) + site (enumerated, deduped against the manual dup) = the dup
	// appears exactly once and as the manual entry.
	if dupCount != 1 {
		t.Fatalf("expected the duplicate URL once, got %d (%+v)", dupCount, repos)
	}
	if manual != 2 {
		t.Fatalf("expected 2 manual repos, got %d (%+v)", manual, repos)
	}
	if enumerated != 0 {
		t.Fatalf("enumerated repo should have been deduped by the manual pin, got %d (%+v)", enumerated, repos)
	}
}

// srvCloneURL builds an absolute clone URL on the mock server's host.
func srvCloneURL(r *http.Request, path string) string {
	return "http://" + r.Host + path
}

func TestRepoPathFromURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widgets.git":      "acme/widgets",
		"https://github.com/acme/widgets":          "acme/widgets",
		"https://gitlab.com/group/sub/project.git": "group/sub/project",
		"https://git.internal/solo":                "solo",
	}
	for in, want := range cases {
		got, err := repoPathFromURL(in)
		if err != nil {
			t.Fatalf("repoPathFromURL(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("repoPathFromURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := repoPathFromURL("https://git.internal/"); err == nil {
		t.Error("expected error for pathless url")
	}
}

func TestListBranchesForgejo(t *testing.T) {
	// Mock Forgejo: the repo object gives the default branch; /branches lists them.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fj-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/repos/ivan/site":
			json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"}) //nolint:errcheck
		case "/api/v1/repos/ivan/site/branches":
			json.NewEncoder(w).Encode([]map[string]string{ //nolint:errcheck
				{"name": "main"}, {"name": "dev"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "ivan", createBackendRequest{
		Name: "fj", Type: backendForgejo, BaseURL: srv.URL,
		Auth: authConfig{Mode: modeToken, Token: "fj-tok", Username: "ivan"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create backend: %d %s", rec.Code, rec.Body.String())
	}

	repoURL := srv.URL + "/ivan/site.git"
	rec = httptest.NewRecorder()
	handleListBranches(rec, req("GET", "/repos/branches?url="+url.QueryEscape(repoURL), "ivan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list branches: %d %s", rec.Code, rec.Body.String())
	}
	var branches []branchView
	json.Unmarshal(rec.Body.Bytes(), &branches) //nolint:errcheck
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d (%+v)", len(branches), branches)
	}
	var sawDefault bool
	for _, b := range branches {
		if b.Name == "main" {
			sawDefault = b.Default
		}
		if b.Name == "dev" && b.Default {
			t.Errorf("dev should not be marked default: %+v", branches)
		}
	}
	if !sawDefault {
		t.Errorf("main should be marked default: %+v", branches)
	}
}

func TestListBranchesUnlinkedHost(t *testing.T) {
	// No backend linked for the URL's host → 404.
	rec := httptest.NewRecorder()
	handleListBranches(rec, req("GET", "/repos/branches?url="+url.QueryEscape("https://nowhere.example/a/b.git"), "judy", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
	}

	// Missing url query param → 400.
	rec = httptest.NewRecorder()
	handleListBranches(rec, req("GET", "/repos/branches", "judy", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestListBranchesGenericEmpty(t *testing.T) {
	// Generic backends have no branch API → empty list (UI falls back to free-text).
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "kim", createBackendRequest{
		Name: "internal", Type: backendGeneric, BaseURL: "https://scm.internal",
		Auth: authConfig{Mode: modeBasic, Username: "kim", Password: "pw"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create backend: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleListBranches(rec, req("GET", "/repos/branches?url="+url.QueryEscape("https://scm.internal/team/app.git"), "kim", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("expected empty array, got %s", rec.Body.String())
	}
}
