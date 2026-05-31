package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// webhookPayload is the JSON body expected on POST /hooks.
type webhookPayload struct {
	Repo    string `json:"repo"`
	Event   string `json:"event"`
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Pusher  string `json:"pusher"`
	Message string `json:"message"`
}

// signTrigger creates an HMAC-SHA256 token that the workflows service can
// verify to authenticate hook-originated trigger requests.
func signTrigger(workflowID, triggeredBy string) (token, timestamp string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks:%s:%s:%s", workflowID, triggeredBy, ts)
	return hex.EncodeToString(mac.Sum(nil)), ts
}

// triggerResult holds the outcome of a single rule match and dispatch attempt.
type triggerResult struct {
	Trigger  HookTrigger
	RuleName string
	Success  bool
}

// matchAndDispatch queries matching rules for the given payload and dispatches
// a workflow run for each match. sigHeader is the X-Hub-Signature-256 header
// value from the caller; pass "" to skip per-rule HMAC checks (e.g. when the
// GitHub App handler has already verified the App-level signature).
// Returns the trigger results and (successCount, failCount).
func matchAndDispatch(ctx context.Context, eventID string, payload webhookPayload, payloadMap map[string]string, rawBody []byte, sigHeader string) ([]triggerResult, int, int) {
	matchedRules, err := getMatchedRules(ctx, payload.Repo, payload.Event)
	if err != nil {
		slog.Error("matchAndDispatch: query rules", "error", err)
		return nil, 0, 0
	}

	var results []triggerResult
	successCount := 0
	failCount := 0

	for _, rws := range matchedRules {
		// Apply ref_filter.
		if !matchesRefFilter(rws.RefFilter, payload.Ref) {
			continue
		}

		// HMAC verification: if the rule has a secret, the caller must supply
		// X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, body))>.
		// When sigHeader is "" (GitHub App handler), rules with a secret are skipped.
		if rws.Secret != nil && *rws.Secret != "" {
			expected := "sha256=" + computeHMAC(*rws.Secret, rawBody)
			if !hmac.Equal([]byte(sigHeader), []byte(expected)) {
				slog.Warn("matchAndDispatch: HMAC mismatch, skipping rule",
					"rule_id", rws.RuleID, "event_id", eventID)
				continue
			}
		}

		meterRulesMatched.Add(ctx, 1, metric.WithAttributes(
			attribute.String("repo", payload.Repo),
			attribute.String("workflow.id", rws.WorkflowID),
		))

		// Build workflow inputs: system defaults + input_mapping overrides.
		inputs := map[string]string{
			"HOOK_REPO":   payload.Repo,
			"HOOK_EVENT":  payload.Event,
			"HOOK_REF":    payload.Ref,
			"HOOK_COMMIT": payload.Commit,
		}
		for wfKey, payloadField := range rws.InputMapping {
			if v, found := payloadMap[payloadField]; found {
				inputs[wfKey] = v
			}
		}

		triggerID := uuid.New().String()
		trig := HookTrigger{
			TriggerID:  triggerID,
			EventID:    eventID,
			RuleID:     rws.RuleID,
			WorkflowID: rws.WorkflowID,
			CreatedAt:  time.Now().UTC(),
		}

		runID, trigErr := dispatchWorkflow(ctx, rws.WorkflowID, rws.CreatedBy, rws.OrgID, inputs)
		success := false
		if trigErr != nil {
			slog.Error("matchAndDispatch: dispatch workflow failed",
				"rule_id", rws.RuleID, "workflow_id", rws.WorkflowID, "error", trigErr)
			errStr := trigErr.Error()
			trig.Status = "failed"
			trig.Error = &errStr
			failCount++
		} else {
			trig.Status = "triggered"
			trig.RunID = runID
			success = true
			successCount++
			meterRunsTriggered.Add(ctx, 1, metric.WithAttributes(
				attribute.String("workflow.id", rws.WorkflowID),
			))
		}
		results = append(results, triggerResult{Trigger: trig, RuleName: rws.Name, Success: success})

		// Persist the trigger record (fire-and-forget after response is sent).
		go func(t HookTrigger) {
			if err := t.Add(context.Background()); err != nil {
				slog.Error("matchAndDispatch: insert trigger", "trigger_id", t.TriggerID, "error", err)
			}
		}(trig)
	}

	return results, successCount, failCount
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleWebhook")
	defer span.End()

	// Read the full body (up to maxBodyBytes) before we do anything else
	// because we may need to re-read it for HMAC verification.
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		span.SetStatus(codes.Error, "read body failed")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		span.SetStatus(codes.Error, "invalid json")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// X-Hook-Event header overrides the event field in the body.
	if h := r.Header.Get("X-Hook-Event"); h != "" {
		payload.Event = h
	}

	if payload.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	if payload.Event == "" {
		http.Error(w, "event is required", http.StatusBadRequest)
		return
	}

	meterHooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("repo", payload.Repo)))

	// Build a string-map of the payload fields for storage and input mapping.
	payloadMap := map[string]string{
		"repo":    payload.Repo,
		"event":   payload.Event,
		"ref":     payload.Ref,
		"commit":  payload.Commit,
		"pusher":  payload.Pusher,
		"message": payload.Message,
	}

	eventID := uuid.New().String()
	newEvent := HookEvent{
		EventID:   eventID,
		Repo:      payload.Repo,
		EventType: payload.Event,
		Ref:       payload.Ref,
		Payload:   payloadMap,
		Status:    "received",
		CreatedAt: time.Now().UTC(),
	}
	if err := newEvent.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert event failed")
		slog.Error("webhook: insert event", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	trigResults, successCount, failCount := matchAndDispatch(ctx, eventID, payload, payloadMap, rawBody, r.Header.Get("X-Hub-Signature-256"))

	// Collect triggers for response.
	triggers := make([]HookTrigger, 0, len(trigResults))
	for _, tr := range trigResults {
		triggers = append(triggers, tr.Trigger)
	}

	// Derive overall event status.
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

	// Update the hook_events row (fire-and-forget).
	go func() {
		updateEventStatus(eventID, total, eventStatus)
	}()

	event := HookEvent{
		EventID:      eventID,
		Repo:         payload.Repo,
		EventType:    payload.Event,
		Ref:          payload.Ref,
		Payload:      payloadMap,
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
		attribute.String("repo", payload.Repo),
		attribute.Int("rules.matched", total),
	)
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event) //nolint:errcheck
}

// computeHMAC returns the hex-encoded HMAC-SHA256 of body using the given secret.
func computeHMAC(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body) //nolint:errcheck
	return hex.EncodeToString(mac.Sum(nil))
}

// dispatchWorkflow calls POST {workflowsURL}/internal/workflows/{id}/runs with
// a signed HMAC token.  It returns the run_id from the response, or an error.
func dispatchWorkflow(ctx context.Context, workflowID, triggeredBy, orgID string, inputs map[string]string) (*string, error) {
	token, ts := signTrigger(workflowID, triggeredBy)

	body, err := json.Marshal(map[string]any{
		"triggered_by": triggeredBy,
		"org_id":       orgID,
		"inputs":       inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal dispatch body: %w", err)
	}

	url := workflowsURL + "/internal/workflows/" + workflowID + "/runs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build dispatch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hooks-Token", token)
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("workflows returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil || result.RunID == "" {
		// Response was successful but we couldn't parse run_id — that's fine.
		return nil, nil
	}
	return &result.RunID, nil
}
