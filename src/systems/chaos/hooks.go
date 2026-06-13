package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

const (
	chaosHookSource        = "chaos"
	eventExperimentStarted = "experiment.started"
	eventExperimentPassed  = "experiment.passed"
	eventExperimentFailed  = "experiment.failed"
)

var (
	hooksURL      = envOrDefault("HOOKS_URL", "")
	hooksEventKey = secret("HOOKS_TRIGGER_KEY")
)

func hooksEnabled() bool {
	return hooksURL != "" && hooksEventKey != ""
}

// notifyHooks emits an experiment lifecycle event to the hooks service. It is
// fire-and-forget on a detached context so it never blocks the request path,
// matching the tickets integration.
func notifyHooks(ctx context.Context, event string, e Experiment) {
	if !hooksEnabled() {
		return
	}
	payload := map[string]string{
		"experiment_id":   e.ExperimentID,
		"experiment_type": e.ExperimentType,
		"outpost_id":      e.OutpostID,
		"target_app_ns":   e.TargetAppNS,
		"target_app":      e.TargetAppLabel,
		"status":          e.Status,
		"verdict":         e.Verdict,
		"fail_step":       e.FailStep,
	}
	body := map[string]any{
		"source":     chaosHookSource,
		"event":      event,
		"ref":        e.ExperimentType,
		"org_id":     e.OrgID,
		"created_by": e.UserID,
		"payload":    payload,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		slog.Warn("notify hooks: marshal failed", "experiment_id", e.ExperimentID, "event", event, "error", err)
		return
	}
	emitCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	go sendHookEvent(emitCtx, event, e.OrgID, e.UserID, raw, e.ExperimentID)
}

func sendHookEvent(ctx context.Context, event, orgID, createdBy string, raw []byte, experimentID string) {
	ctx, span := otel.Tracer("chaos").Start(ctx, "notifyHooks")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	token, ts := signHookEvent(chaosHookSource, event, orgID, createdBy)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hooksURL+"/internal/events", bytes.NewReader(raw))
	if err != nil {
		span.RecordError(err)
		slog.Warn("notify hooks: build request failed", "experiment_id", experimentID, "event", event, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hooks-Token", token)
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("notify hooks: request failed", "experiment_id", experimentID, "event", event, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("notify hooks: non-2xx", "experiment_id", experimentID, "event", event, "status", resp.StatusCode)
	}
}

func signHookEvent(source, event, orgID, createdBy string) (token, timestamp string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hooksEventKey))
	fmt.Fprintf(mac, "event:%s:%s:%s:%s:%s", source, event, orgID, createdBy, ts)
	return hex.EncodeToString(mac.Sum(nil)), ts
}
