package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// applyStepsMsg is a convenience that calls Update and returns the updated model.
func applyStepsMsg(m stepsModel, msg tea.Msg) stepsModel {
	updated, _ := m.Update(msg)
	return updated.(stepsModel)
}

// ── Model: initial state ──────────────────────────────────────────────────────

func TestStepsModel_InitialState(t *testing.T) {
	m := newStepsModel()
	if m.view != stepsViewList {
		t.Errorf("initial view = %v, want stepsViewList", m.view)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if m.err != nil {
		t.Errorf("initial err = %v, want nil", m.err)
	}
	if m.steps != nil {
		t.Errorf("initial steps should be nil, got %v", m.steps)
	}
}

func TestStepsModel_Init_BatchesFetch(t *testing.T) {
	if newStepsModel().Init() == nil {
		t.Error("Init should emit a fetch cmd")
	}
}

// ── Model: messages ───────────────────────────────────────────────────────────

func TestStepsModel_StepsMsg_PopulatesTable(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{
		{StepID: "s1", Name: "build", Action: "forge/run", Timeout: 60},
	}))
	if m.loading {
		t.Error("loading should be false after tuiStepsMsg")
	}
	if len(m.steps) != 1 || m.steps[0].Name != "build" {
		t.Errorf("steps = %v, want [build]", m.steps)
	}
}

func TestStepsModel_ActionsMsg_Caches(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiActionsMsg([]string{"forge/run", "http"}))
	if len(m.actions) != 2 || m.actions[1] != "http" {
		t.Errorf("actions = %v, want [forge/run http]", m.actions)
	}
}

func TestStepsModel_ErrMsg(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiErrMsg{err: fmt.Errorf("connection refused")})
	if m.err == nil {
		t.Fatal("error should be set")
	}
	if m.loading {
		t.Error("loading should be false after errMsg")
	}
}

func TestStepsModel_WindowResize(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 {
		t.Errorf("dimensions = %dx%d, want 120x40", m.width, m.height)
	}
}

// ── Model: list-view keys ─────────────────────────────────────────────────────

func TestStepsModel_List_QGoesHome(t *testing.T) {
	m := newStepsModel()
	m.loading = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q returned nil cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("q cmd returned %T, want goHomeMsg", cmd())
	}
}

func TestStepsModel_CtrlC_Quits(t *testing.T) {
	m := newStepsModel()
	m.loading = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c returned nil cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

func TestStepsModel_List_RRefreshes(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key should emit a refresh cmd")
	}
}

func TestStepsModel_N_OpensCreateStepForm(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(stepsModel)
	if m2.view != stepsViewCreate {
		t.Errorf("view = %v, want stepsViewCreate", m2.view)
	}
	if cmd == nil {
		t.Error("opening the step form should return a focus cmd")
	}
	if m2.form.value("action") != "forge/run" {
		t.Errorf("action default = %q, want forge/run", m2.form.value("action"))
	}
}

// ── Model: create-step flow ───────────────────────────────────────────────────

func TestStepsModel_CreateStep_Esc_BacksToList(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm(nil)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("esc in create-step view should return to the steps list")
	}
}

func TestStepsModel_CreateStep_Submit_MissingName_StaysWithError(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm(nil)
	// Clear the defaulted action so name is the first failure.
	m.form.fields[0].input.SetValue("")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(stepsModel)
	if m2.view != stepsViewCreate {
		t.Error("submit with no name should stay in the create-step view")
	}
	if m2.form.errMsg == "" {
		t.Error("submit with no name should set an inline error")
	}
	if cmd != nil {
		t.Error("invalid submit should not emit a request cmd")
	}
}

func TestStepsModel_StepCreatedMsg_ReturnsToListAndRefetches(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	updated, cmd := m.Update(tuiStepCreatedMsg{})
	m2 := updated.(stepsModel)
	if m2.view != stepsViewList {
		t.Error("tuiStepCreatedMsg should return to the steps list")
	}
	if !m2.loading || cmd == nil {
		t.Error("tuiStepCreatedMsg should set loading and emit a refetch cmd")
	}
}

func TestStepsModel_FormErrMsg_ShowsInlineError(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm(nil)
	updated, _ := m.Update(tuiFormErrMsg{err: fmt.Errorf("boom")})
	if !strings.Contains(updated.(stepsModel).form.errMsg, "boom") {
		t.Error("form error should surface in the create-step view")
	}
}

// ── Step form: action-type selector ───────────────────────────────────────────

func TestStepsModel_CreateStepForm_UsesActionSelector(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"tickets/create", "forge/run", "http"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(stepsModel)
	// Selector defaults to forge/run regardless of catalog order.
	if got := m2.form.value("action"); got != "forge/run" {
		t.Errorf("action = %q, want forge/run (default selection)", got)
	}
	// ←/→ cycles through the catalog. Focus tab order: name(0), action(1).
	f, _, _ := m2.form.update(tea.KeyMsg{Type: tea.KeyTab}) // focus action
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})      // cycle to http
	if got := f.value("action"); got != "http" {
		t.Errorf("action after cycle = %q, want http", got)
	}
}

func TestStepsModel_CreateStepForm_FallsBackToTextDefault(t *testing.T) {
	m := newStepsModel() // m.actions is nil
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(stepsModel)
	if got := m2.form.value("action"); got != "forge/run" {
		t.Errorf("action = %q, want forge/run text default when catalog unavailable", got)
	}
}

// ── View: rendering ───────────────────────────────────────────────────────────

func TestStepsView_LoadingList(t *testing.T) {
	m := newStepsModel() // loading=true by default
	if !strings.Contains(m.View(), "Loading") {
		t.Errorf("loading view should say Loading, got: %q", m.View())
	}
}

func TestStepsView_EmptyList(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{}))
	if !strings.Contains(m.View(), "No steps") {
		t.Errorf("empty-steps view should say 'No steps', got: %q", m.View())
	}
}

func TestStepsView_List_RendersTitle(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{
		{Name: "build", Action: "forge/run"},
	}))
	v := m.View()
	if !strings.Contains(v, "Steps") || !strings.Contains(v, "build") {
		t.Errorf("steps view should show the title and step name, got: %q", v)
	}
}

func TestStepsView_List_HelpMentionsNew(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{}))
	if !strings.Contains(m.View(), "new") {
		t.Error("steps help should mention the new-step shortcut")
	}
}

func TestStepsView_Create_RendersForm(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm(nil)
	if !strings.Contains(m.View(), "New Step") {
		t.Error("create-step view should show the form heading")
	}
}

func TestStepsView_ErrorState_401_ShowsAuthHint(t *testing.T) {
	m := newStepsModel()
	m.err = fmt.Errorf("HTTP 401: unauthorized")
	if !strings.Contains(m.View(), "auth login") {
		t.Errorf("401 error view should hint at armory auth login, got: %q", m.View())
	}
}

// ── Fetch: tuiFetchSteps ──────────────────────────────────────────────────────

func TestTUIFetchSteps_Success(t *testing.T) {
	steps := []tuiStep{{StepID: "s1", Name: "unit_tests", Action: "forge/run"}}
	body, _ := json.Marshal(steps)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := tuiFetchSteps()
	if rec.Method != "GET" || rec.Path != "/workflows/steps" {
		t.Errorf("request = %s %s, want GET /workflows/steps", rec.Method, rec.Path)
	}
	result, ok := msg.(tuiStepsMsg)
	if !ok {
		t.Fatalf("msg = %T, want tuiStepsMsg", msg)
	}
	if len(result) != 1 || result[0].Name != "unit_tests" {
		t.Errorf("result = %v, want [unit_tests]", result)
	}
}

func TestTUIFetchSteps_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	setupCLI(t, srv)
	if _, ok := tuiFetchSteps().(tuiErrMsg); !ok {
		t.Error("HTTP error should return tuiErrMsg")
	}
}

// ── Fetch: tuiFetchActions ────────────────────────────────────────────────────

func TestTUIFetchActions_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK,
		`[{"name":"forge/run"},{"name":"tickets/create"},{"name":"http"}]`)
	setupCLI(t, srv)
	msg := tuiFetchActions()
	if rec.Method != "GET" || rec.Path != "/workflows/actions" {
		t.Errorf("request = %s %s, want GET /workflows/actions", rec.Method, rec.Path)
	}
	acts, ok := msg.(tuiActionsMsg)
	if !ok || len(acts) != 3 || acts[0] != "forge/run" {
		t.Errorf("msg = %#v, want tuiActionsMsg with 3 names", msg)
	}
}

func TestTUIFetchActions_ErrorDegradesToEmpty(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	msg, ok := tuiFetchActions().(tuiActionsMsg)
	if !ok || len(msg) != 0 {
		t.Errorf("actions = %#v, want empty tuiActionsMsg on error", msg)
	}
}

// ── Create step: ciSubmitCreateStep ───────────────────────────────────────────

func TestCISubmitCreateStep_ForgeRun_PostsCorrectPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"step_id":"s1"}`)
	setupCLI(t, srv)

	msg := ciSubmitCreateStep("unit_tests", "forge/run", "ubuntu:22.04",
		"go test ./...", "", "FOO=bar", "runs the tests", 120)()

	if _, ok := msg.(tuiStepCreatedMsg); !ok {
		t.Fatalf("msg = %T, want tuiStepCreatedMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/workflows/steps" {
		t.Errorf("request = %s %s, want POST /workflows/steps", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["name"] != "unit_tests" || got["action"] != "forge/run" {
		t.Errorf("name/action = %v/%v, want unit_tests/forge/run", got["name"], got["action"])
	}
	if got["timeout"].(float64) != 120 {
		t.Errorf("timeout = %v, want 120", got["timeout"])
	}
	with, _ := got["with"].(map[string]any)
	if with["image"] != "ubuntu:22.04" || with["run"] != "go test ./..." {
		t.Errorf("with = %v, want image+run set", got["with"])
	}
	env, _ := with["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Errorf("with.env = %v, want FOO=bar", with["env"])
	}
}

func TestCISubmitCreateStep_ForgeRun_MissingImage_ReturnsFormErr(t *testing.T) {
	// buildWith rejects forge/run without an image before any HTTP call.
	msg := ciSubmitCreateStep("s", "forge/run", "", "echo hi", "", "", "", 30)()
	if _, ok := msg.(tuiFormErrMsg); !ok {
		t.Errorf("msg = %T, want tuiFormErrMsg for forge/run without image", msg)
	}
}

func TestCISubmitCreateStep_OtherAction_PassesWithJSON(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"step_id":"s1"}`)
	setupCLI(t, srv)

	msg := ciSubmitCreateStep("notify", "tickets/create", "", "",
		`{"title":"hi","priority":"high"}`, "", "", 30)()
	if _, ok := msg.(tuiStepCreatedMsg); !ok {
		t.Fatalf("msg = %T, want tuiStepCreatedMsg", msg)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	with, _ := got["with"].(map[string]any)
	if with["title"] != "hi" || with["priority"] != "high" {
		t.Errorf("with = %v, want title/priority from JSON", got["with"])
	}
}

func TestCISubmitCreateStep_OtherAction_BadWithJSON_ReturnsFormErr(t *testing.T) {
	msg := ciSubmitCreateStep("notify", "tickets/create", "", "", "{not json", "", "", 30)()
	if _, ok := msg.(tuiFormErrMsg); !ok {
		t.Errorf("msg = %T, want tuiFormErrMsg for malformed --with JSON", msg)
	}
}
