package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// maxParallelSteps caps concurrent step goroutines within a single parallel group
// to avoid goroutine explosion on large workflows.
const maxParallelSteps = 10

// tokenStore holds the current run token and session ID, safe for concurrent
// reads by parallel step goroutines and writes by the rotation goroutine.
type tokenStore struct {
	mu        sync.RWMutex
	token     string
	sessionID string
}

func newTokenStore(token, sessionID string) *tokenStore {
	return &tokenStore{token: token, sessionID: sessionID}
}

func (ts *tokenStore) getToken() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.token
}

func (ts *tokenStore) getSessionID() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.sessionID
}

// swap atomically replaces the token+sessionID and returns the old sessionID
// so the caller can revoke it after a grace period.
func (ts *tokenStore) swap(token, sessionID string) (oldSessionID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	old := ts.sessionID
	ts.token = token
	ts.sessionID = sessionID
	return old
}

// rotationInterval returns a random duration in [30 min, 60 min).
func rotationInterval() time.Duration {
	return 30*time.Minute + time.Duration(rand.Int63n(int64(30*time.Minute)))
}

// stepGroup is a set of steps that execute together (sequential = 1 step, parallel = N steps).
type stepGroup struct {
	steps   []WorkflowStep
	indices []int
}

func groupSteps(steps []WorkflowStep) []stepGroup {
	var groups []stepGroup
	i := 0
	for i < len(steps) {
		ws := steps[i]
		if ws.ParallelGroup == nil {
			groups = append(groups, stepGroup{steps: []WorkflowStep{ws}, indices: []int{i}})
			i++
			continue
		}
		g := *ws.ParallelGroup
		var grp stepGroup
		for i < len(steps) && steps[i].ParallelGroup != nil && *steps[i].ParallelGroup == g {
			grp.steps = append(grp.steps, steps[i])
			grp.indices = append(grp.indices, i)
			i++
		}
		groups = append(groups, grp)
	}
	return groups
}

// WorkerPool runs workflow runs from the pending queue in PostgreSQL.
type WorkerPool struct {
	cancels sync.Map // runID -> context.CancelFunc
}

func newWorkerPool() *WorkerPool { return &WorkerPool{} }

func (p *WorkerPool) Start(ctx context.Context, n int) {
	for range n {
		go p.loop(ctx)
	}
}

func (p *WorkerPool) Cancel(runID string) bool {
	if fn, ok := p.cancels.Load(runID); ok {
		fn.(context.CancelFunc)()
		return true
	}
	return false
}

func (p *WorkerPool) loop(ctx context.Context) {
	interval := time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			found := p.tryOne(ctx)
			// Slow down when the queue is empty (up to 5 s); reset immediately on work found.
			var next time.Duration
			if found {
				next = time.Second
			} else {
				next = min(interval*2, 5*time.Second)
			}
			if next != interval {
				interval = next
				ticker.Reset(interval)
			}
		}
	}
}

func (p *WorkerPool) tryOne(ctx context.Context) bool {
	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return false
	}
	defer tx.Rollback() //nolint:errcheck

	type runPickup struct {
		RunID        string
		WorkflowID   string
		Inputs       []byte
		Token        string
		RunSessionID string
		TriggeredBy  string
	}
	var pickup runPickup
	result := tx.Raw(`
		SELECT run_id, workflow_id, inputs, token, run_session_id, triggered_by
		FROM workflow_runs
		WHERE status = 'pending'
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&pickup)
	if result.Error != nil || result.RowsAffected == 0 {
		return false
	}

	var inputs map[string]string
	if err := json.Unmarshal(pickup.Inputs, &inputs); err != nil {
		slog.Error("worker: unmarshal inputs", "run_id", pickup.RunID, "error", err)
		return false
	}

	if pickup.Token != "" {
		plainToken, err := decryptToken(pickup.Token)
		if err != nil {
			slog.Error("worker: decrypt run token", "run_id", pickup.RunID, "error", err)
			return false
		}
		pickup.Token = plainToken
	}

	if result := tx.Exec("UPDATE workflow_runs SET status='running', started_at=now() WHERE run_id=?", pickup.RunID); result.RowsAffected == 0 {
		slog.Warn("worker: status update matched no rows, skipping", "run_id", pickup.RunID)
		return false
	}
	if err := tx.Commit().Error; err != nil {
		slog.Error("worker: commit failed", "run_id", pickup.RunID, "error", err)
		return false
	}

	p.executeRun(ctx, pickup.RunID, pickup.WorkflowID, pickup.Token, pickup.RunSessionID, pickup.TriggeredBy, inputs)
	return true
}

func (p *WorkerPool) executeRun(ctx context.Context, runID, workflowID, token, sessionID, triggeredBy string, inputs map[string]string) {
	runCtx, cancel := context.WithCancel(ctx)
	p.cancels.Store(runID, cancel)
	defer func() {
		cancel()
		p.cancels.Delete(runID)
	}()

	slog.Info("worker: starting run", "run_id", runID, "workflow_id", workflowID)

	workflow, err := getWorkflow(runCtx, workflowID)
	if err != nil {
		slog.Error("worker: fetch workflow", "run_id", runID, "workflow_id", workflowID, "error", err)
		p.failRun(runID, sessionID)
		return
	}

	store := newTokenStore(token, sessionID)
	go p.rotateToken(runCtx, store, runID, triggeredBy, workflow.RoleID)

	finalStatus := StatusCompleted
	for _, group := range groupSteps(workflow.Steps) {
		if runCtx.Err() != nil {
			finalStatus = StatusCancelled
			break
		}

		connect().WithContext(runCtx).Exec( //nolint:errcheck — best-effort progress tracking; failure doesn't affect step execution
			"UPDATE workflow_runs SET current_step=? WHERE run_id=?", group.indices[0], runID)

		if len(group.steps) == 1 {
			ws := group.steps[0]
			i := group.indices[0]
			stepRunID := uuid.New().String()
			if err := p.startStepRun(runID, stepRunID, i, ws.Name); err != nil {
				slog.Error("worker: start step run", "run_id", runID, "step", i, "error", err)
				finalStatus = StatusFailed
				break
			}
			output, stepErr := p.executeStep(runCtx, store, ws.Step, inputs)
			if stepErr != nil {
				if errors.Is(stepErr, context.Canceled) {
					p.finishStepRun(stepRunID, StatusCancelled, strPtr(stepErr.Error()))
					finalStatus = StatusCancelled
				} else {
					p.finishStepRun(stepRunID, StatusFailed, strPtr(stepErr.Error()))
					finalStatus = StatusFailed
				}
				break
			}
			meterStepsCompleted.Add(ctx, 1, metric.WithAttributes(
				attribute.String("workflow.id", workflowID),
				attribute.String("status", StatusCompleted),
			))
			p.finishStepRun(stepRunID, StatusCompleted, strPtr(output))
			slog.Info("worker: step completed", "run_id", runID, "step", i, "action", ws.Action)
			continue
		}

		// Parallel group.
		type parallelResult struct {
			stepRunID string
			action    string
			stepIdx   int
			output    string
			err       error
		}

		stepRunIDs := make([]string, len(group.steps))
		for j, ws := range group.steps {
			sid := uuid.New().String()
			stepRunIDs[j] = sid
			if err := p.startStepRun(runID, sid, group.indices[j], ws.Name); err != nil {
				slog.Error("worker: start parallel step run", "run_id", runID, "step", group.indices[j], "error", err)
				finalStatus = StatusFailed
				break
			}
		}
		if finalStatus != StatusCompleted {
			break
		}

		results := make(chan parallelResult, len(group.steps))
		// sem is a counting semaphore: acquire by sending, release by receiving.
		// The select allows a cancelled context to bypass the semaphore so the
		// goroutine can exit immediately rather than blocking on a full channel.
		sem := make(chan struct{}, maxParallelSteps)
		for j, ws := range group.steps {
			go func(j int, ws WorkflowStep) {
				select {
				case sem <- struct{}{}: // acquire
				case <-runCtx.Done():
					results <- parallelResult{stepRunIDs[j], ws.Action, group.indices[j], "", context.Canceled}
					return
				}
				defer func() { <-sem }() // release
				out, err := p.executeStep(runCtx, store, ws.Step, inputs)
				results <- parallelResult{stepRunIDs[j], ws.Action, group.indices[j], out, err}
			}(j, ws)
		}

		groupFailed := false
		for range group.steps {
			r := <-results
			if r.err != nil {
				var status string
				if errors.Is(r.err, context.Canceled) {
					status = StatusCancelled
					if finalStatus == StatusCompleted {
						finalStatus = StatusCancelled
					}
				} else {
					status = StatusFailed
					finalStatus = StatusFailed
				}
				p.finishStepRun(r.stepRunID, status, strPtr(r.err.Error()))
				slog.Info("worker: parallel step failed", "run_id", runID, "step", r.stepIdx, "action", r.action, "error", r.err)
				groupFailed = true
			} else {
				meterStepsCompleted.Add(ctx, 1, metric.WithAttributes(
					attribute.String("workflow.id", workflowID),
					attribute.String("status", StatusCompleted),
				))
				p.finishStepRun(r.stepRunID, StatusCompleted, strPtr(r.output))
				slog.Info("worker: parallel step completed", "run_id", runID, "step", r.stepIdx, "action", r.action)
			}
		}
		if groupFailed {
			break
		}
	}

	meterRunsCompleted.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow.id", workflowID),
		attribute.String("status", finalStatus),
	))
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — if this fails the run stays in 'running'; the stuck-run recovery on next startup will fix it
		"UPDATE workflow_runs SET status=?, ended_at=now(), token=NULL, run_session_id=NULL WHERE run_id=? AND status='running'",
		finalStatus, runID,
	)
	revokeRunToken(context.Background(), store.getSessionID())
	slog.Info("worker: run finished", "run_id", runID, "status", finalStatus)
}

// rotateToken runs until ctx is cancelled, rotating the run credential every
// 30–60 minutes so no single token stays live for the full run duration.
// The outgoing session is revoked after a 60 s grace period to avoid
// invalidating any in-flight step requests that still carry the old token.
func (p *WorkerPool) rotateToken(ctx context.Context, store *tokenStore, runID, triggeredBy, roleID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(rotationInterval()):
		}
		newToken, newSID, err := createRunToken(ctx, triggeredBy, roleID)
		if err != nil {
			slog.Warn("worker: token rotation failed", "run_id", runID, "error", err)
			continue
		}
		encNewToken, err := encryptToken(newToken)
		if err != nil {
			slog.Warn("worker: token rotation encryption failed, discarding new token", "run_id", runID, "error", err)
			revokeRunToken(context.Background(), newSID)
			continue
		}
		if dbErr := connect().WithContext(ctx).Exec(
			"UPDATE workflow_runs SET token=?, run_session_id=? WHERE run_id=?",
			encNewToken, newSID, runID,
		).Error; dbErr != nil {
			slog.Warn("worker: token rotation DB update failed, discarding new token", "run_id", runID, "error", dbErr)
			revokeRunToken(context.Background(), newSID)
			continue
		}
		oldSID := store.swap(newToken, newSID)
		slog.Info("worker: run token rotated", "run_id", runID)
		go func(sid string) {
			time.Sleep(60 * time.Second)
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rcancel()
			revokeRunToken(rctx, sid)
		}(oldSID)
	}
}

// executeStep dispatches a step to either the http escape-hatch or the registry
// action catalog. Returns the step output and a non-nil error on failure.
// context.Canceled means the run was cancelled.
func (p *WorkerPool) executeStep(ctx context.Context, store *tokenStore, step Step, inputs map[string]string) (string, error) {
	with := substituteWith(step.With, inputs)
	// Clamp to [defaultTimeout, maxTimeout]. Zero means "use default"; negative
	// is treated the same way since a non-positive duration would fire immediately.
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	} else if timeout > maxTimeout {
		timeout = maxTimeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	if step.Action == ActionHTTP {
		return p.executeHTTP(stepCtx, store, with)
	}

	actionCatalogMu.RLock()
	def, ok := actionCatalog[step.Action]
	actionCatalogMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown action %q — register it in the service catalog or use the http escape hatch", step.Action)
	}
	return p.executeAction(stepCtx, store, def, with)
}

// executeAction dispatches a catalog action: applies body transforms, sends the
// HTTP request, then polls if the action is async.
func (p *WorkerPool) executeAction(ctx context.Context, store *tokenStore, def ActionDef, with map[string]any) (string, error) {
	body := make(map[string]any, len(with))
	for k, v := range with {
		body[k] = v
	}
	for _, t := range def.BodyTransforms {
		val, exists := body[t.FromKey]
		if !exists {
			continue
		}
		delete(body, t.FromKey)
		if len(t.Wrap) > 0 {
			if s, ok := val.(string); ok {
				wrapped := make([]any, len(t.Wrap)+1)
				for i, w := range t.Wrap {
					wrapped[i] = w
				}
				wrapped[len(t.Wrap)] = s
				body[t.ToKey] = wrapped
			} else {
				body[t.ToKey] = val
			}
		} else {
			body[t.ToKey] = val
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal %s payload: %w", def.Name, err)
	}

	url := strings.TrimRight(def.ServiceURL, "/") + def.Path
	req, err := http.NewRequestWithContext(ctx, def.Method, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build %s request: %w", def.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := store.getToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", def.Name, err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	resp.Body.Close()

	if def.Async == nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return string(respBody), fmt.Errorf("%s returned %d: %s", def.Name, resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		return string(respBody), nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s returned %d: %s", def.Name, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var submission map[string]any
	if err := json.Unmarshal(respBody, &submission); err != nil {
		return "", fmt.Errorf("%s: parse submission response: %w", def.Name, err)
	}
	idVal, ok := submission[def.Async.IDField]
	if !ok {
		return "", fmt.Errorf("%s: submission response missing field %q", def.Name, def.Async.IDField)
	}
	jobID, ok := idVal.(string)
	if !ok || jobID == "" {
		return "", fmt.Errorf("%s: async ID field %q is not a string", def.Name, def.Async.IDField)
	}
	return p.pollAction(ctx, store, def, jobID)
}

// pollAction polls the job status URL until a terminal state is reached or the
// context is cancelled.
func (p *WorkerPool) pollAction(ctx context.Context, store *tokenStore, def ActionDef, jobID string) (string, error) {
	interval := time.Duration(def.Async.PollIntervalSecs) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	pollURL := strings.TrimRight(def.ServiceURL, "/") + strings.ReplaceAll(def.Async.PollPath, "{id}", jobID)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return "", fmt.Errorf("build poll request: %w", err)
		}
		if tok := store.getToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			continue // transient — keep polling
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		resp.Body.Close()

		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			continue
		}
		status, _ := result[def.Async.StatusField].(string)

		for _, s := range def.Async.SuccessStates {
			if status == s {
				out := ""
				if def.Async.OutputField != "" {
					if v, ok := result[def.Async.OutputField]; ok {
						out, _ = v.(string)
					}
				}
				return out, nil
			}
		}
		for _, s := range def.Async.FailureStates {
			if status == s {
				var parts []string
				if def.Async.OutputField != "" {
					if v, ok := result[def.Async.OutputField]; ok {
						if sv, ok := v.(string); ok && sv != "" {
							parts = append(parts, sv)
						}
					}
				}
				for _, f := range def.Async.ErrorFields {
					if v, ok := result[f]; ok {
						if sv, ok := v.(string); ok && sv != "" {
							parts = append(parts, sv)
						}
					}
				}
				exitCode := result["exit_code"]
				return strings.Join(parts, "\n"), fmt.Errorf("%s %s (exit code: %v)", def.Name, status, exitCode)
			}
		}
		for _, s := range def.Async.CancelStates {
			if status == s {
				return "", context.Canceled
			}
		}
		// Status is not in any known terminal or cancel set — keep polling.
		// Log a warning if the status is non-empty and unrecognised so operators
		// can detect misconfigured action definitions before the step times out.
		if status != "" {
			allKnown := append(append(def.Async.SuccessStates, def.Async.FailureStates...), def.Async.CancelStates...)
			known := false
			for _, s := range allKnown {
				if status == s {
					known = true
					break
				}
			}
			if !known {
				slog.Warn("poll: unrecognised status value, continuing to poll — check action definition",
					"action", def.Name, "job_id", jobID, "status", status)
			}
		}
	}
}

// executeHTTP performs a raw HTTP call. All HTTP parameters come from With:
// service, method, path, body (any), headers (map), expected_status (int).
func (p *WorkerPool) executeHTTP(ctx context.Context, store *tokenStore, with map[string]any) (string, error) {
	service := withString(with, "service")
	serviceURLsMu.RLock()
	baseURL, ok := serviceURLs[service]
	serviceURLsMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown service %q — register it via SERVICES env var", service)
	}

	method := strings.ToUpper(withString(with, "method"))
	if method == "" {
		method = http.MethodPost
	}
	path := withString(with, "path")
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("invalid path: traversal sequences are not allowed")
	}
	url := strings.TrimRight(baseURL, "/") + path

	var bodyReader io.Reader
	if rawBody, ok := with["body"]; ok && rawBody != nil {
		bodyJSON, err := json.Marshal(rawBody)
		if err == nil && string(bodyJSON) != "null" {
			bodyReader = strings.NewReader(string(bodyJSON))
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := store.getToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	reservedHeaders := map[string]bool{
		"authorization": true,
		"x-service-key": true,
		"cookie":        true,
		"x-user-id":    true,
	}
	for k, v := range withStringMap(with, "headers") {
		if !reservedHeaders[strings.ToLower(k)] {
			req.Header.Set(k, v)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, url, err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	resp.Body.Close()

	expectedStatus := int(withInt64(with, "expected_status"))
	if expectedStatus != 0 {
		if resp.StatusCode != expectedStatus {
			return string(raw), fmt.Errorf("expected status %d, got %d: %s", expectedStatus, resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return string(raw), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return string(raw), fmt.Errorf("%s %s returned %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return string(raw), nil
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
	return connect().Exec(
		`INSERT INTO workflow_step_runs (step_run_id, run_id, step_index, step_name, status, started_at)
		 VALUES (?, ?, ?, ?, 'running', now())`,
		stepRunID, runID, index, name,
	).Error
}

func (p *WorkerPool) finishStepRun(stepRunID, status string, output *string) {
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — step result is best-effort; run status is the authoritative record
		`UPDATE workflow_step_runs SET status=?, response_body=?, ended_at=now() WHERE step_run_id=?`,
		status, output, stepRunID,
	)
}

func (p *WorkerPool) failRun(runID, sessionID string) {
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — stuck-run recovery will catch this on restart
		"UPDATE workflow_runs SET status='failed', ended_at=now(), token=NULL, run_session_id=NULL WHERE run_id=?", runID,
	)
	revokeRunToken(context.Background(), sessionID)
	slog.Warn("worker: run failed before first step", "run_id", runID)
}

func strPtr(s string) *string { return &s }
