package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// verifyHooksTrigger validates the HMAC-SHA256 token produced by the hooks
// service. Returns false if the key is unconfigured, the token is malformed,
// or the 30-second window has elapsed.
func verifyHooksTrigger(workflowID, triggeredBy, token, timestamp string) bool {
	if hooksTriggerKey == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if math.Abs(float64(time.Now().Unix()-ts)) > 30 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks:%s:%s:%s", workflowID, triggeredBy, timestamp)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(token), []byte(expected))
}

type internalTriggerRequest struct {
	TriggeredBy string            `json:"triggered_by"`
	OrgID       string            `json:"org_id"`
	Inputs      map[string]string `json:"inputs"`
}

// handleInternalTriggerRun accepts hook-service-signed trigger requests.
// It verifies the HMAC token, then enqueues a run attributed to the rule's creator.
func handleInternalTriggerRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleInternalTriggerRun")
	defer span.End()

	workflowID := r.PathValue("id")
	token := r.Header.Get("X-Hooks-Token")
	timestamp := r.Header.Get("X-Hooks-Timestamp")

	var req internalTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if !verifyHooksTrigger(workflowID, req.TriggeredBy, token, timestamp) {
		span.SetStatus(codes.Error, "invalid hooks token")
		slog.Warn("internal trigger: invalid hooks token", "workflow_id", workflowID)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if req.TriggeredBy == "" {
		http.Error(w, "triggered_by is required", http.StatusBadRequest)
		return
	}
	if req.Inputs == nil {
		req.Inputs = map[string]string{}
	}

	wf, err := getWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("internal trigger: get workflow", "workflow_id", workflowID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}

	if req.OrgID != "" && wf.OrgID != req.OrgID {
		span.SetStatus(codes.Error, "cross-org trigger denied")
		slog.Warn("internal trigger: org mismatch", "workflow_id", workflowID, "workflow_org", wf.OrgID, "req_org", req.OrgID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	run := WorkflowRun{
		RunID:       uuid.New().String(),
		WorkflowID:  wf.WorkflowID,
		TriggeredBy: req.TriggeredBy,
		OrgID:       req.OrgID,
		Status:      StatusPending,
		Inputs:      req.Inputs,
		Token:       "",
		CreatedAt:   time.Now().UTC(),
	}

	if err := run.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("internal trigger: db error", "workflow_id", workflowID, "error", err)
		http.Error(w, "failed to trigger run", http.StatusInternalServerError)
		return
	}

	meterRunsTriggered.Add(ctx, 1, metric.WithAttributes(attribute.String("workflow.id", wf.WorkflowID)))
	span.SetAttributes(attribute.String("run.id", run.RunID), attribute.String("workflow.id", wf.WorkflowID))
	span.SetStatus(codes.Ok, "")
	slog.Info("workflow run triggered by hooks", "run_id", run.RunID, "workflow_id", wf.WorkflowID, "triggered_by", req.TriggeredBy)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

// verifyHooksWorkflowCheck validates the HMAC for a workflow ownership check
// from the hooks service. The token covers "hooks-check:{workflowID}:{timestamp}".
func verifyHooksWorkflowCheck(workflowID, token, timestamp string) bool {
	if hooksTriggerKey == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || math.Abs(float64(time.Now().Unix()-ts)) > 30 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks-check:%s:%s", workflowID, timestamp)
	return hmac.Equal([]byte(token), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// handleInternalGetWorkflow returns the org_id of a workflow to an authenticated
// hooks-service ownership check request.
func handleInternalGetWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("id")
	if !verifyHooksWorkflowCheck(workflowID, r.Header.Get("X-Hooks-Token"), r.Header.Get("X-Hooks-Timestamp")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wf, err := getWorkflow(r.Context(), workflowID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"org_id": wf.OrgID}) //nolint:errcheck
}

// verifyHooksPoll validates the HMAC-SHA256 token used by the hooks service to
// poll run status. The token covers "hooks-poll:{runID}:{timestamp}".
func verifyHooksPoll(runID, token, timestamp string) bool {
	if hooksTriggerKey == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || math.Abs(float64(time.Now().Unix()-ts)) > 30 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks-poll:%s:%s", runID, timestamp)
	return hmac.Equal([]byte(token), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// handleInternalGetRun returns the current status of a workflow run to an
// authenticated hooks-service poll request.
func handleInternalGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !verifyHooksPoll(runID, r.Header.Get("X-Hooks-Token"), r.Header.Get("X-Hooks-Timestamp")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var run WorkflowRun
	if err := connect().WithContext(r.Context()).Where("run_id = ?", runID).First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}
