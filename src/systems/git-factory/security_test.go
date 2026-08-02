package main

// Security regression tests for the delete path.
//
// handleDeleteRepo takes the repo id straight from the URL and hands it to
// Repo.Remove, which calls DeleteGitRepo(ctx, re) BEFORE the owner-scoped DB
// delete runs. DeleteGitRepo builds a filesystem path out of that id and shells
// out to `rm -rf`. Both tests below pin the invariants that ordering breaks.

import (
	"net/http"
	"os"
	"testing"
)

// A delete attempt against a repo owned by someone else must not touch that
// repo's bytes. The DB row is already protected (the owner-scoped Delete matches
// zero rows and the handler returns 404) — but DeleteGitRepo has already run by
// then, so the victim's on-disk repository is destroyed while their metadata row
// survives.
func TestHandleDeleteRepo_OtherUsersOnDiskRepoSurvives(t *testing.T) {
	_, storageRoot := setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	victim := seedRepo(t, "e4000000-0000-0000-0000-000000000000", "user-2", "bob", "secret", "")
	path := repoDiskPathForTest(storageRoot, victim.ID)
	if !isBareRepo(path) {
		t.Fatalf("precondition: expected the victim's bare repo at %s", path)
	}

	if rec := doDelete(t, victim.ID, true); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("another user's on-disk repo at %s was deleted by an unauthorized caller — "+
			"DeleteGitRepo runs before the owner-scoped check in Repo.Remove", path)
	}
}

// The id is attacker-controlled and reaches DeleteGitRepo, which slices it as
// re.ID[:2] to build the shard directory. An id shorter than two characters
// slices out of range and panics the handler.
func TestHandleDeleteRepo_ShortIDDoesNotPanic(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStub(t, "user-1")

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("handleDeleteRepo panicked on a 2-character id: %v — "+
				"the id from the URL is sliced before it is validated", p)
		}
	}()

	if rec := doDelete(t, "ab", true); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d for an unknown short id", rec.Code, http.StatusNotFound)
	}
}
