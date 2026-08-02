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

// TestUpdateRepoSyncPartial pins the partial-update contract of PUT /repos/{id}: a
// field the body omits keeps its stored value. Force-writing both columns on every
// request (the old behaviour) meant a branches-only PUT silently turned sync OFF, and
// an enabled-only PUT wiped the allowlist — so sync fell back to "main" and would run
// configs from a branch the user had deliberately excluded, which is the whole point
// of the allowlist.
func TestUpdateRepoSyncPartial(t *testing.T) {
	rec := httptest.NewRecorder()
	handleCreateBackend(rec, req("POST", "/backends", "nora", createBackendRequest{
		Name: "internal", Type: backendGeneric, BaseURL: "https://scm.nora",
		Auth: authConfig{Mode: modeBasic, Username: "nora", Password: "pw"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create backend: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleCreateRepo(rec, req("POST", "/repos", "nora", createRepoRequest{URL: "https://scm.nora/team/app.git"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create repo: %d %s", rec.Code, rec.Body.String())
	}
	var created repoView
	json.Unmarshal(rec.Body.Bytes(), &created) //nolint:errcheck

	put := func(t *testing.T, owner, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/repos/"+created.ID, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+owner)
		r.SetPathValue("id", created.ID)
		handleUpdateRepo(rec, r)
		return rec
	}
	stored := func(t *testing.T) GitRepo {
		t.Helper()
		var got GitRepo
		if err := gormDB.Where("id = ?", created.ID).First(&got).Error; err != nil {
			t.Fatalf("read back repo: %v", err)
		}
		return got
	}

	// Both fields supplied: both are written.
	if rec := put(t, "nora", `{"workflow_sync_enabled":true,"workflow_sync_branches":["release","main"]}`); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if got := stored(t); !got.WorkflowSyncEnabled || len(got.WorkflowSyncBranches) != 2 {
		t.Fatalf("after enable: %+v", got)
	}

	// Branches only: sync must STAY enabled.
	if rec := put(t, "nora", `{"workflow_sync_branches":["release"]}`); rec.Code != http.StatusOK {
		t.Fatalf("branches only: %d %s", rec.Code, rec.Body.String())
	}
	got := stored(t)
	if !got.WorkflowSyncEnabled {
		t.Error("a branches-only update disabled sync")
	}
	if len(got.WorkflowSyncBranches) != 1 || got.WorkflowSyncBranches[0] != "release" {
		t.Errorf("branches = %v, want [release]", got.WorkflowSyncBranches)
	}

	// Enabled only: the allowlist must survive untouched.
	if rec := put(t, "nora", `{"workflow_sync_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enabled only: %d %s", rec.Code, rec.Body.String())
	}
	got = stored(t)
	if len(got.WorkflowSyncBranches) != 1 || got.WorkflowSyncBranches[0] != "release" {
		t.Errorf("an enabled-only update wiped the allowlist: %v", got.WorkflowSyncBranches)
	}
	if want := []string{"release"}; effectiveSyncBranches(got)[0] != want[0] {
		t.Errorf("effective branches = %v, want %v", effectiveSyncBranches(got), want)
	}

	// An EXPLICIT empty list still clears it — absent and empty are different.
	if rec := put(t, "nora", `{"workflow_sync_branches":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("clear branches: %d %s", rec.Code, rec.Body.String())
	}
	got = stored(t)
	if len(got.WorkflowSyncBranches) != 0 {
		t.Errorf("explicit [] did not clear the allowlist: %v", got.WorkflowSyncBranches)
	}
	if !got.WorkflowSyncEnabled {
		t.Error("clearing branches also disabled sync")
	}

	// An explicit false is still written (Select forces the zero value through).
	if rec := put(t, "nora", `{"workflow_sync_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if stored(t).WorkflowSyncEnabled {
		t.Error("explicit false did not disable sync")
	}

	// Another user cannot touch it, and an empty body still 404s a repo that is not theirs.
	if rec := put(t, "oscar", `{"workflow_sync_enabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user update: expected 404, got %d", rec.Code)
	}
	if rec := put(t, "oscar", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user empty update: expected 404, got %d", rec.Code)
	}
	if stored(t).WorkflowSyncEnabled {
		t.Error("another user's update took effect")
	}
}
