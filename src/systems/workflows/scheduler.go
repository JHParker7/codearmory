package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// The frontier scheduler: executes a run by walking the workflow graph, rather
// than by iterating precomputed batches.
//
// The pre-graph engine grouped steps with groupSteps and ran a plain `for range`
// over the result — a static plan computed before the first step ran, with no
// readiness check and no way to hang a predicate on a transition. This replaces
// that loop with a readiness-based frontier so that edges can carry conditions.
//
// For a workflow with no explicit routes the edges are derived from
// parallel_group (deriveRoutes), and the observable behaviour is identical to the
// batch engine — that equivalence is what the existing test suite pins down.

// nodeState is a node's lifecycle state within one run.
type nodeState string

const (
	nodePending   nodeState = "pending"
	nodeRunning   nodeState = "running"
	nodeCompleted nodeState = "completed"
	nodeFailed    nodeState = "failed"
	// nodeSkipped means every inbound route resolved and none was taken, so the
	// node will never run. It is distinct from failed because a join must be able
	// to tell "my branch wasn't chosen" from "my branch broke" — see readyNodes.
	nodeSkipped nodeState = "skipped"
	// nodeAwaiting is an approval gate that has been reached. The run does not
	// pause here: the branch parks and the rest of the frontier keeps draining.
	nodeAwaiting  nodeState = "awaiting"
	nodeCancelled nodeState = "cancelled"
)

// terminal reports whether a node has reached a state from which its outbound
// routes can be resolved. An awaiting gate is NOT terminal — it is parked.
func (s nodeState) terminal() bool {
	switch s {
	case nodeCompleted, nodeFailed, nodeSkipped, nodeCancelled:
		return true
	}
	return false
}

// edgeState tracks whether a route has been decided.
type edgeState int

const (
	edgePending edgeState = iota
	edgeTaken
	edgeNotTaken
)

// runState is the mutable state of one run's traversal.
//
// It is owned exclusively by the scheduler goroutine: every read and write of
// nodes, edges, and outputs happens there. Worker goroutines receive an immutable
// snapshot at launch and return a result over a channel, so no mutex is needed —
// structurally the same discipline the batch engine used, just at node granularity
// instead of group granularity.
type runState struct {
	nodes   map[string]nodeState
	edges   []edgeState
	outputs map[string]string
}

func newRunState(g *workflowGraph, outputs map[string]string, completed map[string]bool) *runState {
	st := &runState{
		nodes:   make(map[string]nodeState, len(g.steps)),
		edges:   make([]edgeState, len(g.routes)),
		outputs: outputs,
	}
	for _, ws := range g.steps {
		if completed[ws.Name] {
			st.nodes[ws.Name] = nodeCompleted
		} else {
			st.nodes[ws.Name] = nodePending
		}
	}
	return st
}

// nodeResult is one finished node, reported back to the scheduler goroutine.
type nodeResult struct {
	name   string
	state  nodeState
	output string
}

// resolveOutbound decides every outbound route of a node that has just reached a
// terminal state, then cascades: any successor whose inbound routes are now all
// resolved with none taken is itself skipped, which resolves ITS outbound routes,
// and so on.
//
// Resolution rules:
//   - a skipped or cancelled source takes none of its routes, without evaluating
//     any condition: it produced no output, so a predicate over its status would
//     be answering a question about a node that never ran;
//   - an empty condition is taken iff the source completed — the default that
//     reproduces the linear semantics;
//   - otherwise the condition decides. This is how failure routing works: a route
//     with `when: steps.build.status == "failed"` sees the failed status here, so
//     continuing past a failure is strictly opt-in.
func (p *WorkerPool) resolveOutbound(ctx context.Context, g *workflowGraph, st *runState, runID string, inputs map[string]string, name string) {
	queue := []string{name}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		src := st.nodes[n]
		for _, ri := range g.out[n] {
			if st.edges[ri] != edgePending {
				continue
			}
			r := g.routes[ri]
			st.edges[ri] = p.decideRoute(ctx, g, st, runID, inputs, r, ri, src)
			// The successor may now be fully resolved with nothing taken.
			if to := r.To; st.nodes[to] == nodePending && g.allResolved(st, to) && !g.anyTaken(st, to) {
				st.nodes[to] = nodeSkipped
				queue = append(queue, to)
			}
		}
	}
}

// decideRoute resolves a single route given its source node's terminal state.
func (p *WorkerPool) decideRoute(ctx context.Context, g *workflowGraph, st *runState, runID string, inputs map[string]string, r WorkflowRoute, ri int, src nodeState) edgeState {
	if src == nodeSkipped || src == nodeCancelled {
		return edgeNotTaken
	}
	if r.When == "" {
		if src == nodeCompleted {
			return edgeTaken
		}
		return edgeNotTaken
	}
	prog := g.programs[ri]
	ok, err := evalWhen(prog, buildExprEnv(inputs, runID, st.nodes, st.outputs))
	if err != nil {
		// A condition that cannot be evaluated must not silently vanish: record a
		// visible failed step run naming the route, and leave the edge not-taken.
		slog.WarnContext(ctx, "worker: route condition failed", "run_id", runID, "from", r.From, "to", r.To, "error", err)
		if sid := uuid.New().String(); p.startStepRun(runID, sid, g.index[r.To], "route "+r.From+"->"+r.To) == nil {
			p.finishStepRun(sid, StatusFailed, strPtr(err.Error()), nil, nil, nil)
		}
		return edgeNotTaken
	}
	if ok {
		return edgeTaken
	}
	return edgeNotTaken
}

// allResolved reports whether every inbound route of a node has been decided.
func (g *workflowGraph) allResolved(st *runState, node string) bool {
	for _, ri := range g.in[node] {
		if st.edges[ri] == edgePending {
			return false
		}
	}
	return true
}

// anyTaken reports whether at least one inbound route of a node was taken.
// An entry node (no inbound routes) is vacuously "not taken", so readyNodes
// special-cases it.
func (g *workflowGraph) anyTaken(st *runState, node string) bool {
	for _, ri := range g.in[node] {
		if st.edges[ri] == edgeTaken {
			return true
		}
	}
	return false
}

// readyNodes returns the pending nodes that may launch now.
//
// The join rule is ALL-INBOUND-RESOLVED, AT-LEAST-ONE-TAKEN. The "resolved"
// half rather than "completed" is what makes a conditional diamond work: given
// A->B (when x), A->C (when !x), B->D, C->D, exactly one of B/C runs and the
// other is skipped; C->D then resolves not-taken, B->D is taken, and D sees all
// inbound resolved with one taken, so D runs. Under an all-inbound-COMPLETED
// rule D would wait on C forever.
func (g *workflowGraph) readyNodes(st *runState) []string {
	var ready []string
	for _, ws := range g.steps {
		n := ws.Name
		if st.nodes[n] != nodePending {
			continue
		}
		if len(g.in[n]) == 0 {
			ready = append(ready, n) // entry node
			continue
		}
		if g.allResolved(st, n) && g.anyTaken(st, n) {
			ready = append(ready, n)
		}
	}
	return ready
}

// awaitingNodes returns the parked approval gates, in array order.
func (g *workflowGraph) awaitingNodes(st *runState) []string {
	var out []string
	for _, ws := range g.steps {
		if st.nodes[ws.Name] == nodeAwaiting {
			out = append(out, ws.Name)
		}
	}
	return out
}

// statusPaused is an internal sentinel: the frontier drained with at least one
// approval gate parked, so the caller pauses the run instead of completing it.
const statusPaused = "paused"

// runGraph is the frontier loop. It returns a terminal run status, or statusPaused
// when the run stopped on one or more approval gates.
func (p *WorkerPool) runGraph(ctx context.Context, g *workflowGraph, st *runState, store *tokenStore, runID, workflowID string, inputs map[string]string, depth int) string {
	finalStatus := StatusCompleted
	resCh := make(chan nodeResult, len(g.steps))
	// One run-wide leg semaphore. The batch engine's maxParallelSteps was a true
	// ceiling only because a matrix step was always alone in its group; a frontier
	// of N nodes each fanning out would otherwise multiply it. Legs are leaves —
	// they never wait on another leg — so a shared semaphore cannot deadlock.
	legSem := make(chan struct{}, maxParallelSteps)
	inFlight := 0

	// Seed: a resumed run's already-completed nodes must resolve their outbound
	// routes before the first launch, so the frontier opens at the resume point.
	for _, ws := range g.steps {
		if st.nodes[ws.Name] == nodeCompleted {
			p.resolveOutbound(ctx, g, st, runID, inputs, ws.Name)
		}
	}

	for {
		if ctx.Err() == nil {
			for _, n := range g.readyNodes(st) {
				ws := g.steps[g.index[n]]
				// An approval gate parks its branch; it does NOT pause the run here.
				// Pausing with siblings in flight would abandon their goroutines,
				// orphan their step runs in `running`, and re-execute them on resume.
				if ws.Action == ActionApproval {
					st.nodes[n] = nodeAwaiting
					continue
				}
				st.nodes[n] = nodeRunning
				visible := g.visibleFor(n, st.outputs)
				idx := g.index[n]
				inFlight++
				go func(n string, ws WorkflowStep, idx int, visible map[string]string) {
					resCh <- p.runNode(ctx, store, runID, workflowID, ws, idx, inputs, visible, depth, legSem)
				}(n, ws, idx, visible)
			}
			p.publishCurrentStep(ctx, g, st, runID)
		} else if finalStatus == StatusCompleted {
			finalStatus = StatusCancelled
		}

		if inFlight == 0 {
			// The frontier is drained: everything runnable has finished, or the run
			// was cancelled and the in-flight work has been collected.
			break
		}

		// Collect. Never exit with goroutines outstanding — Complete() would write a
		// terminal run status while step runs were still being written.
		r := <-resCh
		inFlight--
		st.nodes[r.name] = r.state
		if r.state == nodeCompleted {
			st.outputs[r.name] = r.output
		}
		switch r.state {
		case nodeFailed:
			// A routed failure handler does not turn a red run green.
			finalStatus = StatusFailed
		case nodeCancelled:
			if finalStatus == StatusCompleted {
				finalStatus = StatusCancelled
			}
		}
		p.resolveOutbound(ctx, g, st, runID, inputs, r.name)
	}

	if finalStatus == StatusCompleted && len(g.awaitingNodes(st)) > 0 {
		return statusPaused
	}
	return finalStatus
}

// publishCurrentStep keeps WorkflowRun.CurrentStep meaningful as a progress hint:
// the lowest step index among running nodes. A graph has a frontier rather than a
// single current step, but the portal and CLI only use this for a progress line,
// and for a linear pipeline it is exactly the old value.
func (p *WorkerPool) publishCurrentStep(ctx context.Context, g *workflowGraph, st *runState, runID string) {
	lowest := -1
	for _, ws := range g.steps {
		if st.nodes[ws.Name] == nodeRunning {
			if i := g.index[ws.Name]; lowest < 0 || i < lowest {
				lowest = i
			}
		}
	}
	if lowest >= 0 {
		(WorkflowRun{RunID: runID}).SetCurrentStep(ctx, lowest)
	}
}

// runNode executes one graph node and reports its terminal state.
//
// A node is one step occurrence; its matrix/scatter fan-out is INTERNAL to it (all
// legs share the node's step index and re-collapse into one output), so the
// scheduler never sees legs. Wrapping the node in a single-member stepGroup lets
// buildGroupTasks, groupConcurrency, and runTaskGroup be reused verbatim.
func (p *WorkerPool) runNode(ctx context.Context, store *tokenStore, runID, workflowID string, ws WorkflowStep, idx int, inputs, visible map[string]string, depth int, legSem chan struct{}) nodeResult {
	// Scatter owns its whole resolve/clone/run/gather orchestration.
	if ws.Scatter != nil {
		output, status := p.runScatterGroup(ctx, store, runID, ws, idx, inputs, visible, depth, legSem)
		return nodeResult{name: ws.Name, state: statusToNodeState(status), output: output}
	}

	group := stepGroup{steps: []WorkflowStep{ws}, indices: []int{idx}}
	tasks, aggregateName, terr := buildGroupTasks(group, substContext{inputs: inputs, outputs: visible, runID: runID})
	if terr != nil {
		if sid := uuid.New().String(); p.startStepRun(runID, sid, idx, ws.Name) == nil {
			p.finishStepRun(sid, StatusFailed, strPtr(terr.Error()), nil, nil, nil)
		}
		slog.WarnContext(ctx, "worker: matrix expansion failed", "run_id", runID, "step", idx, "error", terr)
		return nodeResult{name: ws.Name, state: nodeFailed}
	}
	if len(tasks) == 0 {
		// A matrix that fanned out to zero did no work. Failing is deliberate:
		// silently dropping the step would leave it absent from the run view while
		// the run still showed as passed.
		if sid := uuid.New().String(); p.startStepRun(runID, sid, idx, ws.Name) == nil {
			p.finishStepRun(sid, StatusFailed, strPtr("matrix produced no values to run"), nil, nil, nil)
		}
		slog.WarnContext(ctx, "worker: matrix produced no values, failing run", "run_id", runID, "step", idx)
		return nodeResult{name: ws.Name, state: nodeFailed}
	}

	results, status := p.runTaskGroup(ctx, store, runID, workflowID, tasks, inputs, visible, depth, groupConcurrency(group), legSem)
	if status != StatusCompleted {
		return nodeResult{name: ws.Name, state: statusToNodeState(status)}
	}
	// A matrix step's per-value executions combine into one JSON-array output under
	// the base step name; a plain step publishes its single output.
	if aggregateName != "" {
		return nodeResult{name: ws.Name, state: nodeCompleted, output: aggregateTaskOutputs(results)}
	}
	return nodeResult{name: ws.Name, state: nodeCompleted, output: results[0].output}
}

// pauseAtGates records a step run for every parked gate and pauses the run.
//
// It runs only once the frontier has drained, so nothing is in flight and every
// node that could run has been recorded. That ordering is what makes resume safe:
// pausing while siblings were still executing would abandon their goroutines,
// leave their step runs stuck in `running`, and re-execute them on resume — a
// double deploy for any non-idempotent step.
//
// A run can park on more than one gate at once (two concurrent branches), which is
// why every gate gets its own awaiting step run.
func (p *WorkerPool) pauseAtGates(ctx context.Context, g *workflowGraph, st *runState, runID string, inputs map[string]string) error {
	awaiting := g.awaitingNodes(st)
	lowest := -1
	for _, n := range awaiting {
		i := g.index[n]
		if lowest < 0 || i < lowest {
			lowest = i
		}
		ws := g.steps[i]
		msg := substitute(withString(ws.With, "message"), substContext{
			inputs:  inputs,
			outputs: g.visibleFor(n, st.outputs),
			runID:   runID,
		})
		if err := p.startApprovalStepRun(runID, uuid.New().String(), i, ws.Name, msg); err != nil {
			return err
		}
	}
	return (WorkflowRun{RunID: runID}).PauseForApproval(ctx, lowest)
}

// acquireLeg takes a slot from the run-wide leg budget, returning false when the
// run was cancelled while waiting. A nil semaphore means "unbudgeted", which keeps
// callers that construct a task group directly (tests) working.
func acquireLeg(ctx context.Context, legSem chan struct{}) bool {
	if legSem == nil {
		return ctx.Err() == nil
	}
	select {
	case legSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func releaseLeg(legSem chan struct{}) {
	if legSem != nil {
		<-legSem
	}
}

// statusToNodeState maps a step-group status onto the node's state.
func statusToNodeState(status string) nodeState {
	switch status {
	case StatusCompleted:
		return nodeCompleted
	case StatusCancelled:
		return nodeCancelled
	default:
		return nodeFailed
	}
}
