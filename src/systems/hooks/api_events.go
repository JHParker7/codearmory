package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

func handleListEvents(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleListEvents")
	defer span.End()

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "listEvent", "hooks/events")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	repo := r.URL.Query().Get("repo")

	var events []HookEvent
	var result *gorm.DB
	if repo != "" {
		result = db.WithContext(ctx).Raw(
			`SELECT DISTINCT he.event_id, he.repo, he.event_type, he.ref, he.payload,
			        he.rules_matched, he.status, he.created_at
			 FROM hook_events he
			 JOIN hook_triggers ht ON ht.event_id = he.event_id
			 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
			 WHERE (pr.created_by = ? OR (pr.org_id != '' AND pr.org_id = ?))
			   AND he.repo = ?
			 ORDER BY he.created_at DESC LIMIT 100`,
			userID, orgID, repo,
		).Scan(&events)
	} else {
		result = db.WithContext(ctx).Raw(
			`SELECT DISTINCT he.event_id, he.repo, he.event_type, he.ref, he.payload,
			        he.rules_matched, he.status, he.created_at
			 FROM hook_events he
			 JOIN hook_triggers ht ON ht.event_id = he.event_id
			 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
			 WHERE pr.created_by = ? OR (pr.org_id != '' AND pr.org_id = ?)
			 ORDER BY he.created_at DESC LIMIT 100`,
			userID, orgID,
		).Scan(&events)
	}
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list events: db error", "user_id", userID, "error", result.Error)
		http.Error(w, "failed to list events", http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []HookEvent{}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events) //nolint:errcheck
}

func handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleGetEvent")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "getEvent", "hooks/events/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	// Fetch the event.
	var event HookEvent
	if result := db.WithContext(ctx).Where("event_id=?", id).First(&event); result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			http.Error(w, "event not found", http.StatusNotFound)
			return
		}
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get event: db error", "event_id", id, "user_id", userID, "error", result.Error)
		http.Error(w, "failed to get event", http.StatusInternalServerError)
		return
	}

	// Fetch associated triggers.
	var triggers []HookTrigger
	if result := db.WithContext(ctx).
		Where("event_id=?", id).
		Order("created_at").
		Find(&triggers); result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db query triggers failed")
		slog.Error("get event: query triggers", "event_id", id, "user_id", userID, "error", result.Error)
		http.Error(w, "failed to get event triggers", http.StatusInternalServerError)
		return
	}
	if triggers == nil {
		triggers = []HookTrigger{}
	}

	// Verify the caller can access at least one of the triggered rules, using a
	// single JOIN query instead of N per-trigger lookups.
	var accessCount int
	if result := db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM hook_triggers ht
		 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
		 WHERE ht.event_id = ?
		   AND (pr.created_by = ? OR (pr.org_id != '' AND pr.org_id = ?))`,
		id, userID, orgID,
	).Scan(&accessCount); result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db access check failed")
		slog.Error("get event: access check", "event_id", id, "user_id", userID, "error", result.Error)
		http.Error(w, "failed to get event", http.StatusInternalServerError)
		return
	}
	// Fallback for events that matched no rules (status='received'): check
	// whether the caller has any active rule for this repo.
	if accessCount == 0 {
		if result := db.WithContext(ctx).Raw(
			`SELECT COUNT(*) FROM pipeline_rules
			 WHERE repo = ? AND active = true
			   AND (created_by = ? OR (org_id != '' AND org_id = ?))`,
			event.Repo, userID, orgID,
		).Scan(&accessCount); result.Error != nil {
			slog.Warn("get event: repo fallback check failed", "event_id", id, "error", result.Error)
		}
	}
	if accessCount == 0 {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}

	event.Triggers = triggers
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}
