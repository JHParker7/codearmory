package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Commit statuses (checks).
//
// The platform could already run CI off a push — events carries repo.push, workflows
// runs the pipeline — but there was nowhere to report the ANSWER back to. A run that
// nobody can see the result of cannot gate anything, so "don't merge until CI is green"
// was unexpressible even though every piece to compute it existed.
//
// A status is (commit, context) → state. The context is the check's name ("build",
// "lint"), and posting the same context again supersedes it: a re-run reports the new
// result rather than accumulating a history that policy would then have to interpret.
// The trail is deliberately not kept — a commit's CURRENT verdict is what a merge gate
// asks about, and keeping every attempt invites reading a stale success.
//
// This is git_factory's own surface, distinct from the GitHub App in `events` (which
// creates check runs on github.com). Repos hosted HERE report here.

type CommitStatus struct {
	// A composite key rather than a surrogate id: (repo, sha, context) IS the identity,
	// and making the database say so is what makes re-posting an upsert instead of a
	// duplicate the merge gate would have to de-duplicate at read time.
	RepoID      string    `gorm:"primaryKey" json:"repo_id"`
	SHA         string    `gorm:"primaryKey" json:"sha"`
	Context     string    `gorm:"primaryKey" json:"context"`
	State       string    `json:"state"` // pending | success | failure | error
	Description string    `json:"description,omitempty"`
	TargetURL   string    `json:"target_url,omitempty"` // where to see the run
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const (
	statusPending = "pending"
	statusSuccess = "success"
	statusFailure = "failure"
	statusError   = "error"
)

func validStatusState(s string) bool {
	switch s {
	case statusPending, statusSuccess, statusFailure, statusError:
		return true
	}
	return false
}

// shaRe is the accepted commit-id shape. A status is keyed by SHA and never reaches
// git, so this is about keying integrity rather than argv safety: an unconstrained
// string would let "main" and a SHA both be written as if they were commits, and a gate
// asking about the resolved SHA would silently miss the status posted against the name.
var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// statusContextRe keeps a context to a readable identifier. It is compared verbatim
// against a protection's required-checks list, so whitespace or case drift would mean a
// required check silently never matches — which fails OPEN.
var statusContextRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// handleSetStatus records (or supersedes) one check result for a commit.
func handleSetStatus(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleSetStatus")
	defer span.End()

	// Writing a status is a write to the repo's record — it is what merge policy reads,
	// so it must not be grantable to anyone who can merely read the repo.
	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "setStatus")
	if !ok {
		return
	}
	sha := strings.ToLower(strings.TrimSpace(r.PathValue("sha")))
	if !shaRe.MatchString(sha) {
		http.Error(w, "sha must be a hex commit id", http.StatusBadRequest)
		return
	}
	var req struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Context = strings.TrimSpace(req.Context)
	req.State = strings.TrimSpace(req.State)
	if !statusContextRe.MatchString(req.Context) {
		http.Error(w, "context must be a short identifier (letters, digits, . _ / -)", http.StatusBadRequest)
		return
	}
	if !validStatusState(req.State) {
		http.Error(w, "state must be pending, success, failure or error", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	st := CommitStatus{
		RepoID: re.ID, SHA: sha, Context: req.Context, State: req.State,
		Description: req.Description, TargetURL: req.TargetURL, CreatedBy: userID,
		CreatedAt: now, UpdatedAt: now,
	}
	// Upsert on the composite key: re-running a check replaces its verdict.
	if err := connect().WithContext(ctx).
		Where("repo_id = ? AND sha = ? AND context = ?", re.ID, sha, req.Context).
		Assign(map[string]any{
			"state": req.State, "description": req.Description,
			"target_url": req.TargetURL, "created_by": userID, "updated_at": now,
		}).
		FirstOrCreate(&st).Error; err != nil {
		slog.ErrorContext(ctx, "set status", "Repo_id", re.ID, "sha", sha, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "commit status set", "Repo_id", re.ID, "sha", sha,
		"context", req.Context, "state", req.State)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, st)
}

// statusesFor returns every current status for a commit.
func statusesFor(ctx context.Context, repoID, sha string) ([]CommitStatus, error) {
	var out []CommitStatus
	err := connectRead().WithContext(ctx).
		Where("repo_id = ? AND sha = ?", repoID, sha).Order("context").Find(&out).Error
	if out == nil {
		out = []CommitStatus{}
	}
	return out, err
}

// combinedState reduces a commit's statuses to one verdict, worst-wins.
//
// Order is deliberate: any failure or error makes the whole thing failed, and a check
// still running makes it pending. Success requires every context to have succeeded, so
// "no statuses at all" is NOT success — it is pending, because a commit nothing has
// reported on has not passed anything. A gate that treated it as success would let a
// commit merge simply by outrunning CI.
func combinedState(sts []CommitStatus) string {
	if len(sts) == 0 {
		return statusPending
	}
	worst := statusSuccess
	for _, s := range sts {
		switch s.State {
		case statusFailure, statusError:
			return statusFailure
		case statusPending:
			worst = statusPending
		}
	}
	return worst
}

func handleListStatuses(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListStatuses")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listStatus")
	if !ok {
		return
	}
	sha := strings.ToLower(strings.TrimSpace(r.PathValue("sha")))
	if !shaRe.MatchString(sha) {
		http.Error(w, "sha must be a hex commit id", http.StatusBadRequest)
		return
	}
	sts, err := statusesFor(ctx, re.ID, sha)
	if err != nil {
		slog.ErrorContext(ctx, "list statuses", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"sha":      sha,
		"state":    combinedState(sts),
		"statuses": sts,
	})
}
