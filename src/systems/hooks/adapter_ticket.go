package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// ticketEventPayload is the JSON body expected on POST /internal/events. It is
// produced by trusted internal services (currently the tickets service) that
// emit lifecycle events users can route to workflows via pipeline rules.
//
// Source is a shared constant ("tickets") rather than a globally-unique repo,
// so OrgID/CreatedBy scope matching to the owning tenant. Ref carries an
// optional discriminator (e.g. the new ticket status) that rules can target
// with ref_filter.
type ticketEventPayload struct {
	Source    string            `json:"source"`
	Event     string            `json:"event"`
	Ref       string            `json:"ref"`
	OrgID     string            `json:"org_id"`
	CreatedBy string            `json:"created_by"`
	Payload   map[string]string `json:"payload"`
}

// verifyInternalEvent validates the HMAC-SHA256 token an internal emitter signs
// over "event:{source}:{event}:{org_id}:{created_by}:{timestamp}" with the shared
// hooks trigger key. org_id/created_by are part of the signed message so the
// tenant scope (which rules fire) cannot be swapped on replay within the window.
// Returns false if the key is unconfigured, the token is malformed, or the
// 30-second window has elapsed.
func verifyInternalEvent(source, event, orgID, createdBy, token, timestamp string) bool {
	if hooksTriggerKey == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || math.Abs(float64(time.Now().Unix()-ts)) > 30 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s", source, event, orgID, createdBy, timestamp)
	return hmac.Equal([]byte(token), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// handleInternalEvent ingests an authenticated event from a trusted internal
// service and dispatches matching pipeline rules. The shared-key HMAC stands in
// for the per-rule webhook signature, so dispatch runs with skipHMAC=true and
// matching is confined to the emitting tenant.
func handleInternalEvent(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleInternalEvent")
	defer span.End()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		span.SetStatus(codes.Error, "read body failed")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var payload ticketEventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		span.SetStatus(codes.Error, "invalid json")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if !verifyInternalEvent(payload.Source, payload.Event, payload.OrgID, payload.CreatedBy, r.Header.Get("X-Hooks-Token"), r.Header.Get("X-Hooks-Timestamp")) {
		span.SetStatus(codes.Error, "invalid hooks token")
		slog.WarnContext(ctx, "internal event: invalid hooks token", "source", payload.Source, "event", payload.Event)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if payload.Source == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}
	if payload.Event == "" {
		http.Error(w, "event is required", http.StatusBadRequest)
		return
	}
	if payload.OrgID == "" && payload.CreatedBy == "" {
		http.Error(w, "org_id or created_by is required", http.StatusBadRequest)
		return
	}
	if payload.Payload == nil {
		payload.Payload = map[string]string{}
	}

	meterHooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("source", payload.Source)))

	eventID := uuid.New().String()
	newEvent := HookEvent{
		EventID:   eventID,
		Source:    payload.Source,
		EventType: payload.Event,
		Ref:       payload.Ref,
		Payload:   payload.Payload,
		Status:    "received",
		CreatedAt: time.Now().UTC(),
	}
	if err := newEvent.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert event failed")
		slog.ErrorContext(ctx, "internal event: insert event", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Trusted caller (shared-key HMAC verified) → skip per-rule HMAC; confine
	// matching to the emitting org (or owning user for personal events).
	trigResults, successCount, failCount := matchAndDispatch(
		ctx, eventID,
		payload.Source, payload.Event, payload.Ref,
		payload.Payload, nil,
		nil, "", true,
		payload.OrgID, payload.CreatedBy,
	)

	triggers := make([]HookTrigger, 0, len(trigResults))
	for _, tr := range trigResults {
		triggers = append(triggers, tr.Trigger)
	}

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
		Source:       payload.Source,
		EventType:    payload.Event,
		Ref:          payload.Ref,
		Payload:      payload.Payload,
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
		attribute.String("source", payload.Source),
		attribute.String("event.type", payload.Event),
		attribute.Int("rules.matched", total),
	)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}
