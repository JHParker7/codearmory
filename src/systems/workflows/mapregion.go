package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Map regions: a SUBGRAPH repeated once per value.
//
// A matrix repeats one step; a scatter repeats one step on its own workspace clone.
// Neither can express "per directory: build, then test, then push only if prod" —
// which needs a multi-step body AND a per-iteration clone (volumes are
// ReadWriteOnce, so parallel iterations cannot share a checkout).
//
// A region is the set of steps sharing a MapID. The scheduler treats it as ONE
// super-node: it becomes ready when the routes into it are taken, expands into N
// iterations, and completes only when all of them do — so a downstream step (deploy,
// integration) sees the whole region as a single dependency and runs once, after
// everything.
//
// Inside an iteration the region's own routes apply, so the body branches and joins
// exactly like the main graph — it IS the main graph's machinery, recursed:
// runIteration builds a sub-graph of the region's nodes and hands it to runGraph.

// mapRegion is a resolved region: its definition plus the nodes that belong to it.
type mapRegion struct {
	def   MapDef
	nodes []string // step names in the region, in step order
	// firstIndex is the workflow-level step index of nodes[0], used to record a step
	// run for the region's own orchestration (clone/gather) — which belongs to no
	// user-authored step of its own.
	firstIndex int
}

// validateMaps checks the map regions of a workflow, returning a user-facing message
// or "" when valid. Matches the validateStepRefShape convention.
func validateMaps(steps []WorkflowStep, defs []MapDef, routes []WorkflowRoute) string {
	if len(defs) == 0 {
		for i, ws := range steps {
			if ws.MapID != "" {
				return fmt.Sprintf("step %d (%s): map_id %q names no declared map", i, ws.Name, ws.MapID)
			}
		}
		return ""
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.ID == "" {
			return "map: id is required"
		}
		if seen[d.ID] {
			return fmt.Sprintf("map %q: duplicate id", d.ID)
		}
		seen[d.ID] = true
		if msg := validateMapDef(d); msg != "" {
			return msg
		}
	}
	members := map[string]int{}
	for i, ws := range steps {
		if ws.MapID == "" {
			continue
		}
		if !seen[ws.MapID] {
			return fmt.Sprintf("step %d (%s): map_id %q names no declared map", i, ws.Name, ws.MapID)
		}
		members[ws.MapID]++
		// A step's own fan-out would nest inside the region's, multiplying the work
		// with no way to address the inner bindings; and a gate inside a region would
		// have to pause each iteration independently, which the resume path (keyed by
		// node name) cannot express.
		if ws.Matrix != nil {
			return fmt.Sprintf("step %d (%s): matrix cannot be combined with map_id", i, ws.Name)
		}
		if ws.Scatter != nil {
			return fmt.Sprintf("step %d (%s): scatter cannot be combined with map_id", i, ws.Name)
		}
		if ws.Approval != nil || ws.Action == ActionApproval {
			return fmt.Sprintf("step %d (%s): an approval gate cannot be inside a map region", i, ws.Name)
		}
	}
	for _, d := range defs {
		if members[d.ID] == 0 {
			return fmt.Sprintf("map %q: no step declares map_id %q", d.ID, d.ID)
		}
	}
	return validateRegionBoundary(steps, defs, routes)
}

// validateMapDef checks one region's fan-out config, mirroring validateMatrix.
func validateMapDef(d MapDef) string {
	if strings.TrimSpace(d.Var) == "" {
		return fmt.Sprintf("map %q: var is required", d.ID)
	}
	if strings.ContainsAny(d.Var, " \t${}") {
		return fmt.Sprintf("map %q: var must not contain whitespace or ${}", d.ID)
	}
	hasValues, hasFrom := len(d.Values) > 0, d.ValuesFrom != ""
	if hasValues == hasFrom {
		return fmt.Sprintf("map %q: exactly one of values or values_from is required", d.ID)
	}
	if len(d.Values) > maxMapValues {
		return fmt.Sprintf("map %q: %d values (max %d)", d.ID, len(d.Values), maxMapValues)
	}
	if d.MaxConcurrent < 0 {
		return fmt.Sprintf("map %q: max_concurrent must not be negative", d.ID)
	}
	if d.FailureTolerance < 0 || d.FailureTolerance > 100 {
		return fmt.Sprintf("map %q: failure_tolerance must be a percentage between 0 and 100", d.ID)
	}
	return ""
}

// validateRegionBoundary rejects a region a route re-enters.
//
// A region is scheduled as one super-node, so an edge from outside into a node that
// is not where the region starts, or an edge from inside back out and in again, has
// no meaning: the region expands once, as a unit. Catching it here beats a run that
// deadlocks or silently runs a body twice.
func validateRegionBoundary(steps []WorkflowStep, defs []MapDef, routes []WorkflowRoute) string {
	regionOf := map[string]string{}
	for _, ws := range steps {
		if ws.MapID != "" {
			regionOf[ws.Name] = ws.MapID
		}
	}
	for _, r := range routes {
		from, to := regionOf[r.From], regionOf[r.To]
		if from != "" && to != "" && from != to {
			return fmt.Sprintf("route %s->%s: a route cannot cross directly between map regions %q and %q", r.From, r.To, from, to)
		}
	}
	return ""
}

// regionsOf groups a workflow's steps into map regions, keyed by MapID. A region
// with a MapID naming no MapDef is dropped (validateMaps rejects it on the API path).
func regionsOf(steps []WorkflowStep, defs []MapDef) map[string]*mapRegion {
	byID := make(map[string]*mapRegion, len(defs))
	for _, d := range defs {
		byID[d.ID] = &mapRegion{def: d}
	}
	for i, ws := range steps {
		if ws.MapID == "" {
			continue
		}
		if r, ok := byID[ws.MapID]; ok {
			if len(r.nodes) == 0 {
				r.firstIndex = i
			}
			r.nodes = append(r.nodes, ws.Name)
		}
	}
	// A declared-but-empty region would otherwise be a super-node with no work that
	// never completes; drop it so it cannot wedge the frontier.
	for id, r := range byID {
		if len(r.nodes) == 0 {
			delete(byID, id)
		}
	}
	return byID
}

// regionOfNode maps each step name to the region it belongs to, or "" for none.
func regionOfNode(regions map[string]*mapRegion) map[string]string {
	m := map[string]string{}
	for id, r := range regions {
		for _, n := range r.nodes {
			m[n] = id
		}
	}
	return m
}

// mapIterName labels an iteration's step run, mirroring matrixTaskName so the run
// view collapses a region's fan-out the same way it collapses a matrix's.
func mapIterName(base, varName, val string) string {
	return fmt.Sprintf("%s [%s=%s]", base, varName, val)
}

// resolveMapValues produces the region's value list at run time. ValuesFrom is
// substituted first, then parsed exactly like a matrix's values_from (JSON array, or
// comma/whitespace-separated), so a plain `ls` output feeds a region directly.
func resolveMapValues(d MapDef, sc substContext) ([]string, error) {
	if d.ValuesFrom != "" {
		raw := substitute(d.ValuesFrom, sc)
		vals := parseMatrixList(raw)
		if len(vals) > maxMapValues {
			return nil, fmt.Errorf("map %q resolved to %d values (max %d)", d.ID, len(vals), maxMapValues)
		}
		return vals, nil
	}
	if len(d.Values) > maxMapValues {
		return nil, fmt.Errorf("map %q has %d values (max %d)", d.ID, len(d.Values), maxMapValues)
	}
	return d.Values, nil
}

// mapConcurrency mirrors groupConcurrency: conservative by default, raisable to the
// run-wide ceiling, or pinned to 1 when Sequential.
func mapConcurrency(d MapDef) int {
	switch {
	case d.Sequential:
		return 1
	case d.MaxConcurrent > 0:
		return min(d.MaxConcurrent, maxParallelSteps)
	default:
		return defaultFanoutConcurrency
	}
}

// maxToleratedFailures is how many iterations may fail while the map still passes:
// floor(FailureTolerance/100 * total). The (result+1)th failure trips the map. 0
// tolerance yields 0 (strict — the first failure trips it); 100 yields total (no
// failure ever trips it).
func maxToleratedFailures(d MapDef, total int) int {
	if d.FailureTolerance <= 0 {
		return 0
	}
	if d.FailureTolerance >= 100 {
		return total
	}
	return d.FailureTolerance * total / 100
}

// iterVolume is an iteration's clone volume name — scoped to the run so run-end
// teardown reaps it, and deterministic so the body can mount it by name.
//
// regionKey disambiguates the REGION. Without it the name was "<base>-m<i>", keyed
// only on the iteration index within one region, so two regions declaring the same
// base volume — which is the normal thing to do, since both usually clone "workspace"
// — collided on their first iterations: per-service leg 0 and per-extra leg 0 were
// both "workspace-m0". They do not merely share the volume, they destroy it: each
// iteration DELETES its clone when it ends (see the defer in runIteration), so
// whichever region finished first pulled the volume out from under a leg of the other
// still using it. That surfaced as "volume workspace-m0 not found ... (create it
// first)" from a build step whose own volume had been correct moments earlier.
//
// The collision needs both regions non-empty to appear at all, so a pipeline can run
// green for a long time and then break on the first push that happens to change a
// service AND a non-service module.
//
// firstIndex (the region's first step's workflow-level index) is the key: unique per
// region because a step belongs to at most one region, and small, which matters —
// forge caps a volume name at 40 chars as a DNS-1123 label, so the region's author-
// chosen id is not safe to interpolate here.
func iterVolume(base string, regionKey, i int) string {
	return fmt.Sprintf("%s-r%d-m%d", base, regionKey, i)
}

// subGraph builds the graph of one region: its own nodes, and only the routes whose
// BOTH ends are inside it. Routes crossing the boundary are the region's inbound and
// outbound edges — the scheduler resolves those against the region as a whole, so an
// iteration must not see them.
func (g *workflowGraph) subGraph(nodes []string) *workflowGraph {
	inside := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		inside[n] = true
	}
	var steps []WorkflowStep
	for _, ws := range g.steps {
		if inside[ws.Name] {
			steps = append(steps, ws)
		}
	}
	var routes []WorkflowRoute
	for _, r := range g.routes {
		if inside[r.From] && inside[r.To] {
			routes = append(routes, r)
		}
	}
	sub := newGraph(steps, routes)
	// Keep the WORKFLOW-level step indices through ARBITRARY nesting: an iteration's
	// step runs must still name the step they came from, not a position within the
	// region. Use g's REPORTED index (g.stepIndex resolves g.outerIndex when g is
	// itself a sub, else g.index) — plain g.index would, for a loop nested inside a
	// loop, report the inner loop's steps at their position within the PARENT sub-graph
	// (e.g. spec at 1 instead of 4), so the run view maps them to the wrong step: the
	// inner-loop step shows pending while its run is attributed to some other node.
	sub.outerIndex = make(map[string]int, len(steps))
	for _, ws := range steps {
		sub.outerIndex[ws.Name] = g.stepIndex(ws.Name)
	}
	return sub
}

// runMapRegion expands a region and runs every iteration, returning the region's
// per-node aggregated outputs and its overall status.
//
// The region completes only when all iterations are terminal — the barrier the
// downstream graph depends on. Any iteration failing fails the region: a partial map
// has not done the work its successors assume.
func (p *WorkerPool) runMapRegion(
	ctx context.Context, store *tokenStore, runID, workflowID string, g *workflowGraph, region *mapRegion,
	inputs, visible map[string]string, depth int, legSem chan struct{},
) (map[string]string, string, int) {
	sc := substContext{inputs: inputs, outputs: visible, runID: runID, workflowID: workflowID}
	values, err := resolveMapValues(region.def, sc)
	if err != nil {
		return nil, p.mapFail(runID, g, region, err.Error()), 0
	}
	if len(values) == 0 {
		// A fan-out that resolved to nothing did no work. By default that is a bug (a
		// mistyped values_from), so fail loudly. But when the region opted into
		// allow_empty, "no items" is a legitimate outcome — record the region as a
		// completed no-op (kept visible in the run view) and let the run stay green.
		if region.def.AllowEmpty {
			return map[string]string{}, p.mapSkip(runID, g, region, fmt.Sprintf("map %q produced no values — skipped (allow_empty)", region.def.ID)), 0
		}
		return nil, p.mapFail(runID, g, region, fmt.Sprintf("map %q produced no values to run", region.def.ID)), 0
	}

	sub := g.subGraph(region.nodes)
	// Built once for the whole region, not per iteration: it is the same set for every
	// value, and a wide fan-out would otherwise rebuild an identical map N times.
	known := g.stepNames()
	type iterResult struct {
		outputs map[string]string
		status  string
	}
	results := make([]iterResult, len(values))
	sem := make(chan struct{}, mapConcurrency(region.def))

	// Fail-fast tolerance: run under a child context so that once more than the
	// tolerated number of iterations have failed we can cancel the rest. maxFail is
	// the count the map can absorb and still pass; the (maxFail+1)th failure trips it.
	maxFail := maxToleratedFailures(region.def, len(values))
	mapCtx, cancelMap := context.WithCancel(ctx)
	defer cancelMap()
	var mu sync.Mutex
	failed := 0
	tripped := false

	var wg sync.WaitGroup
	for i, val := range values {
		wg.Add(1)
		go func(i int, val string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-mapCtx.Done():
				results[i] = iterResult{status: StatusCancelled}
				return
			}
			// The tolerance may have been exceeded while this iteration waited for a
			// concurrency slot — don't start new work once the map has tripped.
			if mapCtx.Err() != nil {
				results[i] = iterResult{status: StatusCancelled}
				return
			}
			results[i] = iterResult{}
			outs, st := p.runIteration(mapCtx, store, runID, workflowID, sub, region, i, val, inputs, visible, known, depth, legSem)
			results[i] = iterResult{outputs: outs, status: st}
			if st == StatusFailed {
				mu.Lock()
				failed++
				if !tripped && failed > maxFail {
					tripped = true
					slog.WarnContext(ctx, "worker: map failure tolerance exceeded — cancelling remaining iterations",
						"run_id", runID, "map", region.def.ID, "failed", failed, "total", len(values), "tolerance_pct", region.def.FailureTolerance)
					cancelMap()
				}
				mu.Unlock()
			}
		}(i, val)
	}
	wg.Wait()

	// Decide the region's status. An OUTER cancellation (run cancel/timeout) always
	// dominates — never report a timed-out run as a passing map. Otherwise apply the
	// tolerance: a trip is a hard fail; failures within tolerance pass.
	var status string
	switch {
	case ctx.Err() != nil:
		status = StatusCompleted
		for _, r := range results {
			status = worstStatus(status, r.status)
		}
	case tripped:
		status = StatusFailed
	default:
		// failed <= maxFail: tolerated. Still surface any non-failure bad status
		// (e.g. a paused iteration, which is rejected at validation but guarded here).
		status = StatusCompleted
		for _, r := range results {
			if r.status == StatusFailed || r.status == StatusCancelled {
				continue
			}
			status = worstStatus(status, r.status)
		}
	}
	// Each region node's output is the JSON array of that node's output across
	// iterations, published under the node's own name — the same shape a matrix step
	// publishes, so a downstream ${steps.build.output} reads uniformly.
	agg := map[string]string{}
	for _, n := range region.nodes {
		outs := make([]string, 0, len(results))
		for _, r := range results {
			outs = append(outs, r.outputs[n])
		}
		b, _ := json.Marshal(outs)
		agg[n] = string(b)
	}
	return agg, status, len(values)
}

// runIteration runs the region's subgraph once, for one value.
func (p *WorkerPool) runIteration(
	ctx context.Context, store *tokenStore, runID, workflowID string, sub *workflowGraph, region *mapRegion,
	i int, val string, inputs, visible map[string]string, known map[string]bool, depth int, legSem chan struct{},
) (map[string]string, string) {
	mapVars := map[string]string{region.def.Var: val}

	// Give the iteration its own clone of the base workspace, if one is configured.
	// This is the whole reason a region exists rather than a matrix over sub-runs:
	// volumes are ReadWriteOnce, so iterations cannot share a checkout.
	//
	// The clone is released as soon as the iteration ends, so the concurrent volume
	// footprint tracks max_concurrent rather than the number of values — otherwise a
	// fan-out over N items would need N clones alive at once and trip forge's
	// per-workflow volume cap.
	if region.def.Volume != "" {
		if st := p.prepareIterVolume(ctx, store, runID, region, i, val, depth); st != StatusCompleted {
			return nil, st
		}
		defer p.deleteRunVolumes(context.Background(), store, runID, iterVolume(region.def.Volume, region.firstIndex, i))
	}

	// The iteration runs on its own state: its nodes start pending, and its visible
	// outputs begin as the region's inbound view, so the body can reference steps
	// upstream of the region.
	st := newRunState(sub, cloneOutputs(visible), nil)
	status := p.runGraph(ctx, sub, st, store, runID, workflowID, inputs, depth, iterCtx{
		mapVars: mapVars,
		label:   func(base string) string { return mapIterName(base, region.def.Var, val) },
		volume:  iterVolumeMount(region, runID, i),
		// The body runs against the ISOLATED sub-graph, so upstream outputs must be
		// carried in explicitly or references like ${steps.commit.output.SHA} resolve
		// to empty (an unresolved ${...} then reaches forge as a literal — the
		// "not a valid image reference" failure this fixes).
		inbound: visible,
		// The outer pipeline's step names, so an unresolved reference to an upstream
		// step is reported as "not an ancestor" rather than "no such step" — the
		// sub-graph on its own knows only the body's nodes.
		known: known,
	}, legSem)
	if status == statusPaused {
		// An approval gate inside a map region would have to pause N iterations
		// independently and resume each — the resume path keys completion by node
		// name, which cannot distinguish iterations. Rejected at validation; this is
		// the belt-and-braces.
		slog.ErrorContext(ctx, "worker: approval gate inside a map region is not supported", "run_id", runID, "map", region.def.ID)
		return nil, StatusFailed
	}
	// Gather the iteration's owned outputs back into the base workspace.
	if status == StatusCompleted && region.def.Volume != "" && len(region.def.Outputs) > 0 {
		if gst := p.gatherIter(ctx, store, runID, region, i, mapVars, depth); gst != StatusCompleted {
			status = gst
		}
	}
	return st.outputs, status
}

// iterVolumeMount is the clone volume the iteration's steps should run against, or
// nil when the region configures no clone.
func iterVolumeMount(region *mapRegion, runID string, i int) map[string]any {
	if region.def.Volume == "" {
		return nil
	}
	mount := region.def.MountPath
	if mount == "" {
		mount = defaultScatterMountPath
	}
	return volMount(runID, iterVolume(region.def.Volume, region.firstIndex, i), mount, true, false)
}

// prepareIterVolume provisions and fills one iteration's clone, reusing scatter's
// create+copy steps so both fan-outs clone a workspace the same way.
//
// A failure here records a VISIBLE step run against the region's first node: the
// clone is not a user-authored step, so without one the run view would show every
// step green under a failed run and never say why.
func (p *WorkerPool) prepareIterVolume(ctx context.Context, store *tokenStore, runID string, region *mapRegion, i int, val string, depth int) string {
	cfg := &ScatterConfig{
		Volume:    region.def.Volume,
		MountPath: region.def.MountPath,
		SizeMB:    region.def.SizeMB,
		Medium:    region.def.Medium,
	}
	shard := iterVolume(region.def.Volume, region.firstIndex, i)
	for _, step := range []Step{scatterCreateShardStep(cfg, runID, shard), scatterCloneStep(cfg, runID, shard)} {
		if _, err := p.executeStep(ctx, store, step, substContext{runID: runID, depth: depth}); err != nil {
			slog.WarnContext(ctx, "worker: map iteration volume prepare failed", "run_id", runID, "map", region.def.ID, "iter", i, "error", err)
			p.iterFail(runID, region, val, "preparing this iteration's workspace clone: "+err.Error())
			return StatusFailed
		}
	}
	return StatusCompleted
}

// iterFail records a failed step run for one iteration's own orchestration (its
// clone or gather), labelled like the iteration's body steps so it lands beside them
// in the run view.
func (p *WorkerPool) iterFail(runID string, region *mapRegion, val, msg string) {
	if len(region.nodes) == 0 {
		return
	}
	name := mapIterName(region.nodes[0], region.def.Var, val)
	if sid := uuid.New().String(); p.startStepRun(runID, sid, region.firstIndex, name) == nil {
		p.finishStepRun(sid, StatusFailed, strPtr(msg), nil, nil, nil)
	}
}

// gatherIter unions an iteration's owned outputs back into the base workspace.
func (p *WorkerPool) gatherIter(ctx context.Context, store *tokenStore, runID string, region *mapRegion, i int, mapVars map[string]string, depth int) string {
	cfg := &ScatterConfig{Volume: region.def.Volume, MountPath: region.def.MountPath, Outputs: region.def.Outputs}
	shard := iterVolume(region.def.Volume, region.firstIndex, i)
	sc := substContext{runID: runID, mapVars: mapVars, depth: depth}
	paths := make([]string, 0, len(cfg.Outputs))
	for _, o := range cfg.Outputs {
		paths = append(paths, substitute(o, sc))
	}
	step, ok := scatterGatherStep(cfg, runID, []string{shard}, paths)
	if !ok {
		return StatusCompleted
	}
	if _, err := p.executeStep(ctx, store, step, sc); err != nil {
		slog.WarnContext(ctx, "worker: map iteration gather failed", "run_id", runID, "map", region.def.ID, "iter", i, "error", err)
		p.iterFail(runID, region, mapVars[region.def.Var], "gathering this iteration's outputs: "+err.Error())
		return StatusFailed
	}
	return StatusCompleted
}

// mapFail records a visible failed step run against the region's first node, so a
// region that could not expand names its cause in the run view rather than vanishing.
func (p *WorkerPool) mapFail(runID string, g *workflowGraph, region *mapRegion, msg string) string {
	if len(region.nodes) == 0 {
		return StatusFailed
	}
	name := region.nodes[0]
	if sid := uuid.New().String(); p.startStepRun(runID, sid, g.stepIndex(name), name) == nil {
		p.finishStepRun(sid, StatusFailed, strPtr(msg), nil, nil, nil)
	}
	return StatusFailed
}

// mapSkip records an allow_empty region that resolved to no values as a COMPLETED
// no-op — the same visibility as mapFail (a step run the user can see), but green so
// the run proceeds. Mirrors mapFail deliberately, so the two stay in step.
func (p *WorkerPool) mapSkip(runID string, g *workflowGraph, region *mapRegion, msg string) string {
	if len(region.nodes) == 0 {
		return StatusCompleted
	}
	name := region.nodes[0]
	if sid := uuid.New().String(); p.startStepRun(runID, sid, g.stepIndex(name), name) == nil {
		p.finishStepRun(sid, StatusCompleted, strPtr(msg), nil, nil, nil)
	}
	return StatusCompleted
}

// withIterVolume attaches the iteration's clone to a step's volumes, replacing any
// mount the body declared on the SAME volume name so the body's own `${run_id}`
// workspace mount is redirected to this iteration's clone rather than the shared base
// (which is ReadWriteOnce and would Multi-Attach across parallel iterations).
func withIterVolume(with map[string]any, vol map[string]any) map[string]any {
	out := make(map[string]any, len(with)+1)
	for k, v := range with {
		out[k] = v
	}
	mountPath, _ := vol["mount_path"].(string)
	kept := []any{}
	if existing, ok := with["volumes"].([]any); ok {
		for _, e := range existing {
			m, ok := e.(map[string]any)
			if !ok {
				kept = append(kept, e)
				continue
			}
			// Drop a mount that would collide with the clone's mount point.
			if mp, _ := m["mount_path"].(string); mp == mountPath {
				continue
			}
			kept = append(kept, e)
		}
	}
	out["volumes"] = append([]any{vol}, kept...)
	return out
}

func cloneOutputs(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
