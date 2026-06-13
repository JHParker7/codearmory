package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

type internalEvent struct {
	EventID     string         `json:"event_id"`
	OutpostID   string         `json:"outpost_id"`
	OrgID       string         `json:"org_id"`
	UserID      string         `json:"user_id"`
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// handleInternalEvent consumes argo events relayed by the gateway dispatcher.
// app-state updates the App and advances any in-flight sync; sync-started flips
// the matching sync to running. At-least-once; updates are idempotent.
func handleInternalEvent(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("argo").Start(r.Context(), "handleInternalEvent")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	// Authenticate over the raw bytes before parsing — don't let an unauthenticated
	// caller probe JSON validity or make us parse pre-auth.
	if !verifyInternal("event", body,
		r.Header.Get("X-Internal-Token"), r.Header.Get("X-Internal-Timestamp")) {
		span.SetStatus(codes.Error, "invalid internal token")
		slog.Warn("internal event: invalid token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev internalEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Defense-in-depth: the shared internal key also authenticates other consumers,
	// so never act on an event routed here for a different integration.
	if ev.Integration != "" && ev.Integration != "argo" {
		slog.Warn("internal event: ignoring non-argo integration", "integration", ev.Integration)
		w.WriteHeader(http.StatusOK)
		return
	}
	span.SetAttributes(attribute.String("event.type", ev.Type))

	switch ev.Type {
	case "sync-started":
		syncID, _ := ev.Payload["sync_id"].(string)
		if syncID != "" {
			if err := markSyncRunning(ctx, syncID); err != nil {
				span.RecordError(err)
			}
		}
	case "app-state":
		name, _ := ev.Payload["app_name"].(string)
		if name == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		syncStatus, _ := ev.Payload["sync_status"].(string)
		healthStatus, _ := ev.Payload["health_status"].(string)
		revision, _ := ev.Payload["revision"].(string)
		opPhase, _ := ev.Payload["operation_phase"].(string)

		if err := upsertAppState(ctx, ev.OrgID, ev.UserID, ev.OutpostID, name, syncStatus, healthStatus, revision, opPhase); err != nil {
			span.RecordError(err)
			http.Error(w, "failed to update app", http.StatusInternalServerError)
			return
		}
		if s, changed, err := updateInFlightSync(ctx, ev.OutpostID, name, syncStatus, healthStatus, opPhase, syncStatusMessage(syncStatus, healthStatus, opPhase)); err != nil {
			span.RecordError(err)
		} else if changed {
			slog.Info("sync advanced", "sync_id", s.SyncID, "status", s.Status, "app", name)
			if s.Status == SyncSynced || s.Status == SyncFailed {
				meterSyncsResolved.Add(ctx, 1, metric.WithAttributes(attribute.String("status", s.Status)))
			}
		}
	default:
		slog.Debug("internal event: ignoring unknown type", "type", ev.Type)
	}

	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusOK)
}

func syncStatusMessage(syncStatus, healthStatus, opPhase string) string {
	if opPhase == "Failed" || opPhase == "Error" {
		return "operation " + opPhase
	}
	return "sync=" + syncStatus + " health=" + healthStatus
}
