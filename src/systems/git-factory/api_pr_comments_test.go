package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A PR comment is editable and deletable ONLY by its author — even by a caller who
// is otherwise fully authorized to write the repo. authorizeRepo gates who may reach
// the route at all; the author check is the second gate a write collaborator must
// still fail on someone else's words. This is the one property beyond the standard
// per-record pattern (which ownerfirst_test.go already covers for these routes).
func TestPRComment_OnlyAuthorEditsOrDeletes(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	// setupTestDB migrates only the repo/shard tables; this feature's tables are ours.
	if err := connect().AutoMigrate(&PullRequest{}, &PRComment{}); err != nil {
		t.Fatalf("migrate pull tables: %v", err)
	}

	re := seedRepo(t, uuid.New().String(), "user-alice", "alice", "repo", "")

	pr := PullRequest{
		ID: uuid.New().String(), RepoID: re.ID, Number: 1, Title: "t",
		SourceRef: "feature", TargetRef: "main", State: prOpen, Author: "user-alice",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().Create(&pr).Error; err != nil {
		t.Fatalf("seed pull: %v", err)
	}
	c := PRComment{
		ID: uuid.New().String(), RepoID: re.ID, PullID: pr.ID, Author: "user-alice",
		Body: "alice's note", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().Create(&c).Error; err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	call := func(method, body string, h func(http.ResponseWriter, *http.Request)) int {
		req := httptest.NewRequest(method, "/repos/"+re.ID+"/pulls/1/comments/"+c.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.SetPathValue("id", re.ID)
		req.SetPathValue("number", "1")
		req.SetPathValue("commentID", c.ID)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	// bob is a full WRITE collaborator — authorized on the repo and holding the
	// update/delete comment actions — but he is not the author.
	newGatekeeperStubGrants(t, gatekeeperGrants{
		userID: "user-bob", username: "bob",
		shares: map[string][]string{resRepoOf(re): shareActions["write"]},
	})
	if code := call(http.MethodPatch, `{"body":"hijacked"}`, handleUpdatePRComment); code != http.StatusForbidden {
		t.Errorf("write collaborator edited another user's comment: status %d, want 403", code)
	}
	if code := call(http.MethodDelete, "", handleDeletePRComment); code != http.StatusForbidden {
		t.Errorf("write collaborator deleted another user's comment: status %d, want 403", code)
	}

	// The author herself edits and deletes her own comment.
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-alice", username: "alice"})
	if code := call(http.MethodPatch, `{"body":"revised"}`, handleUpdatePRComment); code != http.StatusOK {
		t.Errorf("author could not edit her own comment: status %d, want 200", code)
	}
	if code := call(http.MethodDelete, "", handleDeletePRComment); code != http.StatusNoContent {
		t.Errorf("author could not delete her own comment: status %d, want 204", code)
	}
}
