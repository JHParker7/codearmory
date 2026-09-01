package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Pull-request reviews (ARCHITECTURE §2a, extended).
//
// A review is a reviewer's verdict on a PR — approve, request changes, or a plain
// comment — a Postgres row hung off the PR by its stable UUID, like a PR comment. A
// reviewer may review more than once; only their LATEST review counts toward the
// decision, so re-approving after a fix supersedes the earlier changes-requested.
//
// Reviews are advisory by default: they surface on the PR. They only GATE a merge
// when the target branch's protection has RequireReview on (protect.go) — then the PR
// needs an approval from someone OTHER than its author and no outstanding
// changes-requested. Off by default, so an existing repo's merges never change until
// a maintainer turns it on.

type PRReview struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	RepoID    string    `gorm:"index" json:"repo_id"`
	PullID    string    `gorm:"index" json:"pull_id"`
	Reviewer  string    `json:"reviewer"` // gatekeeper user_id
	State     string    `json:"state"`    // approved | changes_requested | commented
	Body      string    `json:"body,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	reviewApproved         = "approved"
	reviewChangesRequested = "changes_requested"
	reviewCommented        = "commented"
)

func validReviewState(s string) bool {
	switch s {
	case reviewApproved, reviewChangesRequested, reviewCommented:
		return true
	}
	return false
}

// reviewDecision is the merge-relevant summary of a PR's reviews: how many distinct
// reviewers currently approve, and whether anyone currently blocks with a
// changes-requested. Author self-approval is excluded, so approving your own PR never
// satisfies a required review.
type reviewDecision struct {
	Approvals        int  `json:"approvals"`
	ChangesRequested bool `json:"changes_requested"`
	Reviews          int  `json:"reviews"`
}

// decideReviews reduces a PR's review history to the current verdict, counting only the
// LATEST review per reviewer and ignoring the author's own. reviews must be ordered
// oldest-first so the last write per reviewer wins.
func decideReviews(reviews []PRReview, author string) reviewDecision {
	latest := make(map[string]PRReview, len(reviews))
	for _, r := range reviews {
		latest[r.Reviewer] = r
	}
	d := reviewDecision{Reviews: len(reviews)}
	for reviewer, r := range latest {
		if reviewer == author {
			continue // your own approval never counts toward a required review
		}
		switch r.State {
		case reviewApproved:
			d.Approvals++
		case reviewChangesRequested:
			d.ChangesRequested = true
		}
	}
	return d
}

func loadReviews(ctx context.Context, repoID, pullID string) []PRReview {
	var out []PRReview
	connectRead().WithContext(ctx).
		Where("repo_id = ? AND pull_id = ?", repoID, pullID).
		Order("created_at asc").Find(&out)
	return out
}

// reviewRequiredFor reports whether a merge into branch is gated on review — any
// protection rule whose pattern matches the branch and carries RequireReview. The
// pattern is the same glob the push hook uses; path.Match covers the "main" and
// "release/*" shapes it allows.
func reviewRequiredFor(ctx context.Context, repoID, branch string) bool {
	rules, err := protectionsFor(ctx, repoID)
	if err != nil {
		return false // fail open: a lookup error must not wedge every merge
	}
	for _, r := range rules {
		if !r.RequireReview {
			continue
		}
		if ok, _ := path.Match(r.Pattern, branch); ok || r.Pattern == branch {
			return true
		}
	}
	return false
}

func handleSubmitReview(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleSubmitReview")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "createReview")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	if pr.State != prOpen {
		http.Error(w, "a "+pr.State+" pull request cannot be reviewed", http.StatusConflict)
		return
	}
	var req struct {
		State string `json:"state"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if !validReviewState(req.State) {
		http.Error(w, "state must be one of approved, changes_requested, commented", http.StatusBadRequest)
		return
	}
	// A comment-only review with no words says nothing; approve/request-changes carry a
	// verdict on their own.
	if req.State == reviewCommented && req.Body == "" {
		http.Error(w, "a commented review needs a body", http.StatusBadRequest)
		return
	}
	if len(req.Body) > prCommentBodyMax {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	rv := PRReview{
		ID: uuid.New().String(), RepoID: re.ID, PullID: pr.ID,
		Reviewer: userID, State: req.State, Body: req.Body,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).Create(&rv).Error; err != nil {
		slog.ErrorContext(ctx, "submit review", "Repo_id", re.ID, "number", pr.Number, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "pull request reviewed", "Repo_id", re.ID, "number", pr.Number, "reviewer", userID, "state", req.State)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, rv)
}

func handleListReviews(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListReviews")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listReview")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	reviews := loadReviews(ctx, re.ID, pr.ID)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"reviews":  reviews,
		"decision": decideReviews(reviews, pr.Author),
	})
}
