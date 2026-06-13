package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

func handleListEvents(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleListEvents")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listEvent", "hooks/events")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	events, err := listEvents(ctx, userID, orgID, r.URL.Query().Get("source"))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list events: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list events", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events) //nolint:errcheck
}

func handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleGetEvent")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getEvent", "hooks/events/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	event, err := getEvent(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "event not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "get event: db error", "event_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get event", http.StatusInternalServerError)
		return
	}

	triggers, err := getTriggersForEvent(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query triggers failed")
		slog.ErrorContext(ctx, "get event: query triggers", "event_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get event triggers", http.StatusInternalServerError)
		return
	}

	// Verify the caller can access at least one of the triggered rules, using a
	// single JOIN query instead of N per-trigger lookups.
	accessCount, err := countEventAccess(ctx, id, userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db access check failed")
		slog.ErrorContext(ctx, "get event: access check", "event_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get event", http.StatusInternalServerError)
		return
	}
	// Fallback for events that matched no rules (status='received'): check
	// whether the caller has any active rule for this repo.
	if accessCount == 0 {
		accessCount, err = countRulesForRepo(ctx, event.Source, userID, orgID)
		if err != nil {
			slog.WarnContext(ctx, "get event: repo fallback check failed", "event_id", id, "error", err)
		}
	}
	if accessCount == 0 {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}

	event.Triggers = triggers
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}
