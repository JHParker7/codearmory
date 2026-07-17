package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// The frontier scheduler: executes a run by walking the workflow graph, rather
// than by iterating precomputed batches.
//
// The pre-graph engine ran a plain `for range` over precomputed batches — a static
// plan fixed before the first step ran, with no readiness check and no way to hang a
// predicate on a transition. This replaces that loop with a readiness-based frontier
// so that edges can carry conditions.
//
// A workflow with no explicit routes has its edges derived as a chain in array order
// (deriveRoutes), so a plain sequence still behaves exactly as it always did.

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
	// ticket mirrors the run's progress into a ticket, and is non-nil ONLY on the
	// top-level state. A map region's iterations build their own runState
	// (runIteration), so an iteration structurally cannot comment: a 15-value map
	// posts the 2 comments its body is worth, not 30.
	ticket *ticketReporter
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

// nodeResult is one finished unit, reported back to the scheduler goroutine: either
// a single node (name/output) or a whole map region (region/regionOutputs).
type nodeResult struct {
	name   string
	state  nodeState
	output string
	// region is set when this result is a map region rather than one node; every
	// member then takes `state`, and regionOutputs carries each member's aggregated
	// output across iterations.
	region        *mapRegion
	regionOutputs map[string]string
	// legs is how many parallel executions the node expanded into (matrix/scatter
	// legs, or a map region's iterations). 0/1 means it ran once. Reported to the
	// run's ticket so "build (15 legs) completed" reads as what actually happened.
	legs int
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
		if sid := uuid.New().String(); p.startStepRun(runID, sid, g.stepIndex(r.To), "route "+r.From+"->"+r.To) == nil {
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

// nodeReady is the join rule: ALL-INBOUND-RESOLVED, AT-LEAST-ONE-TAKEN.
//
// "Resolved" rather than "completed" is what makes a conditional diamond work:
// given A->B (when x), A->C (when !x), B->D, C->D, exactly one of B/C runs and the
// other is skipped; C->D then resolves not-taken, B->D is taken, and D sees all
// inbound resolved with one taken, so D runs. Under an all-inbound-COMPLETED rule D
// would wait on C forever.
func (g *workflowGraph) nodeReady(st *runState, n string) bool {
	if st.nodes[n] != nodePending {
		return false
	}
	if len(g.in[n]) == 0 {
		return true // entry node
	}
	return g.allResolved(st, n) && g.anyTaken(st, n)
}

// readyNodes returns the pending non-region nodes that may launch now.
func (g *workflowGraph) readyNodes(st *runState) []string {
	var ready []string
	for _, ws := range g.steps {
		if g.regionOf[ws.Name] != "" {
			continue // region members launch as a unit, via readyRegions
		}
		if g.nodeReady(st, ws.Name) {
			ready = append(ready, ws.Name)
		}
	}
	return ready
}

// boundary splits a region's inbound route indices (from outside in) from the rest.
// Routes wholly inside the region belong to an iteration's subgraph, not to the
// region's own readiness.
func (g *workflowGraph) inboundOf(region *mapRegion) []int {
	inside := make(map[string]bool, len(region.nodes))
	for _, n := range region.nodes {
		inside[n] = true
	}
	var in []int
	for _, n := range region.nodes {
		for _, ri := range g.in[n] {
			if !inside[g.routes[ri].From] {
				in = append(in, ri)
			}
		}
	}
	return in
}

// regionReady reports whether a map region may expand now.
//
// A region is ONE super-node: it waits on the routes crossing INTO it, using the same
// all-resolved/at-least-one-taken rule a node does. With no external inbound routes it
// is an entry unit, ready at t=0.
func (g *workflowGraph) regionReady(st *runState, region *mapRegion) bool {
	pending := false
	for _, n := range region.nodes {
		if st.nodes[n] == nodePending {
			pending = true
		} else {
			return false // already expanded (or skipped): never re-enter
		}
	}
	if !pending {
		return false
	}
	inbound := g.inboundOf(region)
	if len(inbound) == 0 {
		return true
	}
	taken := false
	for _, ri := range inbound {
		switch st.edges[ri] {
		case edgePending:
			return false
		case edgeTaken:
			taken = true
		}
	}
	return taken
}

// regionSkipped reports whether every route into a region resolved with none taken,
// so the whole region is skipped — the region-level twin of a skipped node.
func (g *workflowGraph) regionSkipped(st *runState, region *mapRegion) bool {
	for _, n := range region.nodes {
		if st.nodes[n] != nodePending {
			return false
		}
	}
	inbound := g.inboundOf(region)
	if len(inbound) == 0 {
		return false
	}
	for _, ri := range inbound {
		if st.edges[ri] != edgeNotTaken {
			return false
		}
	}
	return true
}

// readyRegions returns the map regions that may expand now.
func (g *workflowGraph) readyRegions(st *runState) []*mapRegion {
	var out []*mapRegion
	for _, ws := range g.steps {
		id := g.regionOf[ws.Name]
		if id == "" {
			continue
		}
		r := g.regions[id]
		// Offer each region once: only when the iteration reaches its first node.
		if r == nil || r.nodes[0] != ws.Name {
			continue
		}
		if g.regionReady(st, r) {
			out = append(out, r)
		}
	}
	return out
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

// iterCtx scopes a subgraph run to one map iteration: its ${map.*} bindings, how its
// step runs are labelled, and the clone volume its steps run against. The zero value
// is the main graph — no iteration, no relabelling, no injected volume.
type iterCtx struct {
	mapVars map[string]string
	label   func(base string) string
	volume  map[string]any
}

// name applies the iteration's labelling to a step name.
func (ic iterCtx) name(base string) string {
	if ic.label == nil {
		return base
	}
	return ic.label(base)
}

// runGraph is the frontier loop. It returns a terminal run status, or statusPaused
// when the run stopped on one or more approval gates.
//
// It is re-entered per map iteration (with the region's subgraph and an iterCtx), so
// a region's body branches and joins with exactly the main graph's semantics.
func (p *WorkerPool) runGraph(ctx context.Context, g *workflowGraph, st *runState, store *tokenStore, runID, workflowID string, inputs map[string]string, depth int, ic iterCtx, legSem chan struct{}) string {
	finalStatus := StatusCompleted
	resCh := make(chan nodeResult, len(g.steps)+1)
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
			// Map regions expand as one unit: every member launches together and the
			// region's successors wait for all its iterations.
			for _, region := range g.readyRegions(st) {
				for _, n := range region.nodes {
					st.nodes[n] = nodeRunning
				}
				visible := g.visibleForRegion(region, st.outputs)
				inFlight++
				go func(region *mapRegion, visible map[string]string) {
					agg, status, iters := p.runMapRegion(ctx, store, runID, workflowID, g, region, inputs, visible, depth, legSem)
					resCh <- nodeResult{region: region, state: statusToNodeState(status), regionOutputs: agg, legs: iters}
				}(region, visible)
			}
			for _, n := range g.readyNodes(st) {
				ws := g.steps[g.index[n]]
				// An approval gate parks its branch; it does NOT pause the run here.
				// Pausing with siblings in flight would abandon their goroutines,
				// orphan their step runs in `running`, and re-execute them on resume.
				if ws.Action == ActionApproval || ws.Approval != nil {
					st.nodes[n] = nodeAwaiting
					continue
				}
				st.nodes[n] = nodeRunning
				visible := g.visibleFor(n, st.outputs)
				idx := g.stepIndex(n)
				inFlight++
				go func(n string, ws WorkflowStep, idx int, visible map[string]string) {
					resCh <- p.runNode(ctx, store, runID, workflowID, ws, idx, inputs, visible, depth, ic, legSem)
				}(n, ws, idx, visible)
			}
			p.skipSkippedRegions(ctx, g, st, runID, inputs)
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
		// A region reports once for all its members: they share its state, and each
		// publishes the JSON array of that node's output across iterations — the same
		// shape a matrix step publishes, so a downstream ${steps.build.output} reads
		// uniformly whether build was mapped or not.
		names := []string{r.name}
		if r.region != nil {
			names = r.region.nodes
			for n, out := range r.regionOutputs {
				st.outputs[n] = out
			}
		}
		for _, n := range names {
			st.nodes[n] = r.state
			if r.region == nil && r.state == nodeCompleted {
				st.outputs[n] = r.output
			}
			st.ticket.stepDone(ctx, n, r.state, r.legs)
		}
		// A title naming a step's output (the commit a checkout resolved) can only be
		// rendered once that step has produced it. Cheap: a no-op unless the title is
		// still pending.
		st.ticket.retitle(ctx, inputs, st.outputs)
		switch r.state {
		case nodeFailed:
			// A routed failure handler does not turn a red run green.
			finalStatus = StatusFailed
		case nodeCancelled:
			if finalStatus == StatusCompleted {
				finalStatus = StatusCancelled
			}
		}
		for _, n := range names {
			p.resolveOutbound(ctx, g, st, runID, inputs, n)
		}
	}

	if finalStatus == StatusCompleted && len(g.awaitingNodes(st)) > 0 {
		return statusPaused
	}
	return finalStatus
}

// skipSkippedRegions marks every member of a region whose inbound routes all resolved
// not-taken, then cascades — the region-level twin of a skipped node, so a map behind
// an untaken branch does not wedge the frontier.
func (p *WorkerPool) skipSkippedRegions(ctx context.Context, g *workflowGraph, st *runState, runID string, inputs map[string]string) {
	for _, r := range g.regions {
		if !g.regionSkipped(st, r) {
			continue
		}
		for _, n := range r.nodes {
			st.nodes[n] = nodeSkipped
		}
		for _, n := range r.nodes {
			p.resolveOutbound(ctx, g, st, runID, inputs, n)
		}
	}
}

// visibleForRegion is the output view a map region's iterations start from: the union
// of its members' ancestors, minus the region's own nodes (an iteration's own outputs
// are produced inside it). This is what lets a body reference a step upstream of the
// map, e.g. ${steps.discover.output}.
func (g *workflowGraph) visibleForRegion(region *mapRegion, outputs map[string]string) map[string]string {
	inside := make(map[string]bool, len(region.nodes))
	for _, n := range region.nodes {
		inside[n] = true
	}
	v := map[string]string{}
	for _, n := range region.nodes {
		for a := range g.ancestors[n] {
			if inside[a] {
				continue
			}
			if out, ok := outputs[a]; ok {
				v[a] = out
			}
		}
	}
	return v
}

// publishCurrentStep keeps WorkflowRun.CurrentStep meaningful as a progress hint:
// the lowest step index among running nodes. A graph has a frontier rather than a
// single current step, but the portal and CLI only use this for a progress line,
// and for a linear pipeline it is exactly the old value.
func (p *WorkerPool) publishCurrentStep(ctx context.Context, g *workflowGraph, st *runState, runID string) {
	lowest := -1
	for _, ws := range g.steps {
		if st.nodes[ws.Name] == nodeRunning {
			if i := g.stepIndex(ws.Name); lowest < 0 || i < lowest {
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
func (p *WorkerPool) runNode(ctx context.Context, store *tokenStore, runID, workflowID string, ws WorkflowStep, idx int, inputs, visible map[string]string, depth int, ic iterCtx, legSem chan struct{}) nodeResult {
	// Inside a map iteration the step runs against that iteration's workspace clone,
	// and its step run is labelled with the binding so the run view can tell the
	// iterations apart.
	if ic.volume != nil {
		ws.With = withIterVolume(ws.With, ic.volume)
	}

	// Scatter owns its whole resolve/clone/run/gather orchestration.
	if ws.Scatter != nil {
		output, status := p.runScatterGroup(ctx, store, runID, ws, idx, inputs, visible, depth, legSem)
		return nodeResult{name: ws.Name, state: statusToNodeState(status), output: output}
	}

	group := stepGroup{steps: []WorkflowStep{ws}, indices: []int{idx}}
	tasks, aggregateName, terr := buildGroupTasks(group, substContext{inputs: inputs, outputs: visible, mapVars: ic.mapVars, runID: runID, workflowID: workflowID})
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

	// Relabel this iteration's step runs (build -> "build [dir=src/cli]"), mirroring
	// how a matrix labels its legs.
	for i := range tasks {
		tasks[i].name = ic.name(tasks[i].name)
		tasks[i].mapVars = ic.mapVars
	}

	results, status := p.runTaskGroup(ctx, store, runID, workflowID, tasks, inputs, visible, depth, groupConcurrency(group), legSem)
	if status != StatusCompleted {
		return nodeResult{name: ws.Name, state: statusToNodeState(status)}
	}
	// A matrix step's per-value executions combine into one JSON-array output under
	// the base step name; a plain step publishes its single output.
	if aggregateName != "" {
		return nodeResult{name: ws.Name, state: nodeCompleted, output: aggregateTaskOutputs(results), legs: len(results)}
	}
	return nodeResult{name: ws.Name, state: nodeCompleted, output: results[0].output, legs: len(results)}
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
		i := g.stepIndex(n)
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
