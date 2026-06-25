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

// rotationIntervalFn yields the run-token rotation period; a package var so tests
// can shorten it. Default: a random duration in [30 min, 60 min).
var rotationIntervalFn = func() time.Duration {
	return 30*time.Minute + time.Duration(rand.Int63n(int64(30*time.Minute)))
}

// rotationInterval returns a random duration in [30 min, 60 min).
func rotationInterval() time.Duration { return rotationIntervalFn() }

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
	run, err := (WorkflowRun{}).Dequeue(ctx)
	if err != nil || run == nil {
		return false
	}

	if run.Token != "" {
		plainToken, err := decryptToken(run.Token)
		if err != nil {
			// Dequeue already committed status='running'; fail the run rather than
			// returning, or it would be stranded forever (Dequeue selects only
			// 'pending' rows and stuck-run recovery runs only at startup).
			slog.ErrorContext(ctx, "worker: decrypt run token", "run_id", run.RunID, "error", err)
			p.failRun(run.RunID, run.RunSessionID)
			return true
		}
		run.Token = plainToken
	}

	p.executeRun(ctx, run.RunID, run.WorkflowID, run.Token, run.RunSessionID, run.TriggeredBy, run.Inputs)
	return true
}

func (p *WorkerPool) executeRun(ctx context.Context, runID, workflowID, token, sessionID, triggeredBy string, inputs map[string]string) {
	runCtx, cancel := context.WithCancel(ctx)
	p.cancels.Store(runID, cancel)
	defer func() {
		cancel()
		p.cancels.Delete(runID)
	}()

	slog.InfoContext(ctx, "worker: starting run", "run_id", runID, "workflow_id", workflowID)

	workflow, err := getWorkflow(runCtx, workflowID)
	if err != nil {
		slog.ErrorContext(ctx, "worker: fetch workflow", "run_id", runID, "workflow_id", workflowID, "error", err)
		p.failRun(runID, sessionID)
		return
	}

	store := newTokenStore(token, sessionID)
	go p.rotateToken(runCtx, store, runID, triggeredBy, workflow.RoleID)

	// stepOutputs accumulates each completed step's output by name so later steps
	// can interpolate ${steps.NAME.output...} into their With values. Only outputs
	// from prior groups are visible to a group, so each group is handed a snapshot
	// taken before it starts — never the live map (which the result loop mutates).
	stepOutputs := map[string]string{}

	finalStatus := StatusCompleted
	for _, group := range groupSteps(workflow.Steps) {
		if runCtx.Err() != nil {
			finalStatus = StatusCancelled
			break
		}

		(WorkflowRun{RunID: runID}).SetCurrentStep(runCtx, group.indices[0])
		visible := snapshotOutputs(stepOutputs)

		if len(group.steps) == 1 {
			ws := group.steps[0]
			i := group.indices[0]
			stepRunID := uuid.New().String()
			if err := p.startStepRun(runID, stepRunID, i, ws.Name); err != nil {
				slog.ErrorContext(ctx, "worker: start step run", "run_id", runID, "step", i, "error", err)
				finalStatus = StatusFailed
				break
			}
			res, stepErr := p.executeStep(runCtx, store, ws.Step, substContext{inputs: inputs, outputs: visible})
			if stepErr != nil {
				if errors.Is(stepErr, context.Canceled) {
					p.finishStepRun(stepRunID, StatusCancelled, strPtr(stepErr.Error()), res.MemoryUsedMB, res.MemoryLimitMB)
					finalStatus = StatusCancelled
				} else {
					p.finishStepRun(stepRunID, StatusFailed, strPtr(stepErr.Error()), res.MemoryUsedMB, res.MemoryLimitMB)
					finalStatus = StatusFailed
				}
				break
			}
			meterStepsCompleted.Add(ctx, 1, metric.WithAttributes(
				attribute.String("workflow.id", workflowID),
				attribute.String("status", StatusCompleted),
			))
			p.finishStepRun(stepRunID, StatusCompleted, strPtr(res.Output), res.MemoryUsedMB, res.MemoryLimitMB)
			stepOutputs[ws.Name] = res.Output
			slog.InfoContext(ctx, "worker: step completed", "run_id", runID, "step", i, "action", ws.Action)
			continue
		}

		// Parallel group.
		type parallelResult struct {
			stepRunID string
			name      string
			action    string
			stepIdx   int
			output    string
			usedMB    *int64
			limitMB   *int64
			err       error
		}

		stepRunIDs := make([]string, len(group.steps))
		for j, ws := range group.steps {
			sid := uuid.New().String()
			stepRunIDs[j] = sid
			if err := p.startStepRun(runID, sid, group.indices[j], ws.Name); err != nil {
				slog.ErrorContext(ctx, "worker: start parallel step run", "run_id", runID, "step", group.indices[j], "error", err)
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
					results <- parallelResult{stepRunID: stepRunIDs[j], name: ws.Name, action: ws.Action, stepIdx: group.indices[j], err: context.Canceled}
					return
				}
				defer func() { <-sem }() // release
				res, err := p.executeStep(runCtx, store, ws.Step, substContext{inputs: inputs, outputs: visible})
				results <- parallelResult{stepRunIDs[j], ws.Name, ws.Action, group.indices[j], res.Output, res.MemoryUsedMB, res.MemoryLimitMB, err}
			}(j, ws)
		}

		groupFailed := false
		groupOutputs := make(map[string]string, len(group.steps))
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
				p.finishStepRun(r.stepRunID, status, strPtr(r.err.Error()), r.usedMB, r.limitMB)
				slog.WarnContext(ctx, "worker: parallel step failed", "run_id", runID, "step", r.stepIdx, "action", r.action)
				groupFailed = true
			} else {
				meterStepsCompleted.Add(ctx, 1, metric.WithAttributes(
					attribute.String("workflow.id", workflowID),
					attribute.String("status", StatusCompleted),
				))
				p.finishStepRun(r.stepRunID, StatusCompleted, strPtr(r.output), r.usedMB, r.limitMB)
				groupOutputs[r.name] = r.output
				slog.InfoContext(ctx, "worker: parallel step completed", "run_id", runID, "step", r.stepIdx, "action", r.action)
			}
		}
		if groupFailed {
			break
		}
		// Publish the group's outputs only after it fully succeeds, so the next
		// group can reference them (the live map is never read concurrently).
		for name, out := range groupOutputs {
			stepOutputs[name] = out
		}
	}

	meterRunsCompleted.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow.id", workflowID),
		attribute.String("status", finalStatus),
	))
	(WorkflowRun{RunID: runID}).Complete(context.Background(), finalStatus)
	revokeRunToken(context.Background(), store.getSessionID())
	slog.InfoContext(ctx, "worker: run finished", "run_id", runID, "status", finalStatus)
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
			slog.WarnContext(ctx, "worker: token rotation failed", "run_id", runID, "error", err)
			continue
		}
		encNewToken, err := encryptToken(newToken)
		if err != nil {
			slog.WarnContext(ctx, "worker: token rotation encryption failed, discarding new token", "run_id", runID, "error", err)
			revokeRunToken(context.Background(), newSID)
			continue
		}
		if dbErr := (WorkflowRun{RunID: runID}).UpdateToken(ctx, encNewToken, newSID); dbErr != nil {
			slog.WarnContext(ctx, "worker: token rotation DB update failed, discarding new token", "run_id", runID, "error", dbErr)
			revokeRunToken(context.Background(), newSID)
			continue
		}
		oldSID := store.swap(newToken, newSID)
		slog.DebugContext(ctx, "worker: run token rotated", "run_id", runID)
		go func(sid string) {
			time.Sleep(60 * time.Second)
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rcancel()
			revokeRunToken(rctx, sid)
		}(oldSID)
	}
}

// stepResult is the outcome of running a step. Output is persisted as the step's
// response body. MemoryUsedMB/MemoryLimitMB are populated only for forge-backed
// async steps whose poll response carried them (the forge execution JSON), and
// are nil for every other action — so memory surfaces wherever the step ran a
// container, without coupling the generic poller to forge.
type stepResult struct {
	Output        string
	MemoryUsedMB  *int64
	MemoryLimitMB *int64
}

// jsonInt64Ptr converts a value pulled from a decoded JSON object into an *int64,
// returning nil when it is absent or not numeric. JSON numbers decode to float64
// in a map[string]any (or json.Number when UseNumber is set), so both are handled.
func jsonInt64Ptr(v any) *int64 {
	switch n := v.(type) {
	case float64:
		i := int64(n)
		return &i
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return &i
		}
	}
	return nil
}

// executeStep dispatches a step to either the http escape-hatch or the registry
// action catalog. Returns the step output and a non-nil error on failure.
// context.Canceled means the run was cancelled. sc carries the run inputs and
// prior step outputs interpolated into the step's With values.
func (p *WorkerPool) executeStep(ctx context.Context, store *tokenStore, step Step, sc substContext) (stepResult, error) {
	with := substituteWith(step.With, sc)
	// Clamp to [defaultTimeout, maxTimeout]. Zero means "use default"; negative
	// is treated the same way since a non-positive duration would fire immediately.
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	} else if timeout > maxTimeout {
		timeout = maxTimeout
	}

	if step.Action == ActionHTTP {
		stepCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		out, err := p.executeHTTP(stepCtx, store, with)
		return stepResult{Output: out}, err
	}

	actionCatalogMu.RLock()
	def, ok := actionCatalog[step.Action]
	actionCatalogMu.RUnlock()
	if !ok {
		return stepResult{}, fmt.Errorf("unknown action %q — register it in the service catalog or use the http escape hatch", step.Action)
	}

	// Async actions submit a job to a remote service (e.g. a forge execution)
	// that polls to its own terminal state and enforces the job's own timeout.
	// The step timeout clock starts here — before the remote job's container is
	// even scheduled — so bounding the poll by it can expire on a job that
	// actually succeeded (the "context deadline exceeded" on a forge run that
	// forge reported completed). Give async steps the max budget and let the
	// remote service terminate the job; synchronous catalog steps keep the step
	// timeout. Each HTTP request is still bounded by httpClient's own timeout.
	budget := timeout
	if def.Async != nil {
		budget = maxTimeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(budget)*time.Second)
	defer cancel()
	return p.executeAction(stepCtx, store, def, with)
}

// executeAction dispatches a catalog action: applies body transforms, sends the
// HTTP request, then polls if the action is async.
func (p *WorkerPool) executeAction(ctx context.Context, store *tokenStore, def ActionDef, with map[string]any) (stepResult, error) {
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

	// Substitute {param} placeholders in the action path from the with map,
	// removing those keys from the body so they aren't double-sent.
	resolvedPath := def.Path
	for k, v := range body {
		placeholder := "{" + k + "}"
		if strings.Contains(resolvedPath, placeholder) {
			if sv, ok := v.(string); ok {
				resolvedPath = strings.ReplaceAll(resolvedPath, placeholder, sv)
				delete(body, k)
			}
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return stepResult{}, fmt.Errorf("marshal %s payload: %w", def.Name, err)
	}

	url := strings.TrimRight(def.ServiceURL, "/") + resolvedPath
	req, err := http.NewRequestWithContext(ctx, def.Method, url, bytes.NewReader(payload))
	if err != nil {
		return stepResult{}, fmt.Errorf("build %s request: %w", def.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := store.getToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return stepResult{}, fmt.Errorf("%s: %w", def.Name, err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	resp.Body.Close()

	if def.Async == nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return stepResult{Output: string(respBody)}, fmt.Errorf("%s returned %d: %s", def.Name, resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		return stepResult{Output: string(respBody)}, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return stepResult{}, fmt.Errorf("%s returned %d: %s", def.Name, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var submission map[string]any
	if err := json.Unmarshal(respBody, &submission); err != nil {
		return stepResult{}, fmt.Errorf("%s: parse submission response: %w", def.Name, err)
	}
	idVal, ok := submission[def.Async.IDField]
	if !ok {
		return stepResult{}, fmt.Errorf("%s: submission response missing field %q", def.Name, def.Async.IDField)
	}
	jobID, ok := idVal.(string)
	if !ok || jobID == "" {
		return stepResult{}, fmt.Errorf("%s: async ID field %q is not a string", def.Name, def.Async.IDField)
	}
	return p.pollAction(ctx, store, def, jobID)
}

// pollAction polls the job status URL until a terminal state is reached or the
// context is cancelled.
func (p *WorkerPool) pollAction(ctx context.Context, store *tokenStore, def ActionDef, jobID string) (stepResult, error) {
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
			return stepResult{}, ctx.Err()
		case <-ticker.C:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return stepResult{}, fmt.Errorf("build poll request: %w", err)
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

		// memory_used_mb / memory_limit_mb are present on a forge execution's poll
		// response and absent elsewhere; jsonInt64Ptr yields nil when missing, so
		// this is a no-op for non-forge actions. Captured on both success and
		// failure — an OOM-killed step is exactly when its memory matters.
		used := jsonInt64Ptr(result["memory_used_mb"])
		limit := jsonInt64Ptr(result["memory_limit_mb"])

		for _, s := range def.Async.SuccessStates {
			if status == s {
				out := ""
				if def.Async.OutputField != "" {
					if v, ok := result[def.Async.OutputField]; ok {
						out, _ = v.(string)
					}
				}
				return stepResult{Output: out, MemoryUsedMB: used, MemoryLimitMB: limit}, nil
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
				return stepResult{Output: strings.Join(parts, "\n"), MemoryUsedMB: used, MemoryLimitMB: limit},
					fmt.Errorf("%s %s (exit code: %v)", def.Name, status, exitCode)
			}
		}
		for _, s := range def.Async.CancelStates {
			if status == s {
				// Carry memory through on cancel too — a step cancelled after an
				// OOM/timeout still has meaningful usage figures.
				return stepResult{MemoryUsedMB: used, MemoryLimitMB: limit}, context.Canceled
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
				slog.WarnContext(ctx, "poll: unrecognised status value, continuing to poll — check action definition",
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
		"x-user-id":     true,
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

// snapshotOutputs copies the accumulated step-output map so a group of steps can
// read a stable view while the run loop continues to mutate the live map. Returns
// nil for an empty map so substContext stays "empty" and substitution is skipped.
func snapshotOutputs(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func (p *WorkerPool) startStepRun(runID, stepRunID string, index int, name string) error {
	return (WorkflowStepRun{StepRunID: stepRunID, RunID: runID, StepIndex: index, StepName: name}).Add(context.Background())
}

func (p *WorkerPool) finishStepRun(stepRunID, status string, output *string, usedMB, limitMB *int64) {
	(WorkflowStepRun{StepRunID: stepRunID}).Complete(context.Background(), status, output, usedMB, limitMB)
}

func (p *WorkerPool) failRun(runID, sessionID string) {
	(WorkflowRun{RunID: runID}).Complete(context.Background(), StatusFailed)
	revokeRunToken(context.Background(), sessionID)
	slog.Warn("worker: run failed before first step", "run_id", runID)
}

func strPtr(s string) *string { return &s }
