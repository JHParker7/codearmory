package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// Forks and the cross-repo pull requests they exist to enable.

func TestCopyRepoObjects_ForkGetsTheHistory(t *testing.T) {
	setupTestDB(t)
	src := seedRepoVisible(t, uuid.New().String(), "u1", "ns1", "src", visibilityPublic)
	sha := commitOnBranch(t, src.ID, "main")

	fork := seedRepoVisible(t, uuid.New().String(), "u2", "ns2", "fork", visibilityPrivate)
	if err := copyRepoObjects(context.Background(), src.ID, fork.ID); err != nil {
		t.Fatalf("copyRepoObjects: %v", err)
	}

	// The fork must resolve the SAME commit — that is what makes it a fork rather than
	// a new empty repo wearing the label.
	if got := refSHA(context.Background(), fork.ID, "main"); got != sha {
		t.Errorf("fork head = %q, want the source's %q", got, sha)
	}
}

// The source's pre-receive hook encodes the SOURCE owner's branch protection. Inheriting
// it would apply one person's policy to another person's repo.
func TestCopyRepoObjects_DoesNotInheritHooks(t *testing.T) {
	setupTestDB(t)
	src := seedRepoVisible(t, uuid.New().String(), "u1", "ns1", "src", visibilityPublic)
	commitOnBranch(t, src.ID, "main")

	srcDir, err := localDirFor(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("src dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "hooks"), 0o750); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "hooks", "pre-receive"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o750); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	fork := seedRepoVisible(t, uuid.New().String(), "u2", "ns2", "fork", visibilityPrivate)
	if err := copyRepoObjects(context.Background(), src.ID, fork.ID); err != nil {
		t.Fatalf("copyRepoObjects: %v", err)
	}
	forkDir, err := localDirFor(context.Background(), fork.ID)
	if err != nil {
		t.Fatalf("fork dir: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(forkDir, "hooks", "pre-receive")); err == nil {
		t.Errorf("fork inherited the source's pre-receive hook: %s", b)
	}
}

// A fork PR is diffed and merged through the target repo's object database, so the
// source commits have to actually arrive in it — under a ref that is NOT a branch.
func TestFetchCrossRepo_BringsCommitsUnderAScratchRef(t *testing.T) {
	setupTestDB(t)
	target := seedRepoVisible(t, uuid.New().String(), "u1", "ns1", "target", visibilityPublic)
	commitOnBranch(t, target.ID, "main")

	source := seedRepoVisible(t, uuid.New().String(), "u2", "ns2", "fork", visibilityPrivate)
	sha := commitOnBranch(t, source.ID, "feature")

	pullID := uuid.New().String()
	if err := fetchCrossRepo(context.Background(), target.ID, source.ID, "feature", pullID); err != nil {
		t.Fatalf("fetchCrossRepo: %v", err)
	}

	// Resolvable inside the target...
	if got := refSHA(context.Background(), target.ID, crossRepoRef(pullID)); got != sha {
		t.Errorf("scratch ref = %q, want %q", got, sha)
	}
	// ...but NOT as a branch. A fork's branch must never appear in the target's branch
	// list or be advertised to a clone.
	branches, err := listRefs(context.Background(), target.ID, "refs/heads/")
	if err != nil {
		t.Fatalf("listRefs: %v", err)
	}
	for _, b := range branches {
		if b.Name == "feature" {
			t.Error("the fork's branch appeared as a branch in the target repo")
		}
	}
}

// A same-repo PR must keep resolving to its own branch — the fork plumbing must not
// change the ordinary path.
func TestPullRequest_LocalSourceRef(t *testing.T) {
	same := PullRequest{ID: "p1", RepoID: "r1", SourceRef: "feature"}
	if same.crossRepo() {
		t.Error("a PR with no source_repo_id reported as cross-repo")
	}
	if got := same.localSourceRef(); got != "feature" {
		t.Errorf("same-repo localSourceRef = %q, want the branch name", got)
	}
	if got := same.sourceRepo(); got != "r1" {
		t.Errorf("same-repo sourceRepo = %q, want the PR's own repo", got)
	}

	// An explicit source repo equal to the target is still same-repo, not a fork.
	selfRef := PullRequest{ID: "p2", RepoID: "r1", SourceRepoID: "r1", SourceRef: "feature"}
	if selfRef.crossRepo() {
		t.Error("source_repo_id equal to repo_id reported as cross-repo")
	}

	fork := PullRequest{ID: "p3", RepoID: "r1", SourceRepoID: "r2", SourceRef: "feature"}
	if !fork.crossRepo() {
		t.Fatal("a PR from another repo did not report as cross-repo")
	}
	if got := fork.localSourceRef(); got != crossRepoRef("p3") {
		t.Errorf("fork localSourceRef = %q, want the scratch ref", got)
	}
	if got := fork.sourceRepo(); got != "r2" {
		t.Errorf("fork sourceRepo = %q, want the source repo id", got)
	}
}

// The scratch ref must be namespaced per PR, or two fork PRs into the same repo would
// overwrite each other's source commits.
func TestCrossRepoRef_IsPerPullAndNotABranch(t *testing.T) {
	a, b := crossRepoRef("pull-a"), crossRepoRef("pull-b")
	if a == b {
		t.Error("two pull requests share a scratch ref")
	}
	for _, ref := range []string{a, b} {
		if len(ref) < len("refs/") || ref[:5] != "refs/" {
			t.Errorf("scratch ref %q is not a full ref", ref)
		}
		if got := ref[:len("refs/heads/")]; got == "refs/heads/" {
			t.Errorf("scratch ref %q lives under refs/heads and would appear as a branch", ref)
		}
		// It becomes a git argv element, so it must survive the same allowlist a branch
		// name does.
		if !branchNameRe.MatchString(ref) {
			t.Errorf("scratch ref %q would be rejected by the ref allowlist", ref)
		}
	}
}
