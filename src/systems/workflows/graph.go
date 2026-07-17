package main

import (
	"fmt"

	"github.com/expr-lang/expr/vm"
)

// This file holds the graph view of a workflow: the node set (the existing step
// array) plus the edges ("routes") between nodes.
//
// A workflow is a graph (V, E). V has always existed — Workflow.StepRefs, whose
// names are unique (enforced by validateUniqueStepNames) and are already the
// identity used by ${steps.<name>.output}, by the run-time output map, and by
// WorkflowStepRun.StepName. What did NOT exist is E: ordering was *implied* by
// array position.
//
// So the node identity is the step NAME, and a route is a directed edge between
// two names. Keeping the array as the node set is what lets StepIndex stay
// meaningful (it is still the position in StepRefs), which in turn is why the
// portal, the CLI, and the run records need no change.
//
// A workflow with no explicit routes has them DERIVED at load time (deriveRoutes)
// as a plain chain in array order, so a route-less array is simply a sequence and
// needs no stored representation that could drift.
//
// Routes are the only encoding for parallelism. The positional parallel_group this
// replaced could group only ADJACENT array entries, so it could not express a step
// depending on two non-adjacent steps, a diamond join, or any cross-stage edge —
// two edges out of one node say all of that directly.

// maxRoutes caps the edge count. A complete DAG over maxSteps nodes would be
// maxSteps*(maxSteps-1)/2 = 1225 edges; this bounds validation and scheduling
// work well below that while leaving room for any realistic pipeline.
const maxRoutes = 200

// maxWhenLen caps a route condition's source length. It is a cheap structural gate
// applied before the expression compiler ever sees the string.
const maxWhenLen = 1000

// validateWhen checks a route's condition. This is the structural check only;
// compiling it against the expression environment (which is what turns a typo
// into an authoring-time 400 rather than a silent false at run time) is layered
// on in expressions.go.
func validateWhen(r WorkflowRoute) string {
	if r.When == "" {
		return ""
	}
	if len(r.When) > maxWhenLen {
		return fmt.Sprintf("route %s->%s: when exceeds %d characters", r.From, r.To, maxWhenLen)
	}
	return compileWhenErr(r)
}

// WorkflowRoute is a directed edge between two step refs, identified by step name.
//
// When is an expression evaluated once From reaches a terminal state; the edge is
// "taken" only if it evaluates true. An empty When means "taken iff From
// completed" — the default that reproduces the pre-graph linear semantics
// exactly, and what deriveRoutes emits.
//
// There is no entry sentinel: a node with no inbound route IS an entry node.
type WorkflowRoute struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}

// workflowGraph is the runtime view of a workflow: the node set (index-addressable,
// so StepIndex keeps working) plus resolved edges and the derived structure the
// scheduler needs. Built per run and per validation; never stored.
type workflowGraph struct {
	steps  []WorkflowStep
	routes []WorkflowRoute
	// index maps a step name to its position in steps — i.e. its StepIndex.
	index map[string]int
	// out/in map a step name to the indices (into routes) of its outbound/inbound edges.
	out map[string][]int
	in  map[string][]int
	// entries are the names with no inbound route: the nodes ready at t=0.
	entries []string
	// programs holds each route's compiled condition, indexed like routes (nil for
	// an unconditional route). Compiled once per run rather than per evaluation.
	programs []*vm.Program
	// ancestors is the transitive closure: ancestors[n] is every node that can
	// reach n. It scopes each node's visible outputs, which is what keeps a run
	// deterministic once nodes stop executing in lockstep batches.
	ancestors map[string]map[string]bool
	// regions are the map regions over this node set, by id; regionOf maps a node to
	// its region ("" when in none). A region schedules as ONE super-node: it expands
	// into N iterations of its own subgraph and its successors wait for all of them.
	// See mapregion.go.
	regions  map[string]*mapRegion
	regionOf map[string]string
	// outerIndex overrides the step index REPORTED for a node, when this graph is a
	// map region's subgraph. `index` is the position within THIS graph's step slice
	// (used to fetch a node), but WorkflowStepRun.StepIndex must stay the position in
	// the WORKFLOW's array — it is the join key between a run record and the
	// definition (wf.Steps[sr.StepIndex]). Without this a subgraph would re-index
	// from zero and an iteration's step runs would name the wrong step.
	outerIndex map[string]int
}

// stepIndex is the index to RECORD for a node: its position in the workflow's step
// array, even when running inside a map region's subgraph.
func (g *workflowGraph) stepIndex(n string) int {
	if g.outerIndex != nil {
		if i, ok := g.outerIndex[n]; ok {
			return i
		}
	}
	return g.index[n]
}

// withMaps attaches the workflow's map regions. Deliberately not part of newGraph, so
// a graph stays constructible from just (nodes, edges) — which is what keeps the pure
// graph helpers testable without a whole workflow.
func (g *workflowGraph) withMaps(defs []MapDef) *workflowGraph {
	g.regions = regionsOf(g.steps, defs)
	g.regionOf = regionOfNode(g.regions)
	return g
}

// names returns the step names of a group, in order.
func groupNames(steps []WorkflowStep) []string {
	out := make([]string, 0, len(steps))
	for _, ws := range steps {
		out = append(out, ws.Name)
	}
	return out
}

// deriveRoutes gives a route-less workflow its edges: a plain chain in array order.
//
// An array with no routes is a sequence — nothing more. Parallelism is expressed by
// routes and only by routes, so there is no positional encoding left to decode here.
func deriveRoutes(steps []WorkflowStep) []WorkflowRoute {
	var routes []WorkflowRoute
	for i := 1; i < len(steps); i++ {
		routes = append(routes, WorkflowRoute{From: steps[i-1].Name, To: steps[i].Name})
	}
	return routes
}

// compileWhenErr compiles a route condition and returns a user-facing message, or
// "" when it is valid. It lives here as a seam so validateGraph is complete on its
// own; expressions.go provides the real implementation.
func compileWhenErr(r WorkflowRoute) string {
	if _, err := compileWhen(r.When); err != nil {
		return fmt.Sprintf("route %s->%s: %v", r.From, r.To, err)
	}
	return ""
}

// newGraph indexes a node set and its routes. It assumes the caller has already
// validated shape (validateGraph); it does not re-check. A route naming an
// unknown node is skipped rather than panicking, so a mis-ordered caller
// degrades instead of crashing the worker.
func newGraph(steps []WorkflowStep, routes []WorkflowRoute) *workflowGraph {
	g := &workflowGraph{
		steps:  steps,
		routes: routes,
		index:  make(map[string]int, len(steps)),
		out:    make(map[string][]int),
		in:     make(map[string][]int),
	}
	for i, ws := range steps {
		g.index[ws.Name] = i
	}
	for ri, r := range routes {
		if _, ok := g.index[r.From]; !ok {
			continue
		}
		if _, ok := g.index[r.To]; !ok {
			continue
		}
		g.out[r.From] = append(g.out[r.From], ri)
		g.in[r.To] = append(g.in[r.To], ri)
	}
	for _, ws := range steps {
		if len(g.in[ws.Name]) == 0 {
			g.entries = append(g.entries, ws.Name)
		}
	}
	g.ancestors = buildAncestors(steps, routes, g.index)
	// Compile conditions once per graph. A compile error here cannot happen on the
	// API path (validateGraph rejects it first); if one somehow does, the nil
	// program makes the route unconditional-on-success, which is the safe default.
	g.programs = make([]*vm.Program, len(routes))
	for ri, r := range routes {
		if prog, err := compileWhen(r.When); err == nil {
			g.programs[ri] = prog
		}
	}
	return g
}

// buildAncestors computes the transitive closure of "can reach". With maxSteps=50
// nodes the naive fixed-point is trivially cheap and much easier to read than a
// bitset; it terminates because validateGraph rejects cycles, and even on a cycle
// the changed-flag loop still converges (the closure is monotone).
func buildAncestors(steps []WorkflowStep, routes []WorkflowRoute, index map[string]int) map[string]map[string]bool {
	anc := make(map[string]map[string]bool, len(steps))
	for _, ws := range steps {
		anc[ws.Name] = map[string]bool{}
	}
	for changed := true; changed; {
		changed = false
		for _, r := range routes {
			if _, ok := index[r.From]; !ok {
				continue
			}
			to, ok := anc[r.To]
			if !ok {
				continue
			}
			// From is a direct ancestor of To, as is everything upstream of From.
			if !to[r.From] {
				to[r.From] = true
				changed = true
			}
			for a := range anc[r.From] {
				if !to[a] {
					to[a] = true
					changed = true
				}
			}
		}
	}
	return anc
}

// visibleFor returns the subset of outputs a node may reference: those produced by
// its transitive ancestors, and nothing else.
//
// Every ancestor is in a terminal state by the readiness rule, so this map is
// complete and stable at the moment the node launches. Scoping it this way is what
// makes a graph run deterministic regardless of goroutine scheduling — without it
// a node would see whichever unrelated concurrent node happened to finish first.
// It is also stricter than the pre-graph engine, where a step referencing a
// sibling in its own parallel group silently resolved to an empty string.
func (g *workflowGraph) visibleFor(node string, outputs map[string]string) map[string]string {
	anc := g.ancestors[node]
	v := make(map[string]string, len(anc))
	for name := range anc {
		if out, ok := outputs[name]; ok {
			v[name] = out
		}
	}
	return v
}

// topoOrder runs Kahn's algorithm over the node set. ok is false when the graph
// contains a cycle, in which case residual names the nodes that could not be
// emitted (i.e. those on or downstream of a cycle).
func (g *workflowGraph) topoOrder() (order []string, residual []string, ok bool) {
	indeg := make(map[string]int, len(g.steps))
	for _, ws := range g.steps {
		indeg[ws.Name] = len(g.in[ws.Name])
	}
	// Seed from the entry set, preserving array order for a stable result.
	var queue []string
	for _, ws := range g.steps {
		if indeg[ws.Name] == 0 {
			queue = append(queue, ws.Name)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		for _, ri := range g.out[n] {
			to := g.routes[ri].To
			indeg[to]--
			if indeg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if len(order) == len(g.steps) {
		return order, nil, true
	}
	for _, ws := range g.steps {
		if indeg[ws.Name] > 0 {
			residual = append(residual, ws.Name)
		}
	}
	return order, residual, false
}

// buildGraph returns the runtime graph for a workflow: its stored routes when it
// has any, otherwise a chain derived from the step array.
//
// The two are mutually exclusive by validation, so there is never a stored graph
// and an array encoding that can disagree — and a workflow authored before routes
// existed needs no migration, since nothing is persisted for it.
func (wf Workflow) buildGraph() *workflowGraph {
	routes := wf.Routes
	if len(routes) == 0 {
		routes = deriveRoutes(wf.Steps)
	}
	return newGraph(wf.Steps, routes).withMaps(wf.Maps)
}

// validateGraph checks a node set and its routes, returning a user-facing message
// or "" when valid. It matches the validateStepRefShape convention (message, not
// error) so the API handlers can 400 with it directly.
//
// It assumes step names are already known unique (validateUniqueStepNames runs
// first on the create/update path), since names are the node identity here.
func validateGraph(steps []WorkflowStep, routes []WorkflowRoute, maps []MapDef) string {
	if msg := validateMaps(steps, maps, routes); msg != "" {
		return msg
	}
	if len(routes) == 0 {
		return ""
	}
	if len(routes) > maxRoutes {
		return fmt.Sprintf("too many routes: %d (max %d)", len(routes), maxRoutes)
	}
	known := make(map[string]bool, len(steps))
	for _, ws := range steps {
		known[ws.Name] = true
	}
	seen := make(map[WorkflowRoute]bool, len(routes))
	for _, r := range routes {
		if r.From == "" || r.To == "" {
			return "route: from and to are required"
		}
		if !known[r.From] {
			return fmt.Sprintf("route %s->%s: unknown step %q", r.From, r.To, r.From)
		}
		if !known[r.To] {
			return fmt.Sprintf("route %s->%s: unknown step %q", r.From, r.To, r.To)
		}
		if r.From == r.To {
			return fmt.Sprintf("route %s->%s: a step cannot route to itself", r.From, r.To)
		}
		if seen[r] {
			return fmt.Sprintf("route %s->%s: duplicate route", r.From, r.To)
		}
		seen[r] = true
		if msg := validateWhen(r); msg != "" {
			return msg
		}
	}
	// Cycle detection. This also subsumes an "unreachable node" check: in a DAG
	// every node is reachable from some entry (a node with no inbound edge is its
	// own entry), so unreachability implies a cycle.
	g := newGraph(steps, routes)
	if _, residual, ok := g.topoOrder(); !ok {
		return fmt.Sprintf("routes form a cycle involving: %v", residual)
	}
	if len(g.entries) == 0 {
		return "routes leave no entry step (every step has an inbound route)"
	}
	return ""
}
