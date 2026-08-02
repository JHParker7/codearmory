package main

// Unit-level spec for the git storage boundary — git.go ALONE (ARCHITECTURE §1:
// all git-on-disk access flows through one narrow interface; §4: on-disk layout;
// §5-Step1: single-node, in-process git).
//
// These tests call CreateGitRepo directly and assert on the bytes it writes to
// disk. They deliberately do NOT touch the database, GORM, or Repo.Add — so a
// bug in db.go can never turn this file red while git.go is actually working, and
// vice-versa. The only shared scaffolding used from db_test.go is pure, DB-free
// (isBareRepo, repoDiskPathForTest).
//
// git.go owns the id → disk-path mapping: given a stable repo id it must
// `git init --bare` ${GIT_STORAGE_ROOT}/{id[:2]}/{id}.git. These are RED until
// Step 1: the current stub os.MkdirAll's a plain, NON-bare dir at the relative
// "temp/repos/<arg>" and never runs `git init`, ignoring GIT_STORAGE_ROOT.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// setupGitStorage isolates git.go's on-disk side effects with no database in
// sight: it points GIT_STORAGE_ROOT at a fresh temp dir and chdirs into that temp
// dir so the current stub's relative "temp/repos/..." writes stay in the sandbox
// instead of littering the source tree. Everything is restored on cleanup. Tests
// using it must not run in parallel — they share the process-global working
// directory and environment.
func setupGitStorage(t *testing.T) (storageRoot string) {
	t.Helper()

	dir := t.TempDir()
	storageRoot = filepath.Join(dir, "storage")
	t.Setenv("GIT_STORAGE_ROOT", storageRoot)

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	return storageRoot
}

// checkBareRepoAt asserts a fully-initialized *bare* git repo exists at path, and
// — the whole point — pinpoints WHICH invariant is unmet so a failure says what is
// actually missing instead of a blanket "not a bare repo". It walks from weakest
// to strongest condition and reports the first that breaks:
//
//	directory absent            → git.go created nothing here at all
//	present but no HEAD/objects → a plain dir exists, but `git init` never ran
//	initialized but not bare    → it's a working tree, not the bare repo we need
//
// Uses t.Errorf (not Fatalf) so callers checking several repos see every failure
// in one run rather than only the first. Returns true when the repo is good.
func checkBareRepoAt(t *testing.T, label, path string) bool {
	t.Helper()

	fi, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		t.Errorf("%s: MISSING — no directory at %s; git.go created nothing here", label, path)
		return false
	case err != nil:
		t.Errorf("%s: cannot stat %s: %v", label, path, err)
		return false
	case !fi.IsDir():
		t.Errorf("%s: %s exists but is a file, not a repo directory", label, path)
		return false
	}

	// HEAD, objects/ and refs/ are what `git init` writes; a bare os.MkdirAll
	// (the current stub) produces none of them.
	for _, internal := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(path, internal)); err != nil {
			t.Errorf("%s: directory exists at %s but %q is missing — a plain dir was "+
				"created and `git init` never ran there", label, path, internal)
			return false
		}
	}

	if !isBareRepo(path) {
		t.Errorf("%s: %s is an initialized repo but NOT bare "+
			"(git rev-parse --is-bare-repository != true) — a hosting backend must "+
			"`git init --bare`, not create a working tree", label, path)
		return false
	}
	return true
}

// Happy path — the reason the storage boundary exists at all: CreateGitRepo must
// leave a real, usable bare git repository at the id-derived path (ARCHITECTURE
// §2a/§4). checkBareRepoAt reports precisely how far short the current stub falls.
// RED until Step 1.
func TestCreateGitRepo_InitsBareRepoAtIdPath(t *testing.T) {
	storageRoot := setupGitStorage(t)
	re := Repo{ID: "f0000000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}

	if err := CreateGitRepo(context.Background(), re); err != nil {
		t.Fatalf("CreateGitRepo: %v", err)
	}

	checkBareRepoAt(t, "repo", repoDiskPathForTest(storageRoot, re.ID))
}

// The disk path derives from the STABLE id, so distinct ids yield distinct,
// independently valid bare repos — this is what lets db.go treat a rename as a
// metadata-only operation (ARCHITECTURE §4). git.go never sees namespace/name, so
// the "path is keyed by id, not name" property is enforced here purely by feeding
// two different ids. Each repo is checked separately, so a failure names which one
// broke and why. RED until Step 1.
func TestCreateGitRepo_DistinctIdsGetDistinctRepos(t *testing.T) {
	storageRoot := setupGitStorage(t)
	ctx := context.Background()

	// Same name + owner on purpose: only the id differs, proving the path keys on
	// the id alone and nothing else leaks into it.
	reA := Repo{ID: "f1000000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	reB := Repo{ID: "f2000000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}
	if err := CreateGitRepo(ctx, reA); err != nil {
		t.Fatalf("CreateGitRepo A: %v", err)
	}
	if err := CreateGitRepo(ctx, reB); err != nil {
		t.Fatalf("CreateGitRepo B: %v", err)
	}

	pathA := repoDiskPathForTest(storageRoot, reA.ID)
	pathB := repoDiskPathForTest(storageRoot, reB.ID)
	if pathA == pathB {
		t.Fatalf("distinct ids resolved to the same on-disk path %s — the path must "+
			"derive from the full id", pathA)
	}

	checkBareRepoAt(t, "repo A ("+reA.ID+")", pathA)
	checkBareRepoAt(t, "repo B ("+reB.ID+")", pathB)
}

// CreateGitRepo must honor GIT_STORAGE_ROOT and the id-derived layout, leaving
// nothing at the legacy relative path "temp/repos/<arg>" the stub writes today
// (ARCHITECTURE §4, §5-Step1). setupGitStorage has chdir'd us into a temp dir, so
// this relative path stays inside the sandbox. RED until Step 1.
func TestCreateGitRepo_LegacyTempPathNotUsed(t *testing.T) {
	setupGitStorage(t)
	re := Repo{ID: "f3000000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}

	if err := CreateGitRepo(context.Background(), re); err != nil {
		t.Fatalf("CreateGitRepo: %v", err)
	}

	// GIT_STORAGE_ROOT is an absolute temp dir, so honoring it means nothing lands
	// under the relative "temp/repos" tree the stub defaults to. checking the whole
	// subtree (not one id) catches any regression back to the relative prefix.
	legacy := filepath.Join("temp", "repos")
	if _, err := os.Stat(legacy); err == nil {
		t.Fatalf("git.go wrote to the relative path %q — it must use "+
			"${GIT_STORAGE_ROOT}/{id[:2]}/{id}.git instead (ARCHITECTURE §4)", legacy)
	}
}

// DeleteGitRepo is the inverse of CreateGitRepo: after creating a bare repo at the
// id-derived path, deleting it must leave nothing behind. Exercised directly here
// (the handler test covers it only through the DB layer).
func TestDeleteGitRepo_RemovesRepo(t *testing.T) {
	storageRoot := setupGitStorage(t)
	ctx := context.Background()
	re := Repo{ID: "f4000000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "widgets"}

	if err := CreateGitRepo(ctx, re); err != nil {
		t.Fatalf("CreateGitRepo: %v", err)
	}
	path := repoDiskPathForTest(storageRoot, re.ID)
	if !isBareRepo(path) {
		t.Fatalf("precondition failed: expected a bare repo at %s", path)
	}

	if err := DeleteGitRepo(ctx, re); err != nil {
		t.Fatalf("DeleteGitRepo: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("DeleteGitRepo left %s on disk — the id-derived dir must be gone", path)
	}
}

// Deleting a repo that was never created must not error — delete is idempotent so
// a partially-failed create (row exists, bytes never landed) can still be cleaned up.
func TestDeleteGitRepo_MissingIsNoError(t *testing.T) {
	setupGitStorage(t)
	re := Repo{ID: "f5000000-0000-0000-0000-000000000000", Owner: "user-1", Namespace: "alice", Name: "ghost"}
	if err := DeleteGitRepo(context.Background(), re); err != nil {
		t.Errorf("DeleteGitRepo of a nonexistent repo = %v, want nil (delete must be idempotent)", err)
	}
}
