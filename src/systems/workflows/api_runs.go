package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

type triggerRunRequest struct {
	// Inputs are extra env vars injected into every step. Step-level env takes precedence.
	Inputs map[string]string `json:"inputs"`
}

// canAccessRun returns true when the caller may read the run:
// the caller triggered it, or they share the same org.
func canAccessRun(run WorkflowRun, userID, orgID string) bool {
	return run.TriggeredBy == userID || (orgID != "" && run.OrgID == orgID)
}

func handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleTriggerRun")
	defer span.End()

	workflowID := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "triggerRun", "workflows/runs")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	wf, err := getWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("trigger run: get workflow", "workflow_id", workflowID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(wf, userID, orgID) {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	var req triggerRunRequest
	if r.ContentLength != 0 {
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
	}
	if req.Inputs == nil {
		req.Inputs = map[string]string{}
	}

	token := bearerToken(r)

	run := WorkflowRun{
		RunID:       uuid.New().String(),
		WorkflowID:  wf.WorkflowID,
		TriggeredBy: userID,
		OrgID:       orgID,
		Status:      StatusPending,
		Inputs:      req.Inputs,
		Token:       token,
		StepRuns:    []WorkflowStepRun{},
		CreatedAt:   time.Now().UTC(),
	}

	if err := db.WithContext(ctx).Create(&run).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("trigger run: db error", "workflow_id", workflowID, "user_id", userID, "error", err)
		http.Error(w, "failed to trigger run", http.StatusInternalServerError)
		return
	}

	meterRunsTriggered.Add(ctx, 1, metric.WithAttributes(attribute.String("workflow.id", wf.WorkflowID)))
	span.SetAttributes(attribute.String("run.id", run.RunID), attribute.String("workflow.id", wf.WorkflowID))
	span.SetStatus(codes.Ok, "")
	slog.Info("workflow run triggered", "run_id", run.RunID, "workflow_id", wf.WorkflowID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

func handleListRuns(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListRuns")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRun", "workflows/runs")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	q := r.URL.Query()
	workflowID := q.Get("workflow_id")

	query := db.WithContext(ctx).
		Where("triggered_by=? OR (org_id != '' AND org_id = ?)", userID, orgID)
	if workflowID != "" {
		query = query.Where("workflow_id=?", workflowID)
	}

	var runs []WorkflowRun
	if err := query.Order("created_at DESC").Limit(100).Find(&runs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list runs: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list runs", http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []WorkflowRun{}
	}
	for i := range runs {
		runs[i].StepRuns = []WorkflowStepRun{}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runs) //nolint:errcheck
}

func handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetRun")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRun", "workflows/runs/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	run, err := getRun(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		slog.Error("get run: db error", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run", http.StatusInternalServerError)
		return
	}
	if !canAccessRun(run, userID, orgID) {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	var stepRuns []WorkflowStepRun
	if err := db.WithContext(ctx).Raw(
		`SELECT step_run_id, run_id, step_index, step_name, status,
		        response_status, response_body, started_at, ended_at
		 FROM workflow_step_runs WHERE run_id=? ORDER BY step_index`, id,
	).Scan(&stepRuns).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query step runs failed")
		slog.Error("get run: step runs query", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run steps", http.StatusInternalServerError)
		return
	}
	if stepRuns == nil {
		stepRuns = []WorkflowStepRun{}
	}
	run.StepRuns = stepRuns

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

func handleCancelRun(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCancelRun")
		defer span.End()

		id := r.PathValue("id")
		userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "cancelRun", "workflows/runs/"+id)
		if !ok {
			span.SetStatus(codes.Error, "forbidden")
			return
		}

		run, err := getRun(ctx, id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "run not found", http.StatusNotFound)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "db error")
			slog.Error("cancel run: get run", "run_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to get run", http.StatusInternalServerError)
			return
		}
		if !canAccessRun(run, userID, orgID) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}

		result := db.WithContext(ctx).Exec(
			"UPDATE workflow_runs SET status='cancelled', ended_at=now(), token=NULL WHERE run_id=? AND status IN ('pending','running')", id,
		)
		if result.Error != nil {
			slog.Error("cancel run: db update", "run_id", id, "user_id", userID, "error", result.Error)
			http.Error(w, "failed to cancel run", http.StatusInternalServerError)
			return
		}
		if result.RowsAffected == 0 {
			http.Error(w, "run is not in a cancellable state", http.StatusConflict)
			return
		}
		// Signal the worker goroutine to stop early; the DB write above is the
		// authoritative cancellation. This is best-effort — the worker may have
		// already finished before the signal arrives.
		pool.Cancel(id)

		span.SetStatus(codes.Ok, "")
		slog.Info("workflow run cancelled", "run_id", id, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func getRun(ctx context.Context, id string) (WorkflowRun, error) {
	var run WorkflowRun
	result := db.WithContext(ctx).Where("run_id=?", id).First(&run)
	return run, result.Error
}
