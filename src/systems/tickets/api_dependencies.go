package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// Dependencies: "this ticket cannot be worked until that one is finished".
//
// Ordering, not hierarchy. ParentID already expresses containment — a sub-ticket
// is part of its parent — and overloading it with ordering would mean work could
// not be broken down without implying a sequence, nor sequenced without implying
// containment. They are separate relations because they answer separate
// questions.
//
// The service stores and validates the edges; it does NOT decide whether a
// dependency is satisfied. Boards define their own status columns, so only the
// caller knows which of its columns means done — see TicketDependencyView.
//
// Both directions of the edge are authorised as a change to the DEPENDENT
// ticket, using the existing updateTicket action: adding a blocker changes when
// that ticket may be worked, which is a property of it. The blocker itself is
// only read, and must merely be accessible.

// maxDependencyNodes bounds the cycle walk. A dependency graph is a graph, not
// the tree ParentID walks, so the bound is on nodes visited rather than depth —
// and it protects against a cycle that already exists in the data as much as
// against one being created.
const maxDependencyNodes = 256

type dependencyRequest struct {
	DependsOn string `json:"depends_on"`
}

func handleAddDependency(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleAddDependency")
	defer span.End()

	id := r.PathValue("id")
	ns := r.PathValue("ns")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateTicket", ticketResource(ns, id))
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req dependencyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	t, ok := loadAccessibleTicket(ctx, w, span, id, ns, userID, orgID, "add dependency")
	if !ok {
		return
	}

	if req.DependsOn == "" || req.DependsOn == t.TicketID {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "a ticket cannot depend on itself", http.StatusBadRequest)
		return
	}

	blocker, err := getTicket(ctx, req.DependsOn)
	if err != nil || !canAccessTicket(blocker, userID, orgID) {
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db error")
			slog.ErrorContext(ctx, "add dependency: db error", "ticket_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to add dependency", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Ok, "")
		http.Error(w, "dependency ticket not found or not accessible", http.StatusBadRequest)
		return
	}

	cyclic, err := wouldCycle(ctx, t.TicketID, blocker.TicketID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "add dependency: cycle check failed", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to add dependency", http.StatusInternalServerError)
		return
	}
	if cyclic {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "that dependency would create a cycle", http.StatusBadRequest)
		return
	}

	dep := TicketDependency{
		TicketID:    t.TicketID,
		DependsOnID: blocker.TicketID,
		CreatedBy:   userID,
		CreatedAt:   time.Now().UTC(),
	}
	if err := dep.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "add dependency: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to add dependency", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(
		attribute.String("ticket.id", t.TicketID),
		attribute.String("depends_on.id", blocker.TicketID),
	)

	writeDependencies(ctx, w, span, t, userID, orgID, "add dependency")
}

func handleRemoveDependency(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleRemoveDependency")
	defer span.End()

	id := r.PathValue("id")
	ns := r.PathValue("ns")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateTicket", ticketResource(ns, id))
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	t, ok := loadAccessibleTicket(ctx, w, span, id, ns, userID, orgID, "remove dependency")
	if !ok {
		return
	}

	// Removing an edge that is not there succeeds. The caller asked for a state
	// ("this no longer waits for that") which is already true, and a 404 would
	// make every client read before writing to avoid a spurious error.
	dep := TicketDependency{TicketID: t.TicketID, DependsOnID: r.PathValue("depends_on_id")}
	if err := dep.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "remove dependency: db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to remove dependency", http.StatusInternalServerError)
		return
	}

	writeDependencies(ctx, w, span, t, userID, orgID, "remove dependency")
}

// loadAccessibleTicket fetches a ticket and enforces that the namespace in the
// URL is the record's own — a caller naming their OWN namespace against someone
// else's id would otherwise be authorised for it. A denial is reported as "not
// found" so existence does not leak.
func loadAccessibleTicket(ctx context.Context, w http.ResponseWriter, span trace.Span, id, ns, userID, orgID, op string) (Ticket, bool) {
	t, err := getTicket(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "ticket not found", http.StatusNotFound)
			return Ticket{}, false
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, op+": db error", "ticket_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to load ticket", http.StatusInternalServerError)
		return Ticket{}, false
	}
	if !namespaceMatches(t.Namespace, ns) || !canAccessTicket(t, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "ticket not found", http.StatusNotFound)
		return Ticket{}, false
	}
	return t, true
}

// writeDependencies responds with the ticket's dependency list as it now stands,
// so a caller never has to re-read to find out what it just did.
func writeDependencies(ctx context.Context, w http.ResponseWriter, span trace.Span, t Ticket, userID, orgID, op string) {
	list := []Ticket{t}
	if err := loadDependencies(ctx, list, userID, orgID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, op+": load failed", "ticket_id", t.TicketID, "user_id", userID, "error", err)
		http.Error(w, "failed to load dependencies", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"ticket_id":  t.TicketID,
		"depends_on": list[0].DependsOn,
	})
}

// wouldCycle reports whether making ticketID depend on blockerID closes a loop —
// that is, whether blockerID already depends, directly or transitively, on
// ticketID.
//
// A cycle is not a cosmetic problem here: every ticket in it waits for another
// ticket in it, so the whole set is unworkable forever, and an agent runtime
// polling for ready work would simply never see them again with nothing to say
// why.
func wouldCycle(ctx context.Context, ticketID, blockerID string) (bool, error) {
	seen := map[string]bool{blockerID: true}
	queue := []string{blockerID}
	for len(queue) > 0 && len(seen) <= maxDependencyNodes {
		cur := queue[0]
		queue = queue[1:]
		next, err := dependencyIDs(ctx, cur)
		if err != nil {
			return false, err
		}
		for _, id := range next {
			if id == ticketID {
				return true, nil
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	return false, nil
}
