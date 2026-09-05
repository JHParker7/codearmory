package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// Pull requests (ARCHITECTURE §2a, extended).
//
// A PR is metadata plus two refs; everything interesting — the diff, whether it
// merges, and the merge itself — is computed from git at request time rather than
// stored. Storing a diff or a mergeability verdict would go stale the moment either
// branch moves, and a stale "mergeable" badge is worse than none.
//
// The merge writes a real merge commit through the gitplane boundary (mergeBranches),
// so it works unchanged when the git plane moves to a remote node (§5 Step 3).

type PullRequest struct {
	ID        string `gorm:"primaryKey" json:"id"`
	RepoID    string `gorm:"index" json:"repo_id"`
	Number    int    `json:"number"` // per-repo, human-facing
	Title     string `json:"title"`
	Body      string `json:"body"`
	SourceRef string `json:"source_ref"` // the branch being merged
	TargetRef string `json:"target_ref"` // the branch merged into
	State     string `json:"state"`      // open | merged | closed
	Author    string `json:"author"`     // gatekeeper user_id (the DISPLAYED opener)
	// OpenedByUserID is the identity that actually called createPull. It differs from
	// Author only when an authorised automation caller attributed the PR to a bot
	// display identity via the createPull `author` override: Author is the bot shown
	// in the UI, OpenedByUserID is the real caller, kept for accountability. Empty
	// when no override was used (then the opener IS Author) and on pre-existing rows.
	OpenedByUserID string    `json:"opened_by,omitempty"`
	MergeCommit    string    `json:"merge_commit,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const (
	prOpen   = "open"
	prMerged = "merged"
	prClosed = "closed"
)

// nextPRNumber allocates the next per-repo number. Repo-scoped rather than global so
// numbers read like every other git platform's ("#3 in this repo", not "#4171").
func nextPRNumber(ctx context.Context, repoID string) int {
	var max struct{ N int }
	connectRead().WithContext(ctx).Model(&PullRequest{}).
		Select("COALESCE(MAX(number),0) as n").Where("repo_id = ?", repoID).Scan(&max)
	return max.N + 1
}

// automationAuthors returns the set of user ids that a createPull caller may name in
// the `author` override, read from GIT_FACTORY_AUTOMATION_USERS (comma-separated). A
// workflow opening a PR with its own per-run repo token can thereby show a stable bot
// as the opener while the real caller stays authorised and recorded (OpenedByUserID).
// Restricting to this allowlist keeps the override pointing only at sanctioned bot
// accounts. Empty/unset turns the feature off.
func automationAuthors() map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("GIT_FACTORY_AUTOMATION_USERS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

func handleCreatePull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreatePull")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "createPull")
	if !ok {
		return
	}
	var req struct {
		Title     string `json:"title"`
		Body      string `json:"body"`
		SourceRef string `json:"source_ref"`
		TargetRef string `json:"target_ref"`
		// Author, when set, attributes the PR to an allowlisted automation identity
		// (see automationAuthors). It lets a workflow that opens a PR with its own
		// per-run repo credential still show a stable bot as the opener, without that
		// bot holding any credential or grant. Ignored for a self-attribution.
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || req.SourceRef == "" {
		http.Error(w, "title and source_ref are required", http.StatusBadRequest)
		return
	}
	if req.TargetRef == "" {
		req.TargetRef = headBranch(ctx, re.ID)
	}
	// Both refs become git arguments, so they are held to the branch allowlist.
	if !branchNameRe.MatchString(req.SourceRef) || !branchNameRe.MatchString(req.TargetRef) {
		http.Error(w, "invalid branch name", http.StatusBadRequest)
		return
	}
	if req.SourceRef == req.TargetRef {
		http.Error(w, "source and target must differ", http.StatusBadRequest)
		return
	}
	// Both must exist — a PR from a branch that was never pushed is a typo, and
	// catching it here beats a confusing failure at merge time.
	for _, ref := range []string{req.SourceRef, req.TargetRef} {
		if !branchExists(ctx, re.ID, ref) {
			http.Error(w, "branch "+ref+" does not exist", http.StatusBadRequest)
			return
		}
	}

	// Optional author override: the caller has already passed the createPull check
	// above, so this only decides ATTRIBUTION, never access. It is guarded to the
	// automation allowlist so it can name a sanctioned bot but never impersonate a
	// human, and the true caller is retained in OpenedByUserID for accountability.
	author, openedBy := userID, ""
	if req.Author != "" && req.Author != userID {
		if !automationAuthors()[req.Author] {
			http.Error(w, "author override must name an allowlisted automation account", http.StatusForbidden)
			return
		}
		author, openedBy = req.Author, userID
	}
	pr := PullRequest{
		ID: uuid.New().String(), RepoID: re.ID, Number: nextPRNumber(ctx, re.ID),
		Title: req.Title, Body: req.Body, SourceRef: req.SourceRef, TargetRef: req.TargetRef,
		State: prOpen, Author: author, OpenedByUserID: openedBy,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).Create(&pr).Error; err != nil {
		slog.ErrorContext(ctx, "create pull", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "pull request opened", "Repo_id", re.ID, "number", pr.Number, "author", pr.Author, "opened_by", userID)
	// Announce the open so a trigger can run CI or a review on it. Detached (a slow
	// events service must not hold the request), tenant = repo owner (notifyPullRequest).
	head, _ := branchTip(ctx, re.ID, pr.SourceRef)
	notifyPullRequest(ctx, re, "opened", pr, head)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, pr)
}

func handleListPulls(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListPulls")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listPull")
	if !ok {
		return
	}
	q := connectRead().WithContext(ctx).Where("repo_id = ?", re.ID)
	if state := r.URL.Query().Get("state"); state != "" {
		q = q.Where("state = ?", state)
	}
	var pulls []PullRequest
	if err := q.Order("number DESC").Find(&pulls).Error; err != nil {
		slog.ErrorContext(ctx, "list pulls", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if pulls == nil {
		pulls = []PullRequest{}
	}
	writeJSON(w, http.StatusOK, pulls)
}

// handleGetPull returns the PR together with a LIVE view of it: the diffstat, how far
// ahead it is, and whether it currently merges. Computed per request, because all
// three change whenever either branch moves.
func handleGetPull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGetPull")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getPull")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}

	out := map[string]any{"pull_request": pr}
	if pr.State == prOpen {
		// Report a failure rather than omitting the key: a caller cannot distinguish
		// "no files changed" from "we could not work it out" if the field is missing.
		if changes, err := diffStat(ctx, re.ID, pr.TargetRef, pr.SourceRef); err == nil {
			out["files"] = changes
		} else {
			out["files_error"] = err.Error()
		}
		if patch, err := diffPatch(ctx, re.ID, pr.TargetRef, pr.SourceRef); err == nil {
			out["diff"] = patch // the full review diff (target...source)
		}
		out["commits"] = commitsBetween(ctx, re.ID, pr.TargetRef, pr.SourceRef)
		res, err := tryMerge(ctx, re.ID, pr.TargetRef, pr.SourceRef)
		if err != nil {
			res = mergeResult{Mergeable: false, Reason: err.Error()}
		}
		out["merge"] = res
		// The checks a workflow (or any CI) reported on this PR's head commit, so a
		// reviewer sees green/red without leaving the PR. Combined worst-first; "" means
		// nothing has reported yet.
		if sha, ok := branchTip(ctx, re.ID, pr.SourceRef); ok {
			statuses := loadStatuses(ctx, re.ID, sha)
			out["status"] = map[string]any{"sha": sha, "state": combinedState(statuses), "statuses": statuses}
		}
		// The review verdict and whether this PR's target gates merge on it, so a
		// caller knows both where the review stands and whether it is binding.
		reviews := loadReviews(ctx, re.ID, pr.ID)
		out["reviews"] = map[string]any{
			"decision": decideReviews(reviews, pr.Author),
			"required": reviewRequiredFor(ctx, re.ID, pr.TargetRef),
			"reviews":  reviews,
		}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, out)
}

// handleMergePull merges the PR and records the resulting commit.
func handleMergePull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleMergePull")
	defer span.End()

	// Merging writes to the repository, so it needs the write action rather than the
	// read one the rest of this file uses.
	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "mergePull")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	if pr.State != prOpen {
		http.Error(w, "pull request is "+pr.State, http.StatusConflict)
		return
	}
	// A protected target branch gates the merge on review: an approval from someone
	// other than the author, and nobody currently requesting changes. Off unless the
	// branch's protection turns it on, so unprotected repos merge exactly as before.
	if reviewRequiredFor(ctx, re.ID, pr.TargetRef) {
		d := decideReviews(loadReviews(ctx, re.ID, pr.ID), pr.Author)
		if d.ChangesRequested {
			http.Error(w, "changes have been requested; resolve the review before merging", http.StatusConflict)
			return
		}
		if d.Approvals < 1 {
			http.Error(w, "the target branch requires an approving review before merge", http.StatusConflict)
			return
		}
	}

	var req struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.SourceRef + "\n\n" + pr.Title
	}

	// Snapshot the refs before the merge moves TargetRef, so the push event below can
	// report what the ref pointed at beforehand. A consumer that has to guess reaches
	// for the merge commit's first parent, which is right for a plain merge and wrong
	// for a squash — where one commit carries every file from the branch.
	before := refSnapshot(ctx, re.ID)

	sha, err := mergeBranches(ctx, re.ID, pr.TargetRef, pr.SourceRef, msg, userID)
	if err != nil {
		// Conflicts and races are the caller's problem to resolve, not server errors.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	pr.State, pr.MergeCommit, pr.UpdatedAt = prMerged, sha, time.Now().UTC()
	if err := connect().WithContext(ctx).Model(&PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{"state": prMerged, "merge_commit": sha, "updated_at": pr.UpdatedAt}).Error; err != nil {
		// The merge landed; only the bookkeeping failed. Say so rather than implying
		// the merge did not happen — a retry would fail with "already merged".
		slog.ErrorContext(ctx, "pull merged but not recorded", "Repo_id", re.ID, "number", pr.Number, "commit", sha, "error", err)
		http.Error(w, "merged as "+sha+" but the pull request could not be updated", http.StatusInternalServerError)
		return
	}
	_ = touchRepo(ctx, re.ID)
	notifyPush(ctx, re, userID, []string{pr.TargetRef}, before)
	notifyPullRequest(ctx, re, "merged", pr, sha)
	slog.InfoContext(ctx, "pull request merged", "Repo_id", re.ID, "number", pr.Number, "commit", sha)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, pr)
}

func handleClosePull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleClosePull")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "updatePull")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	if pr.State == prMerged {
		http.Error(w, "a merged pull request cannot be closed", http.StatusConflict)
		return
	}
	if err := connect().WithContext(ctx).Model(&PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{"state": prClosed, "updated_at": time.Now().UTC()}).Error; err != nil {
		slog.ErrorContext(ctx, "close pull", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	pr.State = prClosed
	notifyPullRequest(ctx, re, "closed", pr, "")
	writeJSON(w, http.StatusOK, pr)
}

func loadPull(ctx context.Context, repoID, number string) (PullRequest, error) {
	var pr PullRequest
	err := connectRead().WithContext(ctx).
		Where("repo_id = ? AND number = ?", repoID, number).First(&pr).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PullRequest{}, errPullNotFound
	}
	return pr, err
}

var errPullNotFound = errors.New("pull request not found")

func pullErr(ctx context.Context, w http.ResponseWriter, err error, repoID string) {
	if errors.Is(err, errPullNotFound) {
		http.Error(w, "pull request not found", http.StatusNotFound)
		return
	}
	slog.ErrorContext(ctx, "load pull", "Repo_id", repoID, "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
