package cmd

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeForm feeds each rune of s into the form's active field.
func typeForm(f tuiForm, s string) tuiForm {
	for _, r := range s {
		f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return f
}

// ── tuiForm: focus / navigation ───────────────────────────────────────────────

func TestTUIForm_NewFocusesFirstField(t *testing.T) {
	f, cmd := newTUIForm("T",
		formInput("a", "A", ""),
		formInput("b", "B", ""),
	)
	if f.focus != 0 {
		t.Errorf("focus = %d, want 0", f.focus)
	}
	if cmd == nil {
		t.Error("newTUIForm should return a blink cmd for the focused field")
	}
}

func TestTUIForm_TabAdvancesFocus(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f, action, _ := f.update(tea.KeyMsg{Type: tea.KeyTab})
	if action != formNone {
		t.Errorf("tab action = %v, want formNone", action)
	}
	if f.focus != 1 {
		t.Errorf("focus after tab = %d, want 1", f.focus)
	}
}

func TestTUIForm_TabWrapsAround(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab})
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab})
	if f.focus != 0 {
		t.Errorf("focus after wrapping = %d, want 0", f.focus)
	}
}

func TestTUIForm_ShiftTabRetreats(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if f.focus != 1 {
		t.Errorf("focus after shift+tab from 0 = %d, want 1 (wrap)", f.focus)
	}
}

func TestTUIForm_EscReturnsCancel(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	_, action, _ := f.update(tea.KeyMsg{Type: tea.KeyEsc})
	if action != formCancel {
		t.Errorf("esc action = %v, want formCancel", action)
	}
}

func TestTUIForm_EnterReturnsSubmit(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	_, action, _ := f.update(tea.KeyMsg{Type: tea.KeyEnter})
	if action != formSubmit {
		t.Errorf("enter action = %v, want formSubmit", action)
	}
}

// ── tuiForm: value handling ───────────────────────────────────────────────────

func TestTUIForm_TypingPopulatesActiveField(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f = typeForm(f, "hello")
	if got := f.value("a"); got != "hello" {
		t.Errorf("value(a) = %q, want hello", got)
	}
	if got := f.value("b"); got != "" {
		t.Errorf("value(b) = %q, want empty (not focused)", got)
	}
}

func TestTUIForm_ValueTrimsWhitespace(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	f.fields[0].input.SetValue("  spaced  ")
	if got := f.value("a"); got != "spaced" {
		t.Errorf("value should be trimmed, got %q", got)
	}
}

func TestTUIForm_ValueMissingKey(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	if got := f.value("nope"); got != "" {
		t.Errorf("value(nope) = %q, want empty", got)
	}
}

// TestTUIForm_UpdateDoesNotAliasCaller guards against the slice-aliasing trap:
// bubbletea passes models by value, so update must not mutate the caller's
// field backing array.
func TestTUIForm_UpdateDoesNotAliasCaller(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	_ = typeForm(f, "x")
	if got := f.value("a"); got != "" {
		t.Errorf("original form mutated by update: value(a) = %q, want empty", got)
	}
}

// ── tuiForm: view ─────────────────────────────────────────────────────────────

func TestTUIForm_ViewRendersTitleLabelsAndHint(t *testing.T) {
	f, _ := newTUIForm("New Thing", formInput("a", "Alpha", ""))
	v := f.view(80, 24)
	for _, want := range []string{"New Thing", "Alpha", "submit", "cancel"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q, got: %q", want, v)
		}
	}
}

func TestTUIForm_ViewShowsError(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	f.errMsg = "image is required"
	if !strings.Contains(f.view(80, 24), "image is required") {
		t.Error("view should render the inline error message")
	}
}

// ── tuiForm: select / cycle fields ────────────────────────────────────────────

func TestTUIForm_SelectDefault_SelectsValue(t *testing.T) {
	f, _ := newTUIForm("T", formSelectDefault("a", "A", []string{"x", "y", "z"}, "y"))
	if got := f.value("a"); got != "y" {
		t.Errorf("value(a) = %q, want y (the default)", got)
	}
}

func TestTUIForm_SelectDefault_MissingFallsToFirst(t *testing.T) {
	f, _ := newTUIForm("T", formSelectDefault("a", "A", []string{"x", "y"}, "nope"))
	if got := f.value("a"); got != "x" {
		t.Errorf("value(a) = %q, want x (first option when default absent)", got)
	}
}

func TestTUIForm_RightCyclesForward(t *testing.T) {
	f, _ := newTUIForm("T", formSelect("a", "A", []string{"x", "y", "z"}))
	f, action, _ := f.update(tea.KeyMsg{Type: tea.KeyRight})
	if action != formNone {
		t.Errorf("right action = %v, want formNone", action)
	}
	if got := f.value("a"); got != "y" {
		t.Errorf("value after right = %q, want y", got)
	}
}

func TestTUIForm_LeftCyclesBackwardWithWrap(t *testing.T) {
	f, _ := newTUIForm("T", formSelect("a", "A", []string{"x", "y", "z"}))
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyLeft}) // wraps to last
	if got := f.value("a"); got != "z" {
		t.Errorf("value after left from first = %q, want z (wrap)", got)
	}
}

func TestTUIForm_EmptyOptionReadsAsEmptyString(t *testing.T) {
	// Leading "" is the "(default)" sentinel used by the runner selector.
	f, _ := newTUIForm("T", formSelect("runner", "Runner", []string{"", "large"}))
	if got := f.value("runner"); got != "" {
		t.Errorf("value = %q, want empty string for the default option", got)
	}
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})
	if got := f.value("runner"); got != "large" {
		t.Errorf("value after right = %q, want large", got)
	}
}

func TestTUIForm_SelectIgnoresTextRunes(t *testing.T) {
	f, _ := newTUIForm("T", formSelect("a", "A", []string{"x", "y"}))
	f = typeForm(f, "abc") // runes should not change a select
	if got := f.value("a"); got != "x" {
		t.Errorf("value = %q, want x (typing must not affect a select)", got)
	}
}

func TestTUIForm_LeftRightOnTextFieldDoesNotCycleNeighbours(t *testing.T) {
	// A text field must consume ←/→ for cursor movement, not change a sibling.
	f, _ := newTUIForm("T",
		formInput("name", "Name", ""),
		formSelect("opt", "Opt", []string{"x", "y"}),
	)
	// focus is on the text field (index 0); right should not cycle the select.
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})
	if got := f.value("opt"); got != "x" {
		t.Errorf("select value = %q, want x (unchanged while text field focused)", got)
	}
}

func TestTUIForm_ViewRendersSelectAndCycleHint(t *testing.T) {
	f, _ := newTUIForm("T", formSelect("a", "Alpha", []string{"one", "two"}))
	v := f.view(80, 24)
	if !strings.Contains(v, "one") || !strings.Contains(v, "‹") {
		t.Errorf("select field should render the chosen value in cycle chrome, got: %q", v)
	}
	if !strings.Contains(v, "cycle") {
		t.Error("a form with a select should show the ←/→ cycle hint")
	}
}

func TestTUIForm_ViewEmptyOptionShowsDefaultLabel(t *testing.T) {
	f, _ := newTUIForm("T", formSelect("a", "A", []string{""}))
	if !strings.Contains(f.view(80, 24), "(default)") {
		t.Error("an empty select option should render as (default)")
	}
}

// ── parseCommandLine ──────────────────────────────────────────────────────────

func TestParseCommandLine_Simple(t *testing.T) {
	got, err := parseCommandLine("echo hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"echo", "hello", "world"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseCommandLine_DoubleQuotes(t *testing.T) {
	got, err := parseCommandLine(`sh -c "echo hi there"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"sh", "-c", "echo hi there"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseCommandLine_SingleQuotes(t *testing.T) {
	got, err := parseCommandLine(`sh -c 'a b c'`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"sh", "-c", "a b c"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseCommandLine_Empty(t *testing.T) {
	got, err := parseCommandLine("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestParseCommandLine_UnterminatedQuote(t *testing.T) {
	if _, err := parseCommandLine(`echo "oops`); err == nil {
		t.Error("expected an error for an unterminated quote")
	}
}

func TestParseCommandLine_EscapedSpace(t *testing.T) {
	got, err := parseCommandLine(`a\ b c`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a b", "c"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// ── parseEnvAssignments ───────────────────────────────────────────────────────

func TestParseEnvAssignments_Empty(t *testing.T) {
	got, err := parseEnvAssignments("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParseEnvAssignments_Pairs(t *testing.T) {
	got, err := parseEnvAssignments("FOO=bar BAZ=qux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Errorf("got %v, want FOO=bar BAZ=qux", got)
	}
}

func TestParseEnvAssignments_QuotedValue(t *testing.T) {
	got, err := parseEnvAssignments(`MSG="hello world"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["MSG"] != "hello world" {
		t.Errorf("MSG = %q, want 'hello world'", got["MSG"])
	}
}

func TestParseEnvAssignments_Invalid(t *testing.T) {
	if _, err := parseEnvAssignments("NOTANASSIGNMENT"); err == nil {
		t.Error("expected an error for a token without '='")
	}
}

// ── splitEvents ───────────────────────────────────────────────────────────────

func TestSplitEvents_Spaces(t *testing.T) {
	if got := splitEvents("push pull_request"); !equalSlice(got, []string{"push", "pull_request"}) {
		t.Errorf("got %v, want [push pull_request]", got)
	}
}

func TestSplitEvents_Commas(t *testing.T) {
	if got := splitEvents("push,pull_request, tag"); !equalSlice(got, []string{"push", "pull_request", "tag"}) {
		t.Errorf("got %v, want [push pull_request tag]", got)
	}
}

func TestSplitEvents_Empty(t *testing.T) {
	if got := splitEvents("   "); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
