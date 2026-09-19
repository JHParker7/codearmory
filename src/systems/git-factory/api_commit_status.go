package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Commit statuses (ARCHITECTURE §2a, extended) — the CI↔PR bridge.
//
// A status is how a check (a workflow run, a scanner, a linter) reports its verdict
// onto a commit, so a pull request can show green/red without git-factory knowing
// anything about the runner. It is pure metadata keyed by (repo, sha, context): a
// Postgres row like a PR comment, no git-plane work beyond resolving a PR's head.
//
// One row per (repo, sha, context): a re-post of the same context REPLACES the
// previous verdict (pending → success), which is what a caller polling a run wants —
// the current state of each check, not a history it has to reduce itself. The
// combined state over a commit's contexts is computed on read.
//
// Statuses may be posted for a sha that is not yet the tip of any branch (a run
// reports "pending" as it starts), so existence in the repo is not required — only a
// well-formed hex sha, which is data here (stored, never a git argv) but kept clean.

type CommitStatus struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	RepoID      string    `gorm:"index" json:"repo_id"`
	SHA         string    `gorm:"index" json:"sha"`
	Context     string    `json:"context"` // the check's name, e.g. "workflow/plan-arm"
	State       string    `json:"state"`   // pending | success | failure | error
	Description string    `json:"description,omitempty"`
	TargetURL   string    `json:"target_url,omitempty"` // a link to the run/logs
	Creator     string    `json:"creator"`              // gatekeeper user_id
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

// commitSHARe keeps the stored sha clean — abbreviated (7) through full (64, for
// sha-256 repos). It is data, not a git argv, so this is hygiene, not a shell guard.
var commitSHARe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// contextMax and descriptionMax bound the two free-text fields the way GitHub does:
// a context is a short check name, a description a one-line summary.
const (
	contextMax     = 255
	descriptionMax = 1024
)

// combinedState reduces a commit's per-context states to one, worst-first: a single
// error or failure fails the commit, an unfinished check leaves it pending, and only
// an all-success set (with at least one check) is success. No checks at all is "" —
// the caller distinguishes "nothing ran" from "everything passed".
func combinedState(statuses []CommitStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	seen := map[string]bool{}
	for _, s := range statuses {
		seen[s.State] = true
	}
	switch {
	case seen[statusError]:
		return statusError
	case seen[statusFailure]:
		return statusFailure
	case seen[statusPending]:
		return statusPending
	default:
		return statusSuccess
	}
}

// loadStatuses returns the current status of every context on a commit (one row per
// context), newest first.
func loadStatuses(ctx context.Context, repoID, sha string) []CommitStatus {
	var out []CommitStatus
	connectRead().WithContext(ctx).
		Where("repo_id = ? AND sha = ?", repoID, sha).
		Order("updated_at desc").Find(&out)
	return out
}

func handlePostStatus(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handlePostStatus")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "createStatus")
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	if !commitSHARe.MatchString(sha) {
		http.Error(w, "invalid commit sha", http.StatusBadRequest)
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
	if req.Context == "" {
		http.Error(w, "context is required", http.StatusBadRequest)
		return
	}
	if !validStatusState(req.State) {
		http.Error(w, "state must be one of pending, success, failure, error", http.StatusBadRequest)
		return
	}
	if len(req.Context) > contextMax || len(req.Description) > descriptionMax {
		http.Error(w, "context or description too long", http.StatusRequestEntityTooLarge)
		return
	}

	now := time.Now().UTC()
	// Upsert by (repo, sha, context): a new verdict for a context replaces the old.
	var existing CommitStatus
	err := connectRead().WithContext(ctx).
		Where("repo_id = ? AND sha = ? AND context = ?", re.ID, sha, req.Context).First(&existing).Error
	if err == nil {
		if uerr := connect().WithContext(ctx).Model(&CommitStatus{}).Where("id = ?", existing.ID).
			Updates(map[string]any{
				"state": req.State, "description": req.Description,
				"target_url": req.TargetURL, "creator": userID, "updated_at": now,
			}).Error; uerr != nil {
			slog.ErrorContext(ctx, "update commit status", "Repo_id", re.ID, "sha", sha, "error", uerr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		existing.State, existing.Description, existing.TargetURL = req.State, req.Description, req.TargetURL
		existing.Creator, existing.UpdatedAt = userID, now
		span.SetStatus(codes.Ok, "")
		writeJSON(w, http.StatusOK, existing)
		return
	}
	s := CommitStatus{
		ID: uuid.New().String(), RepoID: re.ID, SHA: sha, Context: req.Context,
		State: req.State, Description: req.Description, TargetURL: req.TargetURL,
		Creator: userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := connect().WithContext(ctx).Create(&s).Error; err != nil {
		slog.ErrorContext(ctx, "create commit status", "Repo_id", re.ID, "sha", sha, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "commit status posted", "Repo_id", re.ID, "sha", sha, "context", req.Context, "state", req.State)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, s)
}

func handleListStatuses(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListStatuses")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listStatus")
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	if !commitSHARe.MatchString(sha) {
		http.Error(w, "invalid commit sha", http.StatusBadRequest)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, loadStatuses(ctx, re.ID, sha))
}

func handleCombinedStatus(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCombinedStatus")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listStatus")
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	if !commitSHARe.MatchString(sha) {
		http.Error(w, "invalid commit sha", http.StatusBadRequest)
		return
	}
	statuses := loadStatuses(ctx, re.ID, sha)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"sha":      sha,
		"state":    combinedState(statuses),
		"statuses": statuses,
	})
}
