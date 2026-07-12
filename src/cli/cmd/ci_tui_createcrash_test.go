package cmd

import (
	"runtime/debug"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive feeds one message through the model and renders, recovering any panic and
// reporting which step blew up (with the stack) instead of aborting the whole test.
func drive(t *testing.T, m tea.Model, label string, msg tea.Msg) tea.Model {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC at step %q: %v\n%s", label, r, debug.Stack())
		}
	}()
	nm, _ := m.Update(msg)
	_ = nm.View()
	return nm
}

// TestTuiCreatePipeline_NoPanic drives the "create a new pipeline" flow headlessly —
// window size, list load (empty and populated), 'n' to open the create form, typing
// into each field, and submit — asserting none of the synchronous Update/View steps
// panic. Reproduces the "tui crashed when trying to create a new pipeline" report
// (ticket 05df34c7) as a regression guard.
func TestTuiCreatePipeline_NoPanic(t *testing.T) {
	key := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

	scenarios := []struct {
		name string
		load tea.Msg
	}{
		{"empty list", tuiPipelinesMsg{}},
		{"one pipeline", tuiPipelinesMsg{{WorkflowID: "w1", Name: "demo"}}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var m tea.Model = newTUIModel()
			m = drive(t, m, "window-size", tea.WindowSizeMsg{Width: 100, Height: 40})
			m = drive(t, m, "list-loaded", sc.load)
			m = drive(t, m, "press-n", key("n"))
			// Type a name, tab to steps, type a DSL, tab to desc.
			m = drive(t, m, "type-name", key("mypipe"))
			m = drive(t, m, "tab-1", tea.KeyMsg{Type: tea.KeyTab})
			m = drive(t, m, "type-steps", key("build->test"))
			m = drive(t, m, "tab-2", tea.KeyMsg{Type: tea.KeyTab})
			// Submit (ctrl+s). The save cmd is returned but not executed here, so this
			// exercises only the synchronous validate/render path.
			m = drive(t, m, "submit", tea.KeyMsg{Type: tea.KeyCtrlS})
			// Post-create success: the model returns to the list and re-renders.
			m = drive(t, m, "created", tuiPipelineCreatedMsg{})
			// List reload + flow-diagram defs with the mix that stresses the diagram:
			// sequential, a parallel group, a matrix fan-out, and an approval gate.
			m = drive(t, m, "list-reload", tuiPipelinesMsg{{WorkflowID: "w1", Name: "mypipe"}})
			grp := 1
			m = drive(t, m, "diagram-defs", tuiPipelineDefMsg{workflowID: "w1", steps: []tuiWorkflowStep{
				{Name: "build", Action: "forge/run"},
				{Name: "a", Action: "forge/run", ParallelGroup: &grp},
				{Name: "b", Action: "forge/run", ParallelGroup: &grp},
				{Name: "fan", Action: "forge/run", Matrix: &matrixConfig{Var: "r", Values: []string{"x", "y"}}},
				{Name: "gate", Action: "approval", Approval: &approvalGate{Message: "ok?"}},
			}})
			// Run-detail render with matrix legs (same step_index) + a gate row.
			m = drive(t, m, "run-detail", tuiRunDetailMsg(tuiRunFull{
				tuiRun:   tuiRun{RunID: "r1", Status: "running"},
				StepRuns: []tuiStepRun{{StepIndex: 0, StepName: "build", Status: "completed"}, {StepIndex: 1, StepName: "fan [r=x]", Status: "running"}, {StepIndex: 1, StepName: "fan [r=y]", Status: "running"}},
			}))
			// Also exercise a tiny/degenerate window, a common TUI panic trigger.
			_ = drive(t, m, "tiny-window", tea.WindowSizeMsg{Width: 1, Height: 1})
		})
	}
}
