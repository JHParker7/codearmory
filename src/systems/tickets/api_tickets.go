package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
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
		Active:           true,
		Comments:         []TicketComment{},
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	if err := db.WithContext(ctx).Create(&t).Error; err != nil {
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

	query := db.WithContext(ctx).
		Where("active = ? AND (created_by = ? OR (org_id != '' AND org_id = ?))", true, userID, orgID)

	if statusFilter != "" {
		query = query.Where("status = ?", statusFilter)
	}
	if priorityFilter != "" {
		query = query.Where("priority = ?", priorityFilter)
	}
	if assigneeFilter != "" {
		query = query.Where("assignee_id = ?", assigneeFilter)
	}

	var tickets []Ticket
	if err := query.Order("created_at DESC").Limit(100).Find(&tickets).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list tickets: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list tickets", http.StatusInternalServerError)
		return
	}

	for i := range tickets {
		tickets[i].Comments = []TicketComment{}
	}
	if tickets == nil {
		tickets = []Ticket{}
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "updateTicket", "tickets/tickets/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

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

	if err := db.WithContext(ctx).Model(&Ticket{}).Where("ticket_id=? AND active=?", id, true).Updates(map[string]any{
		"title":              req.Title,
		"description":        req.Description,
		"status":             req.Status,
		"priority":           req.Priority,
		"assignee_id":        req.AssigneeID,
		"workflow_id":        req.WorkflowID,
		"run_id":             req.RunID,
		"forge_execution_id": req.ForgeExecutionID,
		"updated_at":         time.Now().UTC(),
	}).Error; err != nil {
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

	if err := db.WithContext(ctx).Model(&Ticket{}).Where("ticket_id=? AND active=?", id, true).Update("active", false).Error; err != nil {
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
	var t Ticket
	if err := db.WithContext(ctx).Where("ticket_id=? AND active=?", id, true).First(&t).Error; err != nil {
		return t, err
	}
	t.Comments = []TicketComment{}
	return t, nil
}
