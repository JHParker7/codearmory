package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// sessionResponse is the safe public shape of a Session — JWT and PubKey are
// deliberately omitted so the token material is never returned via the API.
type sessionResponse struct {
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

func toSessionResponse(s Session) sessionResponse {
	return sessionResponse{
		SessionID: s.SessionID,
		UserID:    s.UserID,
		ExpiresAt: s.ExpiresAt,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		Active:    s.Active,
	}
}

func handleGetSession(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleGetSession")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("session.id", id),
	)
	slog.Info("get session request", "caller_id", callerID, "session_id", id)

	if !requirePermission(w, r, "getSession", "gatekeeper/sessions/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Session{SessionID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "session not found")
		slog.Warn("get session: not found", "caller_id", callerID, "session_id", id)
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s := row.(Session)
	span.AddEvent("db.read", trace.WithAttributes(
		attribute.String("session.id", id),
		attribute.String("session.user_id", s.UserID),
		attribute.String("session.expires_at", s.ExpiresAt.String()),
	))
	span.SetStatus(codes.Ok, "")
	slog.Info("get session: success", "caller_id", callerID, "session_id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toSessionResponse(s))
}

func handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("gatekeeper").Start(r.Context(), "handleDeleteSession")
	defer span.End()
	r = r.WithContext(ctx)

	id := r.PathValue("id")
	callerID, _ := ctx.Value(userIDKey).(string)
	span.SetAttributes(
		attribute.String("caller.id", callerID),
		attribute.String("session.id", id),
	)
	slog.Info("delete session request", "caller_id", callerID, "session_id", id)

	if !requirePermission(w, r, "deleteSession", "gatekeeper/sessions/"+id) {
		span.SetStatus(codes.Error, "forbidden")
		return
	}
	span.AddEvent("permission.granted")

	row, err := (Session{SessionID: id}).Get(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "session not found")
		slog.Warn("delete session: not found", "caller_id", callerID, "session_id", id)
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	span.AddEvent("db.read", trace.WithAttributes(attribute.String("session.id", id)))

	if err := row.(Session).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete session: db error", "caller_id", callerID, "session_id", id, "error", err)
		http.Error(w, "failed to delete session", http.StatusInternalServerError)
		return
	}
	span.AddEvent("db.soft_delete", trace.WithAttributes(attribute.String("session.id", id)))
	span.SetStatus(codes.Ok, "")
	slog.Info("delete session: success", "caller_id", callerID, "session_id", id)
	w.WriteHeader(http.StatusNoContent)
}
