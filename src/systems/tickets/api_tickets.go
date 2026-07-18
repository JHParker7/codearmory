package main

import (
	"context"
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
	Status           string  `json:"status"`
	Priority         string  `json:"priority"`
	Project          string  `json:"project"`
	BoardID          *string `json:"board_id"`
	ParentID         *string `json:"parent_id"`
	Timescale        string  `json:"timescale"`
	DueDate          *string `json:"due_date"`
	AssigneeID       *string `json:"assignee_id"`
	WorkflowID       *string `json:"workflow_id"`
	RunID            *string `json:"run_id"`
	ForgeExecutionID *string `json:"forge_execution_id"`
}

type updateTicketRequest struct {
	Title            string  `json:"title"`
	Description      *string `json:"description"`
	Status           string  `json:"status"`
	Priority         string  `json:"priority"`
	Project          string  `json:"project"`
	BoardID          *string `json:"board_id"`
	ParentID         *string `json:"parent_id"`
	Timescale        string  `json:"timescale"`
	DueDate          *string `json:"due_date"`
	AssigneeID       *string `json:"assignee_id"`
	WorkflowID       *string `json:"workflow_id"`
	RunID            *string `json:"run_id"`
	ForgeExecutionID *string `json:"forge_execution_id"`
}

// normalizeBoardID validates a ticket's requested board: a nil or empty pointer
// means "no board" (returns nil); otherwise the board must exist and be
// accessible to the caller. The bool is false when the board is missing or
// inaccessible (caller should 400); a non-nil error is a DB failure (500).
func normalizeBoardID(ctx context.Context, boardID *string, userID, orgID string) (*string, bool, error) {
	if boardID == nil || *boardID == "" {
		return nil, true, nil
	}
	b, err := getBoard(ctx, *boardID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !canAccessBoard(b, userID, orgID) {
		return nil, false, nil
	}
	id := b.BoardID
	return &id, true, nil
}

// normalizeParentID validates a ticket's requested parent (sub-ticket hierarchy):
// nil/empty means no parent. Otherwise the parent must exist, be accessible, not be
// the ticket itself, and not create a cycle — its ancestor chain must not reach
// selfID (empty on create, since a brand-new ticket can't be an ancestor). The bool
// is false when the parent is missing/inaccessible/cyclic (caller 400s); a non-nil
// error is a DB failure (500).
func normalizeParentID(ctx context.Context, parentID *string, selfID, userID, orgID string) (*string, bool, error) {
	if parentID == nil || *parentID == "" {
		return nil, true, nil
	}
	if *parentID == selfID {
		return nil, false, nil // a ticket cannot be its own parent
	}
	// Walk the ancestor chain from the requested parent to the root, checking each
	// exists + is accessible and that we never reach selfID (a cycle). maxParentDepth
	// bounds the walk against any pre-existing cycle in the data.
	const maxParentDepth = 64
	cur := *parentID
	for range maxParentDepth {
		row, err := (Ticket{TicketID: cur}).Get(ctx)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, false, nil
			}
			return nil, false, err
		}
		t := row.(Ticket)
		if !canAccessTicket(t, userID, orgID) {
			return nil, false, nil
		}
		if t.ParentID == nil || *t.ParentID == "" {
			break // reached a root without hitting selfID
		}
		if *t.ParentID == selfID {
			return nil, false, nil // would form a cycle
		}
		cur = *t.ParentID
	}
	id := *parentID
	return &id, true, nil
}

// resolveCreateBoardID resolves the board a ticket will be placed on. Every
// ticket must belong to a board: a nil or empty requested board falls back to
// the caller's default board (created on first use), while a named board must
// exist and be accessible. The bool is false when a named board is missing or
// inaccessible (caller should 400); a non-nil error is a DB failure (500).
func resolveCreateBoardID(ctx context.Context, boardID *string, userID, orgID string) (*string, bool, error) {
	if boardID == nil || *boardID == "" {
		b, err := getOrCreateDefaultBoard(ctx, userID, orgID)
		if err != nil {
			return nil, false, err
		}
		id := b.BoardID
		return &id, true, nil
	}
	return normalizeBoardID(ctx, boardID, userID, orgID)
}

// boardScope flattens a ticket's resolved board pointer to the string board
// scope used by the field-def helpers ("" = the org/global status set).
func boardScope(boardID *string) string {
	if boardID == nil {
		return ""
	}
	return *boardID
}

// parseDueDate parses a due date string in YYYY-MM-DD or RFC3339 format.
func parseDueDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	if t, err := time.Parse("2006-01-02", *s); err == nil {
		ut := t.UTC()
		return &ut, nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil, err
	}
	ut := t.UTC()
	return &ut, nil
}

func handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleCreateTicket")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createTicket", "tickets/tickets")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
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
	if !slices.Contains(getFieldDefValues(ctx, orgID, FieldKindPriority, "", validPriorities), priority) {
		http.Error(w, "invalid priority", http.StatusBadRequest)
		return
	}

	// Every ticket must belong to a board: resolve the requested board (or fall
	// back to the caller's default board) so the status can be scoped to that
	// board's own columns.
	boardID, boardOK, err := resolveCreateBoardID(ctx, req.BoardID, userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "board lookup failed")
		slog.ErrorContext(ctx, "create ticket: board lookup", "user_id", userID, "error", err)
		http.Error(w, "failed to create ticket", http.StatusInternalServerError)
		return
	}
	if !boardOK {
		http.Error(w, "board not found or not accessible", http.StatusBadRequest)
		return
	}
	scopeBoard := boardScope(boardID)

	parentID, parentOK, err := normalizeParentID(ctx, req.ParentID, "", userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parent lookup failed")
		http.Error(w, "failed to create ticket", http.StatusInternalServerError)
		return
	}
	if !parentOK {
		http.Error(w, "parent ticket not found or not accessible", http.StatusBadRequest)
		return
	}

	// Status defaults to the board's left-most column when the client omits it.
	statusValues := getFieldDefValues(ctx, orgID, FieldKindStatus, scopeBoard, validStatuses)
	status := req.Status
	if status == "" {
		status = statusValues[0]
	}
	if !slices.Contains(statusValues, status) {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	dueDate, err := parseDueDate(req.DueDate)
	if err != nil {
		http.Error(w, "invalid due_date: use YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	t := Ticket{
		TicketID:         uuid.New().String(),
		Title:            req.Title,
		Description:      req.Description,
		Status:           status,
		Priority:         priority,
		Project:          req.Project,
		BoardID:          boardID,
		ParentID:         parentID,
		Timescale:        req.Timescale,
		DueDate:          dueDate,
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
		slog.ErrorContext(ctx, "create ticket: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to create ticket", http.StatusInternalServerError)
		return
	}

	meterTicketsCreated.Add(ctx, 1, metric.WithAttributes(attribute.String("priority", t.Priority)))
	notifyHooks(ctx, eventTicketCreated, t.Status, t, nil)
	span.SetAttributes(attribute.String("ticket.id", t.TicketID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "ticket created", "ticket_id", t.TicketID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(t) //nolint:errcheck
}

func handleListTickets(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleListTickets")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listTicket", "tickets/tickets")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	q := r.URL.Query()
	statusFilter := q.Get("status")
	priorityFilter := q.Get("priority")
	assigneeFilter := q.Get("assignee_id")
	timescaleFilter := q.Get("timescale")
	projectFilter := q.Get("project")
	boardFilter := q.Get("board_id")

	// Validate the status filter against the columns of the board being viewed
	// (a real board_id), else the org/global status set. "none" (unassigned) and
	// "" both map to the org/global scope.
	statusScopeBoard := ""
	if boardFilter != "" && boardFilter != "none" {
		statusScopeBoard = boardFilter
	}
	if statusFilter != "" && !slices.Contains(getFieldDefValues(ctx, orgID, FieldKindStatus, statusScopeBoard, validStatuses), statusFilter) {
		http.Error(w, "invalid status filter", http.StatusBadRequest)
		return
	}
	if priorityFilter != "" && !slices.Contains(getFieldDefValues(ctx, orgID, FieldKindPriority, "", validPriorities), priorityFilter) {
		http.Error(w, "invalid priority filter", http.StatusBadRequest)
		return
	}

	tickets, err := listTickets(ctx, userID, orgID, statusFilter, priorityFilter, assigneeFilter, timescaleFilter, projectFilter, boardFilter)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list tickets: db error", "user_id", userID, "error", err)
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
		slog.ErrorContext(ctx, "get ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	comments, err := listComments(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "get ticket: comments error", "ticket_id", id, "user_id", userID, "error", err)
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
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	existing, err := getTicket(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "update ticket: fetch error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	var req updateTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Partial update: keep the current title when the caller omits it, matching
	// how status/priority/timescale below fall back to the existing values. This
	// lets clients (e.g. the portal's open/close toggle) PATCH a single field
	// without resending the whole ticket.
	if req.Title == "" {
		req.Title = existing.Title
	}

	// Validate a board change up-front (before any mutation) so the status can be
	// scoped to the resulting board's own columns. A nil pointer means the field
	// was omitted (keep current); an explicit "" re-homes the ticket to the
	// caller's default board, since every ticket must belong to a board.
	boardChanged := req.BoardID != nil
	var newBoardID *string
	if boardChanged {
		bid, boardOK, err := resolveCreateBoardID(ctx, req.BoardID, userID, orgID)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "board lookup failed")
			http.Error(w, "failed to update ticket", http.StatusInternalServerError)
			return
		}
		if !boardOK {
			http.Error(w, "board not found or not accessible", http.StatusBadRequest)
			return
		}
		newBoardID = bid
	}
	effectiveBoardID := boardScope(existing.BoardID)
	if boardChanged {
		effectiveBoardID = boardScope(newBoardID)
	}

	// A parent change is validated up-front (exists, accessible, no cycle). A nil
	// pointer keeps the current parent; an explicit "" clears it.
	parentChanged := req.ParentID != nil
	var newParentID *string
	if parentChanged {
		pid, parentOK, err := normalizeParentID(ctx, req.ParentID, existing.TicketID, userID, orgID)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "parent lookup failed")
			http.Error(w, "failed to update ticket", http.StatusInternalServerError)
			return
		}
		if !parentOK {
			http.Error(w, "parent ticket not found, not accessible, or would create a cycle", http.StatusBadRequest)
			return
		}
		newParentID = pid
	}

	if req.Status == "" {
		req.Status = existing.Status
	}
	statusValues := getFieldDefValues(ctx, orgID, FieldKindStatus, effectiveBoardID, validStatuses)
	if !slices.Contains(statusValues, req.Status) {
		// Moving a ticket to a board whose columns don't include the current
		// status snaps it to that board's left-most column rather than 400ing.
		if boardChanged {
			req.Status = statusValues[0]
		} else {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
	}
	if req.Priority == "" {
		req.Priority = existing.Priority
	}
	if !slices.Contains(getFieldDefValues(ctx, orgID, FieldKindPriority, "", validPriorities), req.Priority) {
		http.Error(w, "invalid priority", http.StatusBadRequest)
		return
	}

	dueDate, err := parseDueDate(req.DueDate)
	if err != nil {
		http.Error(w, "invalid due_date: use YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	wasOpen := existing.Status != StatusResolved && existing.Status != StatusClosed
	prevStatus := existing.Status

	existing.Title = req.Title
	if req.Description != nil {
		existing.Description = *req.Description
	}
	existing.Status = req.Status
	existing.Priority = req.Priority
	// Guard Timescale/DueDate like the nullable fields below so a partial PUT that
	// omits them doesn't silently wipe the stored values. Timescale is a plain
	// string (empty = omitted); DueDate is a pointer (nil = omitted, "" = clear).
	if req.Timescale != "" {
		existing.Timescale = req.Timescale
	}
	if req.Project != "" {
		existing.Project = req.Project
	}
	if req.DueDate != nil {
		existing.DueDate = dueDate
	}
	if req.AssigneeID != nil {
		existing.AssigneeID = req.AssigneeID
	}
	if req.WorkflowID != nil {
		existing.WorkflowID = req.WorkflowID
	}
	if req.RunID != nil {
		existing.RunID = req.RunID
	}
	if req.ForgeExecutionID != nil {
		existing.ForgeExecutionID = req.ForgeExecutionID
	}
	if boardChanged {
		existing.BoardID = newBoardID
	}
	if parentChanged {
		existing.ParentID = newParentID
	}

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to update ticket", http.StatusInternalServerError)
		return
	}

	isNowTerminal := req.Status == StatusResolved || req.Status == StatusClosed
	if wasOpen && isNowTerminal {
		meterTicketsResolved.Add(ctx, 1, metric.WithAttributes(attribute.String("status", req.Status)))
	}

	notifyHooks(ctx, eventTicketUpdated, existing.Status, existing, nil)
	if existing.Status != prevStatus {
		notifyHooks(ctx, eventTicketStatus, existing.Status, existing, map[string]string{
			"old_status": prevStatus,
			"new_status": existing.Status,
		})
	}

	t, err := getTicket(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db fetch after update failed")
		slog.ErrorContext(ctx, "update ticket: fetch after update", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get updated ticket", http.StatusInternalServerError)
		return
	}
	t.Comments, err = listComments(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db list comments failed")
		slog.ErrorContext(ctx, "update ticket: list comments", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get updated ticket", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "ticket updated", "ticket_id", id, "user_id", userID, "status", req.Status)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t) //nolint:errcheck
}

func handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleDeleteTicket")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteTicket", "tickets/tickets/"+id)
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
		slog.ErrorContext(ctx, "delete ticket: fetch error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete ticket", http.StatusInternalServerError)
		return
	}
	if !canAccessTicket(t, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	}

	if err := t.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete ticket", http.StatusInternalServerError)
		return
	}

	notifyHooks(ctx, eventTicketDeleted, t.Status, t, nil)
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "ticket deleted", "ticket_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
