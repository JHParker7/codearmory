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

// ruleWithSecret is used when scanning rows for webhook processing so that the
// HMAC secret can be read without exposing it through the PipelineRule type.
type ruleWithSecret struct {
	PipelineRule
	secret *string
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
	payloadJSON, _ := json.Marshal(payloadMap)

	eventID := uuid.New().String()
	if _, err := db.Exec(ctx,
		`INSERT INTO hook_events (event_id, repo, event_type, ref, payload, status)
		 VALUES ($1, $2, $3, $4, $5, 'received')`,
		eventID, payload.Repo, payload.Event, payload.Ref, payloadJSON,
	); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert event failed")
		slog.Error("webhook: insert event", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Query matching active rules for this repo + event type.
	rows, err := db.Query(ctx,
		`SELECT rule_id, name, repo, events, ref_filter, workflow_id, secret, input_mapping, created_by, org_id
		 FROM pipeline_rules
		 WHERE repo = $1 AND active = true AND $2 = ANY(events)`,
		payload.Repo, payload.Event,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query rules failed")
		slog.Error("webhook: query rules", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var matchedRules []ruleWithSecret
	for rows.Next() {
		var rws ruleWithSecret
		var mappingJSON []byte
		var refFilter *string
		if err := rows.Scan(
			&rws.RuleID, &rws.Name, &rws.Repo, &rws.Events, &refFilter,
			&rws.WorkflowID, &rws.secret, &mappingJSON, &rws.CreatedBy, &rws.OrgID,
		); err != nil {
			slog.Error("webhook: scan rule", "error", err)
			continue
		}
		if refFilter != nil {
			rws.RefFilter = *refFilter
		}
		if err := json.Unmarshal(mappingJSON, &rws.InputMapping); err != nil {
			slog.Error("webhook: unmarshal input_mapping", "rule_id", rws.RuleID, "error", err)
			continue
		}
		matchedRules = append(matchedRules, rws)
	}
	if err := rows.Err(); err != nil {
		slog.Error("webhook: rows error", "error", err)
	}

	var triggers []HookTrigger
	successCount := 0
	failCount := 0

	for _, rws := range matchedRules {
		// Apply ref_filter.
		if !matchesRefFilter(rws.RefFilter, payload.Ref) {
			continue
		}

		// HMAC verification: if the rule has a secret, the caller must supply
		// X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, body))>.
		if rws.secret != nil && *rws.secret != "" {
			sigHeader := r.Header.Get("X-Hub-Signature-256")
			expected := "sha256=" + computeHMAC(*rws.secret, rawBody)
			if !hmac.Equal([]byte(sigHeader), []byte(expected)) {
				slog.Warn("webhook: HMAC mismatch, skipping rule",
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
		if trigErr != nil {
			slog.Error("webhook: dispatch workflow failed",
				"rule_id", rws.RuleID, "workflow_id", rws.WorkflowID, "error", trigErr)
			errStr := trigErr.Error()
			trig.Status = "failed"
			trig.Error = &errStr
			failCount++
		} else {
			trig.Status = "triggered"
			trig.RunID = runID
			successCount++
			meterRunsTriggered.Add(ctx, 1, metric.WithAttributes(
				attribute.String("workflow.id", rws.WorkflowID),
			))
		}
		triggers = append(triggers, trig)

		// Persist the trigger record (fire-and-forget after response is sent).
		go func(t HookTrigger) {
			_, err := db.Exec(context.Background(),
				`INSERT INTO hook_triggers (trigger_id, event_id, rule_id, workflow_id, run_id, status, error)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				t.TriggerID, t.EventID, t.RuleID, t.WorkflowID, t.RunID, t.Status, t.Error,
			)
			if err != nil {
				slog.Error("webhook: insert trigger", "trigger_id", t.TriggerID, "error", err)
			}
		}(trig)
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
		_, err := db.Exec(context.Background(),
			`UPDATE hook_events SET rules_matched=$1, status=$2 WHERE event_id=$3`,
			total, eventStatus, eventID,
		)
		if err != nil {
			slog.Error("webhook: update event status", "event_id", eventID, "error", err)
		}
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
