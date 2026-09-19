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

// Pull-request conversation comments (ARCHITECTURE §2a, extended).
//
// A comment is pure metadata — a Postgres row, like RepoShare or BranchProtection —
// so nothing here touches the git plane. It hangs off a PullRequest by the PR's
// stable UUID (PullID), not its per-repo number, so a comment survives even though
// numbers are only unique within a repo. RepoID is carried too so every read is
// scoped by repo as defence in depth behind authorizeRepo.
//
// Authorship is fixed at create time from the caller's gatekeeper identity; only
// the author may edit or delete their own comment. Repo access (who may comment at
// all) is the gatekeeper action on the repo — createPullComment for write, a
// reviewer or collaborator inherits it through shareActions.

type PRComment struct {
	ID     string `gorm:"primaryKey" json:"id"`
	RepoID string `gorm:"index" json:"repo_id"`
	PullID string `gorm:"index" json:"pull_id"`
	Author string `json:"author"` // gatekeeper user_id (the DISPLAYED author)
	// CreatedByUserID is the identity that actually called createPullComment. It differs
	// from Author only when an authorised automation caller attributed the comment to a
	// bot display identity via the `author` override (see automationAuthors); the real
	// caller is kept here for accountability. Empty when no override was used.
	CreatedByUserID string    `json:"created_by,omitempty"`
	Body            string    `json:"body"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// prCommentBodyMax caps a comment the way the PR body and ticket description are
// capped (64 KB): a review note, not a file upload.
const prCommentBodyMax = 64 * 1024

func handleListPRComments(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListPRComments")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listPullComment")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	var comments []PRComment
	if err := connectRead().WithContext(ctx).
		Where("repo_id = ? AND pull_id = ?", re.ID, pr.ID).
		Order("created_at asc").Find(&comments).Error; err != nil {
		slog.ErrorContext(ctx, "list pull comments", "Repo_id", re.ID, "number", pr.Number, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, comments)
}

func handleCreatePRComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreatePRComment")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "createPullComment")
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
		// Author, when set, attributes the comment to an allowlisted automation identity
		// (see automationAuthors), so an automated review/scan/release comment shows the
		// bot rather than the run's user. Same allowlist guard as createPull.
		Author string `json:"author"`
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
	if len(req.Body) > prCommentBodyMax {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Author override: attribution only (the createPullComment check already passed),
	// confined to the automation allowlist so it names a sanctioned bot but not a human.
	author, createdBy := userID, ""
	if req.Author != "" && req.Author != userID {
		if !automationAuthors()[req.Author] {
			http.Error(w, "author override must name an allowlisted automation account", http.StatusForbidden)
			return
		}
		author, createdBy = req.Author, userID
	}
	c := PRComment{
		ID: uuid.New().String(), RepoID: re.ID, PullID: pr.ID,
		Author: author, CreatedByUserID: createdBy, Body: req.Body,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		slog.ErrorContext(ctx, "create pull comment", "Repo_id", re.ID, "number", pr.Number, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "pull request comment added", "Repo_id", re.ID, "number", pr.Number, "author", c.Author, "created_by", userID)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, c)
}

func handleUpdatePRComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleUpdatePRComment")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "updatePullComment")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	c, err := loadPRComment(ctx, re.ID, pr.ID, r.PathValue("commentID"))
	if err != nil {
		commentErr(ctx, w, err, re.ID)
		return
	}
	// Only the author edits their own words; repo access alone is not enough.
	if c.Author != userID {
		http.Error(w, "only the author can edit this comment", http.StatusForbidden)
		return
	}
	var req struct {
		Body string `json:"body"`
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
	if len(req.Body) > prCommentBodyMax {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := connect().WithContext(ctx).Model(&PRComment{}).Where("id = ?", c.ID).
		Updates(map[string]any{"body": req.Body, "updated_at": time.Now().UTC()}).Error; err != nil {
		slog.ErrorContext(ctx, "update pull comment", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	c.Body = req.Body
	c.UpdatedAt = time.Now().UTC()
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, c)
}

func handleDeletePRComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleDeletePRComment")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "deletePullComment")
	if !ok {
		return
	}
	pr, err := loadPull(ctx, re.ID, r.PathValue("number"))
	if err != nil {
		pullErr(ctx, w, err, re.ID)
		return
	}
	c, err := loadPRComment(ctx, re.ID, pr.ID, r.PathValue("commentID"))
	if err != nil {
		commentErr(ctx, w, err, re.ID)
		return
	}
	if c.Author != userID {
		http.Error(w, "only the author can delete this comment", http.StatusForbidden)
		return
	}
	if err := connect().WithContext(ctx).Where("id = ?", c.ID).Delete(&PRComment{}).Error; err != nil {
		slog.ErrorContext(ctx, "delete pull comment", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// loadPRComment fetches one comment scoped to its repo AND pull, so a valid id from
// another repo/PR cannot be addressed through this repo's path.
func loadPRComment(ctx context.Context, repoID, pullID, commentID string) (PRComment, error) {
	var c PRComment
	err := connectRead().WithContext(ctx).
		Where("id = ? AND repo_id = ? AND pull_id = ?", commentID, repoID, pullID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PRComment{}, errCommentNotFound
	}
	return c, err
}

var errCommentNotFound = errors.New("comment not found")

func commentErr(ctx context.Context, w http.ResponseWriter, err error, repoID string) {
	if errors.Is(err, errCommentNotFound) {
		http.Error(w, "comment not found", http.StatusNotFound)
		return
	}
	slog.ErrorContext(ctx, "load pull comment", "Repo_id", repoID, "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
