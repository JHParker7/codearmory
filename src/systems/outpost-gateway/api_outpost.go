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
)

// commandLongPollTimeout is how long GET /outpost/commands holds the connection
// open waiting for work before returning an empty batch.
const commandLongPollTimeout = 30 * time.Second
const commandPollInterval = 1 * time.Second

type registerRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
}

type registerResponse struct {
	OutpostID  string `json:"outpost_id"`
	OutpostKey string `json:"outpost_key"`
	Modules    string `json:"modules"`
}

// handleRegister enrolls an outpost: it validates the single-use enrollment
// token, mints a long-lived outpost key, and transitions the outpost to
// connected. The enrollment token is cleared so it cannot be reused.
func handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleRegister")
	defer span.End()

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	outpostID, secretPart, ok := splitEnrollmentToken(req.EnrollmentToken)
	if !ok {
		http.Error(w, "malformed enrollment token", http.StatusUnauthorized)
		return
	}
	o, err := getOutpost(ctx, outpostID)
	if err != nil || o.Status != OutpostPending || o.EnrollTokenHash == "" {
		http.Error(w, "invalid or already-used enrollment token", http.StatusUnauthorized)
		return
	}
	if !checkBcrypt(o.EnrollTokenHash, secretPart) {
		http.Error(w, "invalid enrollment token", http.StatusUnauthorized)
		return
	}

	key, keyHash, err := mintOutpostKey()
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to mint outpost key", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	o.KeyHash = keyHash
	o.EnrollTokenHash = "" // single-use
	o.Status = OutpostConnected
	o.LastSeenAt = &now
	if err := o.Save(ctx); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to enroll outpost", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(attribute.String("outpost.id", o.OutpostID))
	span.SetStatus(codes.Ok, "")
	slog.Info("outpost enrolled", "outpost_id", o.OutpostID, "modules", o.Modules)
	meterOutpostsEnrolled.Add(ctx, 1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(registerResponse{OutpostID: o.OutpostID, OutpostKey: key, Modules: o.Modules}) //nolint:errcheck
}

// handleCommands is the command long-poll. It claims pending commands for the
// authenticated outpost and returns them; if none are ready it holds the
// connection open up to commandLongPollTimeout, polling periodically.
func handleCommands(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleCommands")
	defer span.End()

	o, ok := authenticateOutpost(ctx, w, r)
	if !ok {
		return
	}
	_ = touchOutpost(ctx, o.OutpostID)

	deadline := time.Now().Add(commandLongPollTimeout)
	for {
		cmds, err := claimCommands(ctx, o.OutpostID, 16)
		if err != nil {
			span.RecordError(err)
			http.Error(w, "failed to claim commands", http.StatusInternalServerError)
			return
		}
		if len(cmds) > 0 {
			span.SetAttributes(attribute.Int("commands.count", len(cmds)))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(cmds) //nolint:errcheck
			return
		}
		if time.Now().After(deadline) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]OutpostCommand{}) //nolint:errcheck
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(commandPollInterval):
		}
	}
}

// handleAckCommand marks a delivered command done.
func handleAckCommand(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleAckCommand")
	defer span.End()

	o, ok := authenticateOutpost(ctx, w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := ackCommand(ctx, o.OutpostID, id); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to ack command", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type ingestEvent struct {
	EventID     string         `json:"event_id"`
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// handleEvents ingests an outpost→control event into the outbox. The outpost is
// authenticated by its key; the event is stamped with the outpost's org so the
// dispatcher and consumers can scope it.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleEvents")
	defer span.End()

	o, ok := authenticateOutpost(ctx, w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	var ev ingestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if ev.Integration == "" || ev.Type == "" {
		http.Error(w, "integration and type are required", http.StatusBadRequest)
		return
	}
	// An outpost may only post events for modules it actually has enabled, mirroring
	// the command path's outpostHasModule check (see api_internal.go). This stops a
	// chaos-only outpost from forging argo events.
	if !outpostHasModule(o, ev.Integration) {
		http.Error(w, "outpost does not have the "+ev.Integration+" module enabled", http.StatusForbidden)
		return
	}
	id := ev.EventID
	if id == "" {
		id = uuid.New().String()
	}
	if err := addEvent(ctx, OutpostEvent{
		ID:          id,
		OutpostID:   o.OutpostID,
		OrgID:       o.OrgID,
		UserID:      o.UserID,
		Integration: ev.Integration,
		Type:        ev.Type,
		Payload:     ev.Payload,
		Status:      EvPending,
		NextRetryAt: time.Now().UTC(),
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		// A duplicate id (at-least-once outpost retry) is success, not an error.
		if isDuplicateKey(err) {
			w.WriteHeader(http.StatusOK)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to enqueue event", http.StatusInternalServerError)
		return
	}
	meterEventsIngested.Add(ctx, 1)
	span.SetAttributes(attribute.String("event.integration", ev.Integration), attribute.String("event.type", ev.Type))
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusAccepted)
}

// handleHeartbeat records outpost liveness.
func handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("outpost-gateway").Start(r.Context(), "handleHeartbeat")
	defer span.End()

	o, ok := authenticateOutpost(ctx, w, r)
	if !ok {
		return
	}
	if err := touchOutpost(ctx, o.OutpostID); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to record heartbeat", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
