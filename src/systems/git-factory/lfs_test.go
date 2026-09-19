package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The oid becomes a path segment in the object store, so this is the traversal guard.
func TestLFSObjectPath_RejectsUnsafeOIDs(t *testing.T) {
	repo := uuid.New().String()
	bad := []string{
		"",
		"../../../etc/passwd",
		"..",
		strings.Repeat("a", 63), // too short
		strings.Repeat("a", 65), // too long
		strings.Repeat("A", 64), // uppercase is not the hex LFS uses
		strings.Repeat("g", 64), // not hex
		"/" + strings.Repeat("a", 63),
	}
	for _, oid := range bad {
		if _, err := lfsObjectPath(repo, oid); err == nil {
			t.Errorf("lfsObjectPath accepted oid %q", oid)
		}
	}
	good := strings.Repeat("ab", 32)
	p, err := lfsObjectPath(repo, good)
	if err != nil {
		t.Fatalf("lfsObjectPath rejected a valid oid: %v", err)
	}
	if !strings.Contains(p, repo) {
		t.Errorf("object path %q is not scoped to the repo", p)
	}
	if !strings.HasSuffix(p, good) {
		t.Errorf("object path %q does not end in the oid", p)
	}
}

// A crafted repo id must not build a path either — the id reaches this from a URL on
// some paths, exactly like repoDiskPath.
func TestLFSObjectPath_RejectsNonCanonicalRepoID(t *testing.T) {
	oid := strings.Repeat("ab", 32)
	for _, id := range []string{"", "..", "../../etc", "not-a-uuid", strings.ToUpper(uuid.New().String())} {
		if _, err := lfsObjectPath(id, oid); err == nil {
			t.Errorf("lfsObjectPath accepted repo id %q", id)
		}
	}
}

// Objects are stored PER REPO. A global content-addressed store would let anyone
// holding an oid — which is just the sha256 of a file they already have — read an
// object uploaded to a private repo they cannot see.
func TestLFSObjectPath_IsScopedPerRepo(t *testing.T) {
	oid := strings.Repeat("cd", 32)
	a, err := lfsObjectPath(uuid.New().String(), oid)
	if err != nil {
		t.Fatalf("path a: %v", err)
	}
	b, err := lfsObjectPath(uuid.New().String(), oid)
	if err != nil {
		t.Fatalf("path b: %v", err)
	}
	if a == b {
		t.Error("the same oid resolved to one shared path across two repos — an object in a private repo would be readable from another")
	}
}

// LFS is opt-in: an operator has to allocate storage for it deliberately.
func TestLFSEnabled_DefaultsOff(t *testing.T) {
	t.Setenv("GIT_LFS_ENABLED", "")
	if lfsEnabled() {
		t.Error("LFS reported enabled with no configuration")
	}
	t.Setenv("GIT_LFS_ENABLED", "true")
	if !lfsEnabled() {
		t.Error("GIT_LFS_ENABLED=true did not enable LFS")
	}
	// Anything that is not "true" leaves it off rather than being guessed at.
	t.Setenv("GIT_LFS_ENABLED", "yes")
	if lfsEnabled() {
		t.Error("an unrecognised GIT_LFS_ENABLED value enabled LFS")
	}
}
