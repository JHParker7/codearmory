package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Add inserts the session.
func (session Session) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.add")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	session.Active = true
	if err := connect().WithContext(ctx).Create(&session).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheTrackUserSession(ctx, session.UserID, session.SessionID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update saves all session fields.
func (session Session) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.update")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	session.UpdatedAt = time.Now()
	if err := connect().WithContext(ctx).Save(&session).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:session:"+session.SessionID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Remove soft-deletes the session by setting active = false.
func (session Session) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.remove")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	if err := connect().WithContext(ctx).Model(&Session{}).Where("session_id = ?", session.SessionID).Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	cacheDel(ctx, "gk:session:"+session.SessionID)
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the active session by SessionID.
func (session Session) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.get")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	if cached, ok := cacheGet[Session](ctx, "gk:session:"+session.SessionID); ok {
		span.SetStatus(codes.Ok, "")
		return cached, nil
	}
	var newSession Session
	if err := connectRead().WithContext(ctx).First(&newSession, "session_id = ? AND active = ?", session.SessionID, true).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// Cache until the session's own expiry, not the default entityTTL. Using a
	// longer TTL would let authMiddleware accept already-expired sessions from cache.
	if ttl := time.Until(newSession.ExpiresAt); ttl > 0 {
		cacheSet(ctx, "gk:session:"+newSession.SessionID, newSession, ttl)
	}
	span.SetStatus(codes.Ok, "")
	return newSession, nil
}

// List retrieves all active sessions matching the non-zero fields of the receiver.
func (session Session) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.session.list")
	defer span.End()
	span.SetAttributes(attribute.String("session.id", session.SessionID))
	var sessions []Session
	session.Active = true
	q := connectRead().WithContext(ctx).Where(session)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&sessions).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(sessions))
	for i, s := range sessions {
		result[i] = s
	}
	return result, nil
}
