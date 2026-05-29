package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// WorkerPool runs workflow runs from the pending queue in PostgreSQL.
type WorkerPool struct {
	db      *gorm.DB
	cancels sync.Map // runID -> context.CancelFunc
}

func newWorkerPool(db *gorm.DB) *WorkerPool {
	return &WorkerPool{db: db}
}

// Start launches n worker goroutines that poll for pending runs.
func (p *WorkerPool) Start(ctx context.Context, n int) {
	for range n {
		go p.loop(ctx)
	}
}

// Cancel signals the active goroutine for runID to stop after the current step.
// Returns false if the run is not currently being executed.
func (p *WorkerPool) Cancel(runID string) bool {
	if fn, ok := p.cancels.Load(runID); ok {
		fn.(context.CancelFunc)()
		return true
	}
	return false
}

func (p *WorkerPool) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tryOne(ctx)
		}
	}
}

func (p *WorkerPool) tryOne(ctx context.Context) {
	tx := p.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return
	}
	defer tx.Rollback() //nolint:errcheck

	type runPickup struct {
		RunID      string
		WorkflowID string
		Inputs     []byte
		Token      string
	}
	var pickup runPickup
	// FOR UPDATE SKIP LOCKED: each worker locks one pending run; siblings skip it.
	result := tx.Raw(`
		SELECT run_id, workflow_id, inputs, token
		FROM workflow_runs
		WHERE status = 'pending'
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&pickup)
	if result.Error != nil || result.RowsAffected == 0 {
		return
	}

	var inputs map[string]string
	if err := json.Unmarshal(pickup.Inputs, &inputs); err != nil {
		slog.Error("worker: unmarshal inputs", "run_id", pickup.RunID, "error", err)
		return
	}

	tx.Exec("UPDATE workflow_runs SET status='running', started_at=now() WHERE run_id=?", pickup.RunID)
	if err := tx.Commit().Error; err != nil {
		return
	}

	p.executeRun(ctx, pickup.RunID, pickup.WorkflowID, pickup.Token, inputs)
}

func (p *WorkerPool) executeRun(ctx context.Context, runID, workflowID, token string, inputs map[string]string) {
	runCtx, cancel := context.WithCancel(ctx)
	p.cancels.Store(runID, cancel)
	defer func() {
		cancel()
		p.cancels.Delete(runID)
	}()

	slog.Info("worker: starting run", "run_id", runID, "workflow_id", workflowID)

	wf, err := getWorkflow(runCtx, workflowID)
	if err != nil {
		slog.Error("worker: fetch workflow", "run_id", runID, "workflow_id", workflowID, "error", err)
		p.failRun(runID)
		return
	}

	finalStatus := StatusCompleted
	for i, step := range wf.Steps {
		if runCtx.Err() != nil {
			finalStatus = StatusCancelled
			break
		}

		stepRunID := uuid.New().String()
		if err := p.startStepRun(runID, stepRunID, i, step.Name); err != nil {
			slog.Error("worker: start step run", "run_id", runID, "step", i, "error", err)
			finalStatus = StatusFailed
			break
		}
		db.WithContext(context.Background()).Exec( //nolint:errcheck
			"UPDATE workflow_runs SET current_step=? WHERE run_id=?", i, runID)

		respStatus, respBody, stepErr := p.executeStep(runCtx, token, step, inputs)

		if stepErr != nil {
			if errors.Is(stepErr, context.Canceled) {
				p.finishStepRun(stepRunID, StatusCancelled, nil, strPtr(stepErr.Error()))
				finalStatus = StatusCancelled
			} else {
				p.finishStepRun(stepRunID, StatusFailed, nil, strPtr(stepErr.Error()))
				finalStatus = StatusFailed
			}
			break
		}

		stepStatus := statusForResponse(step, respStatus)
		meterStepsCompleted.Add(ctx, 1, metric.WithAttributes(
			attribute.String("workflow.id", workflowID),
			attribute.String("status", stepStatus),
		))
		p.finishStepRun(stepRunID, stepStatus, &respStatus, &respBody)

		if stepStatus != StatusCompleted {
			slog.Info("worker: step failed", "run_id", runID, "step", i,
				"service", step.Service, "response_status", respStatus)
			finalStatus = StatusFailed
			break
		}
		slog.Info("worker: step completed", "run_id", runID, "step", i,
			"service", step.Service, "response_status", respStatus)
	}

	meterRunsCompleted.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow.id", workflowID),
		attribute.String("status", finalStatus),
	))
	db.WithContext(context.Background()).Exec( //nolint:errcheck
		"UPDATE workflow_runs SET status=?, ended_at=now(), token=NULL WHERE run_id=? AND status='running'",
		finalStatus, runID,
	)
	slog.Info("worker: run finished", "run_id", runID, "status", finalStatus)
}

// executeStep resolves the target service URL, substitutes ${KEY} placeholders
// from inputs, and makes the HTTP request. Returns the response status code,
// truncated response body, and any transport-level error.
func (p *WorkerPool) executeStep(ctx context.Context, token string, step WorkflowStep, inputs map[string]string) (int, string, error) {
	baseURL, ok := serviceURLs[step.Service]
	if !ok {
		return 0, "", fmt.Errorf("unknown service %q — register it via SERVICES env var", step.Service)
	}

	method := strings.ToUpper(step.Method)
	if method == "" {
		method = http.MethodPost
	}

	path := substitute(step.Path, inputs)
	url := strings.TrimRight(baseURL, "/") + path

	timeout := step.TimeoutSecs
	if timeout <= 0 {
		timeout = defaultTimeout
	} else if timeout > maxTimeout {
		timeout = maxTimeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	var bodyReader io.Reader
	if len(step.Body) > 0 {
		substituted := substitute(string(step.Body), inputs)
		bodyReader = strings.NewReader(substituted)
	}

	req, err := http.NewRequestWithContext(stepCtx, method, url, bodyReader)
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range step.Headers {
		req.Header.Set(k, substitute(v, inputs))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode, string(raw), nil
}

// statusForResponse decides whether the step succeeded based on the HTTP
// response code. If ExpectedStatus is set, it must match exactly. Otherwise
// any 2xx counts as success.
func statusForResponse(step WorkflowStep, code int) string {
	if step.ExpectedStatus != 0 {
		if code == step.ExpectedStatus {
			return StatusCompleted
		}
		return StatusFailed
	}
	if code >= 200 && code < 300 {
		return StatusCompleted
	}
	return StatusFailed
}

// substitute replaces all ${KEY} occurrences in s with the corresponding value
// from inputs. Unrecognised keys are left as-is.
func substitute(s string, inputs map[string]string) string {
	for k, v := range inputs {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

func (p *WorkerPool) startStepRun(runID, stepRunID string, index int, name string) error {
	return db.Exec(
		`INSERT INTO workflow_step_runs (step_run_id, run_id, step_index, step_name, status, started_at)
		 VALUES (?, ?, ?, ?, 'running', now())`,
		stepRunID, runID, index, name,
	).Error
}

func (p *WorkerPool) finishStepRun(stepRunID, status string, respStatus *int, respBody *string) {
	db.WithContext(context.Background()).Exec( //nolint:errcheck
		`UPDATE workflow_step_runs
		 SET status=?, response_status=?, response_body=?, ended_at=now()
		 WHERE step_run_id=?`,
		status, respStatus, respBody, stepRunID,
	)
}

func (p *WorkerPool) failRun(runID string) {
	db.WithContext(context.Background()).Exec( //nolint:errcheck
		"UPDATE workflow_runs SET status='failed', ended_at=now(), token=NULL WHERE run_id=?", runID,
	)
	slog.Warn("worker: run failed before first step", "run_id", runID)
}

func strPtr(s string) *string { return &s }
