package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	// SourceRepoID is the repo SourceRef lives in, for a PR opened from a fork. Empty
	// means the same repo — the ordinary case, and what every pre-fork row reads as, so
	// existing PRs keep working untouched. sourceRepo() resolves the two into one.
	SourceRepoID string    `gorm:"default:''" json:"source_repo_id,omitempty"`
	State        string    `json:"state"`  // open | merged | closed
	Author       string    `json:"author"` // gatekeeper user_id
	MergeCommit  string    `json:"merge_commit,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// sourceRepo is the repo the PR's source branch lives in — itself for a same-repo PR.
func (p PullRequest) sourceRepo() string {
	if p.SourceRepoID == "" {
		return p.RepoID
	}
	return p.SourceRepoID
}

// crossRepo reports whether this PR comes from a fork.
func (p PullRequest) crossRepo() bool {
	return p.SourceRepoID != "" && p.SourceRepoID != p.RepoID
}

// localSourceRef is the ref to diff and merge FROM inside the target repo. For a fork
// PR that is the scratch ref the source commits were fetched into; the branch name
// itself means nothing in the target's object database.
func (p PullRequest) localSourceRef() string {
	if p.crossRepo() {
		return crossRepoRef(p.ID)
	}
	return p.SourceRef
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
		// SourceRepoID opens the PR from a FORK: the source branch lives in that repo
		// rather than this one. Empty is the ordinary same-repo case.
		SourceRepoID string `json:"source_repo_id"`
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
	sourceRepoID := strings.TrimSpace(req.SourceRepoID)
	crossRepo := sourceRepoID != "" && sourceRepoID != re.ID
	if !crossRepo && req.SourceRef == req.TargetRef {
		// Only a conflict within ONE repo. Across repos the same branch name on each
		// side is the normal case ("my main into your main").
		http.Error(w, "source and target must differ", http.StatusBadRequest)
		return
	}
	if !branchExists(ctx, re.ID, req.TargetRef) {
		http.Error(w, "branch "+req.TargetRef+" does not exist", http.StatusBadRequest)
		return
	}

	pr := PullRequest{
		ID: uuid.New().String(), RepoID: re.ID, Number: nextPRNumber(ctx, re.ID),
		Title: req.Title, Body: req.Body, SourceRef: req.SourceRef, TargetRef: req.TargetRef,
		State: prOpen, Author: userID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	if crossRepo {
		// The caller must be able to READ the source repo. Checked through the same
		// gatekeeper path as any other access rather than trusting the id in the body —
		// otherwise "open a PR from repo X" would be a way to copy a repo you cannot see
		// into one you can, and then read its diff.
		srcRepo, err := getRepoByID(ctx, sourceRepoID)
		if err != nil {
			http.Error(w, "source repository not found", http.StatusNotFound)
			return
		}
		if _, _, ok := gatekeeperClient.CheckPermissions(ctx, &notFoundOnDeny{ResponseWriter: w}, r, "getRepo", resRepoOf(srcRepo)); !ok {
			// notFoundOnDeny already answered; a denial reads as 404 so the existence of
			// a private source repo is not confirmed.
			return
		}
		if !branchExists(ctx, srcRepo.ID, req.SourceRef) {
			http.Error(w, "branch "+req.SourceRef+" does not exist in the source repository", http.StatusBadRequest)
			return
		}
		pr.SourceRepoID = srcRepo.ID
		// Bring the commits across before the row exists, so a PR is never created
		// pointing at objects this repo cannot see.
		if err := fetchCrossRepo(ctx, re.ID, srcRepo.ID, req.SourceRef, pr.ID); err != nil {
			slog.ErrorContext(ctx, "create pull: fetch fork ref", "Repo_id", re.ID, "source", srcRepo.ID, "error", err)
			http.Error(w, "could not read the source branch", http.StatusBadGateway)
			return
		}
	} else if !branchExists(ctx, re.ID, req.SourceRef) {
		http.Error(w, "branch "+req.SourceRef+" does not exist", http.StatusBadRequest)
		return
	}

	if err := connect().WithContext(ctx).Create(&pr).Error; err != nil {
		slog.ErrorContext(ctx, "create pull", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "pull request opened", "Repo_id", re.ID, "number", pr.Number, "author", userID)
	notifyPullEvent(ctx, re, eventPullOpened, pr, userID)
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
		// A fork PR's source lives in another repo, which has kept moving since the PR
		// was opened. Refresh the scratch ref first so the diff and the mergeability
		// verdict describe the branch as it is NOW, matching the same-repo behaviour.
		if pr.crossRepo() {
			if err := fetchCrossRepo(ctx, re.ID, pr.sourceRepo(), pr.SourceRef, pr.ID); err != nil {
				slog.WarnContext(ctx, "pull: could not refresh fork ref", "Repo_id", re.ID, "number", pr.Number, "error", err)
			}
		}
		src := pr.localSourceRef()
		// Report a failure rather than omitting the key: a caller cannot distinguish
		// "no files changed" from "we could not work it out" if the field is missing.
		if changes, err := diffStat(ctx, re.ID, pr.TargetRef, src); err == nil {
			out["files"] = changes
		} else {
			out["files_error"] = err.Error()
		}
		if patch, err := diffPatch(ctx, re.ID, pr.TargetRef, src); err == nil {
			out["diff"] = patch // the full review diff (target...source)
		}
		out["commits"] = commitsBetween(ctx, re.ID, pr.TargetRef, src)
		res, err := tryMerge(ctx, re.ID, pr.TargetRef, src)
		if err != nil {
			res = mergeResult{Mergeable: false, Reason: err.Error()}
		}
		out["merge"] = res
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
	// Branch protection. Enforced HERE and not only in the pre-receive hook: the hook
	// sees pushes, and a merge through this handler never reaches it, so without this
	// the API merge path walked straight past every rule on the target branch.
	if err := mergeGate(ctx, re, pr); err != nil {
		slog.InfoContext(ctx, "merge blocked by branch protection",
			"Repo_id", re.ID, "number", pr.Number, "target", pr.TargetRef, "reason", err.Error())
		http.Error(w, "merge blocked: "+err.Error(), http.StatusConflict)
		return
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

	// Same refresh as the read path: merge what the fork branch points at now.
	if pr.crossRepo() {
		if err := fetchCrossRepo(ctx, re.ID, pr.sourceRepo(), pr.SourceRef, pr.ID); err != nil {
			slog.ErrorContext(ctx, "pull: could not refresh fork ref before merge", "Repo_id", re.ID, "error", err)
			http.Error(w, "could not read the source branch", http.StatusBadGateway)
			return
		}
	}
	sha, err := mergeBranches(ctx, re.ID, pr.TargetRef, pr.localSourceRef(), msg, userID)
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
	notifyPullEvent(ctx, re, eventPullMerged, pr, userID)
	slog.InfoContext(ctx, "pull request merged", "Repo_id", re.ID, "number", pr.Number, "commit", sha)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, pr)
}

func handleClosePull(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleClosePull")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "updatePull")
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
	notifyPullEvent(ctx, re, eventPullClosed, pr, userID)
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
