package main

import (
	"context"
	"sort"
	"testing"
)

// driveGraph resolves routes as nodes reach the given terminal states, simulating
// the scheduler's launch/collect cycle without touching the DB or running steps.
// outcomes maps a node name to the state it finishes in; a node absent from the
// map completes. It returns the order nodes were launched in and the final states.
func driveGraph(t *testing.T, g *workflowGraph, outcomes map[string]nodeState, outputs map[string]string) (launched []string, st *runState) {
	t.Helper()
	p := &WorkerPool{}
	st = newRunState(g, outputs, nil)
	for {
		ready := g.readyNodes(st)
		if len(ready) == 0 {
			return launched, st
		}
		for _, n := range ready {
			state, ok := outcomes[n]
			if !ok {
				state = nodeCompleted
			}
			st.nodes[n] = state
			launched = append(launched, n)
			p.resolveOutbound(context.Background(), g, st, "run", nil, n)
		}
	}
}

// The diamond of death: when a conditional branch is skipped, the join must still
// run. This is why `skipped` is a distinct state and why the join rule is
// all-inbound-RESOLVED rather than all-inbound-completed — under the latter, D
// would wait on the skipped branch forever.
func TestScheduler_ConditionalDiamondJoinRuns(t *testing.T) {
	steps := stepsWithGroups("a", "b", "c", "d")
	routes := []WorkflowRoute{
		{From: "a", To: "b", When: `steps.a.output == "yes"`},
		{From: "a", To: "c", When: `steps.a.output != "yes"`},
		{From: "b", To: "d"},
		{From: "c", To: "d"},
	}
	if msg := validateGraph(steps, routes); msg != "" {
		t.Fatalf("graph invalid: %s", msg)
	}
	g := newGraph(steps, routes)

	launched, st := driveGraph(t, g, nil, map[string]string{"a": "yes"})

	got := append([]string(nil), launched...)
	sort.Strings(got)
	// b's branch was taken, c's was not — but d still runs.
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "d" {
		t.Fatalf("launched = %v, want [a b d]", got)
	}
	if st.nodes["c"] != nodeSkipped {
		t.Errorf("c = %s, want skipped", st.nodes["c"])
	}
	if st.nodes["d"] != nodeCompleted {
		t.Errorf("d = %s, want completed — the join must not deadlock on a skipped branch", st.nodes["d"])
	}
}

// The other branch of the same diamond, to prove the condition actually decides.
func TestScheduler_ConditionalDiamondOtherBranch(t *testing.T) {
	steps := stepsWithGroups("a", "b", "c", "d")
	g := newGraph(steps, []WorkflowRoute{
		{From: "a", To: "b", When: `steps.a.output == "yes"`},
		{From: "a", To: "c", When: `steps.a.output != "yes"`},
		{From: "b", To: "d"},
		{From: "c", To: "d"},
	})
	_, st := driveGraph(t, g, nil, map[string]string{"a": "no"})
	if st.nodes["b"] != nodeSkipped {
		t.Errorf("b = %s, want skipped", st.nodes["b"])
	}
	if st.nodes["c"] != nodeCompleted {
		t.Errorf("c = %s, want completed", st.nodes["c"])
	}
	if st.nodes["d"] != nodeCompleted {
		t.Errorf("d = %s, want completed", st.nodes["d"])
	}
}

// A failure with no failure route skips everything downstream, transitively —
// which is how the pre-graph engine's unconditional `break` falls out of the model
// with no special case.
func TestScheduler_FailureSkipsDownstreamTransitively(t *testing.T) {
	steps := stepsWithGroups("a", "b", "c")
	g := newGraph(steps, deriveRoutes(steps))
	launched, st := driveGraph(t, g, map[string]nodeState{"a": nodeFailed}, nil)

	if len(launched) != 1 || launched[0] != "a" {
		t.Fatalf("launched = %v, want only [a]", launched)
	}
	if st.nodes["b"] != nodeSkipped || st.nodes["c"] != nodeSkipped {
		t.Errorf("b=%s c=%s, want both skipped", st.nodes["b"], st.nodes["c"])
	}
}

// A route conditioned on failure is how a cleanup/handler branch runs. Continuing
// past a failure is strictly opt-in: the default (empty when) is success-only.
func TestScheduler_FailureRouteRunsHandler(t *testing.T) {
	steps := stepsWithGroups("build", "publish", "cleanup")
	g := newGraph(steps, []WorkflowRoute{
		{From: "build", To: "publish", When: `steps.build.status == "completed"`},
		{From: "build", To: "cleanup", When: `steps.build.status == "failed"`},
	})
	_, st := driveGraph(t, g, map[string]nodeState{"build": nodeFailed}, nil)
	if st.nodes["publish"] != nodeSkipped {
		t.Errorf("publish = %s, want skipped", st.nodes["publish"])
	}
	if st.nodes["cleanup"] != nodeCompleted {
		t.Errorf("cleanup = %s, want completed — a failure route should run its handler", st.nodes["cleanup"])
	}
}

// A condition can gate on a step's output value, not just its status. This is the
// case a status-only enum cannot express, and the reason for a real expression
// language: "only publish if a release was actually cut".
func TestScheduler_RouteGatesOnOutputJSON(t *testing.T) {
	steps := stepsWithGroups("release", "build")
	g := newGraph(steps, []WorkflowRoute{
		{From: "release", To: "build", When: `steps.release.json.published == true`},
	})

	_, st := driveGraph(t, g, nil, map[string]string{"release": `{"published":false}`})
	if st.nodes["build"] != nodeSkipped {
		t.Errorf("build = %s, want skipped when nothing was published", st.nodes["build"])
	}

	_, st = driveGraph(t, g, nil, map[string]string{"release": `{"published":true}`})
	if st.nodes["build"] != nodeCompleted {
		t.Errorf("build = %s, want completed when a release was published", st.nodes["build"])
	}
}

// An approval gate parks its branch rather than pausing the run, so sibling nodes
// still reach a terminal state. Pausing with siblings in flight is what would
// orphan their step runs and re-execute them on resume.
func TestScheduler_GateParksWithoutBlockingSiblings(t *testing.T) {
	steps := []WorkflowStep{
		{Step: Step{Name: "gate", Action: ActionApproval}},
		{Step: Step{Name: "sibling"}},
	}
	// Two independent entry nodes: a gate and a normal step.
	g := newGraph(steps, nil)
	st := newRunState(g, map[string]string{}, nil)

	ready := g.readyNodes(st)
	if len(ready) != 2 {
		t.Fatalf("ready = %v, want both entries", ready)
	}
	// The scheduler parks the gate and runs the sibling.
	st.nodes["gate"] = nodeAwaiting
	st.nodes["sibling"] = nodeCompleted

	if awaiting := g.awaitingNodes(st); len(awaiting) != 1 || awaiting[0] != "gate" {
		t.Fatalf("awaiting = %v, want [gate]", awaiting)
	}
	// An awaiting node is not terminal — its outbound routes stay undecided.
	if nodeAwaiting.terminal() {
		t.Error("awaiting must not be terminal; a parked gate has not decided its routes")
	}
}

// Resume seeds already-completed nodes so the frontier reopens past them rather
// than re-running them.
func TestScheduler_ResumeSkipsCompletedNodes(t *testing.T) {
	steps := stepsWithGroups("a", "b", "c")
	g := newGraph(steps, deriveRoutes(steps))
	st := newRunState(g, map[string]string{"a": "done"}, map[string]bool{"a": true})

	if st.nodes["a"] != nodeCompleted {
		t.Fatalf("a = %s, want completed on resume", st.nodes["a"])
	}
	// a is complete but its routes are unresolved until the scheduler seeds them,
	// so b is not yet ready.
	if ready := g.readyNodes(st); len(ready) != 0 {
		t.Fatalf("ready before seeding = %v, want none", ready)
	}
	(&WorkerPool{}).resolveOutbound(context.Background(), g, st, "run", nil, "a")
	ready := g.readyNodes(st)
	if len(ready) != 1 || ready[0] != "b" {
		t.Fatalf("ready after seeding = %v, want [b]", ready)
	}
}
