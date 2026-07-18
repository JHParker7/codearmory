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
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// maxParallelSteps caps concurrent step goroutines within a single parallel group
// to avoid goroutine explosion on large workflows. It is the absolute ceiling a
// matrix/scatter max_concurrent can raise its fan-out to.
const maxParallelSteps = 10

// defaultFanoutConcurrency is how many legs a matrix or scatter runs at once when it
// declares no max_concurrent — a conservative default (rather than the full ceiling)
// so an unthrottled fan-out does not swamp a small cluster. Explicit max_concurrent
// still raises it up to maxParallelSteps.
const defaultFanoutConcurrency = 3

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

// stepGroup is ONE node's unit of work, plus the index it occupies in the workflow's
// step array. It holds a slice rather than a single step only because a matrix/scatter
// node expands into legs (buildGroupTasks); it never holds two different steps —
// running distinct steps together is a property of the graph's edges, not of a batch.
type stepGroup struct {
	steps   []WorkflowStep
	indices []int
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

	p.executeRun(ctx, run.RunID, run.WorkflowID, run.Token, run.RunSessionID, run.TriggeredBy, run.Inputs, run.Depth, run.TicketID)
	return true
}

// ticketID is the run's already-open ticket, if it has one — a resumed run adopts it
// rather than opening a second (see ticketReporter.open).
func (p *WorkerPool) executeRun(ctx context.Context, runID, workflowID, token, sessionID, triggeredBy string, inputs map[string]string, depth int, ticketID string) {
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
	// can interpolate ${steps.NAME.output...} into their With values. A step sees
	// only its transitive ancestors' outputs (workflowGraph.visibleFor), handed to
	// it as a snapshot at launch — never the live map, which only the scheduler
	// goroutine mutates. On resume (a run re-dequeued after an approval pause) it is
	// seeded from the already-completed step runs, and `completed` names them so
	// finished nodes are skipped rather than re-executed.
	stepOutputs, completed := rebuildResumeState(runCtx, runID, workflow.Steps)

	// The graph is the execution plan: a workflow's stored routes, or — for one
	// authored as a plain array — a chain derived from that array's order.
	g := workflow.buildGraph()
	st := newRunState(g, stepOutputs, completed)

	// Mirror this run into a ticket, when the workflow opted in. Attached to the
	// TOP-LEVEL state only, which is what stops a map region's iterations commenting
	// once per node per value. A resumed run passes the ticket it already has, so a
	// gate does not open a second one. Nothing here can fail the run — see ticket.go.
	st.ticket = newTicketReporter(&workflow, store, runID)
	st.ticket.open(runCtx, workflow.Name, inputs, ticketID)
	runStart := time.Now()

	// One run-wide leg budget, shared by every node in the frontier, every matrix or
	// scatter fan-out, and every map iteration. maxParallelSteps was a true ceiling
	// in the batch engine only because a fan-out step was always alone in its group;
	// a frontier of N such nodes would otherwise multiply it.
	legSem := make(chan struct{}, maxParallelSteps)
	finalStatus := p.runGraph(runCtx, g, st, store, runID, workflowID, inputs, depth, iterCtx{}, legSem)

	// One or more approval gates parked and the rest of the frontier drained, so
	// every runnable node is finished and recorded. Pause the run: return without
	// completing or revoking the token; the approval API re-mints a fresh run token
	// before re-queueing the run.
	if finalStatus == statusPaused {
		if runCtx.Err() != nil {
			// Cancelled just as we reached the gate: take the normal cancel path
			// (which revokes the token) rather than pausing on a dead run.
			finalStatus = StatusCancelled
		} else if err := p.pauseAtGates(runCtx, g, st, runID, inputs); err != nil {
			slog.ErrorContext(ctx, "worker: pause for approval", "run_id", runID, "error", err)
			finalStatus = StatusFailed
		} else {
			// The run is parked, not finished: the ticket says blocked rather than
			// closing on a run that has not reached its end.
			st.ticket.pause(runCtx)
			slog.InfoContext(ctx, "worker: run paused awaiting approval", "run_id", runID)
			return
		}
	}

	meterRunsCompleted.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow.id", workflowID),
		attribute.String("status", finalStatus),
	))
	// Tear down any shared workspace volumes before the run token is revoked — the
	// token carries the deleteVolume grant. Best-effort: forge's age reaper is the
	// backstop for the crash-before-teardown case (and for stuck-run recovery, which
	// has no token). Uses a background context so a cancelled run still cleans up.
	if workflowUsesVolumes(workflow.Steps, workflow.Maps) {
		p.teardownRunVolumes(context.Background(), store, runID)
	}
	// Resolve the pipeline's declared outputs from the final step outputs — only on
	// success, so a failed run exposes none. This map becomes the run's outputs and,
	// for a sub-run, the parent's workflows/trigger step output.
	var runOutputs map[string]string
	if finalStatus == StatusCompleted {
		runOutputs = resolveWorkflowOutputs(workflow.Outputs, substContext{inputs: inputs, outputs: stepOutputs, runID: runID, workflowID: workflowID})
	}
	// Close the ticket with the run's outcome. A background context: a cancelled run
	// still deserves a ticket that says so, and runCtx is already dead by here.
	st.ticket.closeWith(context.Background(), finalStatus, time.Since(runStart))
	(WorkflowRun{RunID: runID}).Complete(context.Background(), finalStatus, runOutputs)
	revokeRunToken(context.Background(), store.getSessionID())
	slog.InfoContext(ctx, "worker: run finished", "run_id", runID, "status", finalStatus)
}

// resolveWorkflowOutputs resolves each declared output's ${...} template against the
// run's final step outputs, returning the name→value map (nil when none declared).
func resolveWorkflowOutputs(defs []WorkflowOutputDef, sc substContext) map[string]string {
	if len(defs) == 0 {
		return nil
	}
	out := make(map[string]string, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			continue
		}
		out[d.Name] = substitute(d.Value, sc)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ActionForgeCreateVolume is the catalog action a step uses to provision a shared
// workspace volume for the run. Its presence means the run must tear its volumes
// down when it finishes.
const ActionForgeCreateVolume = "forge/create-volume"

// workflowUsesVolumes reports whether any step provisions a shared volume, so the
// run knows to tear volumes down (and to expect the deleteVolume grant on its role).
func workflowUsesVolumes(steps []WorkflowStep, maps []MapDef) bool {
	for _, ws := range steps {
		// A scatter step provisions per-leg clone volumes under the run id, so teardown
		// must run to reap them even if the pipeline declares no create-volume step.
		if ws.Action == ActionForgeCreateVolume || ws.Scatter != nil {
			return true
		}
	}
	// A map region with a volume clones the workspace per iteration under the run id,
	// so those clones need reaping for the same reason.
	for _, d := range maps {
		if d.Volume != "" {
			return true
		}
	}
	return false
}

// teardownRunVolumes deletes every shared volume forge provisioned for this run
// (DELETE /volumes?workflow_id=<runID>). It is idempotent — forge returns deleted:0
// when none remain — and best-effort: any failure is logged and forge's age reaper
// removes whatever is left. Must run before the run token is revoked; the token
// carries the deleteVolume grant via the create-volume companion permission.
func (p *WorkerPool) teardownRunVolumes(ctx context.Context, store *tokenStore, runID string) {
	p.deleteRunVolumes(ctx, store, runID, "")
}

// deleteRunVolumes deletes this run's shared volumes, or just the one named. Naming
// one is how a map iteration releases its workspace clone the moment it finishes:
// without that, clones accumulate for the whole run and the concurrent volume
// footprint scales with the VALUE COUNT rather than with max_concurrent, which
// trips forge's per-workflow volume cap on any sizeable fan-out.
func (p *WorkerPool) deleteRunVolumes(ctx context.Context, store *tokenStore, runID, name string) {
	serviceURLsMu.RLock()
	baseURL, ok := serviceURLs["forge"]
	serviceURLsMu.RUnlock()
	if !ok {
		slog.WarnContext(ctx, "worker: forge service URL unknown, skipping volume teardown", "run_id", runID)
		return
	}
	reqURL := strings.TrimRight(baseURL, "/") + "/volumes?workflow_id=" + neturl.QueryEscape(runID)
	if name != "" {
		reqURL += "&name=" + neturl.QueryEscape(name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		slog.ErrorContext(ctx, "worker: build volume teardown request", "run_id", runID, "error", err)
		return
	}
	if tok := store.getToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "worker: volume teardown failed (reaper will retry)", "run_id", runID, "error", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	if resp.StatusCode >= 300 {
		slog.WarnContext(ctx, "worker: volume teardown non-2xx (reaper will retry)", "run_id", runID, "status", resp.StatusCode)
		return
	}
	slog.InfoContext(ctx, "worker: run volumes torn down", "run_id", runID)
}

// stepTask is one concrete execution within a group: a sequential step, one
// member of a parallel group, or one value of a matrix fan-out. matrix holds the
// per-execution ${matrix.<var>} binding (nil for non-matrix tasks).
type stepTask struct {
	step      Step
	stepIndex int
	name      string
	matrix    map[string]string
	// mapVars are the enclosing map iteration's ${map.*} bindings, if this task runs
	// inside a region. Separate from matrix so a mapped step can still fan out.
	mapVars map[string]string
}

// taskResult is the outcome of one stepTask, keyed back to its position so the
// caller can aggregate matrix outputs in their original value order.
type taskResult struct {
	name    string
	output  string
	logs    string
	usedMB  *int64
	limitMB *int64
	err     error
	idx     int
}

// rebuildResumeState seeds the run's step-output map and completed-node set from
// step runs that finished on an earlier attempt. It only matters when a run is
// re-dequeued after an approval pause — a crashed run is reaped by recoverStuckRuns
// and never resumed, so partially-finished nodes never reach here. A matrix step's
// already-completed executions are recombined into its JSON-array output.
//
// Grouping stays keyed by StepIndex even though the result is keyed by name: a
// matrix leg's StepName is the per-leg label ("build [os=linux]"), not the node's
// name, so grouping by name would shatter a matrix node into one phantom node per
// leg and lose the JSON-array re-aggregation below. StepIndex remains the array
// position, which is exactly the node's identity in the graph.
func rebuildResumeState(ctx context.Context, runID string, steps []WorkflowStep) (outputs map[string]string, completed map[string]bool) {
	outputs = map[string]string{}
	completed = map[string]bool{}
	stepRuns, err := getStepRuns(ctx, runID)
	if err != nil || len(stepRuns) == 0 {
		return outputs, completed
	}
	byIndex := map[int][]WorkflowStepRun{}
	for _, sr := range stepRuns {
		if sr.Status == StatusCompleted {
			byIndex[sr.StepIndex] = append(byIndex[sr.StepIndex], sr)
		}
	}
	for idx, runs := range byIndex {
		if idx < 0 || idx >= len(steps) {
			continue
		}
		name := steps[idx].Name
		completed[name] = true
		if steps[idx].Matrix != nil {
			outs := make([]string, 0, len(runs))
			for _, r := range runs {
				outs = append(outs, derefStr(r.Output))
			}
			b, _ := json.Marshal(outs)
			outputs[name] = string(b)
		} else if len(runs) > 0 {
			outputs[name] = derefStr(runs[0].Output)
		}
	}
	return outputs, completed
}

// buildGroupTasks expands a step group into the executions to run. A matrix step
// (always its own single-step group) fans out one task per resolved value, and
// aggregateName names the step whose per-value outputs combine into one output.
// A plain sequential step yields one task; a parallel group one task per member.
func buildGroupTasks(group stepGroup, sc substContext) (tasks []stepTask, aggregateName string, err error) {
	if len(group.steps) == 1 && group.steps[0].Matrix != nil {
		ws := group.steps[0]
		values, verr := resolveMatrixValues(ws.Matrix, sc)
		if verr != nil {
			return nil, "", verr
		}
		for _, val := range values {
			tasks = append(tasks, stepTask{
				step:      ws.Step,
				stepIndex: group.indices[0],
				name:      matrixTaskName(ws.Name, ws.Matrix.Var, val),
				matrix:    map[string]string{ws.Matrix.Var: val},
			})
		}
		return tasks, ws.Name, nil
	}
	for j, ws := range group.steps {
		tasks = append(tasks, stepTask{step: ws.Step, stepIndex: group.indices[j], name: ws.Name})
	}
	return tasks, "", nil
}

// groupConcurrency reports how many of a group's tasks may run at once. A matrix step
// runs defaultFanoutConcurrency legs at once by default, raised to MaxConcurrent when
// set (capped at the maxParallelSteps ceiling), or pinned to 1 when Sequential. Plain
// parallel and sequential step groups always use the full ceiling.
func groupConcurrency(group stepGroup) int {
	if len(group.steps) == 1 && group.steps[0].Matrix != nil {
		m := group.steps[0].Matrix
		switch {
		case m.Sequential:
			return 1
		case m.MaxConcurrent > 0:
			return min(m.MaxConcurrent, maxParallelSteps)
		default:
			return defaultFanoutConcurrency
		}
	}
	return maxParallelSteps
}

// resolveMatrixValues produces the matrix's value list at run time, resolving any
// ${...} references first. ValuesFrom pulls the list from a reference yielding a
// JSON array or comma-separated string; otherwise each literal Value is resolved.
func resolveMatrixValues(m *MatrixConfig, sc substContext) ([]string, error) {
	var values []string
	if m.ValuesFrom != "" {
		values = parseMatrixList(substitute(m.ValuesFrom, sc))
	} else {
		values = make([]string, 0, len(m.Values))
		for _, v := range m.Values {
			values = append(values, substitute(v, sc))
		}
	}
	if len(values) > maxMatrixValues {
		return nil, fmt.Errorf("matrix expands to %d values, exceeding the limit of %d", len(values), maxMatrixValues)
	}
	return values, nil
}

// parseMatrixList interprets a resolved values_from string as either a JSON array
// (of strings or scalars) or a flat list. The list form accepts BOTH the pipeline
// convention (comma-separated) and the linux convention (whitespace-separated: the
// natural shape of command output like `ls` or `echo a b c`), so a step that emits
// a space- or newline-delimited list feeds a matrix directly. Any run of commas
// and/or whitespace separates values and empty entries are dropped; values that
// contain internal whitespace must use the JSON-array form to survive intact.
func parseMatrixList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []any
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				if s := jsonScalar(v); s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

func matrixTaskName(base, varName, val string) string {
	return fmt.Sprintf("%s [%s=%s]", base, varName, val)
}

// aggregateTaskOutputs combines a matrix step's per-value outputs into one
// JSON-array string, preserving the matrix value order so ${steps.NAME.output}
// returns ["out0","out1",...].
func aggregateTaskOutputs(results []taskResult) string {
	outs := make([]string, len(results))
	for _, r := range results {
		outs[r.idx] = r.output
	}
	b, _ := json.Marshal(outs)
	return string(b)
}

// runTaskGroup executes every task in a group concurrently (capped by concurrency,
// which is the global maxParallelSteps ceiling or a matrix step's lower
// MaxConcurrent), records each as a step run, and returns the per-task results in
// task order plus the group's overall status (Completed unless any task failed or
// was cancelled). A single task still runs through this path so the matrix,
// parallel, and sequential cases share one code path.
//
// legSem is the run-wide leg budget, shared with every other node in the frontier
// and with scatter; concurrency is this node's own cap within that budget. A task
// takes legSem first, then the node's own slot, so a task holding a node slot only
// ever waits on legs that are themselves making progress.
func (p *WorkerPool) runTaskGroup(ctx context.Context, store *tokenStore, runID, workflowID string, tasks []stepTask, inputs, visible map[string]string, depth int, concurrency int, legSem chan struct{}) ([]taskResult, string) {
	results := make([]taskResult, len(tasks))
	stepRunIDs := make([]string, len(tasks))
	for k, t := range tasks {
		sid := uuid.New().String()
		stepRunIDs[k] = sid
		if err := p.startStepRun(runID, sid, t.stepIndex, t.name); err != nil {
			slog.ErrorContext(ctx, "worker: start step run", "run_id", runID, "step", t.stepIndex, "error", err)
			return results, StatusFailed
		}
	}

	resCh := make(chan taskResult, len(tasks))
	// sem is a counting semaphore: acquire by sending, release by receiving. The
	// select lets a cancelled context bypass the semaphore so the goroutine exits
	// immediately rather than blocking on a full channel. concurrency is clamped to
	// at least 1 so a stray zero can never make the semaphore block forever.
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	for k, t := range tasks {
		go func(k int, t stepTask) {
			if !acquireLeg(ctx, legSem) {
				resCh <- taskResult{name: t.name, idx: k, err: context.Canceled}
				return
			}
			defer releaseLeg(legSem)
			select {
			case sem <- struct{}{}: // acquire
			case <-ctx.Done():
				resCh <- taskResult{name: t.name, idx: k, err: context.Canceled}
				return
			}
			defer func() { <-sem }() // release
			res, err := p.executeStep(ctx, store, t.step, substContext{inputs: inputs, outputs: visible, matrix: t.matrix, mapVars: t.mapVars, runID: runID, workflowID: workflowID, depth: depth})
			resCh <- taskResult{name: t.name, output: res.Output, logs: res.Logs, usedMB: res.MemoryUsedMB, limitMB: res.MemoryLimitMB, err: err, idx: k}
		}(k, t)
	}

	status := StatusCompleted
	for range tasks {
		r := <-resCh
		results[r.idx] = r
		sid := stepRunIDs[r.idx]
		if r.err != nil {
			st := StatusFailed
			if errors.Is(r.err, context.Canceled) {
				st = StatusCancelled
				if status == StatusCompleted {
					status = StatusCancelled
				}
			} else {
				status = StatusFailed
			}
			p.finishStepRun(sid, st, strPtr(failureOutput(r.output, r.err)), strPtrOrNil(r.logs), r.usedMB, r.limitMB)
			slog.WarnContext(ctx, "worker: step failed", "run_id", runID, "step", tasks[r.idx].stepIndex, "name", r.name)
		} else {
			meterStepsCompleted.Add(ctx, 1, metric.WithAttributes(
				attribute.String("workflow.id", workflowID),
				attribute.String("status", StatusCompleted),
			))
			p.finishStepRun(sid, StatusCompleted, strPtr(r.output), strPtrOrNil(r.logs), r.usedMB, r.limitMB)
			slog.InfoContext(ctx, "worker: step completed", "run_id", runID, "step", tasks[r.idx].stepIndex, "name", r.name)
		}
	}
	return results, status
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
	Output string
	// Logs is the action's stdout, captured on every terminal outcome for display in
	// the run view (independent of Output, which on success is the consumable
	// output_env map). See WorkflowStepRun.Logs.
	Logs          string
	MemoryUsedMB  *int64
	MemoryLimitMB *int64
}

// asyncLogs returns the action's stdout (its OutputField, e.g. forge's "stdout")
// from a poll response, for display as the step's execution log. Captured on every
// terminal state — success, failure, and cancel — so a step's stdout is always
// viewable in the run view regardless of outcome. Empty when the action declares no
// stdout field (e.g. the http escape hatch) or the field is absent/non-string.
func asyncLogs(async *AsyncConfig, result map[string]any) string {
	if async == nil || async.OutputField == "" {
		return ""
	}
	s, _ := result[async.OutputField].(string)
	return s
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
		// The remote job enforces its own timeout (e.g. forge kills the container at
		// `timeout` seconds), so forward the step's timeout in the request body —
		// otherwise the service falls back to its own default (forge: 30s) and a
		// longer step is killed early. Respect an explicit with.timeout if present.
		if with == nil {
			with = map[string]any{}
		}
		if _, ok := with["timeout"]; !ok {
			with["timeout"] = timeout
		}
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(budget)*time.Second)
	defer cancel()
	return p.executeAction(stepCtx, store, def, with, sc.runID, sc.depth)
}

// executeAction dispatches a catalog action: applies body transforms, sends the
// HTTP request, then polls if the action is async. parentRunID/depth identify the
// run this action executes within; they are forwarded as X-Workflow-Parent-Run /
// X-Workflow-Run-Depth so a workflows/trigger sub-run is created one level deeper
// (harmless headers for every other action's target service).
func (p *WorkerPool) executeAction(ctx context.Context, store *tokenStore, def ActionDef, with map[string]any, parentRunID string, depth int) (stepResult, error) {
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
	// Sub-pipeline nesting context, read by the workflows/trigger create endpoint.
	req.Header.Set("X-Workflow-Run-Depth", strconv.Itoa(depth))
	if parentRunID != "" {
		req.Header.Set("X-Workflow-Parent-Run", parentRunID)
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

		// A 401/403 on the poll is deterministic for a fixed run token — it will never
		// become authorized, so spinning until the budget is a silent hang. Fail fast
		// with the missing-permission context instead. (The async-poll read grant is
		// derived by collectWorkflowPermissions and must exist as a default grant, or
		// the no-escalation filter drops it from the run role — the bug this guards.)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return stepResult{}, fmt.Errorf("%s: poll returned %d — the run role lacks read permission for the submitted job: %s",
				def.Name, resp.StatusCode, strings.TrimSpace(string(body)))
		}

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
				// When the action declares a structured output map (e.g. forge's
				// captured output_env), the SUCCESS output comes ONLY from that map so
				// later steps read ${...output.KEY}. Raw stdout is never a consumable
				// step output for these actions — an empty map means no output. The
				// command's stdout is still captured separately as the step's display
				// logs (asyncLogs below).
				if def.Async.OutputMapField != "" {
					if m, ok := result[def.Async.OutputMapField].(map[string]any); ok && len(m) > 0 {
						if b, mErr := json.Marshal(m); mErr == nil {
							out = string(b)
						}
					}
				} else if def.Async.OutputField != "" {
					if v, ok := result[def.Async.OutputField]; ok {
						out, _ = v.(string)
					}
				}
				return stepResult{Output: out, Logs: asyncLogs(def.Async, result), MemoryUsedMB: used, MemoryLimitMB: limit}, nil
			}
		}
		for _, s := range def.Async.FailureStates {
			if status == s {
				// Collect the action's own failure reason from its error fields (e.g.
				// forge's stderr, or its "command not found" diagnostic).
				var errDetails []string
				for _, f := range def.Async.ErrorFields {
					if v, ok := result[f]; ok {
						if sv, ok := v.(string); ok && sv != "" {
							errDetails = append(errDetails, sv)
						}
					}
				}
				exitCode := result["exit_code"]
				// Surface the action's reported reason as the step error so the run
				// shows the same message the backing service did (e.g. forge: command
				// "sh" not found …). Fall back to the generic exit-code summary only
				// when the action reported no detail.
				err := fmt.Errorf("%s %s (exit code: %v)", def.Name, status, exitCode)
				if detail := strings.TrimSpace(strings.Join(errDetails, "\n")); detail != "" {
					err = fmt.Errorf("%s %s: %s", def.Name, status, detail)
				}
				// stdout goes to Logs (shown for every outcome); Output is left empty so
				// failureOutput surfaces the error reason alone, not stdout duplicated.
				return stepResult{Logs: asyncLogs(def.Async, result), MemoryUsedMB: used, MemoryLimitMB: limit}, err
			}
		}
		for _, s := range def.Async.CancelStates {
			if status == s {
				// Carry memory through on cancel too — a step cancelled after an
				// OOM/timeout still has meaningful usage figures — and the stdout it
				// produced before cancellation, so a cancelled step's logs are viewable.
				return stepResult{Logs: asyncLogs(def.Async, result), MemoryUsedMB: used, MemoryLimitMB: limit}, context.Canceled
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

// startApprovalStepRun records an approval gate paused for a human decision. The
// substituted approval message (if any) is stored as the step's output so the run
// view can show what is being approved.
func (p *WorkerPool) startApprovalStepRun(runID, stepRunID string, index int, name, message string) error {
	return (WorkflowStepRun{StepRunID: stepRunID, RunID: runID, StepIndex: index, StepName: name}).AddAwaitingApproval(message)
}

func (p *WorkerPool) finishStepRun(stepRunID, status string, output, logs *string, usedMB, limitMB *int64) {
	(WorkflowStepRun{StepRunID: stepRunID}).Complete(context.Background(), status, output, logs, usedMB, limitMB)
}

func (p *WorkerPool) failRun(runID, sessionID string) {
	(WorkflowRun{RunID: runID}).Complete(context.Background(), StatusFailed, nil)
	revokeRunToken(context.Background(), sessionID)
	slog.Warn("worker: run failed before first step", "run_id", runID)
}

func strPtr(s string) *string { return &s }

// strPtrOrNil returns nil for an empty string so the column stays NULL (rather
// than storing ""), matching how an absent value reads back as omitted JSON.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// failureOutput combines a failed (or cancelled) step's captured Output — an HTTP
// response body, or any action output that is not surfaced separately as Logs —
// with its error summary, so the run view surfaces the real failure detail instead
// of only "<action> failed (exit code: N)". The error trails the captured output as
// a footer (and stands alone when the step produced none — e.g. forge, whose stdout
// is carried in Logs, so Output is empty here and only the error reason shows).
func failureOutput(output string, err error) string {
	msg := err.Error()
	if output = strings.TrimRight(output, "\n"); output == "" {
		return msg
	}
	return output + "\n\n" + msg
}
