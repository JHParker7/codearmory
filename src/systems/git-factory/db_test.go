package main

// Unit-level executable spec for the repo CREATION path (ARCHITECTURE.md §2a, §4,
// §5-Step1, §6). These are the fast, SQLite-backed, no-docker mirror of the
// integration contract in tests/test_repos.py — same shortening-the-feedback-loop
// intent, runnable with `go test`.
//
// Like the integration suite, some of these FAIL until Step 1 is implemented —
// that is deliberate. Each failing test names the ARCHITECTURE requirement and
// the file that must change. The currently-passing ones lock in behavior that is
// already correct so it doesn't regress.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupTestDB points the package-global GORM handles at a throwaway SQLite
// database (a file under t.TempDir(), which survives GORM's connection pool
// unlike a bare :memory: DSN) and runs the same migration main() does. It also
// isolates on-disk git side effects: GIT_STORAGE_ROOT is set to a fresh temp
// dir, and the process chdirs into the temp dir so the current stub's relative
// "temp/repos/..." writes don't litter the source tree either. Everything is
// restored on cleanup. Tests using it must not run in parallel — they share the
// process-global connections, working directory, and env.
func setupTestDB(t *testing.T) (db *gorm.DB, storageRoot string) {
	t.Helper()

	dir := t.TempDir()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(dir, "test.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&Repo{}, &ShardNode{}, &ReplicaState{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	dbInitMu.Lock()
	prevDB, prevRead := gormDB, gormDBRead
	gormDB, gormDBRead = gdb, gdb
	dbInitMu.Unlock()

	storageRoot = filepath.Join(dir, "storage")
	t.Setenv("GIT_STORAGE_ROOT", storageRoot)

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
		dbInitMu.Lock()
		gormDB, gormDBRead = prevDB, prevRead
		dbInitMu.Unlock()
	})
	return gdb, storageRoot
}

// repoDiskPathForTest mirrors the on-disk layout ARCHITECTURE §4 mandates:
// ${GIT_STORAGE_ROOT}/{id[:2]}/{id}.git — derived from the STABLE id (so a rename
// never moves bytes), never from namespace/name. The test hard-codes the formula
// on purpose: it is part of the contract being pinned.
func repoDiskPathForTest(storageRoot, id string) string {
	return filepath.Join(storageRoot, id[:2], id+".git")
}

// isBareRepo reports whether path is an initialized bare git repository.
func isBareRepo(path string) bool {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--is-bare-repository").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// ── DB layer: Repo.Add ──────────────────────────────────────────────────────

// Passes today: the row is persisted and the default branch is applied.
func TestRepoAdd_PersistsRow(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	re := Repo{ID: "ab111111-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets", Description: "my repo"}
	if err := re.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := getRepo(ctx, "user-1", re.ID)
	if err != nil {
		t.Fatalf("getRepo: %v", err)
	}
	if got.Name != "widgets" || got.Owner != "user-1" || got.Description != "my repo" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want %q (gorm default:main)", got.DefaultBranch, "main")
	}
}

// RED until Step 1: Add must `git init --bare` a real repo at the id-derived
// path (ARCHITECTURE §2a + §4). The current git.go stub makes a plain, non-bare
// os.Mkdir at temp/repos/{ns}{name} and ignores GIT_STORAGE_ROOT — so no bare
// repo appears here. Fix: git.go (shell out to `git init --bare`), db.go Add.
func TestRepoAdd_InitializesBareRepoAtIdPath(t *testing.T) {
	_, storageRoot := setupTestDB(t)
	ctx := context.Background()

	re := Repo{ID: "cd222222-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	if err := re.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}

	path := repoDiskPathForTest(storageRoot, re.ID)
	if !isBareRepo(path) {
		t.Fatalf("expected a bare git repo at %s (ARCHITECTURE §2a/§4); "+
			"git.go must `git init --bare` the id-derived path", path)
	}
}

// RED until Step 1: if the on-disk init fails, the metadata row must be rolled
// back — "never leave a metadata row without a repo" (ARCHITECTURE §2a, §6).
// Here GIT_STORAGE_ROOT is forced to a *file*, so any attempt to create the
// storage directory tree underneath it must fail. The current Add creates the
// row first (via the stub that ignores the failure), leaving an orphan.
func TestRepoAdd_RollsBackRowWhenGitInitFails(t *testing.T) {
	_, dir := setupTestDB(t)
	ctx := context.Background()

	// Replace the storage root with a regular file so mkdir-under-it fails.
	blocker := dir + "-file"
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	t.Setenv("GIT_STORAGE_ROOT", blocker)

	re := Repo{ID: "ef333333-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	err := re.Add(ctx)
	if err == nil {
		t.Fatal("expected Add to fail when git init cannot create the repo on disk")
	}
	if _, gErr := getRepo(ctx, "user-1", re.ID); gErr == nil {
		t.Fatal("orphan metadata row left after failed git init — Add must roll back the row (ARCHITECTURE §2a/§6)")
	}
}

// Passes today: same owner+name violates the ux_owner_name unique index, and the
// SQLite error is recognized by isUniqueViolation (helpers.go).
func TestRepoAdd_DuplicateNameSameOwner(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	first := Repo{ID: "aa111111-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	if err := first.Add(ctx); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	dup := Repo{ID: "aa222222-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	err := dup.Add(ctx)
	if err == nil {
		t.Fatal("expected unique-constraint error on duplicate name, got nil")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("isUniqueViolation should recognize the error, got: %v", err)
	}

	repos, err := listRepos(ctx, "user-1", nil)
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo after rejected duplicate, got %d", len(repos))
	}
}

// Passes today: the unique index is (owner, name), so a different owner may reuse
// a name — the ownership model in ARCHITECTURE §3.
// Different namespaces may reuse a name: the unique key is (namespace, name), so
// alice/widgets and bob/widgets are distinct clone URLs and both must persist.
func TestRepoAdd_SameNameDifferentOwners(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	a := Repo{ID: "aa333333-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	b := Repo{ID: "aa444444-0000-0000-0000-000000000000", Owner: "user-2", Namespace: "bob", Name: "widgets"}
	if err := a.Add(ctx); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	if err := b.Add(ctx); err != nil {
		t.Fatalf("Add b (different owner, same name): %v", err)
	}
	if _, err := getRepo(ctx, "user-2", b.ID); err != nil {
		t.Fatalf("getRepo for second owner: %v", err)
	}
}

// The collision the (namespace, name) unique key exists to prevent: two DIFFERENT
// owners inside the SAME namespace — the realistic case being two members of one
// org — must not both create "widgets", because both rows would resolve to the
// same clone URL /acme/widgets.git. Under the previous (owner, name) key these
// were considered distinct and both were accepted.
func TestRepoAdd_SameNameSameNamespaceDifferentOwnersConflicts(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	first := Repo{ID: "aa555555-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "acme", Name: "widgets"}
	if err := first.Add(ctx); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Same org namespace, same name, different owner.
	dup := Repo{ID: "aa666666-0000-0000-0000-000000000000", Owner: "user-2", Namespace: "acme", Name: "widgets"}
	err := dup.Add(ctx)
	if err == nil {
		t.Fatal("expected a unique-constraint error: acme/widgets already exists, " +
			"so a second owner must not be able to claim the same clone URL")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("isUniqueViolation should recognize the error, got: %v", err)
	}
}

// Passes today: created rows are owner-scoped in listRepos (no cross-owner leak).
func TestRepoAdd_AppearsInOwnerScopedList(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	for _, re := range []Repo{
		{ID: "aa555555-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "one"},
		{ID: "aa666666-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "two"},
		{ID: "aa777777-0000-0000-0000-000000000000", Owner: "user-2", Namespace: "bob", Name: "three"},
	} {
		if err := re.Add(ctx); err != nil {
			t.Fatalf("Add %s: %v", re.ID, err)
		}
	}

	repos, err := listRepos(ctx, "user-1", nil)
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos for user-1, got %d", len(repos))
	}
	for _, r := range repos {
		if r.Owner != "user-1" {
			t.Errorf("listRepos leaked another owner's repo: %+v", r)
		}
	}
}

// ── HTTP handler: handleCreateRepo ──────────────────────────────────────────

// newGatekeeperStub replaces the package-global gatekeeperClient AND the raw
// gatekeeperURL/httpClient globals with ones that talk to a single local stub.
// The stub:
//   - POST /check_permissions → authorises any request carrying a Bearer token, as userID.
//   - GET  /users/{id}        → returns a user whose username mirrors {id}.
//   - GET  /orgs/{id}         → returns an org whose org_name mirrors {id}.
//
// Mirroring the id into username/org_name keeps a created repo's Owner equal to
// the caller's id, so the owner-scoped assertions elsewhere still hold. It
// restores every swapped global on cleanup.
// newGatekeeperStubForNamespace models real gatekeeper now that per-record resources
// are owner-first: it authorizes only resources inside the caller's own namespace, so
// a request for someone else's repo is DENIED here rather than by the service. The
// plain newGatekeeperStub below still authorizes everything, which is right for tests
// about handler mechanics rather than about the access boundary.
func newGatekeeperStubForNamespace(t *testing.T, userID, namespace string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			var body struct{ Resource string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Owner-first: "<namespace>/<service>/..." — anything else belongs to
			// someone else, or is an unscoped collection resource.
			if strings.HasPrefix(body.Resource, namespace+"/") || !strings.Contains(body.Resource, "/repos/") {
				writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": userID})
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			id := strings.TrimPrefix(r.URL.Path, "/users/")
			writeJSON(w, http.StatusOK, map[string]any{"user_id": id, "username": id, "active": true})
		default:
			http.NotFound(w, r)
		}
	}))
	prevClient, prevURL, prevHTTP := gatekeeperClient, gatekeeperURL, httpClient
	gatekeeperClient = &gk.Client{URL: srv.URL, Service: serviceName, HTTPClient: srv.Client()}
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() {
		srv.Close()
		gatekeeperClient = prevClient
		gatekeeperURL = prevURL
		httpClient = prevHTTP
	})
}

func newGatekeeperStub(t *testing.T, userID string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": userID})
		case strings.HasPrefix(r.URL.Path, "/users/"):
			id := strings.TrimPrefix(r.URL.Path, "/users/")
			writeJSON(w, http.StatusOK, map[string]any{"user_id": id, "username": id, "active": true})
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			id := strings.TrimPrefix(r.URL.Path, "/orgs/")
			writeJSON(w, http.StatusOK, map[string]any{"org_id": id, "org_name": id, "active": true})
		default:
			http.NotFound(w, r)
		}
	}))
	prevClient, prevURL, prevHTTP := gatekeeperClient, gatekeeperURL, httpClient
	gatekeeperClient = &gk.Client{URL: srv.URL, Service: serviceName, HTTPClient: srv.Client()}
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() {
		srv.Close()
		gatekeeperClient = prevClient
		gatekeeperURL = prevURL
		httpClient = prevHTTP
	})
}

func postCreateRepo(t *testing.T, body map[string]any, withBearer bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/repos", bytes.NewReader(raw))
	if withBearer {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	rec := httptest.NewRecorder()
	handleCreateRepo(rec, req)
	return rec
}

// Passes today: gatekeeper rejects the missing Bearer before any work.
func TestHandleCreateRepo_Unauthorized(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := postCreateRepo(t, map[string]any{"name": "widgets"}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Passes today: a blank/whitespace name is rejected before any DB write.
func TestHandleCreateRepo_MissingName(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := postCreateRepo(t, map[string]any{"name": "   ", "description": "x"}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	repos, err := listRepos(context.Background(), "user-1", nil)
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 0 {
		t.Fatalf("expected no repos created, got %d", len(repos))
	}
}

// RED until Step 1: repo names become filesystem/URL path segments, so traversal
// and separators must be rejected with 400 BEFORE any create (ARCHITECTURE §6,
// mirrors test_repos.py::test_create_rejects_bad_name). The current handler only
// trims and checks for empty, so "../escape" et al. slip through. Fix:
// api_repo.go handleCreateRepo — validate the name against [A-Za-z0-9._-]+ and
// reject a ".git" name.
func TestHandleCreateRepo_RejectsBadNames(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	for _, bad := range []string{"../escape", "a/b", "with space", ".git"} {
		rec := postCreateRepo(t, map[string]any{"name": bad}, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want %d", bad, rec.Code, http.StatusBadRequest)
		}
	}
}

// RED until Step 1: the create response must carry the fields the Smart-HTTP
// layer and UI depend on — namespace and a derived http_url ending in
// /{namespace}/{name}.git (ARCHITECTURE §4, mirrors
// test_repos.py::test_create_response_has_git_fields). The current handler never
// sets Namespace (it's absent from the token wiring) and there is no http_url
// field at all. Fix: types.go (add derived HTTPURL, gorm:"-"), api_repo.go
// (populate namespace + http_url from GIT_HTTP_BASE_URL).
func TestHandleCreateRepo_ReturnsGitFields(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := postCreateRepo(t, map[string]any{"name": "alpha", "description": "first"}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["id"] == "" || got["id"] == nil {
		t.Error("response missing stable id")
	}
	if got["name"] != "alpha" {
		t.Errorf("name = %v, want alpha", got["name"])
	}
	if got["default_branch"] == "" || got["default_branch"] == nil {
		t.Error("response missing default_branch")
	}
	ns, _ := got["namespace"].(string)
	if ns == "" {
		t.Error("response missing namespace (the /{namespace}/{name}.git clone segment)")
	}
	httpURL, _ := got["http_url"].(string)
	wantSuffix := "/" + ns + "/alpha.git"
	if httpURL == "" || !strings.HasSuffix(httpURL, wantSuffix) {
		t.Errorf("http_url = %q, want it to end in %q", httpURL, wantSuffix)
	}
}

// RED until Step 1 (partially): the happy path must persist an owner-scoped row
// and return 201 with a real bare repo on disk. The 201 + persistence pass today;
// the on-disk bare repo assertion is RED until git.go is implemented.
func TestHandleCreateRepo_Created(t *testing.T) {
	_, storageRoot := setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := postCreateRepo(t, map[string]any{"name": "widgets", "description": "desc"}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatal("expected a generated repo ID in the response")
	}

	// Owner is json:"-", so ownership is asserted against the persisted row.
	stored, err := getRepo(context.Background(), "user-1", id)
	if err != nil {
		t.Fatalf("getRepo after create: %v", err)
	}
	if stored.Name != "widgets" {
		t.Fatalf("stored repo mismatch: %+v", stored)
	}

	if !isBareRepo(repoDiskPathForTest(storageRoot, id)) {
		t.Errorf("expected a bare git repo on disk for created repo %s (ARCHITECTURE §2a)", id)
	}
}

// Passes today: a duplicate name maps to 409 Conflict (via isUniqueViolation).
func TestHandleCreateRepo_DuplicateConflict(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := postCreateRepo(t, map[string]any{"name": "widgets"}, true); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	rec := postCreateRepo(t, map[string]any{"name": "widgets"}, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}
