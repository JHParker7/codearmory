package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

type addCommentRequest struct {
	Body string `json:"body"`
}

func handleAddComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleAddComment")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createComment", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	t, err := getTicket(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("add comment: get ticket", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	var req addCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Body == "" {
		http.Error(w, "body is required", http.StatusBadRequest)
		return
	}

	c := TicketComment{
		CommentID: uuid.New().String(),
		TicketID:  id,
		AuthorID:  userID,
		Body:      req.Body,
		Active:    true,
	}
	if err := c.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("add comment: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to add comment", http.StatusInternalServerError)
		return
	}

	// Refresh comment timestamps from DB.
	if refreshed, err := refreshComment(ctx, c.CommentID); err == nil {
		c = refreshed
	} else {
		slog.Warn("add comment: refresh failed", "comment_id", c.CommentID, "error", err)
	}

	meterCommentsAdded.Add(ctx, 1, metric.WithAttributes(attribute.String("ticket.id", id)))
	span.SetAttributes(attribute.String("comment.id", c.CommentID))
	span.SetStatus(codes.Ok, "")
	slog.Info("comment added", "comment_id", c.CommentID, "ticket_id", id, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleDeleteComment")
	defer span.End()

	ticketID := r.PathValue("id")
	commentID := r.PathValue("comment_id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteComment", "tickets/tickets/"+ticketID+"/comments/"+commentID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	t, err := getTicket(ctx, ticketID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete comment: get ticket", "ticket_id", ticketID, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	// Only the comment author or a ticket owner/org-member can delete.
	existing, err := getComment(ctx, commentID, ticketID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "comment not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete comment: lookup", "comment_id", commentID, "user_id", userID, "error", err)
		http.Error(w, "failed to get comment", http.StatusInternalServerError)
		return
	}
	// Ticket owners/org-members can moderate; author can always delete their own.
	if existing.AuthorID != userID && !canAccessTicket(t, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := existing.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete comment: db error", "comment_id", commentID, "user_id", userID, "error", err)
		http.Error(w, "failed to delete comment", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("comment deleted", "comment_id", commentID, "ticket_id", ticketID, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
