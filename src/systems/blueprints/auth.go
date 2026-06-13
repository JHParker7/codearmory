package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// ── Auth ──────────────────────────────────────────────────────────────────────

func loginToGatekeeper(ctx context.Context, email, password string) (string, bool) {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "loginToGatekeeper")
	defer span.End()

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/login", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "failed to build gatekeeper login request", "error", err)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "gatekeeper login request failed", "error", err)
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, "login rejected")
		slog.WarnContext(ctx, "gatekeeper login rejected", "status", resp.StatusCode)
		return "", false
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", false
	}

	span.SetStatus(codes.Ok, "")
	return result.Token, true
}

func extractToken(ctx context.Context, r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
		return tok, true
	}
	if encoded, ok := strings.CutPrefix(h, "Basic "); ok {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", false
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return "", false
		}
		return loginToGatekeeper(ctx, parts[0], parts[1])
	}
	return "", false
}

func checkPermissions(ctx context.Context, token, resource, action string) bool {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "checkPermissions")
	defer span.End()
	span.SetAttributes(
		attribute.String("permission.resource", resource),
		attribute.String("permission.action", action),
	)

	body, _ := json.Marshal(map[string]string{
		"service":  "blueprints",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "failed to build gatekeeper check_permissions request", "error", err)
		meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", false)))
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "gatekeeper check_permissions failed", "error", err)
		meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", false)))
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, "denied")
		slog.WarnContext(ctx, "permission denied", "resource", resource, "action", action, "status", resp.StatusCode)
		meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", false)))
		return false
	}

	var result struct {
		Authorized bool `json:"authorized"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}

	span.SetAttributes(attribute.Bool("permission.authorized", result.Authorized))
	span.SetStatus(codes.Ok, "")
	meterPermChecks.Add(ctx, 1, metric.WithAttributes(attribute.Bool("authorized", result.Authorized)))
	if !result.Authorized {
		slog.WarnContext(ctx, "permission denied", "resource", resource, "action", action)
	}
	return result.Authorized
}

// requireAuth extracts the token and writes 401 on failure. Returns (token, true) on success.
func requireAuth(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := extractToken(ctx, r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Basic")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return token, ok
}
