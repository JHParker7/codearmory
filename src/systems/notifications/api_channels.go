package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// canAccessChannel returns true when the caller owns the channel or shares its org.
func canAccessChannel(c Channel, userID, orgID string) bool {
	return c.CreatedBy == userID || (orgID != "" && c.OrgID == orgID)
}

// redactedChannel returns a copy of c with secret config values masked, safe for
// API responses.
func redactedChannel(c Channel) Channel {
	c.Config = redactConfig(c.Type, c.Config)
	return c
}

type channelRequest struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Config  map[string]string `json:"config"`
	Enabled *bool             `json:"enabled"`
}

func handleListProviders(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleListProviders")
	defer span.End()

	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listProvider", "notifications/providers"); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providerInfos()) //nolint:errcheck
}

func handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleCreateChannel")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createChannel", "notifications/channels")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	var req channelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if _, known := getNotifier(req.Type); !known {
		http.Error(w, "unknown channel type", http.StatusBadRequest)
		return
	}
	if req.Config == nil {
		req.Config = map[string]string{}
	}
	if err := validateConfig(req.Type, req.Config); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	now := time.Now().UTC()
	c := Channel{
		ChannelID: uuid.New().String(),
		Name:      req.Name,
		Type:      req.Type,
		Config:    req.Config,
		Enabled:   enabled,
		CreatedBy: userID,
		OrgID:     orgID,
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		writeServerError(w, "failed to create channel")
		return
	}

	meterChannelsCreated.Add(ctx, 1, metric.WithAttributes(attribute.String("type", c.Type)))
	span.SetAttributes(attribute.String("channel.id", c.ChannelID))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(redactedChannel(c)) //nolint:errcheck
}

func handleListChannels(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleListChannels")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listChannel", "notifications/channels")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	channels, err := listChannels(ctx, userID, orgID, r.URL.Query().Get("enabled") == "true")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		writeServerError(w, "failed to list channels")
		return
	}
	out := make([]Channel, len(channels))
	for i, c := range channels {
		out[i] = redactedChannel(c)
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out) //nolint:errcheck
}

func handleGetChannel(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleGetChannel")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getChannel", "notifications/channels/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	c, err := getChannel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		writeServerError(w, "failed to get channel")
		return
	}
	if !canAccessChannel(c, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(redactedChannel(c)) //nolint:errcheck
}

func handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleUpdateChannel")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateChannel", "notifications/channels/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	existing, err := getChannel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		writeServerError(w, "failed to get channel")
		return
	}
	if !canAccessChannel(existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	var req channelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		existing.Name = name
	}
	// Type is immutable — changing it would invalidate the stored config schema.
	if req.Config != nil {
		merged := mergeConfig(existing.Type, existing.Config, req.Config)
		if err := validateConfig(existing.Type, merged); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		existing.Config = merged
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		writeServerError(w, "failed to update channel")
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(redactedChannel(existing)) //nolint:errcheck
}

func handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleDeleteChannel")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteChannel", "notifications/channels/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	c, err := getChannel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		writeServerError(w, "failed to delete channel")
		return
	}
	if !canAccessChannel(c, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err := c.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		writeServerError(w, "failed to delete channel")
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

type testChannelRequest struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// handleTestChannel delivers a one-off message synchronously so the caller gets
// immediate success/failure feedback. It records a terminal audit row that the
// retry worker will not pick up.
func handleTestChannel(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleTestChannel")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "testChannel", "notifications/channels/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	c, err := getChannel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		writeServerError(w, "failed to get channel")
		return
	}
	if !canAccessChannel(c, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	var req testChannelRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body is optional
	msg := Message{Subject: req.Subject, Body: req.Body}
	if msg.Subject == "" {
		msg.Subject = "Test notification"
	}
	if msg.Body == "" {
		msg.Body = "This is a test notification from CodeArmory."
	}

	now := time.Now().UTC()
	n := Notification{
		NotificationID: uuid.New().String(),
		ChannelID:      c.ChannelID,
		ChannelType:    c.Type,
		Subject:        msg.Subject,
		Body:           msg.Body,
		Attempts:       1,
		CreatedBy:      userID,
		OrgID:          c.OrgID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	sendErr := deliverNow(ctx, c, msg)
	if sendErr != nil {
		n.Status = StatusFailed
		n.LastError = sendErr.Error()
	} else {
		n.Status = StatusSent
		n.SentAt = &now
		meterSent.Add(ctx, 1, metric.WithAttributes(attribute.String("type", c.Type)))
	}
	if err := n.Add(ctx); err != nil {
		slog.WarnContext(ctx, "test channel: failed to record audit row", "channel_id", c.ChannelID, "error", err)
	}

	if sendErr != nil {
		span.SetStatus(codes.Error, "delivery failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"status": StatusFailed, "error": sendErr.Error()}) //nolint:errcheck
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n) //nolint:errcheck
}
