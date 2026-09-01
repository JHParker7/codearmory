package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/google/uuid"
)

// Loops: a SUBGRAPH repeated SEQUENTIALLY, in place, until a condition holds.
//
// A map fans a body out in PARALLEL over a fixed value list, each iteration on its
// own clone. A loop is the other axis: the SAME body run again and again, in order,
// on the SAME volume, until an exit condition is true or a bounded count is reached.
// It is the retry/converge primitive — "run the developer, then verify; if verify
// failed, run the developer again on the tree it just left; stop when it passes or
// after N attempts."
//
// Like a map region, a loop is scheduled as ONE super-node (loopRegion): the routes
// into it make it ready, it expands into its iterations, and its successors wait for
// the whole thing. The repetition lives entirely inside runLoopRegion — a Go for
// loop, never a WorkflowRoute — so the graph the cycle-checker sees stays a DAG.
// Because iterations are sequential they share the one volume the body mounts (no
// per-iteration clone, unlike a map), which is the whole point: each pass works on
// what the last pass left.

// loopRegion is a resolved loop: its definition and the nodes that belong to it.
type loopRegion struct {
	def   LoopDef
	nodes []string // step names in the loop, in step order
	// firstIndex is the workflow-level index of nodes[0], for the loop's own summary
	// step run (an exhausted loop names its cause), like a map region's firstIndex.
	firstIndex int
}

// validateLoops checks the loops of a workflow, returning a user-facing message or
// "" when valid. Mirrors validateMaps.
func validateLoops(steps []WorkflowStep, defs []LoopDef, routes []WorkflowRoute) string {
	if len(defs) == 0 {
		for i, ws := range steps {
			if ws.LoopID != "" {
				return fmt.Sprintf("step %d (%s): loop_id %q names no declared loop", i, ws.Name, ws.LoopID)
			}
		}
		return ""
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.ID == "" {
			return "loop: id is required"
		}
		if seen[d.ID] {
			return fmt.Sprintf("loop %q: duplicate id", d.ID)
		}
		seen[d.ID] = true
		if msg := validateLoopDef(d); msg != "" {
			return msg
		}
	}
	members := map[string]int{}
	for i, ws := range steps {
		if ws.LoopID == "" {
			continue
		}
		if !seen[ws.LoopID] {
			return fmt.Sprintf("step %d (%s): loop_id %q names no declared loop", i, ws.Name, ws.LoopID)
		}
		members[ws.LoopID]++
		// The same restrictions a map region imposes, and for the same reasons: a
		// step's own fan-out would nest with no way to address the inner bindings, a
		// gate would have to pause each iteration independently, and a node cannot be
		// in both a loop and a map region at once.
		if ws.MapID != "" {
			return fmt.Sprintf("step %d (%s): a step cannot be in both a loop and a map region", i, ws.Name)
		}
		if ws.Matrix != nil {
			return fmt.Sprintf("step %d (%s): matrix cannot be combined with loop_id", i, ws.Name)
		}
		if ws.Scatter != nil {
			return fmt.Sprintf("step %d (%s): scatter cannot be combined with loop_id", i, ws.Name)
		}
		if ws.Approval != nil || ws.Action == ActionApproval {
			return fmt.Sprintf("step %d (%s): an approval gate cannot be inside a loop", i, ws.Name)
		}
	}
	for _, d := range defs {
		if members[d.ID] == 0 {
			return fmt.Sprintf("loop %q: no step declares loop_id %q", d.ID, d.ID)
		}
	}
	return validateLoopBoundary(steps, routes)
}

// validateLoopDef checks one loop's config: the limit against the hard max, and that
// the exit condition compiles (so a typo is a 400 at authoring time, not a branch
// that is silently never taken at 3am — the reason route conditions are compiled).
func validateLoopDef(d LoopDef) string {
	if d.Limit <= 0 {
		return fmt.Sprintf("loop %q: limit must be at least 1", d.ID)
	}
	if d.Limit > loopHardMax {
		return fmt.Sprintf("loop %q: limit %d exceeds the max of %d", d.ID, d.Limit, loopHardMax)
	}
	if _, err := compileWhen(d.Until); err != nil {
		return fmt.Sprintf("loop %q: exit condition (until): %v", d.ID, err)
	}
	return ""
}

// validateLoopBoundary rejects a route that crosses directly between loops or that
// re-enters a loop mid-body — a loop is one super-node, so such an edge has no
// meaning. Mirrors validateRegionBoundary.
func validateLoopBoundary(steps []WorkflowStep, routes []WorkflowRoute) string {
	loopOf := map[string]string{}
	for _, ws := range steps {
		if ws.LoopID != "" {
			loopOf[ws.Name] = ws.LoopID
		}
	}
	for _, r := range routes {
		from, to := loopOf[r.From], loopOf[r.To]
		if from != "" && to != "" && from != to {
			return fmt.Sprintf("route %s->%s: a route cannot cross directly between loops %q and %q", r.From, r.To, from, to)
		}
	}
	return ""
}

// loopsOf groups a workflow's steps into loops, keyed by LoopID. Mirrors regionsOf.
func loopsOf(steps []WorkflowStep, defs []LoopDef) map[string]*loopRegion {
	byID := make(map[string]*loopRegion, len(defs))
	for _, d := range defs {
		byID[d.ID] = &loopRegion{def: d}
	}
	for i, ws := range steps {
		if ws.LoopID == "" {
			continue
		}
		if l, ok := byID[ws.LoopID]; ok {
			if len(l.nodes) == 0 {
				l.firstIndex = i
			}
			l.nodes = append(l.nodes, ws.Name)
		}
	}
	for id, l := range byID {
		if len(l.nodes) == 0 {
			delete(byID, id)
		}
	}
	return byID
}

// loopOfNode maps each step name to the loop it belongs to, or "" for none.
func loopOfNode(loops map[string]*loopRegion) map[string]string {
	m := map[string]string{}
	for id, l := range loops {
		for _, n := range l.nodes {
			m[n] = id
		}
	}
	return m
}

// loopIterName labels an iteration's step run so the run view groups a loop's
// attempts the way it groups a matrix's or a map's legs.
func loopIterName(base string, attempt int) string {
	return fmt.Sprintf("%s [attempt=%d]", base, attempt)
}

// loopVars binds the iteration number for the body, when the loop named a var.
func loopVars(varName, attempt string) map[string]string {
	if varName == "" {
		return nil
	}
	return map[string]string{varName: attempt}
}

// runLoopRegion runs a loop's body repeatedly until its exit condition holds or its
// limit is reached, returning the per-node outputs of the deciding iteration and the
// loop's overall status.
//
// Semantics:
//   - With an exit condition (the retry/converge case): the loop COMPLETES the first
//     iteration after which the condition is true, and FAILS if the limit is reached
//     without it — a body-step failure inside an iteration is not the loop's failure,
//     it is the signal to try again.
//   - With no exit condition (a fixed repeat): the body runs exactly `limit` times and
//     the loop takes the worst iteration status, so a fixed loop still fails loudly if
//     a pass genuinely broke.
func (p *WorkerPool) runLoopRegion(
	ctx context.Context, store *tokenStore, runID, workflowID string, g *workflowGraph, loop *loopRegion,
	inputs, visible map[string]string, depth int, legSem chan struct{},
) (map[string]string, string, int) {
	// Clamp again at run time: validation rejects a static over-limit, but this is the
	// last line of defence for anything that reaches here — the limit can never spin a
	// body more than loopHardMax times.
	limit := loop.def.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > loopHardMax {
		limit = loopHardMax
	}

	prog, err := compileWhen(loop.def.Until)
	if err != nil {
		return nil, p.loopFail(runID, g, loop, fmt.Sprintf("loop %q: exit condition: %v", loop.def.ID, err)), 0
	}

	sub := g.subGraph(loop.nodes)
	known := g.stepNames()

	var last map[string]string
	iters := 0
	// A fixed loop (no condition) starts green and worsens with a bad pass; a
	// conditional loop starts failed and only completes when the condition is met.
	status := StatusFailed
	if loop.def.Until == "" {
		status = StatusCompleted
	}

	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			status = StatusCancelled
			break
		}
		iters = i + 1
		// Each iteration runs on its own state, re-seeded from the loop's inbound view.
		// The TREE state that carries between iterations lives in the shared volume the
		// body mounts, not in these step outputs — which is exactly why a loop shares
		// one volume rather than cloning like a map.
		st := newRunState(sub, cloneOutputs(visible), nil)
		attempt := i + 1
		iterStatus := p.runGraph(ctx, sub, st, store, runID, workflowID, inputs, depth, iterCtx{
			mapVars: loopVars(loop.def.Var, strconv.Itoa(attempt)),
			label:   func(base string) string { return loopIterName(base, attempt) },
			inbound: visible,
			known:   known,
		}, legSem)
		last = st.outputs

		if iterStatus == statusPaused {
			// Rejected at validation; belt-and-braces, mirroring runIteration.
			slog.ErrorContext(ctx, "worker: approval gate inside a loop is not supported", "run_id", runID, "loop", loop.def.ID)
			return loopAgg(loop, last), StatusFailed, iters
		}

		if loop.def.Until == "" {
			status = worstStatus(status, iterStatus)
			continue
		}

		done, cerr := evalWhen(prog, buildExprEnv(inputs, runID, st.nodes, st.outputs))
		if cerr != nil {
			return loopAgg(loop, last), p.loopFail(runID, g, loop, fmt.Sprintf("loop %q: evaluating exit condition: %v", loop.def.ID, cerr)), iters
		}
		if done {
			return loopAgg(loop, last), StatusCompleted, iters
		}
		// Condition not met — run the body again (the retry).
	}

	if status == StatusCancelled {
		return loopAgg(loop, last), status, iters
	}
	if loop.def.Until != "" {
		// Exhausted the limit without the condition ever holding.
		return loopAgg(loop, last), p.loopFail(runID, g, loop,
			fmt.Sprintf("loop %q ran %d attempts without meeting its exit condition (%s)", loop.def.ID, iters, loop.def.Until)), iters
	}
	return loopAgg(loop, last), status, iters
}

// loopAgg publishes each loop node's output as the output the deciding iteration
// produced for it — so a downstream ${steps.dev.output} reads the converged value.
// (A plain string, not the JSON array a map publishes: a loop's nodes run once per
// attempt but only the final attempt's tree survives, so the last value is the value.)
func loopAgg(loop *loopRegion, last map[string]string) map[string]string {
	agg := make(map[string]string, len(loop.nodes))
	for _, n := range loop.nodes {
		agg[n] = last[n]
	}
	return agg
}

// loopFail records a visible failed step run against the loop's first node, so a
// loop that could not expand or that exhausted its attempts names its cause in the
// run view rather than only leaving red iteration rows. Mirrors mapFail.
func (p *WorkerPool) loopFail(runID string, g *workflowGraph, loop *loopRegion, msg string) string {
	if len(loop.nodes) == 0 {
		return StatusFailed
	}
	name := loop.nodes[0]
	if sid := uuid.New().String(); p.startStepRun(runID, sid, g.stepIndex(name), name) == nil {
		p.finishStepRun(sid, StatusFailed, strPtr(msg), nil, nil, nil)
	}
	return StatusFailed
}
