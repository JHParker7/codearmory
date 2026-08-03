package main

import (
	"context"
	"log/slog"
	"maps"

	sdkevents "github.com/code-armory-app/codearmory_sdk/events"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Ticket lifecycle events emitted to the events service. Users react to these by creating a
// trigger whose filter matches the event type — e.g. `type eq ticket.status_changed` plus
// `data.status eq done`.
const (
	ticketsEventSource = "tickets"
	eventTicketCreated = "ticket.created"
	eventTicketUpdated = "ticket.updated"
	eventTicketStatus  = "ticket.status_changed"
	eventTicketDeleted = "ticket.deleted"
)

var (
	// eventsURL is the base URL of the events service. Empty disables the integration
	// entirely (it is optional).
	eventsURL = envOrDefault("EVENTS_URL", "")
	// eventsKey is the shared HMAC key (EVENTS_TRIGGER_KEY) authenticating emitted events.
	eventsKey = secret("EVENTS_TRIGGER_KEY")

	eventEmitter *sdkevents.Emitter
)

// initEventEmitter builds the emitter once httpClient exists. Called from main.
func initEventEmitter() {
	eventEmitter = sdkevents.New(eventsURL, eventsKey, ticketsEventSource, httpClient)
}

// eventsEnabled reports whether the optional events integration is configured.
func eventsEnabled() bool { return eventEmitter.Enabled() }

// ticketEventFields renders the ticket fields a trigger filter can reach by dotted path
// (data.status, data.priority, …).
func ticketEventFields(t Ticket) map[string]any {
	fields := map[string]any{
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

// notifyEvents emits a ticket lifecycle event. It is best-effort and asynchronous: a no-op
// when the integration is unconfigured, and failures never affect the originating ticket
// request. extra augments the payload (e.g. the previous status on a transition).
//
// The former hooks contract carried a separate `ref` discriminator for rule ref_filter
// matching; that is gone because a filter now addresses any field directly — the status a
// caller used to pass as `ref` is already on the payload as data.status.
func notifyEvents(ctx context.Context, eventType string, t Ticket, extra map[string]any) {
	if !eventsEnabled() {
		return
	}
	data := ticketEventFields(t)
	maps.Copy(data, extra)

	ev := sdkevents.Event{
		Type:    eventType,
		Source:  ticketsEventSource,
		Subject: t.TicketID,
		Actor:   sdkevents.Actor{OrgID: t.OrgID, UserID: t.CreatedBy},
		Data:    data,
	}

	// Detach from the request context so the emit survives the response, but carry the trace
	// for correlation.
	emitCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	go emitEvent(emitCtx, ev, t.TicketID)
}

func emitEvent(ctx context.Context, ev sdkevents.Event, ticketID string) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "notifyEvents")
	defer span.End()

	if err := eventEmitter.Emit(ctx, ev); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "emit failed")
		slog.WarnContext(ctx, "notify events: emit failed", "ticket_id", ticketID, "event", ev.Type, "error", err)
		return
	}
	span.SetStatus(codes.Ok, "")
}
