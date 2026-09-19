package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The merge gate. These are security tests, not feature tests: before it existed,
// branch protection was enforced ONLY by the pre-receive hook, so a rule on a branch
// was silently bypassed by merging through the API instead of pushing. Every case below
// should fail loudly if that regresses.

// commitOnBranch materialises the repo's bare directory with one commit on branch, and
// returns its SHA. The check rules resolve the source head through git, so those tests
// need a branch that actually exists rather than a database row alone.
func commitOnBranch(t *testing.T, repoID, branch string) string {
	t.Helper()
	bare, err := repoDiskPath(repoID)
	if err != nil {
		t.Fatalf("repo disk path: %v", err)
	}
	if err := os.MkdirAll(bare, 0o750); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(bare, "init", "-q", "--bare", "--initial-branch=main")

	// Commit in a scratch worktree and push, rather than plumbing objects by hand —
	// this is the same shape a real push takes, so the branch ends up exactly as the
	// service would find it.
	work := t.TempDir()
	run(work, "init", "-q", "--initial-branch="+branch)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "initial")
	run(work, "push", "-q", bare, branch+":"+branch)
	return run(work, "rev-parse", "HEAD")
}

// seedPull creates an open PR row directly. The gate reads the database, so a test does
// not need a real branch to exercise the approval rules — only the check rules resolve a
// head, and those pass one in explicitly.
func seedPull(t *testing.T, repoID, source, target, author string) PullRequest {
	t.Helper()
	pr := PullRequest{
		ID: uuid.New().String(), RepoID: repoID, Number: nextPRNumber(context.Background(), repoID),
		Title: "t", SourceRef: source, TargetRef: target, State: prOpen, Author: author,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().Create(&pr).Error; err != nil {
		t.Fatalf("seed pull: %v", err)
	}
	return pr
}

func seedProtection(t *testing.T, repoID, pattern string, approvals int, checks string, dismissStale bool) {
	t.Helper()
	p := BranchProtection{
		RepoID: repoID, Pattern: pattern,
		NoForce: true, NoDelete: true,
		RequireApprovals: approvals, RequireChecks: checks, DismissStale: dismissStale,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().Create(&p).Error; err != nil {
		t.Fatalf("seed protection: %v", err)
	}
}

func seedReview(t *testing.T, repoID, pullID, reviewer, state, sha string) {
	t.Helper()
	rv := PullReview{
		ID: uuid.New().String(), RepoID: repoID, PullID: pullID,
		Reviewer: reviewer, State: state, CommitSHA: sha,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().Create(&rv).Error; err != nil {
		t.Fatalf("seed review: %v", err)
	}
	// Ordering within latestReviews is by created_at, and sqlite's resolution is coarse
	// enough that two rows written in the same test can tie. Nudge each one forward so
	// "most recent verdict wins" is actually exercised rather than accidentally passing.
	time.Sleep(2 * time.Millisecond)
}

func seedStatus(t *testing.T, repoID, sha, context_, state string) {
	t.Helper()
	st := CommitStatus{
		RepoID: repoID, SHA: sha, Context: context_, State: state,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().Create(&st).Error; err != nil {
		t.Fatalf("seed status: %v", err)
	}
}

// An unprotected branch is unchanged: the gate must not start refusing merges that
// always worked.
func TestMergeGate_NoProtectionAllows(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "main", "author")

	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("mergeGate blocked an unprotected branch: %v", err)
	}
}

// A rule with only wire settings (force/delete) is not a merge rule and must not block.
func TestMergeGate_WireOnlyRuleAllows(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 0, "", false)

	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("a force/delete-only rule blocked a merge: %v", err)
	}
}

// The core regression: a branch requiring approvals must not be mergeable without them.
func TestMergeGate_RequiresApprovals(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 2, "", false)

	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Fatal("merged into a branch requiring 2 approvals with none — branch protection is bypassed")
	}
	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "")
	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Error("one approval satisfied a two-approval rule")
	}
	seedReview(t, re.ID, pr.ID, "rev2", reviewApproved, "")
	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("two approvals did not satisfy a two-approval rule: %v", err)
	}
}

// Two approvals from the SAME person are one approval. Otherwise the rule counts
// clicks rather than people.
func TestMergeGate_ApprovalsAreDistinctReviewers(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 2, "", false)

	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "")
	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "")

	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Error("the same reviewer approving twice satisfied a two-approval rule")
	}
}

// An outstanding "changes requested" blocks regardless of how many approvals exist.
func TestMergeGate_ChangesRequestedBlocks(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 1, "", false)

	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "")
	seedReview(t, re.ID, pr.ID, "rev2", reviewChangesRequested, "")

	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Error("an approval out-voted an unresolved 'changes requested'")
	}
}

// ...but only the reviewer's LATEST verdict counts, so withdrawing an objection works.
func TestMergeGate_LatestVerdictWins(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 1, "", false)

	seedReview(t, re.ID, pr.ID, "rev1", reviewChangesRequested, "")
	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "")

	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("a reviewer who withdrew their objection still blocked the merge: %v", err)
	}
}

// A required check that has never reported is PENDING, not passing. A commit that
// simply outran CI must not merge.
func TestMergeGate_MissingCheckBlocks(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	// A real branch, so the gate can resolve a head to look statuses up against.
	sha := commitOnBranch(t, re.ID, "feature")
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 0, "build", false)

	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Fatal("a required check that never reported was treated as passing")
	}
	seedStatus(t, re.ID, sha, "build", statusFailure)
	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Error("a FAILING required check allowed the merge")
	}
	// Supersede with a pass, the way a re-run does.
	if err := connect().Model(&CommitStatus{}).
		Where("repo_id = ? AND sha = ? AND context = ?", re.ID, sha, "build").
		Update("state", statusSuccess).Error; err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("a green required check still blocked the merge: %v", err)
	}
}

// A status on a DIFFERENT commit must not satisfy the rule — otherwise a green build on
// an old commit lets anything merge.
func TestMergeGate_StatusIsPerCommit(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	commitOnBranch(t, re.ID, "feature")
	pr := seedPull(t, re.ID, "feature", "main", "author")
	seedProtection(t, re.ID, "main", 0, "build", false)

	seedStatus(t, re.ID, "0000000000000000000000000000000000000001", "build", statusSuccess)

	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Error("a green status on an unrelated commit satisfied a required check")
	}
}

// Stale approvals: an approval given against an older head is discounted only when the
// rule asks for it.
func TestMergeGate_DismissStaleApprovals(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	head := commitOnBranch(t, re.ID, "feature")
	pr := seedPull(t, re.ID, "feature", "main", "author")

	// Approval against a head that has since moved.
	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	seedProtection(t, re.ID, "main", 1, "", true)
	if err := mergeGate(context.Background(), re, pr); err == nil {
		t.Error("a stale approval satisfied a rule with dismiss_stale set")
	}

	// Same data, rule without dismissal: the approval stands.
	if err := connect().Model(&BranchProtection{}).
		Where("repo_id = ? AND pattern = ?", re.ID, "main").
		Update("dismiss_stale", false).Error; err != nil {
		t.Fatalf("update rule: %v", err)
	}
	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("dismiss_stale=false still discounted the approval: %v", err)
	}

	// And a fresh approval satisfies the strict rule.
	if err := connect().Model(&BranchProtection{}).
		Where("repo_id = ? AND pattern = ?", re.ID, "main").
		Update("dismiss_stale", true).Error; err != nil {
		t.Fatalf("update rule: %v", err)
	}
	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, head)
	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("an approval against the current head was treated as stale: %v", err)
	}
}

// A glob rule must cover the branch, and a broad rule must not weaken a specific one.
func TestMergeGate_StrictestMatchingRuleWins(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "release/1.0", "author")

	seedProtection(t, re.ID, "release/*", 2, "", false)
	seedProtection(t, re.ID, "*", 0, "", false) // broader, laxer — must not win

	err := mergeGate(context.Background(), re, pr)
	if err == nil {
		t.Fatal("a laxer '*' rule cancelled a stricter 'release/*' rule")
	}
	seedReview(t, re.ID, pr.ID, "rev1", reviewApproved, "")
	seedReview(t, re.ID, pr.ID, "rev2", reviewApproved, "")
	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("two approvals did not satisfy the release rule: %v", err)
	}
}

// A rule on a different branch must not leak onto this one.
func TestMergeGate_RuleAppliesToTargetBranchOnly(t *testing.T) {
	setupTestDB(t)
	re := seedRepoVisible(t, uuid.New().String(), "u1", "ns", "r", visibilityPrivate)
	pr := seedPull(t, re.ID, "feature", "develop", "author")
	seedProtection(t, re.ID, "main", 5, "", false)

	if err := mergeGate(context.Background(), re, pr); err != nil {
		t.Errorf("a rule on 'main' blocked a merge into 'develop': %v", err)
	}
}

func TestCombinedState(t *testing.T) {
	cases := []struct {
		name string
		in   []CommitStatus
		want string
	}{
		// The important one: nothing reported is NOT success.
		{"no statuses is pending", nil, statusPending},
		{"all green", []CommitStatus{{State: statusSuccess}, {State: statusSuccess}}, statusSuccess},
		{"any failure wins", []CommitStatus{{State: statusSuccess}, {State: statusFailure}}, statusFailure},
		{"error counts as failure", []CommitStatus{{State: statusSuccess}, {State: statusError}}, statusFailure},
		{"pending holds it back", []CommitStatus{{State: statusSuccess}, {State: statusPending}}, statusPending},
		{"failure beats pending", []CommitStatus{{State: statusPending}, {State: statusFailure}}, statusFailure},
	}
	for _, c := range cases {
		if got := combinedState(c.in); got != c.want {
			t.Errorf("%s: combinedState = %q, want %q", c.name, got, c.want)
		}
	}
}
