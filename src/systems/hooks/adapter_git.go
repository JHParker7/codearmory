package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// gitPayload is the JSON body expected on POST /hooks/git.
type gitPayload struct {
	Repo    string `json:"repo"`
	Event   string `json:"event"`
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Pusher  string `json:"pusher"`
	Message string `json:"message"`
}

// gitBaseInputs builds the adapter-specific workflow inputs that git rules
// expect by default. These are merged in before input_mapping overrides.
func gitBaseInputs(p gitPayload) map[string]string {
	return map[string]string{
		"HOOK_REPO":   p.Repo,
		"HOOK_REF":    p.Ref,
		"HOOK_COMMIT": p.Commit,
	}
}

func handleGitWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleGitWebhook")
	defer span.End()

	// Read the full body before we do anything else because we need it for HMAC verification.
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		span.SetStatus(codes.Error, "read body failed")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var payload gitPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		span.SetStatus(codes.Error, "invalid json")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// X-Hook-Event header overrides the event field in the body.
	if h := r.Header.Get("X-Hook-Event"); h != "" {
		payload.Event = h
	}

	if payload.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	if payload.Event == "" {
		http.Error(w, "event is required", http.StatusBadRequest)
		return
	}

	meterHooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("source", payload.Repo)))

	payloadMap := map[string]string{
		"repo":    payload.Repo,
		"event":   payload.Event,
		"ref":     payload.Ref,
		"commit":  payload.Commit,
		"pusher":  payload.Pusher,
		"message": payload.Message,
	}

	eventID := uuid.New().String()
	newEvent := HookEvent{
		EventID:   eventID,
		Source:    payload.Repo,
		EventType: payload.Event,
		Ref:       payload.Ref,
		Payload:   payloadMap,
		Status:    "received",
		CreatedAt: time.Now().UTC(),
	}
	if err := newEvent.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert event failed")
		slog.ErrorContext(ctx, "git webhook: insert event", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	trigResults, successCount, failCount := matchAndDispatch(
		ctx, eventID,
		payload.Repo, payload.Event, payload.Ref,
		payloadMap, gitBaseInputs(payload),
		rawBody, r.Header.Get("X-Hub-Signature-256"), false, "", "",
	)

	// Collect triggers for response.
	triggers := make([]HookTrigger, 0, len(trigResults))
	for _, tr := range trigResults {
		triggers = append(triggers, tr.Trigger)
	}

	// Derive overall event status.
	total := successCount + failCount
	eventStatus := "received"
	switch {
	case total == 0:
		eventStatus = "received"
	case failCount == 0:
		eventStatus = "triggered"
	case successCount == 0:
		eventStatus = "failed"
	default:
		eventStatus = "partial"
	}

	updateEventStatus(eventID, total, eventStatus)

	event := HookEvent{
		EventID:      eventID,
		Source:       payload.Repo,
		EventType:    payload.Event,
		Ref:          payload.Ref,
		Payload:      payloadMap,
		RulesMatched: total,
		Status:       eventStatus,
		Triggers:     triggers,
		CreatedAt:    time.Now().UTC(),
	}
	if event.Triggers == nil {
		event.Triggers = []HookTrigger{}
	}

	span.SetAttributes(
		attribute.String("event.id", eventID),
		attribute.String("source", payload.Repo),
		attribute.Int("rules.matched", total),
	)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}
