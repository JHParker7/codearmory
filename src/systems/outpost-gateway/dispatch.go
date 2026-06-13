package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// eventConsumers maps an integration name to the base URL of its consumer
// service, parsed from EVENT_CONSUMERS (e.g. "chaos=http://chaos:8090,argo=http://argo:8091").
// The dispatcher appends /internal/events. Adding a new integration is a config
// change here; the dispatcher core is integration-agnostic.
var eventConsumers = parseConsumers(secret("EVENT_CONSUMERS"))

func parseConsumers(raw string) map[string]string {
	m := map[string]string{}
	for _, pair := range splitCSV(raw) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	return m
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// startDispatcher runs the outbox poller. Each tick claims due events (SKIP
// LOCKED) and delivers them to the integration's consumer; transient failures
// are retried with exponential backoff, exhausted ones are dead-lettered.
func startDispatcher(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				dispatchDue(ctx)
			}
		}
	}()
}

const (
	dispatchBatchSize = 20
	perEventTimeout   = 10 * time.Second
)

func dispatchDue(ctx context.Context) {
	// Events in a batch are delivered sequentially, so the lease must cover the
	// whole batch (batchSize * per-event timeout), not a single event — otherwise
	// a peer poller could reclaim the tail of a slow batch while it's still
	// in-flight and double-deliver. Plus a margin for DB round-trips.
	lease := dispatchBatchSize*perEventTimeout + 30*time.Second
	events, err := claimDueEvents(ctx, dispatchBatchSize, lease)
	if err != nil {
		slog.ErrorContext(ctx, "dispatcher: claim due events", "error", err)
		return
	}
	for _, e := range events {
		deliverEvent(ctx, e)
	}
}

func deliverEvent(ctx context.Context, e OutpostEvent) {
	ctx, span := otel.Tracer("outpost-gateway").Start(ctx, "dispatcher.deliver")
	defer span.End()
	span.SetAttributes(
		attribute.String("event.id", e.ID),
		attribute.String("event.integration", e.Integration),
		attribute.String("event.type", e.Type),
	)

	base, ok := eventConsumers[e.Integration]
	if !ok {
		// No consumer configured for this integration — dead-letter so the loop
		// doesn't spin, and make the misconfiguration visible.
		slog.ErrorContext(ctx, "dispatcher: no consumer configured", "integration", e.Integration, "event_id", e.ID)
		_ = scheduleEventRetry(ctx, OutpostEvent{ID: e.ID, Attempt: maxDeliveryAttempts}, "no consumer for integration "+e.Integration)
		return
	}

	body, err := json.Marshal(map[string]any{
		"event_id":    e.ID,
		"outpost_id":  e.OutpostID,
		"org_id":      e.OrgID,
		"user_id":     e.UserID,
		"integration": e.Integration,
		"type":        e.Type,
		"payload":     e.Payload,
	})
	if err != nil {
		_ = scheduleEventRetry(ctx, e, "marshal: "+err.Error())
		return
	}

	token, ts := signInternal("event", body)
	reqCtx, cancel := context.WithTimeout(ctx, perEventTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/internal/events", bytes.NewReader(body))
	if err != nil {
		_ = scheduleEventRetry(ctx, e, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", token)
	req.Header.Set("X-Internal-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "dispatcher: delivery failed", "event_id", e.ID, "integration", e.Integration, "error", err)
		_ = scheduleEventRetry(ctx, e, err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := "consumer returned " + resp.Status + ": " + strings.TrimSpace(string(respBody))
		slog.WarnContext(ctx, "dispatcher: consumer non-2xx", "event_id", e.ID, "integration", e.Integration, "status", resp.StatusCode)
		_ = scheduleEventRetry(ctx, e, msg)
		return
	}
	if err := markEventDelivered(ctx, e.ID); err != nil {
		slog.ErrorContext(ctx, "dispatcher: mark delivered", "event_id", e.ID, "error", err)
		return
	}
	meterEventsDelivered.Add(ctx, 1)
	slog.DebugContext(ctx, "event delivered", "event_id", e.ID, "integration", e.Integration, "type", e.Type)
}
