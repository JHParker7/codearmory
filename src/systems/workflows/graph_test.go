package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// stepsWithGroups builds a node set from a compact "name:group" spec, where a
// group of -1 means nil (a solo sequential step).
func stepsWithGroups(spec ...string) []WorkflowStep {
	var out []WorkflowStep
	for _, s := range spec {
		name, grp, _ := strings.Cut(s, ":")
		ws := WorkflowStep{Step: Step{Name: name}}
		if grp != "" {
			var g int
			fmt.Sscanf(grp, "%d", &g)
			ws.ParallelGroup = &g
		}
		out = append(out, ws)
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

// deriveRoutes must reproduce the exact semantics groupSteps encodes, since it is
// what preserves the behaviour of every pipeline authored before routes existed.
func TestDeriveRoutes_ReproducesGroupSteps(t *testing.T) {
	cases := []struct {
		name  string
		steps []WorkflowStep
		want  []string
	}{
		{
			name:  "linear chain",
			steps: stepsWithGroups("a", "b", "c"),
			want:  []string{"a->b", "b->c"},
		},
		{
			name:  "parallel group fans out and joins",
			steps: stepsWithGroups("build", "lint:0", "test:0", "deploy"),
			want:  []string{"build->lint", "build->test", "lint->deploy", "test->deploy"},
		},
		{
			// The group integer is a run-length delimiter, not a set label: a
			// non-consecutive repeat of the same number is a NEW group, so this is
			// three groups (a | b | c) and a plain chain — not a->{b,c}.
			name:  "non-consecutive same group is not one group",
			steps: stepsWithGroups("a:0", "b:1", "c:0"),
			want:  []string{"a->b", "b->c"},
		},
		{
			name:  "leading parallel group has no inbound",
			steps: stepsWithGroups("a:0", "b:0", "c"),
			want:  []string{"a->c", "b->c"},
		},
		{
			name:  "trailing parallel group",
			steps: stepsWithGroups("a", "b:2", "c:2"),
			want:  []string{"a->b", "a->c"},
		},
		{
			name:  "single step has no routes",
			steps: stepsWithGroups("only"),
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

// A leading parallel group means several nodes are ready at t=0.
func TestNewGraph_EntriesAreNodesWithNoInbound(t *testing.T) {
	steps := stepsWithGroups("a:0", "b:0", "c")
	g := newGraph(steps, deriveRoutes(steps))
	got := append([]string(nil), g.entries...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("entries = %v, want [a b]", got)
	}
}

// visibleFor is what makes a graph run deterministic: a node sees its ancestors'
// outputs and nothing else, regardless of what finished first.
func TestVisibleFor_ScopesToAncestors(t *testing.T) {
	steps := stepsWithGroups("build", "lint:0", "test:0", "deploy")
	g := newGraph(steps, deriveRoutes(steps))
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
	steps := stepsWithGroups("a", "b", "c")
	// Derived-only workflows have no routes to validate.
	if msg := validateGraph(steps, nil); msg != "" {
		t.Fatalf("no routes should validate, got %q", msg)
	}
	ok := []WorkflowRoute{{From: "a", To: "b"}, {From: "b", To: "c"}}
	if msg := validateGraph(stepsWithGroups("a", "b", "c"), ok); msg != "" {
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
			"routes with parallel_group",
			stepsWithGroups("a", "b:0", "c:0"),
			[]WorkflowRoute{{From: "a", To: "b"}},
			"cannot be combined with routes",
		},
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
			msg := validateGraph(tc.steps, tc.rts)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("validateGraph = %q, want it to contain %q", msg, tc.want)
			}
		})
	}
}

func TestValidateGraph_TooManyRoutes(t *testing.T) {
	steps := stepsWithGroups("a", "b")
	rts := make([]WorkflowRoute, maxRoutes+1)
	for i := range rts {
		rts[i] = WorkflowRoute{From: "a", To: "b", When: fmt.Sprintf("inputs.x == %q", fmt.Sprint(i))}
	}
	if msg := validateGraph(steps, rts); !strings.Contains(msg, "too many routes") {
		t.Fatalf("validateGraph = %q, want too many routes", msg)
	}
}

// A workflow with stored routes uses them; one without derives from parallel_group.
func TestBuildGraph_StoredRoutesWinOverDerivation(t *testing.T) {
	steps := stepsWithGroups("a", "b", "c")
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
