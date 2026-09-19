package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecideReviews_LatestPerReviewerExcludesAuthor(t *testing.T) {
	const author = "user-alice"
	at := func(min int) time.Time { return time.Unix(int64(min)*60, 0).UTC() }
	// Oldest-first, as loadReviews returns them.
	reviews := []PRReview{
		{Reviewer: "user-bob", State: reviewChangesRequested, CreatedAt: at(1)},
		{Reviewer: "user-carol", State: reviewApproved, CreatedAt: at(2)},
		{Reviewer: "user-bob", State: reviewApproved, CreatedAt: at(3)}, // supersedes bob's earlier
		{Reviewer: author, State: reviewApproved, CreatedAt: at(4)},     // author excluded
	}
	d := decideReviews(reviews, author)
	if d.Approvals != 2 {
		t.Errorf("approvals = %d, want 2 (bob's latest + carol; author excluded)", d.Approvals)
	}
	if d.ChangesRequested {
		t.Errorf("changes_requested = true, want false (bob's latest is approve)")
	}
	if d.Reviews != 4 {
		t.Errorf("reviews = %d, want 4", d.Reviews)
	}

	// carol now blocks; only bob still approves.
	reviews = append(reviews, PRReview{Reviewer: "user-carol", State: reviewChangesRequested, CreatedAt: at(5)})
	d = decideReviews(reviews, author)
	if !d.ChangesRequested {
		t.Errorf("changes_requested = false, want true (carol blocks)")
	}
	if d.Approvals != 1 {
		t.Errorf("approvals = %d, want 1 (only bob)", d.Approvals)
	}
}

// A protected target branch blocks the merge until an approving review from someone
// other than the author lands, and a changes-requested keeps it blocked. The gate runs
// before any git work, so this exercises the real handleMergePull path without a repo
// on disk.
func TestMergeGate_RequiresNonAuthorApproval(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	if err := connect().AutoMigrate(&PullRequest{}, &PRReview{}, &BranchProtection{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	re := seedRepo(t, uuid.New().String(), "user-alice", "alice", "repo", "")
	now := time.Now().UTC()
	pr := PullRequest{
		ID: uuid.New().String(), RepoID: re.ID, Number: 1, Title: "t",
		SourceRef: "feature", TargetRef: "main", State: prOpen, Author: "user-alice",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := connect().Create(&pr).Error; err != nil {
		t.Fatalf("seed pull: %v", err)
	}
	if err := connect().Create(&BranchProtection{
		RepoID: re.ID, Pattern: "main", RequireReview: true, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed protection: %v", err)
	}
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-alice", username: "alice"})

	merge := func() int {
		req := httptest.NewRequest(http.MethodPost, "/repos/"+re.ID+"/pulls/1/merge", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-token")
		req.SetPathValue("id", re.ID)
		req.SetPathValue("number", "1")
		rec := httptest.NewRecorder()
		handleMergePull(rec, req)
		return rec.Code
	}
	review := func(reviewer, state string) {
		if err := connect().Create(&PRReview{
			ID: uuid.New().String(), RepoID: re.ID, PullID: pr.ID,
			Reviewer: reviewer, State: state, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatalf("seed review: %v", err)
		}
	}

	if code := merge(); code != http.StatusConflict {
		t.Fatalf("unreviewed merge on a protected branch: status %d, want 409", code)
	}
	review("user-alice", reviewApproved) // author self-approval must not count
	if code := merge(); code != http.StatusConflict {
		t.Fatalf("author self-approval satisfied the gate: status %d, want 409", code)
	}
	review("user-bob", reviewChangesRequested) // an outstanding block
	if code := merge(); code != http.StatusConflict {
		t.Fatalf("changes-requested did not block: status %d, want 409", code)
	}
}
