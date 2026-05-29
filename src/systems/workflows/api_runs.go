package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "triggerRun", "workflows/runs")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	wf, err := getWorkflow(ctx, workflowID)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
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

	inputsJSON, _ := json.Marshal(req.Inputs)
	token := bearerToken(r)

	run := WorkflowRun{
		RunID:       uuid.New().String(),
		WorkflowID:  wf.WorkflowID,
		TriggeredBy: userID,
		OrgID:       orgID,
		Status:      StatusPending,
		Inputs:      req.Inputs,
		CreatedAt:   time.Now().UTC(),
	}

	_, err = db.Exec(ctx,
		`INSERT INTO workflow_runs (run_id, workflow_id, triggered_by, org_id, inputs, token)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		run.RunID, run.WorkflowID, run.TriggeredBy, run.OrgID, inputsJSON, token,
	)
	if err != nil {
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

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "listRun", "workflows/runs")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	q := r.URL.Query()
	workflowID := q.Get("workflow_id")

	var rows pgx.Rows
	var err error
	if workflowID != "" {
		rows, err = db.Query(ctx,
			`SELECT run_id, workflow_id, triggered_by, org_id, status, current_step, inputs, created_at, started_at, ended_at
			 FROM workflow_runs
			 WHERE (triggered_by=$1 OR (org_id != '' AND org_id = $2)) AND workflow_id=$3
			 ORDER BY created_at DESC LIMIT 100`,
			userID, orgID, workflowID,
		)
	} else {
		rows, err = db.Query(ctx,
			`SELECT run_id, workflow_id, triggered_by, org_id, status, current_step, inputs, created_at, started_at, ended_at
			 FROM workflow_runs
			 WHERE triggered_by=$1 OR (org_id != '' AND org_id = $2)
			 ORDER BY created_at DESC LIMIT 100`,
			userID, orgID,
		)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list runs: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list runs", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	runs, err := scanRuns(rows)
	if err != nil {
		slog.Error("list runs: scan error", "user_id", userID, "error", err)
		http.Error(w, "failed to list runs", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runs) //nolint:errcheck
}

func handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetRun")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "getRun", "workflows/runs/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	run, err := getRun(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
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

	stepRows, err := db.Query(ctx,
		`SELECT step_run_id, run_id, step_index, step_name, status,
		        response_status, response_body, started_at, ended_at
		 FROM workflow_step_runs WHERE run_id=$1 ORDER BY step_index`,
		id,
	)
	if err != nil {
		slog.Error("get run: step runs query", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run steps", http.StatusInternalServerError)
		return
	}
	defer stepRows.Close()

	run.StepRuns, err = scanStepRuns(stepRows)
	if err != nil {
		slog.Error("get run: step runs scan", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run steps", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

func handleCancelRun(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCancelRun")
		defer span.End()

		id := r.PathValue("id")
		userID, orgID, ok := checkGatekeeper(ctx, w, r, "cancelRun", "workflows/runs/"+id)
		if !ok {
			span.SetStatus(codes.Error, "forbidden")
			return
		}

		run, err := getRun(ctx, id)
		if err != nil {
			if err == pgx.ErrNoRows {
				http.Error(w, "run not found", http.StatusNotFound)
				return
			}
			slog.Error("cancel run: get run", "run_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to get run", http.StatusInternalServerError)
			return
		}
		if !canAccessRun(run, userID, orgID) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}

		tag, err := db.Exec(context.Background(),
			`UPDATE workflow_runs SET status='cancelled', ended_at=now(), token=null
			 WHERE run_id=$1 AND status IN ('pending', 'running')`, id,
		)
		if err != nil {
			slog.Error("cancel run: db update", "run_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to cancel run", http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 {
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
	row := db.QueryRow(ctx,
		`SELECT run_id, workflow_id, triggered_by, org_id, status, current_step, inputs, created_at, started_at, ended_at
		 FROM workflow_runs WHERE run_id=$1`, id,
	)
	return scanRun(row)
}

func scanRun(row pgx.Row) (WorkflowRun, error) {
	var run WorkflowRun
	var inputsJSON []byte
	err := row.Scan(&run.RunID, &run.WorkflowID, &run.TriggeredBy, &run.OrgID, &run.Status,
		&run.CurrentStep, &inputsJSON, &run.CreatedAt, &run.StartedAt, &run.EndedAt)
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(inputsJSON, &run.Inputs); err != nil {
		return run, err
	}
	return run, nil
}

func scanRuns(rows pgx.Rows) ([]WorkflowRun, error) {
	var runs []WorkflowRun
	for rows.Next() {
		var run WorkflowRun
		var inputsJSON []byte
		if err := rows.Scan(&run.RunID, &run.WorkflowID, &run.TriggeredBy, &run.OrgID, &run.Status,
			&run.CurrentStep, &inputsJSON, &run.CreatedAt, &run.StartedAt, &run.EndedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(inputsJSON, &run.Inputs); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if runs == nil {
		runs = []WorkflowRun{}
	}
	return runs, rows.Err()
}

func scanStepRuns(rows pgx.Rows) ([]WorkflowStepRun, error) {
	var steps []WorkflowStepRun
	for rows.Next() {
		var s WorkflowStepRun
		if err := rows.Scan(&s.StepRunID, &s.RunID, &s.StepIndex, &s.StepName,
			&s.Status, &s.ResponseStatus, &s.ResponseBody, &s.StartedAt, &s.EndedAt); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	if steps == nil {
		steps = []WorkflowStepRun{}
	}
	return steps, rows.Err()
}
