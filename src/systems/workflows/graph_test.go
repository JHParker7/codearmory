package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// stepsNamed builds a node set from bare step names. Edges are supplied separately —
// a step array carries no ordering beyond its sequence.
func stepsNamed(spec ...string) []WorkflowStep {
	var out []WorkflowStep
	for _, name := range spec {
		out = append(out, WorkflowStep{Step: Step{Name: name}})
	}
	return out
}

func routeSet(routes []WorkflowRoute) []string {
	var out []string
	for _, r := range routes {
		out = append(out, r.From+"->"+r.To)
	}
	sort.Strings(out)
	return out
}

// A route-less array is a plain sequence: deriveRoutes is what preserves the
// behaviour of every pipeline authored before routes existed.
func TestDeriveRoutes_IsAPlainChain(t *testing.T) {
	cases := []struct {
		name  string
		steps []WorkflowStep
		want  []string
	}{
		{
			name:  "linear chain",
			steps: stepsNamed("a", "b", "c"),
			want:  []string{"a->b", "b->c"},
		},
		{
			name:  "single step has no routes",
			steps: stepsNamed("only"),
			want:  nil,
		},
		{
			name:  "empty",
			steps: nil,
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routeSet(deriveRoutes(tc.steps))
			if len(got) != len(tc.want) {
				t.Fatalf("routes = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("routes = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Derivation must NEVER invent a fork. Parallelism is expressed by routes alone, so
// an array on its own can only ever mean "one after another".
func TestDeriveRoutes_NeverForks(t *testing.T) {
	steps := stepsNamed("a", "b", "c", "d")
	for _, r := range deriveRoutes(steps) {
		var outbound int
		for _, o := range deriveRoutes(steps) {
			if o.From == r.From {
				outbound++
			}
		}
		if outbound != 1 {
			t.Fatalf("node %q has %d outbound derived routes; a derived graph is a chain", r.From, outbound)
		}
	}
}

// Several nodes are ready at t=0 when none has an inbound route — how a graph
// expresses a pipeline that starts with a fork.
func TestNewGraph_EntriesAreNodesWithNoInbound(t *testing.T) {
	steps := stepsNamed("a", "b", "c")
	g := newGraph(steps, []WorkflowRoute{{From: "a", To: "c"}, {From: "b", To: "c"}})
	got := append([]string(nil), g.entries...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("entries = %v, want [a b]", got)
	}
}

// visibleFor is what makes a graph run deterministic: a node sees its ancestors'
// outputs and nothing else, regardless of what finished first.
func TestVisibleFor_ScopesToAncestors(t *testing.T) {
	steps := stepsNamed("build", "lint", "test", "deploy")
	g := newGraph(steps, []WorkflowRoute{
		{From: "build", To: "lint"}, {From: "build", To: "test"},
		{From: "lint", To: "deploy"}, {From: "test", To: "deploy"},
	})
	outputs := map[string]string{"build": "B", "lint": "L", "test": "T"}

	// Siblings in the same parallel band are not ancestors of one another.
	if v := g.visibleFor("test", outputs); len(v) != 1 || v["build"] != "B" {
		t.Fatalf("test sees %v, want only build", v)
	}
	// deploy joins the band, so it sees everything upstream transitively.
	v := g.visibleFor("deploy", outputs)
	if len(v) != 3 || v["build"] != "B" || v["lint"] != "L" || v["test"] != "T" {
		t.Fatalf("deploy sees %v, want build+lint+test", v)
	}
	// An entry node has no ancestors at all.
	if v := g.visibleFor("build", outputs); len(v) != 0 {
		t.Fatalf("build sees %v, want nothing", v)
	}
}

func TestValidateGraph(t *testing.T) {
	steps := stepsNamed("a", "b", "c")
	// Derived-only workflows have no routes to validate.
	if msg := validateGraph(steps, nil, nil); msg != "" {
		t.Fatalf("no routes should validate, got %q", msg)
	}
	ok := []WorkflowRoute{{From: "a", To: "b"}, {From: "b", To: "c"}}
	if msg := validateGraph(stepsNamed("a", "b", "c"), ok, nil); msg != "" {
		t.Fatalf("valid graph rejected: %q", msg)
	}

	cases := []struct {
		name  string
		steps []WorkflowStep
		rts   []WorkflowRoute
		want  string
	}{
		{"dangling from", steps, []WorkflowRoute{{From: "nope", To: "b"}}, "unknown step"},
		{"dangling to", steps, []WorkflowRoute{{From: "a", To: "nope"}}, "unknown step"},
		{"self loop", steps, []WorkflowRoute{{From: "a", To: "a"}}, "cannot route to itself"},
		{"duplicate", steps, []WorkflowRoute{{From: "a", To: "b"}, {From: "a", To: "b"}}, "duplicate route"},
		{"cycle", steps, []WorkflowRoute{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "a"}}, "cycle"},
		{"missing endpoints", steps, []WorkflowRoute{{From: "a"}}, "from and to are required"},
		{
			"bad condition fails at authoring time",
			steps,
			[]WorkflowRoute{{From: "a", To: "b", When: "steps.a.nosuchfield == 1"}},
			"route a->b",
		},
		{
			"non-boolean condition rejected",
			steps,
			[]WorkflowRoute{{From: "a", To: "b", When: "steps.a.output"}},
			"route a->b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateGraph(tc.steps, tc.rts, nil)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("validateGraph = %q, want it to contain %q", msg, tc.want)
			}
		})
	}
}

func TestValidateGraph_TooManyRoutes(t *testing.T) {
	steps := stepsNamed("a", "b")
	rts := make([]WorkflowRoute, maxRoutes+1)
	for i := range rts {
		rts[i] = WorkflowRoute{From: "a", To: "b", When: fmt.Sprintf("inputs.x == %q", fmt.Sprint(i))}
	}
	if msg := validateGraph(steps, rts, nil); !strings.Contains(msg, "too many routes") {
		t.Fatalf("validateGraph = %q, want too many routes", msg)
	}
}

// A workflow with stored routes uses them; one without derives a chain from the array.
func TestBuildGraph_StoredRoutesWinOverDerivation(t *testing.T) {
	steps := stepsNamed("a", "b", "c")
	wf := Workflow{Steps: steps}
	if got := routeSet(wf.buildGraph().routes); len(got) != 2 || got[0] != "a->b" {
		t.Fatalf("derived routes = %v", got)
	}
	wf.Routes = []WorkflowRoute{{From: "a", To: "c"}}
	got := routeSet(wf.buildGraph().routes)
	if len(got) != 1 || got[0] != "a->c" {
		t.Fatalf("stored routes = %v, want [a->c]", got)
	}
}
