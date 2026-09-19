package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// Pull request reviews and comments.
//
// A PR without a review layer can be opened and merged but not *discussed*, which
// makes "require sign-off before this lands on main" inexpressible — the gap branch
// protection needs filled before it can gate a merge on anything but the ref name.
//
// Two records, deliberately separate:
//
//   - PullReview is a VERDICT (approve / request changes / plain comment). It is the
//     thing policy counts, so it carries the source-branch SHA it was given against —
//     an approval is of a diff, not of a title, and the diff changes when the branch
//     moves. See staleness below.
//   - PullComment is discussion. It never affects policy, so it is free of all that.
//
// Only a reviewer's LATEST verdict counts, matching every other platform: someone who
// requests changes and later approves is not still blocking. That is a query-time
// decision (latestReviews) rather than a mutation of history — the earlier verdicts stay
// readable, which is the point of a review trail.

type PullReview struct {
	ID       string `gorm:"primaryKey" json:"id"`
	RepoID   string `gorm:"index" json:"repo_id"`
	PullID   string `gorm:"index" json:"pull_id"`
	Reviewer string `json:"reviewer"` // gatekeeper user_id
	State    string `json:"state"`    // approved | changes_requested | commented
	Body     string `json:"body"`
	// CommitSHA is the source branch head when the verdict was given. An approval of
	// commit A says nothing about commit B, so this is what makes a stale approval
	// detectable rather than silently load-bearing.
	CommitSHA string    `json:"commit_sha"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PullComment struct {
	ID     string `gorm:"primaryKey" json:"id"`
	RepoID string `gorm:"index" json:"repo_id"`
	PullID string `gorm:"index" json:"pull_id"`
	Author string `json:"author"`
	Body   string `json:"body"`
	// Path and Line make it a review comment on a specific line rather than a general
	// one. Both empty is a conversation comment; the pair is not validated against the
	// diff, because the diff moves and a comment that outlives its line is still worth
	// reading.
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	reviewApproved         = "approved"
	reviewChangesRequested = "changes_requested"
	reviewCommented        = "commented"
)

// validReviewState is the closed set. An unknown state would be stored and then never
// matched by the merge gate, which fails OPEN — so it is rejected at the door.
func validReviewState(s string) bool {
	switch s {
	case reviewApproved, reviewChangesRequested, reviewCommented:
		return true
	}
	return false
}

// latestReviews reduces a PR's review trail to one verdict per reviewer — the most
// recent. This is what policy reads.
func latestReviews(ctx context.Context, pullID string) ([]PullReview, error) {
	var all []PullReview
	if err := connectRead().WithContext(ctx).
		Where("pull_id = ?", pullID).Order("created_at ASC").Find(&all).Error; err != nil {
		return nil, err
	}
	// Later rows overwrite earlier ones for the same reviewer; ordering ASC above is
	// what makes "last write wins" mean "most recent verdict".
	byReviewer := make(map[string]PullReview, len(all))
	for _, rv := range all {
		byReviewer[rv.Reviewer] = rv
	}
	out := make([]PullReview, 0, len(byReviewer))
	for _, rv := range byReviewer {
		out = append(out, rv)
	}
	return out, nil
}

func handleCreateReview(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreateReview")
	defer span.End()

	// Reviewing is a write to the PR's record, but not to the repository — it takes its
	// own action so "may review" can be granted without "may merge".
	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "reviewPull")
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
	var req struct {
		State string `json:"state"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.State = strings.TrimSpace(req.State)
	if !validReviewState(req.State) {
		http.Error(w, "state must be approved, changes_requested or commented", http.StatusBadRequest)
		return
	}
	// A self-approval is not a review. Blocking it here rather than only in the merge
	// gate means the trail never records one, so a later reading of "who approved this"
	// cannot be misled.
	if req.State == reviewApproved && pr.Author == userID {
		http.Error(w, "you cannot approve your own pull request", http.StatusForbidden)
		return
	}

	rv := PullReview{
		ID: uuid.New().String(), RepoID: re.ID, PullID: pr.ID,
		Reviewer: userID, State: req.State, Body: req.Body,
		CommitSHA: refSHA(ctx, re.ID, pr.localSourceRef()),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).Create(&rv).Error; err != nil {
		slog.ErrorContext(ctx, "create review", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "pull request reviewed", "Repo_id", re.ID, "number", pr.Number,
		"reviewer", userID, "state", req.State)
	notifyPullEvent(ctx, re, eventPullReviewed, pr, userID)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, rv)
}

// handleListReviews returns the full trail, plus the reduced per-reviewer verdicts the
// merge gate actually reads — so a UI can show both the history and the current state
// without re-deriving the second from the first and disagreeing with the server.
func handleListReviews(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListReviews")
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
	var all []PullReview
	if err := connectRead().WithContext(ctx).
		Where("pull_id = ?", pr.ID).Order("created_at ASC").Find(&all).Error; err != nil {
		slog.ErrorContext(ctx, "list reviews", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if all == nil {
		all = []PullReview{}
	}
	latest, err := latestReviews(ctx, pr.ID)
	if err != nil {
		slog.ErrorContext(ctx, "latest reviews", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	head := refSHA(ctx, re.ID, pr.localSourceRef())
	writeJSON(w, http.StatusOK, map[string]any{
		"reviews": all,
		"current": latest,
		// The head every "current" verdict is measured against, so a client can render
		// "approved (stale)" without a second round trip.
		"head": head,
	})
}

func handleCreateComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreateComment")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "commentPull")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	var req struct {
		Body string `json:"body"`
		Path string `json:"path"`
		Line int    `json:"line"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" {
		http.Error(w, "body is required", http.StatusBadRequest)
		return
	}
	c := PullComment{
		ID: uuid.New().String(), RepoID: re.ID, PullID: pr.ID,
		Author: userID, Body: req.Body, Path: strings.TrimSpace(req.Path), Line: req.Line,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		slog.ErrorContext(ctx, "create comment", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, c)
}

func handleListComments(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListComments")
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
	var out []PullComment
	if err := connectRead().WithContext(ctx).
		Where("pull_id = ?", pr.ID).Order("created_at ASC").Find(&out).Error; err != nil {
		slog.ErrorContext(ctx, "list comments", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []PullComment{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteComment removes one comment. Scoped to its author: deleting someone
// else's comment is a moderation action this service does not model, and letting any
// repo writer do it silently would be worse than not offering it.
func handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleDeleteComment")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "commentPull")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	var c PullComment
	err = connectRead().WithContext(ctx).
		Where("id = ? AND pull_id = ?", r.PathValue("comment_id"), pr.ID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.Error(w, "comment not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "load comment", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if c.Author != userID {
		// 404 rather than 403, for the same reason authorizeRepo rewrites denials: a
		// 403 confirms the comment exists.
		http.Error(w, "comment not found", http.StatusNotFound)
		return
	}
	if err := connect().WithContext(ctx).Delete(&PullComment{}, "id = ?", c.ID).Error; err != nil {
		slog.ErrorContext(ctx, "delete comment", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
