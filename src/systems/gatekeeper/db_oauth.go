package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// ── OAuthClient ───────────────────────────────────────────────────────────────

func (c OAuthClient) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.add")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", c.ClientID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c OAuthClient) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c OAuthClient) Remove(ctx context.Context) error { return nil }

func (c OAuthClient) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.get")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", c.ClientID))
	var result OAuthClient
	if err := connectRead().WithContext(ctx).Where("client_id = ? AND active = ?", c.ClientID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (c OAuthClient) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_client.list")
	defer span.End()
	var clients []OAuthClient
	q := connectRead().WithContext(ctx).Where("active = ?", true)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&clients).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(clients))
	for i, cl := range clients {
		result[i] = cl
	}
	return result, nil
}

// getOAuthClientByClientID returns the active OAuth client with the given client ID.
func getOAuthClientByClientID(ctx context.Context, clientID string) (OAuthClient, error) {
	row, err := (OAuthClient{ClientID: clientID}).Get(ctx)
	if err != nil {
		return OAuthClient{}, err
	}
	return row.(OAuthClient), nil
}

// listOAuthClients returns all active OAuth clients.
func listOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := (OAuthClient{}).List(ctx, 0, 0)
	if err != nil {
		return nil, err
	}
	clients := make([]OAuthClient, len(rows))
	for i, r := range rows {
		clients[i] = r.(OAuthClient)
	}
	return clients, nil
}

// deactivateOAuthClient soft-deletes an OAuth client by client ID.
// Returns the number of rows affected.
func deactivateOAuthClient(ctx context.Context, id string) (int64, error) {
	result := connect().WithContext(ctx).Model(&OAuthClient{}).Where("client_id = ?", id).Update("active", false)
	return result.RowsAffected, result.Error
}

// ── OAuthCode ─────────────────────────────────────────────────────────────────

func (c OAuthCode) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_code.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c OAuthCode) Update(ctx context.Context) error  { return nil }
func (c OAuthCode) Remove(ctx context.Context) error  { return nil }
func (c OAuthCode) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "db.oauth_code.get")
	defer span.End()
	var result OAuthCode
	if err := connectRead().WithContext(ctx).Where("code = ?", c.Code).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}
func (c OAuthCode) List(ctx context.Context, limit, offset int) ([]db, error) { return nil, nil }

// getOAuthCode returns an unused authorization code for the given client.
func getOAuthCode(ctx context.Context, code, clientID string) (OAuthCode, error) {
	var result OAuthCode
	if err := connectRead().WithContext(ctx).
		Where("code = ? AND client_id = ? AND used = ?", code, clientID, false).
		First(&result).Error; err != nil {
		return OAuthCode{}, err
	}
	return result, nil
}

// redeemOAuthCode atomically marks an unused authorization code as used.
// Returns the number of rows affected (0 if already redeemed by a peer request).
func redeemOAuthCode(ctx context.Context, code string) (int64, error) {
	result := connect().WithContext(ctx).
		Model(&OAuthCode{}).
		Where("code = ? AND used = ?", code, false).
		Update("used", true)
	return result.RowsAffected, result.Error
}
