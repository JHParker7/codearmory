package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// canAccessTicket returns true when the caller owns the ticket or shares its org.
func canAccessTicket(t Ticket, userID, orgID string) bool {
	return t.CreatedBy == userID || (orgID != "" && t.OrgID == orgID)
}

type createTicketRequest struct {
	Title            string  `json:"title"`
	Description      string  `json:"description"`
	Priority         string  `json:"priority"`
	AssigneeID       *string `json:"assignee_id"`
	WorkflowID       *string `json:"workflow_id"`
	RunID            *string `json:"run_id"`
	ForgeExecutionID *string `json:"forge_execution_id"`
}

type updateTicketRequest struct {
	Title            string  `json:"title"`
	Description      string  `json:"description"`
	Status           string  `json:"status"`
	Priority         string  `json:"priority"`
	AssigneeID       *string `json:"assignee_id"`
	WorkflowID       *string `json:"workflow_id"`
	RunID            *string `json:"run_id"`
	ForgeExecutionID *string `json:"forge_execution_id"`
}

func handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleCreateTicket")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createTicket", "tickets/tickets")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	var req createTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	priority := req.Priority
	if priority == "" {
		priority = PriorityMedium
	}
	if !slices.Contains(validPriorities, priority) {
		http.Error(w, "priority must be one of: low, medium, high, critical", http.StatusBadRequest)
		return
	}

	t := Ticket{
		TicketID:         uuid.New().String(),
		Title:            req.Title,
		Description:      req.Description,
		Status:           StatusOpen,
		Priority:         priority,
		CreatedBy:        userID,
		OrgID:            orgID,
		AssigneeID:       req.AssigneeID,
		WorkflowID:       req.WorkflowID,
		RunID:            req.RunID,
		ForgeExecutionID: req.ForgeExecutionID,
		Active:           true,
		Comments:         []TicketComment{},
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	if err := t.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create ticket: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to create ticket", http.StatusInternalServerError)
		return
	}

	meterTicketsCreated.Add(ctx, 1, metric.WithAttributes(attribute.String("priority", t.Priority)))
	span.SetAttributes(attribute.String("ticket.id", t.TicketID))
	span.SetStatus(codes.Ok, "")
	slog.Info("ticket created", "ticket_id", t.TicketID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(t) //nolint:errcheck
}

func handleListTickets(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleListTickets")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTicket", "tickets/tickets")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	q := r.URL.Query()
	statusFilter := q.Get("status")
	priorityFilter := q.Get("priority")
	assigneeFilter := q.Get("assignee_id")

	if statusFilter != "" && !slices.Contains(validStatuses, statusFilter) {
		http.Error(w, "invalid status filter", http.StatusBadRequest)
		return
	}
	if priorityFilter != "" && !slices.Contains(validPriorities, priorityFilter) {
		http.Error(w, "invalid priority filter", http.StatusBadRequest)
		return
	}

	tickets, err := listTickets(ctx, userID, orgID, statusFilter, priorityFilter, assigneeFilter)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list tickets: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list tickets", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tickets) //nolint:errcheck
}

func handleGetTicket(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleGetTicket")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getTicket", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	t, err := getTicket(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	comments, err := listComments(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get ticket: comments error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	t.Comments = comments

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t) //nolint:errcheck
}

func handleUpdateTicket(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleUpdateTicket")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateTicket", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	existing, err := getTicket(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("update ticket: fetch error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(existing, userID, orgID) {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	var req updateTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if req.Status == "" {
		req.Status = existing.Status
	}
	if !slices.Contains(validStatuses, req.Status) {
		http.Error(w, "status must be one of: open, in_progress, resolved, closed", http.StatusBadRequest)
		return
	}
	if req.Priority == "" {
		req.Priority = existing.Priority
	}
	if !slices.Contains(validPriorities, req.Priority) {
		http.Error(w, "priority must be one of: low, medium, high, critical", http.StatusBadRequest)
		return
	}

	wasTerminal := existing.Status == StatusResolved || existing.Status == StatusClosed

	existing.Title = req.Title
	existing.Description = req.Description
	existing.Status = req.Status
	existing.Priority = req.Priority
	existing.AssigneeID = req.AssigneeID
	existing.WorkflowID = req.WorkflowID
	existing.RunID = req.RunID
	existing.ForgeExecutionID = req.ForgeExecutionID

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to update ticket", http.StatusInternalServerError)
		return
	}

	nowTerminal := req.Status == StatusResolved || req.Status == StatusClosed
	if !wasTerminal && nowTerminal {
		meterTicketsResolved.Add(ctx, 1, metric.WithAttributes(attribute.String("status", req.Status)))
	}

	t, err := getTicket(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db fetch after update failed")
		slog.Error("update ticket: fetch after update", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get updated ticket", http.StatusInternalServerError)
		return
	}
	t.Comments, err = listComments(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db list comments failed")
		slog.Error("update ticket: list comments", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get updated ticket", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("ticket updated", "ticket_id", id, "user_id", userID, "status", req.Status)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t) //nolint:errcheck
}

func handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleDeleteTicket")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteTicket", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	t, err := getTicket(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete ticket: fetch error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	if err := t.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete ticket", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("ticket deleted", "ticket_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
