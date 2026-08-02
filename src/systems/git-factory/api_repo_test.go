package main

// Unit-level executable spec for the repo READ/UPDATE/DELETE handlers
// (ARCHITECTURE.md §2a, §3, §6). The create path lives in db_test.go; this file
// covers list/get/update/delete — the rest of the management API contract that
// tests/test_repos.py pins as an integration test, mirrored here as fast,
// SQLite-backed unit tests.
//
// As in db_test.go, most tests lock in behavior that is already correct; a few
// are RED until Step 1 is finished, and each RED test names the ARCHITECTURE
// requirement and the file that must change. Tests share process-global state
// (DB handles, gatekeeperClient, cwd, env) via setupTestDB — they must not run
// in parallel.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// seedRepo inserts a Repo row owned by `owner` directly through the DB layer, so
// handler tests can exercise ownership scoping without going through create.
func seedRepo(t *testing.T, id, owner, ns, name, desc string) Repo {
	t.Helper()
	re := Repo{ID: id, Owner: owner, Namespace: ns, Name: name, Description: desc}
	if err := re.Add(context.Background()); err != nil {
		t.Fatalf("seed repo %s: %v", id, err)
	}
	return re
}

func setBearer(req *http.Request, on bool) {
	if on {
		req.Header.Set("Authorization", "Bearer test-token")
	}
}

func doList(t *testing.T, withBearer bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	setBearer(req, withBearer)
	rec := httptest.NewRecorder()
	handleListRepos(rec, req)
	return rec
}

func doGet(t *testing.T, id string, withBearer bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/repos/"+id, nil)
	req.SetPathValue("id", id)
	setBearer(req, withBearer)
	rec := httptest.NewRecorder()
	handleGetRepo(rec, req)
	return rec
}

func doPatch(t *testing.T, id string, body map[string]any, withBearer bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/repos/"+id, bytes.NewReader(raw))
	req.SetPathValue("id", id)
	setBearer(req, withBearer)
	rec := httptest.NewRecorder()
	handleUpdateRepo(rec, req)
	return rec
}

func doDelete(t *testing.T, id string, withBearer bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/repos/"+id, nil)
	req.SetPathValue("id", id)
	setBearer(req, withBearer)
	rec := httptest.NewRecorder()
	handleDeleteRepo(rec, req)
	return rec
}

// ── RBAC resource naming (ARCHITECTURE §2a) ─────────────────────────────────

// Passes today: resRepo builds "<service>/<collection>/<id>" — the resource
// string gatekeeper auto-scopes by username.
func TestResRepoNaming(t *testing.T) {
	if resRepos != serviceName+"/repos" {
		t.Errorf("resRepos = %q, want %q", resRepos, serviceName+"/repos")
	}
	// Per-record resources lead with the repo's OWNER, not the caller — that is what
	// lets a grant name someone else's repository. The last segment is the stable id,
	// so a rename (metadata-only, §4) cannot invalidate a grant.
	re := Repo{ID: "abc", Namespace: "alice", Name: "widgets"}
	if got, want := resRepoOf(re), "alice/"+serviceName+"/repos/abc"; got != want {
		t.Errorf("resRepoOf = %q, want %q", got, want)
	}
	other := Repo{ID: "abc", Namespace: "bob", Name: "widgets"}
	if resRepoOf(re) == resRepoOf(other) {
		t.Error("two owners' repos produced the same resource — sharing would be inexpressible")
	}
}

// ── list (ARCHITECTURE §2a, §3) ─────────────────────────────────────────────

// Passes today: gatekeeper rejects the missing Bearer before any work.
func TestHandleListRepos_Unauthorized(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := doList(t, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Passes today: an empty list serializes as [] (a JSON array), never null — the
// handler normalizes nil to []Repo{}.
func TestHandleListRepos_EmptyReturnsArray(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := doList(t, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("response is not a JSON array: %v (body %q)", err, rec.Body.String())
	}
	if len(arr) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(arr))
	}
}

// Passes today: list is owner-scoped — another user's repos never leak through
// (ARCHITECTURE §3, mirrors test_repos.py::test_list_excludes_other_users_repo).
func TestHandleListRepos_OwnerScoped(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	seedRepo(t, "b1000000-0000-0000-0000-000000000000", "user-1", "alice", "one", "")
	seedRepo(t, "b2000000-0000-0000-0000-000000000000", "user-1", "alice", "two", "")
	seedRepo(t, "b3000000-0000-0000-0000-000000000000", "user-2", "bob", "three", "")

	rec := doList(t, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 repos for user-1, got %d (body %q)", len(arr), rec.Body.String())
	}
	for _, r := range arr {
		if r["name"] == "three" {
			t.Errorf("list leaked another owner's repo: %v", r)
		}
	}
}

// ── get (ARCHITECTURE §2a, §3) ──────────────────────────────────────────────

func TestHandleGetRepo_Unauthorized(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := doGet(t, "any-id", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Passes today: an unknown id is 404 (mirrors test_repos.py::test_get_not_found).
func TestHandleGetRepo_NotFound(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := doGet(t, "00000000-0000-0000-0000-000000000000", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Passes today: the owner can fetch their own repo.
func TestHandleGetRepo_OwnRepo(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "c1000000-0000-0000-0000-000000000000", "user-1", "alice", "widgets", "fetch me")
	rec := doGet(t, re.ID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["description"] != "fetch me" {
		t.Errorf("description = %v, want %q", got["description"], "fetch me")
	}
}

// Passes today: per-record ownership is enforced in the DB — reading another
// user's repo passes gatekeeper but the owner-filtered lookup returns not-found,
// surfacing as 404 (ARCHITECTURE §3, mirrors test_other_user_cannot_get_repo).
func TestHandleGetRepo_OtherUsersRepoIsNotFound(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	// gatekeeper now owns the per-record decision, so the stub must model it: user-1
	// lives in namespace "alice" and the repo below is in "bob".
	newGatekeeperStubForNamespace(t, "user-1", "alice")

	re := seedRepo(t, "c2000000-0000-0000-0000-000000000000", "user-2", "bob", "secret", "")
	rec := doGet(t, re.ID, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (an owner boundary must read as not-found)", rec.Code, http.StatusNotFound)
	}
}

// ── update (ARCHITECTURE §2a, §3) ───────────────────────────────────────────

func TestHandleUpdateRepo_Unauthorized(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := doPatch(t, "any-id", map[string]any{"name": "x"}, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Passes today: updating an unknown id is 404.
func TestHandleUpdateRepo_NotFound(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := doPatch(t, "00000000-0000-0000-0000-000000000000", map[string]any{"name": "x"}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Passes today: the owner can rename/redescribe their repo and gets the updated
// record back.
func TestHandleUpdateRepo_OwnRepo(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "d1000000-0000-0000-0000-000000000000", "user-1", "alice", "widgets", "old")
	rec := doPatch(t, re.ID, map[string]any{"name": "widgets", "description": "updated"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["description"] != "updated" {
		t.Errorf("description = %v, want %q", got["description"], "updated")
	}
}

// Passes today: a second user cannot update your repo — the owner-scoped Update
// affects zero rows and returns not-found (ARCHITECTURE §3).
func TestHandleUpdateRepo_OtherUsersRepoIsNotFound(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "d2000000-0000-0000-0000-000000000000", "user-2", "bob", "secret", "")
	rec := doPatch(t, re.ID, map[string]any{"name": "hijacked"}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// RED until Step 1: PATCH is a partial update — a description-only edit (no name
// in the body) must succeed (ARCHITECTURE §7, mirrors
// test_repos.py::test_update_own_repo). The current handler unconditionally
// requires a non-empty name and returns 400 when it's absent, clobbering the
// partial-update contract. Fix: api_repo.go handleUpdateRepo — load the existing
// row and only overwrite the fields present in the request, validating name only
// when it is provided.
func TestHandleUpdateRepo_PartialDescriptionOnly(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "d3000000-0000-0000-0000-000000000000", "user-1", "alice", "keepname", "old")
	rec := doPatch(t, re.ID, map[string]any{"description": "updated"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d — PATCH must allow a description-only update (body %q)",
			rec.Code, http.StatusOK, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err == nil {
		if got["description"] != "updated" {
			t.Errorf("description = %v, want %q", got["description"], "updated")
		}
		if got["name"] != "keepname" {
			t.Errorf("name = %v, want it preserved as %q on a partial update", got["name"], "keepname")
		}
	}
}

// ── delete (ARCHITECTURE §2a, §3, §6) ───────────────────────────────────────

func TestHandleDeleteRepo_Unauthorized(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	if rec := doDelete(t, "any-id", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Passes today: deleting an unknown id is 404.
func TestHandleDeleteRepo_NotFound(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	rec := doDelete(t, "00000000-0000-0000-0000-000000000000", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Passes today: delete of an owned repo is 204, and a subsequent get is 404
// (mirrors test_repos.py::test_delete_own_repo).
func TestHandleDeleteRepo_OwnRepo(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "e1000000-0000-0000-0000-000000000000", "user-1", "alice", "widgets", "")
	if rec := doDelete(t, re.ID, true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec := doGet(t, re.ID, true); rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Passes today: a second user cannot delete your repo, and the row survives
// (ARCHITECTURE §3, mirrors test_repos.py::test_delete_other_users_repo).
func TestHandleDeleteRepo_OtherUsersRepoIsNotFound(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "e2000000-0000-0000-0000-000000000000", "user-2", "bob", "secret", "")
	if rec := doDelete(t, re.ID, true); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if _, err := getRepo(context.Background(), "user-2", re.ID); err != nil {
		t.Fatalf("victim's repo must survive a foreign delete attempt, got: %v", err)
	}
}

// RED until Step 1: delete is the reverse of create — "remove row, then rm -rf
// the dir" (ARCHITECTURE §2a, §6). The on-disk bare repo lives at the id-derived
// path (§4); after a successful delete it must be gone. The current Remove only
// deletes the metadata row, orphaning the directory. Fix: api_repo.go
// handleDeleteRepo / db.go Remove — also remove ${GIT_STORAGE_ROOT}/{id[:2]}/{id}.git.
func TestHandleDeleteRepo_RemovesOnDiskRepo(t *testing.T) {
	_, storageRoot := setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	re := seedRepo(t, "e3000000-0000-0000-0000-000000000000", "user-1", "alice", "widgets", "")

	// Simulate the on-disk repo the git plane owns at the id-derived path.
	path := repoDiskPathForTest(storageRoot, re.ID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("prepare on-disk repo: %v", err)
	}

	if rec := doDelete(t, re.ID, true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("on-disk repo still present at %s after delete — delete must rm -rf the id-derived dir (ARCHITECTURE §2a/§6)", path)
	}
}
