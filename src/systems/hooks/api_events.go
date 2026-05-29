package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
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

	var (
		rows pgx.Rows
		err  error
	)
	if repo != "" {
		rows, err = db.Query(ctx,
			`SELECT DISTINCT he.event_id, he.repo, he.event_type, he.ref, he.payload,
			        he.rules_matched, he.status, he.created_at
			 FROM hook_events he
			 JOIN hook_triggers ht ON ht.event_id = he.event_id
			 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
			 WHERE (pr.created_by = $1 OR (pr.org_id != '' AND pr.org_id = $2))
			   AND he.repo = $3
			 ORDER BY he.created_at DESC LIMIT 100`,
			userID, orgID, repo,
		)
	} else {
		rows, err = db.Query(ctx,
			`SELECT DISTINCT he.event_id, he.repo, he.event_type, he.ref, he.payload,
			        he.rules_matched, he.status, he.created_at
			 FROM hook_events he
			 JOIN hook_triggers ht ON ht.event_id = he.event_id
			 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
			 WHERE pr.created_by = $1 OR (pr.org_id != '' AND pr.org_id = $2)
			 ORDER BY he.created_at DESC LIMIT 100`,
			userID, orgID,
		)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list events: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list events", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	events, err := scanEvents(rows)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scan failed")
		slog.Error("list events: scan error", "user_id", userID, "error", err)
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "getEvent", "hooks/events/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	// Fetch the event.
	row := db.QueryRow(ctx,
		`SELECT event_id, repo, event_type, ref, payload, rules_matched, status, created_at
		 FROM hook_events WHERE event_id=$1`, id,
	)
	event, err := scanEvent(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "event not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get event: db error", "event_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get event", http.StatusInternalServerError)
		return
	}

	// Fetch associated triggers so we can authorise the caller.
	trigRows, err := db.Query(ctx,
		`SELECT ht.trigger_id, ht.event_id, ht.rule_id, ht.workflow_id, ht.run_id,
		        ht.status, ht.error, ht.created_at
		 FROM hook_triggers ht
		 WHERE ht.event_id=$1
		 ORDER BY ht.created_at`, id,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query triggers failed")
		slog.Error("get event: query triggers", "event_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get event triggers", http.StatusInternalServerError)
		return
	}
	defer trigRows.Close()

	triggers, err := scanTriggers(trigRows)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scan triggers failed")
		slog.Error("get event: scan triggers", "event_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get event triggers", http.StatusInternalServerError)
		return
	}

	// Verify the caller can access at least one of the triggered rules.
	accessible := false
	for _, trig := range triggers {
		rule, err := getRule(ctx, trig.RuleID)
		if err != nil {
			continue
		}
		if canAccessRule(rule, userID, orgID) {
			accessible = true
			break
		}
	}
	if !accessible {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}

	event.Triggers = triggers
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}

func scanEvent(row pgx.Row) (HookEvent, error) {
	var ev HookEvent
	var payloadJSON []byte
	err := row.Scan(&ev.EventID, &ev.Repo, &ev.EventType, &ev.Ref, &payloadJSON,
		&ev.RulesMatched, &ev.Status, &ev.CreatedAt)
	if err != nil {
		return ev, err
	}
	if err := json.Unmarshal(payloadJSON, &ev.Payload); err != nil {
		return ev, err
	}
	return ev, nil
}

func scanEvents(rows pgx.Rows) ([]HookEvent, error) {
	var events []HookEvent
	for rows.Next() {
		var ev HookEvent
		var payloadJSON []byte
		if err := rows.Scan(&ev.EventID, &ev.Repo, &ev.EventType, &ev.Ref, &payloadJSON,
			&ev.RulesMatched, &ev.Status, &ev.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payloadJSON, &ev.Payload); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if events == nil {
		events = []HookEvent{}
	}
	return events, rows.Err()
}

func scanTriggers(rows pgx.Rows) ([]HookTrigger, error) {
	var triggers []HookTrigger
	for rows.Next() {
		var t HookTrigger
		if err := rows.Scan(&t.TriggerID, &t.EventID, &t.RuleID, &t.WorkflowID,
			&t.RunID, &t.Status, &t.Error, &t.CreatedAt); err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	if triggers == nil {
		triggers = []HookTrigger{}
	}
	return triggers, rows.Err()
}
