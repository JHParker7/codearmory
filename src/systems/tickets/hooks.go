package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Ticket lifecycle events emitted to the hooks service. Users route these to
// workflows by creating a hooks rule with source "ticketsHookSource".
const (
	ticketsHookSource  = "tickets"
	eventTicketCreated = "ticket.created"
	eventTicketUpdated = "ticket.updated"
	eventTicketStatus  = "ticket.status_changed"
	eventTicketDeleted = "ticket.deleted"
)

var (
	// hooksURL is the base URL of the hooks service. Empty disables the
	// integration entirely (it is optional).
	hooksURL = envOrDefault("HOOKS_URL", "")
	// hooksEventKey is the shared HMAC key (HOOKS_TRIGGER_KEY) used to
	// authenticate emitted events to the hooks /internal/events endpoint.
	hooksEventKey = secret("HOOKS_TRIGGER_KEY")
)

// hooksEnabled reports whether the optional hooks integration is configured.
func hooksEnabled() bool {
	return hooksURL != "" && hooksEventKey != ""
}

// ticketEventFields renders the ticket fields exposed to rule input_mapping as
// flat string values.
func ticketEventFields(t Ticket) map[string]string {
	fields := map[string]string{
		"ticket_id":  t.TicketID,
		"title":      t.Title,
		"status":     t.Status,
		"priority":   t.Priority,
		"timescale":  t.Timescale,
		"created_by": t.CreatedBy,
		"org_id":     t.OrgID,
	}
	if t.AssigneeID != nil {
		fields["assignee_id"] = *t.AssigneeID
	}
	if t.WorkflowID != nil {
		fields["workflow_id"] = *t.WorkflowID
	}
	if t.RunID != nil {
		fields["run_id"] = *t.RunID
	}
	return fields
}

// notifyHooks emits a ticket lifecycle event to the hooks service. It is
// best-effort and asynchronous: a no-op when the integration is unconfigured,
// and failures never affect the originating ticket request. ref carries an
// optional discriminator (the ticket status) for rule ref_filter matching, and
// extra augments the event payload (e.g. the previous status on a transition).
func notifyHooks(ctx context.Context, event, ref string, t Ticket, extra map[string]string) {
	if !hooksEnabled() {
		return
	}

	payload := ticketEventFields(t)
	maps.Copy(payload, extra)

	body := map[string]any{
		"source":     ticketsHookSource,
		"event":      event,
		"ref":        ref,
		"org_id":     t.OrgID,
		"created_by": t.CreatedBy,
		"payload":    payload,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		slog.Warn("notify hooks: marshal failed", "ticket_id", t.TicketID, "event", event, "error", err)
		return
	}

	// Detach from the request context so the emit survives the response, but
	// carry the trace for correlation.
	emitCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	go sendHookEvent(emitCtx, event, raw, t.TicketID)
}

// sendHookEvent signs and POSTs an event body to the hooks internal endpoint.
func sendHookEvent(ctx context.Context, event string, raw []byte, ticketID string) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "notifyHooks")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	token, ts := signHookEvent(ticketsHookSource, event)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hooksURL+"/internal/events", bytes.NewReader(raw))
	if err != nil {
		span.RecordError(err)
		slog.Warn("notify hooks: build request failed", "ticket_id", ticketID, "event", event, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hooks-Token", token)
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dispatch failed")
		slog.Warn("notify hooks: request failed", "ticket_id", ticketID, "event", event, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetStatus(codes.Error, "non-2xx from hooks")
		slog.Warn("notify hooks: unexpected status", "ticket_id", ticketID, "event", event, "status", resp.StatusCode)
		return
	}
	span.SetStatus(codes.Ok, "")
}

// signHookEvent produces the HMAC-SHA256 token the hooks service verifies over
// "event:{source}:{event}:{timestamp}".
func signHookEvent(source, event string) (token, timestamp string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hooksEventKey))
	fmt.Fprintf(mac, "event:%s:%s:%s", source, event, ts)
	return hex.EncodeToString(mac.Sum(nil)), ts
}
