package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// checkGatekeeper calls gatekeeper's /check_permissions endpoint using the
// caller's Bearer token. Returns userID, orgID, and true when authorised.
func checkGatekeeper(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (userID, orgID string, ok bool) {
	token, hasBearerPrefix := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !hasBearerPrefix || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}

	body, _ := json.Marshal(map[string]string{
		"service":  "tickets",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.Error("tickets: failed to build gatekeeper request", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("tickets: gatekeeper check_permissions failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		slog.Error("tickets: gatekeeper unavailable", "status", resp.StatusCode)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return "", "", false
	}

	var result struct {
		Authorized bool    `json:"authorized"`
		UserID     string  `json:"user_id"`
		OrgID      *string `json:"org_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Authorized {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", false
	}
	org := ""
	if result.OrgID != nil {
		org = *result.OrgID
	}
	return result.UserID, org, true
}

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

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "createTicket", "tickets/tickets")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

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
		Comments:         []TicketComment{},
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	_, err := db.Exec(ctx,
		`INSERT INTO tickets (ticket_id, title, description, status, priority, created_by, org_id,
		                      assignee_id, workflow_id, run_id, forge_execution_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		t.TicketID, t.Title, t.Description, t.Status, t.Priority, t.CreatedBy, t.OrgID,
		t.AssigneeID, t.WorkflowID, t.RunID, t.ForgeExecutionID,
	)
	if err != nil {
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

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "listTicket", "tickets/tickets")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

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

	// Nullable params: pass nil to skip a filter (NULL = $n is always false in SQL,
	// so `($3::text IS NULL OR col = $3)` skips the filter when $3 is nil).
	var statusArg, priorityArg, assigneeArg *string
	if statusFilter != "" {
		statusArg = &statusFilter
	}
	if priorityFilter != "" {
		priorityArg = &priorityFilter
	}
	if assigneeFilter != "" {
		assigneeArg = &assigneeFilter
	}

	rows, err := db.Query(ctx,
		`SELECT ticket_id, title, description, status, priority, created_by, org_id,
		        assignee_id, workflow_id, run_id, forge_execution_id, created_at, updated_at
		 FROM tickets
		 WHERE active = true
		   AND (created_by = $1 OR (org_id != '' AND org_id = $2))
		   AND ($3::text IS NULL OR status = $3)
		   AND ($4::text IS NULL OR priority = $4)
		   AND ($5::text IS NULL OR assignee_id = $5)
		 ORDER BY created_at DESC LIMIT 100`,
		userID, orgID, statusArg, priorityArg, assigneeArg,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list tickets: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list tickets", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	tickets, err := scanTickets(rows)
	if err != nil {
		slog.Error("list tickets: scan error", "user_id", userID, "error", err)
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "getTicket", "tickets/tickets/"+id)
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "updateTicket", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	existing, err := getTicket(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
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

	_, err = db.Exec(ctx,
		`UPDATE tickets
		 SET title=$1, description=$2, status=$3, priority=$4,
		     assignee_id=$5, workflow_id=$6, run_id=$7, forge_execution_id=$8, updated_at=now()
		 WHERE ticket_id=$9 AND active=true`,
		req.Title, req.Description, req.Status, req.Priority,
		req.AssigneeID, req.WorkflowID, req.RunID, req.ForgeExecutionID, id,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update ticket: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to update ticket", http.StatusInternalServerError)
		return
	}

	wasTerminal := existing.Status == StatusResolved || existing.Status == StatusClosed
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "deleteTicket", "tickets/tickets/"+id)
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

	if _, err = db.Exec(ctx,
		`UPDATE tickets SET active=false, updated_at=now() WHERE ticket_id=$1 AND active=true`, id,
	); err != nil {
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

// ── DB helpers ────────────────────────────────────────────────────────────────

func getTicket(ctx context.Context, id string) (Ticket, error) {
	row := db.QueryRow(ctx,
		`SELECT ticket_id, title, description, status, priority, created_by, org_id,
		        assignee_id, workflow_id, run_id, forge_execution_id, created_at, updated_at
		 FROM tickets WHERE ticket_id=$1 AND active=true`, id,
	)
	return scanTicket(row)
}

func scanTicket(row pgx.Row) (Ticket, error) {
	var t Ticket
	err := row.Scan(
		&t.TicketID, &t.Title, &t.Description, &t.Status, &t.Priority,
		&t.CreatedBy, &t.OrgID,
		&t.AssigneeID, &t.WorkflowID, &t.RunID, &t.ForgeExecutionID,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return t, err
	}
	t.Comments = []TicketComment{}
	return t, nil
}

func scanTickets(rows pgx.Rows) ([]Ticket, error) {
	var tickets []Ticket
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(
			&t.TicketID, &t.Title, &t.Description, &t.Status, &t.Priority,
			&t.CreatedBy, &t.OrgID,
			&t.AssigneeID, &t.WorkflowID, &t.RunID, &t.ForgeExecutionID,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		t.Comments = []TicketComment{}
		tickets = append(tickets, t)
	}
	if tickets == nil {
		tickets = []Ticket{}
	}
	return tickets, rows.Err()
}

