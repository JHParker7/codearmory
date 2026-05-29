package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
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
		if err == pgx.ErrNoRows {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("internal trigger: get workflow", "workflow_id", workflowID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}

	inputsJSON, _ := json.Marshal(req.Inputs)
	run := WorkflowRun{
		RunID:       uuid.New().String(),
		WorkflowID:  wf.WorkflowID,
		TriggeredBy: req.TriggeredBy,
		OrgID:       req.OrgID,
		Status:      StatusPending,
		Inputs:      req.Inputs,
		CreatedAt:   time.Now().UTC(),
	}

	_, err = db.Exec(ctx,
		`INSERT INTO workflow_runs (run_id, workflow_id, triggered_by, org_id, inputs, token)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		run.RunID, run.WorkflowID, run.TriggeredBy, run.OrgID, inputsJSON, "",
	)
	if err != nil {
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
