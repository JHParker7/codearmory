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

// A loop's exit condition must type-check against the same env route conditions use,
// so a compiled loop is safe to reach the scheduler.
func TestLoopUntilCompiles(t *testing.T) {
	if _, err := compileWhen("steps.green.status == 'completed'"); err != nil {
		t.Fatalf("valid until failed to compile: %v", err)
	}
}
