package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// errWorkflowNotFound is returned by dispatchWorkflow when the target workflow
// does not exist. Used to deactivate stale rules that reference deleted workflows.
var errWorkflowNotFound = errors.New("workflow not found")

// hookVarName turns a payload field key into the workflow input var that exposes
// it: uppercased, prefixed HOOK_, with every run of non-alphanumeric characters
// collapsed to a single underscore ("clone_url" -> "HOOK_CLONE_URL",
// "default.branch" -> "HOOK_DEFAULT_BRANCH"). Returns "" for a key with no
// alphanumeric content, so a junk field never yields a bare "HOOK_" var.
func hookVarName(key string) string {
	var b strings.Builder
	b.WriteString("HOOK_")
	prevUnderscore := true // avoids a leading underscore after the prefix
	any := false
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
			prevUnderscore = false
			any = true
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
			any = true
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	if !any {
		return ""
	}
	return strings.TrimRight(b.String(), "_")
}

// genericPayload is the JSON body expected on POST /hooks.
type genericPayload struct {
	Source  string            `json:"source"`
	Event   string            `json:"event"`
	Payload map[string]string `json:"payload"`
}

// signTrigger creates an HMAC-SHA256 token that the workflows service can
// verify to authenticate hook-originated trigger requests.
func signTrigger(workflowID, triggeredBy string) (token, timestamp string) {
	return signTriggerAt(workflowID, triggeredBy, time.Now().Unix())
}

// signTriggerAt is signTrigger with an explicit clock, so the signing contract
// can be tested deterministically.
func signTriggerAt(workflowID, triggeredBy string, unix int64) (token, timestamp string) {
	ts := strconv.FormatInt(unix, 10)
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

// ruleInScope reports whether a rule belongs to the given ownership scope.
// Org-owned events (orgID != "") match only rules in the same org; personal
// events (orgID == "") match only the owning user's personal (no-org) rules.
// Used to confine trusted internal emitters whose source is a shared constant
// (e.g. tickets) so one tenant's events never fire another tenant's rules.
func ruleInScope(rule PipelineRule, orgID, userID string) bool {
	if orgID != "" {
		return rule.OrgID == orgID
	}
	return rule.OrgID == "" && rule.CreatedBy == userID
}

// matchAndDispatch queries matching rules for the given source and event, then
// dispatches a workflow run for each match.
//
// baseInputs contains adapter-specific defaults (e.g. HOOK_REPO for git)
// that are merged before input_mapping overrides are applied.
// sigHeader is the X-Hub-Signature-256 header value from the caller.
// When skipHMAC is true the per-rule HMAC check is bypassed entirely
// (use only when the caller has already performed an equivalent check, e.g. the GitHub App handler).
//
// scopeOrgID/scopeUserID confine matching to a single tenant: when either is
// non-empty, only rules owned by that org (or that user, for personal events)
// fire. Webhook sources are globally unique so they pass "", "" for no scoping;
// trusted internal emitters with a shared source (tickets) pass the event owner.
//
// Returns the trigger results and (successCount, failCount).
func matchAndDispatch(ctx context.Context, eventID string, source, event, ref string, payloadMap map[string]string, baseInputs map[string]string, rawBody []byte, sigHeader string, skipHMAC bool, scopeOrgID, scopeUserID string) ([]triggerResult, int, int) {
	matchedRules, err := getMatchedRules(ctx, source, event)
	if err != nil {
		slog.ErrorContext(ctx, "matchAndDispatch: query rules", "error", err)
		return nil, 0, 0
	}

	applyScope := scopeOrgID != "" || scopeUserID != ""

	var results []triggerResult
	successCount := 0
	failCount := 0

	for _, rws := range matchedRules {
		// Confine to the emitting tenant when a scope is supplied.
		if applyScope && !ruleInScope(rws, scopeOrgID, scopeUserID) {
			continue
		}

		// Apply ref_filter (used by git adapters; empty filter matches everything).
		if !matchesRefFilter(rws.RefFilter, ref) {
			continue
		}

		// HMAC verification: if the rule has a secret and skipHMAC is false,
		// the caller must supply X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, body))>.
		if !skipHMAC && rws.Secret != nil && *rws.Secret != "" {
			expected := "sha256=" + computeHMAC(*rws.Secret, rawBody)
			if !hmac.Equal([]byte(sigHeader), []byte(expected)) {
				slog.WarnContext(ctx, "matchAndDispatch: HMAC mismatch, skipping rule",
					"rule_id", rws.RuleID, "event_id", eventID)
				continue
			}
		}

		meterRulesMatched.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", source),
			attribute.String("workflow.id", rws.WorkflowID),
		))

		// Build workflow inputs in ascending precedence:
		//  1. every payload field, auto-exposed as HOOK_<UPPER_KEY> so a pipeline can
		//     read any webhook value (clone_url, default_branch, repo_id, …) by key
		//     without authoring a per-rule input_mapping;
		//  2. the canonical HOOK_SOURCE / HOOK_EVENT;
		//  3. adapter-specific typed defaults (e.g. the git adapter's HOOK_REPO/REF/COMMIT);
		//  4. the rule's explicit input_mapping overrides — always the last word.
		inputs := map[string]string{}
		for k, v := range payloadMap {
			if hk := hookVarName(k); hk != "" {
				inputs[hk] = v
			}
		}
		inputs["HOOK_SOURCE"] = source
		inputs["HOOK_EVENT"] = event
		for k, v := range baseInputs {
			inputs[k] = v
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
			slog.ErrorContext(ctx, "matchAndDispatch: dispatch workflow failed",
				"rule_id", rws.RuleID, "workflow_id", rws.WorkflowID, "error", trigErr)
			errStr := trigErr.Error()
			trig.Error = &errStr
			if errors.Is(trigErr, errWorkflowNotFound) {
				trig.Status = "failed"
			} else {
				// Transient failure — mark for retry so the run is not silently lost.
				trig.Status = "pending_retry"
			}
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
		// Deactivate rules that reference a deleted workflow so they stop firing.
		if errors.Is(trigErr, errWorkflowNotFound) {
			slog.WarnContext(ctx, "matchAndDispatch: workflow not found — deactivating rule",
				"rule_id", rws.RuleID, "workflow_id", rws.WorkflowID)
			go func(ruleID string) {
				if err := (PipelineRule{RuleID: ruleID}).Remove(context.Background()); err != nil {
					slog.Error("matchAndDispatch: deactivate zombie rule", "rule_id", ruleID, "error", err)
				}
			}(rws.RuleID)
		}

		if err := trig.Add(ctx); err != nil {
			slog.ErrorContext(ctx, "matchAndDispatch: insert trigger", "trigger_id", trig.TriggerID, "error", err)
		}
		// Enqueue retry for transient failures so the dispatch is not lost.
		if trig.Status == "pending_retry" {
			if retryErr := addRetry(ctx, trig.TriggerID, rws.WorkflowID, rws.CreatedBy, rws.OrgID, inputs, *trig.Error); retryErr != nil {
				slog.ErrorContext(ctx, "matchAndDispatch: enqueue retry failed", "trigger_id", trig.TriggerID, "error", retryErr)
			}
		}

		results = append(results, triggerResult{Trigger: trig, RuleName: rws.Name, Success: success})
	}

	return results, successCount, failCount
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleWebhook")
	defer span.End()

	// Read the full body before we do anything else because we may need to
	// re-read it for HMAC verification.
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		span.SetStatus(codes.Error, "read body failed")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var payload genericPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		span.SetStatus(codes.Error, "invalid json")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// X-Hook-Event header overrides the event field in the body.
	if h := r.Header.Get("X-Hook-Event"); h != "" {
		payload.Event = h
	}

	if payload.Source == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}
	if payload.Event == "" {
		http.Error(w, "event is required", http.StatusBadRequest)
		return
	}

	if payload.Payload == nil {
		payload.Payload = map[string]string{}
	}

	meterHooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("source", payload.Source)))

	eventID := uuid.New().String()
	newEvent := HookEvent{
		EventID:   eventID,
		Source:    payload.Source,
		EventType: payload.Event,
		Payload:   payload.Payload,
		Status:    "received",
		CreatedAt: time.Now().UTC(),
	}
	if err := newEvent.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert event failed")
		slog.ErrorContext(ctx, "webhook: insert event", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	trigResults, successCount, failCount := matchAndDispatch(ctx, eventID, payload.Source, payload.Event, "", payload.Payload, nil, rawBody, r.Header.Get("X-Hub-Signature-256"), false, "", "")

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

	updateEventStatus(eventID, total, eventStatus)

	event := HookEvent{
		EventID:      eventID,
		Source:       payload.Source,
		EventType:    payload.Event,
		Payload:      payload.Payload,
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
		attribute.String("source", payload.Source),
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

// dispatchWorkflow calls POST {workflowsURL}/internal/pipelines/{id}/runs with
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

	url := workflowsURL + "/internal/pipelines/" + workflowID + "/runs"
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
	if resp.StatusCode == http.StatusNotFound {
		return nil, errWorkflowNotFound
	}
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
