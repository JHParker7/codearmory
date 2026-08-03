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

// Two fields plus the auto-appended Submit button make three focusable controls,
// so three tabs wrap back to the first field.
func TestTUIForm_TabWrapsAround(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab}) // → b
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab}) // → Submit button
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab}) // → wraps to a
	if f.focus != 0 {
		t.Errorf("focus after wrapping = %d, want 0", f.focus)
	}
}

// shift+tab from the first field wraps to the last control — the Submit button
// (index 2 with two fields).
func TestTUIForm_ShiftTabRetreats(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if f.focus != 2 {
		t.Errorf("focus after shift+tab from 0 = %d, want 2 (wrap to Submit button)", f.focus)
	}
}

func TestTUIForm_EscReturnsCancel(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	_, action, _ := f.update(tea.KeyMsg{Type: tea.KeyEsc})
	if action != formCancel {
		t.Errorf("esc action = %v, want formCancel", action)
	}
}

// Enter no longer submits from a text field — it advances toward the Submit
// button (so multi-line fields can use Enter for newlines). ctrl+s still submits.
func TestTUIForm_EnterAdvancesFromTextField(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""), formInput("b", "B", ""))
	f, action, _ := f.update(tea.KeyMsg{Type: tea.KeyEnter})
	if action != formNone {
		t.Errorf("enter on a text field action = %v, want formNone", action)
	}
	if f.focus != 1 {
		t.Errorf("enter should advance focus 0→1, got %d", f.focus)
	}
}

func TestTUIForm_CtrlSSubmits(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	_, action, _ := f.update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if action != formSubmit {
		t.Errorf("ctrl+s action = %v, want formSubmit", action)
	}
}

// The Submit button submits on Enter (and Space) when focused.
func TestTUIForm_ButtonSubmitsOnEnter(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab}) // a → Submit button
	_, action, _ := f.update(tea.KeyMsg{Type: tea.KeyEnter})
	if action != formSubmit {
		t.Errorf("enter on the Submit button action = %v, want formSubmit", action)
	}
	f2, _ := newTUIForm("T", formInput("a", "A", ""))
	f2, _, _ = f2.update(tea.KeyMsg{Type: tea.KeyTab}) // a → Submit button
	_, action2, _ := f2.update(tea.KeyMsg{Type: tea.KeySpace})
	if action2 != formSubmit {
		t.Errorf("space on the Submit button action = %v, want formSubmit", action2)
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

func TestTUIForm_ViewShowsHelp(t *testing.T) {
	f, _ := newTUIForm("T", formInput("a", "A", ""))
	f.help = "refs: ${steps.STEP.output}"
	if !strings.Contains(f.view(80, 24), "${steps.STEP.output}") {
		t.Error("view should render the form help text when set")
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

func TestTUIForm_SelectKV_ReadsBackValueNotLabel(t *testing.T) {
	// The form shows the name but submits the id at the chosen index.
	f, _ := newTUIForm("T", formSelectKV("workflow", "Workflow",
		[]string{"build-ci", "deploy"}, []string{"wf-1", "wf-2"}))
	if got := f.value("workflow"); got != "wf-1" {
		t.Errorf("value = %q, want wf-1 (the id, not the label)", got)
	}
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})
	if got := f.value("workflow"); got != "wf-2" {
		t.Errorf("value after cycle = %q, want wf-2", got)
	}
}

func TestTUIForm_SelectKV_RendersLabel(t *testing.T) {
	f, _ := newTUIForm("T", formSelectKV("workflow", "Workflow",
		[]string{"build-ci"}, []string{"wf-1"}))
	v := f.view(80, 24)
	if !strings.Contains(v, "build-ci") {
		t.Errorf("select should display the label, got: %q", v)
	}
	if strings.Contains(v, "wf-1") {
		t.Errorf("select should not display the underlying id, got: %q", v)
	}
}

func TestTUIForm_SelectKV_EmptyValueOption(t *testing.T) {
	// A leading "(none)" label mapped to "" reads back as empty (optional field).
	f, _ := newTUIForm("T", formSelectKV("role", "Role",
		[]string{"(none)", "admin"}, []string{"", "role-1"}))
	if got := f.value("role"); got != "" {
		t.Errorf("value = %q, want empty for the (none) option", got)
	}
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})
	if got := f.value("role"); got != "role-1" {
		t.Errorf("value after cycle = %q, want role-1", got)
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
	if !strings.Contains(v, "←/→") {
		t.Error("a form with a select should show the ←/→ options hint")
	}
}

func TestTUIForm_ViewEmptyOptionShowsDefaultLabel(t *testing.T) {
	f, _ := newTUIForm("T", formSelect("a", "A", []string{""}))
	if !strings.Contains(f.view(80, 24), "(default)") {
		t.Error("an empty select option should render as (default)")
	}
}

// ── tuiForm: textarea (multi-line) fields ──────────────────────────────────────

func TestTUIForm_TextareaAcceptsNewline(t *testing.T) {
	f, _ := newTUIForm("T", formTextarea("cmd", "Command", ""))
	// focus starts on the textarea (field 0); enter inserts a newline, not submit.
	f, action, _ := f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyEnter})
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if action != formNone {
		t.Errorf("typing in a textarea returned action %v, want formNone", action)
	}
	if got := f.value("cmd"); got != "a\nb" {
		t.Errorf("textarea value = %q, want \"a\\nb\" (enter inserts a newline)", got)
	}
}

func TestTUIForm_TextareaHintMentionsNewline(t *testing.T) {
	f, _ := newTUIForm("T", formTextarea("cmd", "Command", ""))
	if !strings.Contains(f.view(80, 24), "newline") {
		t.Error("a form with a textarea should hint that enter inserts a newline")
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
