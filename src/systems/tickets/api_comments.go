package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

type addCommentRequest struct {
	Body string `json:"body"`
}

func handleAddComment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleAddComment")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "createComment", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	t, err := getTicket(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		slog.Error("add comment: get ticket", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
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
	}
	_, err = db.Exec(ctx,
		`INSERT INTO ticket_comments (comment_id, ticket_id, author_id, body) VALUES ($1,$2,$3,$4)`,
		c.CommentID, c.TicketID, c.AuthorID, c.Body,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("add comment: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to add comment", http.StatusInternalServerError)
		return
	}

	// Refresh comment timestamps from DB.
	row := db.QueryRow(ctx,
		`SELECT comment_id, ticket_id, author_id, body, created_at, updated_at
		 FROM ticket_comments WHERE comment_id=$1`, c.CommentID,
	)
	if err := row.Scan(&c.CommentID, &c.TicketID, &c.AuthorID, &c.Body, &c.CreatedAt, &c.UpdatedAt); err != nil {
		slog.Warn("add comment: refresh scan failed", "comment_id", c.CommentID, "error", err)
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "deleteComment", "tickets/tickets/"+ticketID+"/comments/"+commentID)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	t, err := getTicket(ctx, ticketID)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		slog.Error("delete comment: get ticket", "ticket_id", ticketID, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	// Only the comment author or a ticket owner/org-member can delete.
	var authorID string
	err = db.QueryRow(ctx,
		`SELECT author_id FROM ticket_comments WHERE comment_id=$1 AND ticket_id=$2 AND active=true`,
		commentID, ticketID,
	).Scan(&authorID)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "comment not found", http.StatusNotFound)
			return
		}
		slog.Error("delete comment: lookup", "comment_id", commentID, "user_id", userID, "error", err)
		http.Error(w, "failed to get comment", http.StatusInternalServerError)
		return
	}
	// Ticket owners/org-members can moderate; author can always delete their own.
	if authorID != userID && !canAccessTicket(t, userID, orgID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if _, err = db.Exec(ctx,
		`UPDATE ticket_comments SET active=false, updated_at=now() WHERE comment_id=$1 AND active=true`,
		commentID,
	); err != nil {
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

func listComments(ctx context.Context, ticketID string) ([]TicketComment, error) {
	rows, err := db.Query(ctx,
		`SELECT comment_id, ticket_id, author_id, body, created_at, updated_at
		 FROM ticket_comments WHERE ticket_id=$1 AND active=true ORDER BY created_at`,
		ticketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var comments []TicketComment
	for rows.Next() {
		var c TicketComment
		if err := rows.Scan(&c.CommentID, &c.TicketID, &c.AuthorID, &c.Body, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	if comments == nil {
		comments = []TicketComment{}
	}
	return comments, rows.Err()
}
