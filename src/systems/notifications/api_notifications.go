package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// writeServerError logs nothing extra (callers record spans) and returns a
// generic 500 so handler error paths stay terse.
func writeServerError(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusInternalServerError)
}

const (
	listDefaultLimit = 100
	listMaxLimit     = 500
)

// pagination parses ?limit= and ?offset= with sane defaults and clamping, so list
// endpoints never silently truncate the result with no way to reach the rest.
func pagination(r *http.Request) (limit, offset int) {
	limit = listDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	limit = min(limit, listMaxLimit)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	return limit, offset
}

type notifyRequest struct {
	// ChannelIDs selects explicit target channels. When empty, the message is
	// sent to every enabled channel visible to the caller.
	ChannelIDs []string `json:"channel_ids"`
	Subject    string   `json:"subject"`
	Body       string   `json:"body"`
}

// handleNotify enqueues a message for delivery to one or more channels. Each
// target becomes a durable Notification row in StatusPending; the worker is then
// nudged to deliver immediately. The created records are returned so the caller
// can poll their status.
func handleNotify(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleNotify")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createNotification", "notifications/notifications")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	var req notifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" {
		http.Error(w, "body is required", http.StatusBadRequest)
		return
	}

	targets, err := resolveTargets(ctx, userID, orgID, req.ChannelIDs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		writeServerError(w, "failed to resolve channels")
		return
	}
	if len(targets) == 0 {
		http.Error(w, "no matching enabled channels", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	created := make([]Notification, 0, len(targets))
	for _, c := range targets {
		n := Notification{
			NotificationID: uuid.New().String(),
			ChannelID:      c.ChannelID,
			ChannelType:    c.Type,
			Subject:        req.Subject,
			Body:           req.Body,
			Status:         StatusPending,
			NextAttemptAt:  now, // due immediately
			CreatedBy:      userID,
			OrgID:          c.OrgID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := n.Add(ctx); err != nil {
			span.RecordError(err)
			slog.ErrorContext(ctx, "notify: enqueue failed", "channel_id", c.ChannelID, "error", err)
			continue
		}
		created = append(created, n)
	}
	if len(created) == 0 {
		span.SetStatus(codes.Error, "all enqueues failed")
		writeServerError(w, "failed to enqueue notifications")
		return
	}

	meterEnqueued.Add(ctx, int64(len(created)), metric.WithAttributes(attribute.Int("targets", len(created))))
	nudgeWorker()
	span.SetAttributes(attribute.Int("enqueued", len(created)))
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(created) //nolint:errcheck
}

// resolveTargets returns the enabled channels a notify request targets: the
// explicit channel_ids the caller may access, or all of their enabled channels
// when none are given. Unknown or inaccessible IDs are silently skipped.
func resolveTargets(ctx context.Context, userID, orgID string, ids []string) ([]Channel, error) {
	// Fan-out targets every enabled channel the caller can see, capped at the list
	// maximum so an unbounded query can't be triggered by a single notify.
	all, err := listChannels(ctx, userID, orgID, true, listMaxLimit, 0)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return all, nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []Channel
	for _, c := range all {
		if want[c.ChannelID] {
			out = append(out, c)
		}
	}
	return out, nil
}

func handleListNotifications(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleListNotifications")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listNotification", "notifications/notifications")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	statusFilter := r.URL.Query().Get("status")
	if statusFilter != "" && statusFilter != StatusPending && statusFilter != StatusSent && statusFilter != StatusFailed {
		http.Error(w, "invalid status filter", http.StatusBadRequest)
		return
	}

	limit, offset := pagination(r)
	notifications, err := listNotifications(ctx, userID, orgID, statusFilter, limit, offset)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		writeServerError(w, "failed to list notifications")
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(notifications) //nolint:errcheck
}

func handleGetNotification(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("notifications").Start(r.Context(), "handleGetNotification")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getNotification", "notifications/notifications/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	n, err := getNotification(ctx, id)
	if err != nil {
		if isNotFound(err) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "notification not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		writeServerError(w, "failed to get notification")
		return
	}
	if n.CreatedBy != userID && !(orgID != "" && n.OrgID == orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n) //nolint:errcheck
}
