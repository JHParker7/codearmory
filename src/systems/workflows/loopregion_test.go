package main

import (
	"strings"
	"testing"
)

func loopStep(name, loopID string) WorkflowStep {
	return WorkflowStep{Step: Step{Name: name, Action: "noop"}, LoopID: loopID}
}

func TestValidateLoopDef(t *testing.T) {
	cases := []struct {
		name string
		def  LoopDef
		want string // substring the message must contain, "" = valid
	}{
		{"zero limit", LoopDef{ID: "l", Limit: 0}, "at least 1"},
		{"over hard max", LoopDef{ID: "l", Limit: loopHardMax + 1}, "exceeds the max"},
		{"at hard max ok", LoopDef{ID: "l", Limit: loopHardMax}, ""},
		{"valid with until", LoopDef{ID: "l", Limit: 5, Until: "steps.verify.status == 'completed'"}, ""},
		{"bad until", LoopDef{ID: "l", Limit: 5, Until: "steps.verify.nope ==="}, "exit condition"},
	}
	for _, c := range cases {
		got := validateLoopDef(c.def)
		if c.want == "" && got != "" {
			t.Errorf("%s: want valid, got %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want message containing %q, got %q", c.name, c.want, got)
		}
	}
}

func TestValidateLoops(t *testing.T) {
	def := []LoopDef{{ID: "retry", Limit: 3, Until: "steps.green.status == 'completed'"}}

	// A valid two-step loop.
	steps := []WorkflowStep{loopStep("dev", "retry"), loopStep("green", "retry")}
	if msg := validateLoops(steps, def, nil); msg != "" {
		t.Errorf("valid loop rejected: %s", msg)
	}

	// loop_id naming no declared loop.
	if msg := validateLoops([]WorkflowStep{loopStep("dev", "ghost")}, def, nil); !strings.Contains(msg, "names no declared loop") {
		t.Errorf("unknown loop id: got %q", msg)
	}

	// A declared loop no step joins.
	if msg := validateLoops([]WorkflowStep{loopStep("x", "")}, def, nil); !strings.Contains(msg, "no step declares") {
		t.Errorf("empty loop: got %q", msg)
	}

	// A step in both a loop and a map region.
	both := WorkflowStep{Step: Step{Name: "dev", Action: "noop"}, LoopID: "retry", MapID: "m"}
	if msg := validateLoops([]WorkflowStep{both}, def, nil); !strings.Contains(msg, "both a loop and a map") {
		t.Errorf("loop+map: got %q", msg)
	}

	// A route crossing between two loops.
	def2 := []LoopDef{{ID: "a", Limit: 2}, {ID: "b", Limit: 2}}
	crossing := []WorkflowStep{loopStep("x", "a"), loopStep("y", "b")}
	if msg := validateLoops(crossing, def2, []WorkflowRoute{{From: "x", To: "y"}}); !strings.Contains(msg, "cannot cross directly between loops") {
		t.Errorf("loop boundary: got %q", msg)
	}
}

func TestLoopsOfGroupsNodes(t *testing.T) {
	def := []LoopDef{{ID: "retry", Limit: 3}}
	steps := []WorkflowStep{
		{Step: Step{Name: "setup", Action: "noop"}},
		loopStep("dev", "retry"),
		loopStep("green", "retry"),
	}
	loops := loopsOf(steps, def)
	l, ok := loops["retry"]
	if !ok {
		t.Fatal("retry loop not grouped")
	}
	if len(l.nodes) != 2 || l.nodes[0] != "dev" || l.nodes[1] != "green" {
		t.Errorf("loop nodes = %v, want [dev green]", l.nodes)
	}
	if l.firstIndex != 1 {
		t.Errorf("firstIndex = %d, want 1", l.firstIndex)
	}
	// A loop no step joins is dropped so it cannot wedge the frontier.
	if _, dropped := loopsOf([]WorkflowStep{{Step: Step{Name: "a"}}}, def)["retry"]; dropped {
		t.Error("empty loop should be dropped")
	}
}

// A nested loop's steps belong to it AND to every ancestor, so the outer loop's node
// set is the whole subtree and every nested node schedules under the OUTERMOST loop.
func TestLoopsOfNested(t *testing.T) {
	defs := []LoopDef{
		{ID: "inner", Limit: 3, Parent: "outer"},
		{ID: "outer", Limit: 3},
	}
	steps := []WorkflowStep{
		{Step: Step{Name: "arch", Action: "noop"}},
		loopStep("spec", "inner"),
		loopStep("red", "inner"),
		loopStep("dev", "outer"),
		loopStep("green", "outer"),
	}
	loops := loopsOf(steps, defs)
	if in := loops["inner"]; in == nil || len(in.nodes) != 2 || in.nodes[0] != "spec" || in.nodes[1] != "red" {
		t.Fatalf("inner nodes = %+v, want [spec red]", loops["inner"])
	}
	out := loops["outer"]
	want := []string{"spec", "red", "dev", "green"}
	if out == nil || len(out.nodes) != len(want) {
		t.Fatalf("outer nodes = %+v, want %v", out, want)
	}
	for i, n := range want {
		if out.nodes[i] != n {
			t.Fatalf("outer nodes = %v, want %v", out.nodes, want)
		}
	}
	if out.firstIndex != 1 {
		t.Errorf("outer firstIndex = %d, want 1 (spec)", out.firstIndex)
	}
	lof := loopOfNode(loops)
	for _, n := range want {
		if lof[n] != "outer" {
			t.Errorf("loopOf[%s] = %q, want outer (outermost)", n, lof[n])
		}
	}
	g := &workflowGraph{loops: loops}
	kids := descendantLoopDefs(g, "outer")
	if len(kids) != 1 || kids[0].ID != "inner" {
		t.Errorf("descendantLoopDefs(outer) = %v, want [inner]", kids)
	}
	if k := descendantLoopDefs(g, "inner"); len(k) != 0 {
		t.Errorf("inner has no descendants, got %v", k)
	}
}

// Nesting validation: a well-formed nest passes (including the derived inner->outer
// edge red->dev, which is internal to the shared outer super-node); a dangling or
// cyclic parent is rejected.
func TestValidateLoopsNesting(t *testing.T) {
	steps := []WorkflowStep{
		loopStep("spec", "inner"), loopStep("red", "inner"),
		loopStep("dev", "outer"), loopStep("green", "outer"),
	}
	routes := []WorkflowRoute{{From: "spec", To: "red"}, {From: "red", To: "dev"}, {From: "dev", To: "green"}}

	good := []LoopDef{{ID: "inner", Limit: 3, Parent: "outer"}, {ID: "outer", Limit: 3}}
	if msg := validateLoops(steps, good, routes); msg != "" {
		t.Errorf("valid nesting rejected: %s", msg)
	}
	bad := []LoopDef{{ID: "inner", Limit: 3, Parent: "ghost"}, {ID: "outer", Limit: 3}}
	if msg := validateLoops(steps, bad, routes); !strings.Contains(msg, "names no declared loop") {
		t.Errorf("dangling parent: got %q", msg)
	}
	cyc := []LoopDef{{ID: "inner", Limit: 3, Parent: "outer"}, {ID: "outer", Limit: 3, Parent: "inner"}}
	if msg := validateLoops(steps, cyc, routes); !strings.Contains(msg, "cyclic") {
		t.Errorf("cyclic parent: got %q", msg)
	}
}

// A loop's exit condition must type-check against the same env route conditions use,
// so a compiled loop is safe to reach the scheduler.
func TestLoopUntilCompiles(t *testing.T) {
	if _, err := compileWhen("steps.green.status == 'completed'"); err != nil {
		t.Fatalf("valid until failed to compile: %v", err)
	}
}


// A nested loop composes its label onto the parent iteration's, keyed by each loop's
// var, so the two loop dimensions are distinct and separately selectable in the run
// view (rather than colliding on a bare inner "[attempt=N]").
func TestLoopIterNameComposes(t *testing.T) {
	outer := loopIterName("spec", "draw", 1)
	if outer != "spec [draw=1]" {
		t.Fatalf("outer label = %q, want %q", outer, "spec [draw=1]")
	}
	inner := loopIterName(outer, "specattempt", 2)
	if inner != "spec [draw=1] [specattempt=2]" {
		t.Fatalf("composed label = %q, want %q", inner, "spec [draw=1] [specattempt=2]")
	}
	if got := loopIterName("dev", "", 3); got != "dev [attempt=3]" {
		t.Errorf("no-var label = %q, want %q", got, "dev [attempt=3]")
	}
	m := mergeVars(map[string]string{"draw": "1"}, map[string]string{"specattempt": "2"})
	if m["draw"] != "1" || m["specattempt"] != "2" {
		t.Errorf("mergeVars = %v, want both keys", m)
	}
}
