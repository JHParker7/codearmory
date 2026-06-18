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

func TestStepsModel_List_EscGoesHome(t *testing.T) {
	m := newStepsModel()
	m.loading = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc returned nil cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc cmd returned %T, want goHomeMsg", cmd())
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

// ── Model: edit-step flow ─────────────────────────────────────────────────────

// sampleStep is a forge/run step used to exercise the edit pre-fill.
func sampleStep() tuiStep {
	return tuiStep{
		StepID:      "s1",
		Name:        "unit_tests",
		Action:      "forge/run",
		With:        map[string]any{"image": "ubuntu:22.04", "run": "go test ./..."},
		Timeout:     120,
		Description: "runs the tests",
	}
}

func TestStepsModel_E_OpensEditStepFormPreFilled(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"forge/run", "http"}
	m.images = []string{"ubuntu:22.04", "alpine:3.19"}
	m = applyStepsMsg(m, tuiStepsMsg([]tuiStep{sampleStep()}))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	m2 := updated.(stepsModel)

	if m2.view != stepsViewEdit {
		t.Fatalf("view = %v, want stepsViewEdit", m2.view)
	}
	if cmd == nil {
		t.Error("opening the edit form should return a focus cmd")
	}
	if m2.editStepID != "s1" {
		t.Errorf("editStepID = %q, want s1", m2.editStepID)
	}
	if m2.form.title != "Edit Step" {
		t.Errorf("form title = %q, want Edit Step", m2.form.title)
	}
	for key, want := range map[string]string{
		"name":       "unit_tests",
		"action":     "forge/run",
		"with.image": "ubuntu:22.04",
		"with.run":   "go test ./...",
		"timeout":    "120",
		"desc":       "runs the tests",
	} {
		if got := m2.form.value(key); got != want {
			t.Errorf("form[%q] = %q, want %q", key, got, want)
		}
	}
}

// enter is an alias for edit on the highlighted row.
func TestStepsModel_Enter_OpensEditStepForm(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{sampleStep()}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(stepsModel).view != stepsViewEdit {
		t.Error("enter on a step row should open the edit form")
	}
}

func TestStepsModel_E_EmptyList_NoOp(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("edit with no steps should stay on the list")
	}
}

func TestStepsModel_EditStep_Esc_BacksToList(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{sampleStep()}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	updated, _ = updated.(stepsModel).Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("esc in edit-step view should return to the steps list")
	}
}

func TestStepsModel_EditStep_Submit_PutsToStepID(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"step_id":"s1"}`)
	setupCLI(t, srv)

	m := newStepsModel()
	m.actions = []string{"forge/run"}
	m.images = []string{"ubuntu:22.04"}
	m = applyStepsMsg(m, tuiStepsMsg([]tuiStep{sampleStep()}))

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	_, cmd := updated.(stepsModel).Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("submitting the edit form should emit a request cmd")
	}
	if _, ok := cmd().(tuiStepCreatedMsg); !ok {
		t.Fatalf("edit submit msg = %T, want a list-refresh msg", cmd())
	}
	if rec.Method != "PUT" || rec.Path != "/workflows/steps/s1" {
		t.Errorf("request = %s %s, want PUT /workflows/steps/s1", rec.Method, rec.Path)
	}
}

// ── Model: create-step flow ───────────────────────────────────────────────────

func TestStepsModel_CreateStep_Esc_BacksToList(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm("", nil, stepCatalogs{})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("esc in create-step view should return to the steps list")
	}
}

func TestStepsModel_CreateStep_Submit_MissingName_StaysWithError(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm("", nil, stepCatalogs{})
	// Clear the defaulted action so name is the first failure.
	m.form.fields[0].input.SetValue("")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
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
	m.form, _ = newCIStepForm("", nil, stepCatalogs{})
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

// Pressing 'n' before the action prefetch lands opens the form with a free-text
// Action fallback. When the catalog arrives the open form must upgrade in place
// to the ←/→ selector, preserving anything already typed.
func TestStepsModel_CreateStepForm_UpgradesWhenCatalogArrivesLate(t *testing.T) {
	m := newStepsModel() // m.actions nil → form opens with text fallback
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(stepsModel)
	// Type a name to prove entered values survive the rebuild.
	m2.form, _, _ = m2.form.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("build")})

	updated, _ = m2.Update(tuiActionsMsg([]string{"tickets/create", "forge/run", "http"}))
	m3 := updated.(stepsModel)

	if got := m3.form.value("name"); got != "build" {
		t.Errorf("name = %q, want build preserved across upgrade", got)
	}
	if got := m3.form.value("action"); got != "forge/run" {
		t.Errorf("action = %q, want forge/run default after upgrade", got)
	}
	// The Action field is now a selector: ←/→ cycles, runes are ignored.
	f, _, _ := m3.form.update(tea.KeyMsg{Type: tea.KeyTab}) // focus action
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})      // cycle forge/run → http
	if got := f.value("action"); got != "http" {
		t.Errorf("action after cycle = %q, want http (selector active, not text)", got)
	}
}

// ── Step form: image selector ──────────────────────────────────────────────────

func TestStepsModel_CreateStepForm_UsesImageSelector(t *testing.T) {
	m := newStepsModel()
	m.images = []string{"alpine:3.19", "ubuntu:22.04"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(stepsModel)
	// The default action is forge/run, whose Image field is a required selector
	// pre-set to the first allowlist entry.
	if got := m2.form.value("with.image"); got != "alpine:3.19" {
		t.Errorf("image default = %q, want alpine:3.19 (first allowlist entry)", got)
	}
	// ←/→ cycles through the allowlist. Tab order: name(0), action(1), image(2).
	f, _, _ := m2.form.update(tea.KeyMsg{Type: tea.KeyTab}) // focus action
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab})        // focus image
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRight})      // cycle to ubuntu:22.04
	if got := f.value("with.image"); got != "ubuntu:22.04" {
		t.Errorf("image after cycle = %q, want ubuntu:22.04", got)
	}
}

func TestStepsModel_CreateStepForm_ImageFallsBackToText(t *testing.T) {
	m := newStepsModel() // m.images is nil
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(stepsModel)
	// With no allowlist the forge/run image field is a free-text input (empty).
	if got := m2.form.value("with.image"); got != "" {
		t.Errorf("image = %q, want empty text input when allowlist unavailable", got)
	}
	// A text field accepts typed input; a selector would ignore runes.
	f, _, _ := m2.form.update(tea.KeyMsg{Type: tea.KeyTab}) // focus action
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab})        // focus image
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if got := f.value("with.image"); got != "x" {
		t.Errorf("image after typing = %q, want x (free-text fallback)", got)
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
	m.form, _ = newCIStepForm("", nil, stepCatalogs{})
	if !strings.Contains(m.View(), "New Step") {
		t.Error("create-step view should show the form heading")
	}
}

// The create form documents the run-time templating so users know step fields can
// reference run inputs and earlier steps' outputs.
func TestStepsView_Create_ShowsTemplatingHelp(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	m.form, _ = newCIStepForm("", nil, stepCatalogs{})
	if !strings.Contains(m.View(), "${steps.STEP.output}") {
		t.Errorf("create-step view should document step-output references; got: %q", m.View())
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

// ── Fetch: tuiFetchImages ──────────────────────────────────────────────────────

func TestTUIFetchImages_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `["alpine:3.19","ubuntu:22.04"]`)
	setupCLI(t, srv)
	msg := tuiFetchImages()
	if rec.Method != "GET" || rec.Path != "/forge/images" {
		t.Errorf("request = %s %s, want GET /forge/images", rec.Method, rec.Path)
	}
	imgs, ok := msg.(tuiImagesMsg)
	if !ok || len(imgs) != 2 || imgs[0] != "alpine:3.19" {
		t.Errorf("msg = %#v, want tuiImagesMsg with 2 images", msg)
	}
}

func TestTUIFetchImages_ErrorDegradesToEmpty(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	msg, ok := tuiFetchImages().(tuiImagesMsg)
	if !ok || len(msg) != 0 {
		t.Errorf("images = %#v, want empty tuiImagesMsg on error", msg)
	}
}

// ── Fetch: tuiFetchTickets / tuiFetchOutposts ──────────────────────────────────

func TestTUIFetchTickets_MapsTitleToID(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK,
		`[{"ticket_id":"t1","title":"Fix login"},{"ticket_id":"t2","title":"Add export"}]`)
	setupCLI(t, srv)
	msg := tuiFetchTickets()
	if rec.Method != "GET" || rec.Path != "/tickets/tickets" {
		t.Errorf("request = %s %s, want GET /tickets/tickets", rec.Method, rec.Path)
	}
	cat, ok := msg.(tuiTicketsMsg)
	if !ok || len(cat.values) != 2 {
		t.Fatalf("msg = %#v, want tuiTicketsMsg with 2 entries", msg)
	}
	// Labels are the human-friendly titles; values are the ids submitted to the API.
	if cat.labels[0] != "Fix login" || cat.values[0] != "t1" {
		t.Errorf("entry 0 = %q/%q, want Fix login/t1", cat.labels[0], cat.values[0])
	}
}

func TestTUIFetchTickets_ErrorDegradesToEmpty(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	cat, ok := tuiFetchTickets().(tuiTicketsMsg)
	if !ok || len(cat.values) != 0 {
		t.Errorf("tickets = %#v, want empty tuiTicketsMsg on error", cat)
	}
}

func TestTUIFetchOutposts_MapsNameToID(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK,
		`[{"outpost_id":"o1","name":"prod-east"}]`)
	setupCLI(t, srv)
	msg := tuiFetchOutposts()
	if rec.Method != "GET" || rec.Path != "/outpost-gateway/outposts" {
		t.Errorf("request = %s %s, want GET /outpost-gateway/outposts", rec.Method, rec.Path)
	}
	cat, ok := msg.(tuiOutpostsMsg)
	if !ok || len(cat.values) != 1 || cat.labels[0] != "prod-east" || cat.values[0] != "o1" {
		t.Errorf("msg = %#v, want prod-east→o1", msg)
	}
}

func TestTUIFetchOutposts_ErrorDegradesToEmpty(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	cat, ok := tuiFetchOutposts().(tuiOutpostsMsg)
	if !ok || len(cat.values) != 0 {
		t.Errorf("outposts = %#v, want empty tuiOutpostsMsg on error", cat)
	}
}

// ── Step form: id pickers (name shown, id submitted) ───────────────────────────

func TestStepsModel_TicketPicker_ShowsTitleSubmitsID(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"tickets/delete"}
	m.tickets = kvCatalog{labels: []string{"Fix login"}, values: []string{"t1"}}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(stepsModel)

	// The Ticket field is a name picker: it submits the id, not a typed string.
	if got := m.form.value("with.id"); got != "t1" {
		t.Errorf("with.id = %q, want t1 (picker submits the id behind the title)", got)
	}
	// And it renders the human-friendly title, not the id.
	if v := m.form.view(80, 24); !strings.Contains(v, "Fix login") {
		t.Errorf("form should display the ticket title; view = %q", v)
	}
}

func TestStepsModel_TicketPicker_FallsBackToTextWhenCatalogEmpty(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"tickets/delete"} // no tickets catalog loaded
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(stepsModel)

	// With no catalog the id field is free text, so a typed id still works.
	f, _, _ := m.form.update(tea.KeyMsg{Type: tea.KeyTab}) // name → action
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyTab})       // action → ticket id
	f, _, _ = f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t9")})
	if got := f.value("with.id"); got != "t9" {
		t.Errorf("with.id = %q, want t9 (free-text fallback)", got)
	}
}

// ── Standalone step test (throwaway pipeline) ───────────────────────────────────

func TestStepsModel_T_WithRefs_OpensTestForm(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{
		{StepID: "s1", Name: "open_ticket", Action: "tickets/create",
			With: map[string]any{"title": "Build failed: ${steps.build.output}"}},
	}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m2 := updated.(stepsModel)
	if m2.view != stepsViewTest {
		t.Fatalf("view = %v, want stepsViewTest", m2.view)
	}
	if cmd == nil {
		t.Error("opening the test form should return a focus cmd")
	}
	// A field exists for the reference, keyed by the reference expression.
	if !hasFieldKey(m2.form, "steps.build.output") {
		t.Error("test form should have a field for the ${steps.build.output} reference")
	}
}

func TestStepsModel_T_NoRefs_RunsImmediately(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{
		{StepID: "s1", Name: "unit_tests", Action: "forge/run",
			With: map[string]any{"image": "ubuntu:22.04", "run": "go test ./..."}},
	}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m2 := updated.(stepsModel)
	if m2.view != stepsViewTesting {
		t.Fatalf("view = %v, want stepsViewTesting (no refs → run immediately)", m2.view)
	}
	if cmd == nil {
		t.Error("starting a no-input test should emit the run cmd")
	}
}

func TestStepsModel_TestDoneMsg_ShowsResult(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewTesting
	m.testStep = tuiStep{Name: "unit_tests"}
	updated, _ := m.Update(stepTestDoneMsg{outcome: stepTestOutcome{status: "completed", output: "PASS"}})
	m2 := updated.(stepsModel)
	if m2.view != stepsViewTestResult {
		t.Fatalf("view = %v, want stepsViewTestResult", m2.view)
	}
	v := m2.View()
	if !strings.Contains(v, "completed") || !strings.Contains(v, "PASS") {
		t.Errorf("result view should show status and output; got: %q", v)
	}
}

func TestStepsModel_TestDoneMsg_IgnoredAfterNavigateAway(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewList // user already left the testing view
	updated, _ := m.Update(stepTestDoneMsg{outcome: stepTestOutcome{status: "completed"}})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("a late test result should be ignored once the user navigated away")
	}
}

func TestStepsModel_TestResult_EscReturnsToList(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewTestResult
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("esc on the test result should return to the steps list")
	}
}

func TestStepsModel_TestForm_EscBacksToList(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{
		{StepID: "s1", Name: "open_ticket", Action: "tickets/create",
			With: map[string]any{"title": "${inputs.MSG}"}},
	}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m2 := updated.(stepsModel)
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(stepsModel).view != stepsViewList {
		t.Error("esc in the test form should return to the steps list")
	}
}

func TestStepsView_List_HelpMentionsTest(t *testing.T) {
	m := applyStepsMsg(newStepsModel(), tuiStepsMsg([]tuiStep{}))
	if !strings.Contains(m.View(), "test") {
		t.Error("steps help should mention the test shortcut")
	}
}

func TestStepsModel_OutpostPicker_UpgradesWhenCatalogArrivesLate(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"chaos/run-experiment"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(stepsModel)
	// Outpost catalog not yet loaded → free-text fallback.
	if hasFieldKind(m.form, "with.outpost_id", fieldSelect) {
		t.Fatal("outpost_id should start as free text before the catalog loads")
	}
	// Catalog lands; the field must upgrade in place to a name picker.
	updated, _ = m.Update(tuiOutpostsMsg(kvCatalog{labels: []string{"prod-east"}, values: []string{"o1"}}))
	m = updated.(stepsModel)
	if !hasFieldKind(m.form, "with.outpost_id", fieldSelect) {
		t.Error("outpost_id should upgrade to a selector once the catalog arrives")
	}
	if got := m.form.value("with.outpost_id"); got != "o1" {
		t.Errorf("with.outpost_id = %q, want o1 (required picker selects first entry)", got)
	}
}

// hasFieldKind reports whether the form has a field with the given key and kind.
func hasFieldKind(f tuiForm, key string, kind fieldKind) bool {
	for i := range f.fields {
		if f.fields[i].key == key {
			return f.fields[i].kind == kind
		}
	}
	return false
}

// ── Create step: ciPostStep ───────────────────────────────────────────────────

func TestCIPostStep_PostsCorrectPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"step_id":"s1"}`)
	setupCLI(t, srv)

	with := map[string]any{"image": "ubuntu:22.04", "run": "go test ./..."}
	msg := ciPostStep("unit_tests", "forge/run", "runs the tests", with, 120)()

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
	gotWith, _ := got["with"].(map[string]any)
	if gotWith["image"] != "ubuntu:22.04" || gotWith["run"] != "go test ./..." {
		t.Errorf("with = %v, want image+run set", got["with"])
	}
}

// ── Edit step: ciPutStep ──────────────────────────────────────────────────────

func TestCIPutStep_PutsCorrectPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"step_id":"s1"}`)
	setupCLI(t, srv)

	with := map[string]any{"image": "alpine:3.19", "run": "go vet ./..."}
	msg := ciPutStep("s1", "unit_tests", "forge/run", "now vets", with, 90)()

	if _, ok := msg.(tuiStepCreatedMsg); !ok {
		t.Fatalf("msg = %T, want a list-refresh msg", msg)
	}
	if rec.Method != "PUT" || rec.Path != "/workflows/steps/s1" {
		t.Errorf("request = %s %s, want PUT /workflows/steps/s1", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["name"] != "unit_tests" || got["action"] != "forge/run" {
		t.Errorf("name/action = %v/%v, want unit_tests/forge/run", got["name"], got["action"])
	}
	if got["timeout"].(float64) != 90 {
		t.Errorf("timeout = %v, want 90", got["timeout"])
	}
	gotWith, _ := got["with"].(map[string]any)
	if gotWith["image"] != "alpine:3.19" || gotWith["run"] != "go vet ./..." {
		t.Errorf("with = %v, want image+run set", got["with"])
	}
}

// ── Edit step: stepEditValues (reverse of buildStepWith) ───────────────────────

func TestStepEditValues_ReversesWithMap(t *testing.T) {
	s := tuiStep{
		StepID:      "s1",
		Name:        "unit_tests",
		Action:      "forge/run",
		Timeout:     120,
		Description: "runs the tests",
		With: map[string]any{
			"image": "ubuntu:22.04",
			"run":   "go test ./...",
			"env":   map[string]any{"CGO_ENABLED": "0", "GOFLAGS": "-count=1"},
			"extra": "keep", // not in forge/run's schema → advanced With JSON
		},
	}
	vals := stepEditValues(s)

	for key, want := range map[string]string{
		"name":       "unit_tests",
		"action":     "forge/run",
		"timeout":    "120",
		"desc":       "runs the tests",
		"with.image": "ubuntu:22.04",
		"with.run":   "go test ./...",
		// env map round-trips to sorted KEY=VALUE tokens.
		"with.env": "CGO_ENABLED=0 GOFLAGS=-count=1",
	} {
		if got := vals[key]; got != want {
			t.Errorf("vals[%q] = %q, want %q", key, got, want)
		}
	}
	// Keys outside the action schema land in the advanced With JSON field.
	if raw := vals["with."+rawWithKey]; !strings.Contains(raw, "extra") || !strings.Contains(raw, "keep") {
		t.Errorf("with.%s = %q, want it to carry the leftover extra key", rawWithKey, raw)
	}
}

// An int-valued with key (e.g. blueprints/backend ttl_secs) round-trips as digits,
// not the float JSON decodes it into.
func TestStepEditValues_FormatsIntWithoutDecimal(t *testing.T) {
	s := tuiStep{
		Action: "blueprints/backend",
		With:   map[string]any{"ttl_secs": float64(14400)},
	}
	if got := stepEditValues(s)["with.ttl_secs"]; got != "14400" {
		t.Errorf("with.ttl_secs = %q, want 14400", got)
	}
}

// ── Create step: buildStepWith ─────────────────────────────────────────────────

// fakeForm returns a valueOf that reads from a static key→value map, matching
// what tuiForm.value would report (callers pass already-prefixed keys).
func fakeForm(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

func TestBuildStepWith_ForgeRun_AssemblesImageRunEnv(t *testing.T) {
	with, err := buildStepWith("forge/run", fakeForm(map[string]string{
		"with.image": "ubuntu:22.04",
		"with.run":   "go test ./...",
		"with.env":   "FOO=bar BAZ=qux",
	}))
	if err != nil {
		t.Fatalf("buildStepWith error: %v", err)
	}
	if with["image"] != "ubuntu:22.04" || with["run"] != "go test ./..." {
		t.Errorf("with = %v, want image+run set", with)
	}
	env, _ := with["env"].(map[string]string)
	if env["FOO"] != "bar" || env["BAZ"] != "qux" {
		t.Errorf("with.env = %v, want FOO=bar BAZ=qux", with["env"])
	}
}

func TestBuildStepWith_ForgeRun_MissingImage_Errors(t *testing.T) {
	_, err := buildStepWith("forge/run", fakeForm(map[string]string{"with.run": "echo hi"}))
	if err == nil {
		t.Error("forge/run without an image should error")
	}
}

func TestBuildStepWith_ForgeRun_MissingRun_Errors(t *testing.T) {
	_, err := buildStepWith("forge/run", fakeForm(map[string]string{"with.image": "ubuntu:22.04"}))
	if err == nil {
		t.Error("forge/run without a run command should error")
	}
}

func TestBuildStepWith_TicketsCreate_AssemblesTypedFields(t *testing.T) {
	with, err := buildStepWith("tickets/create", fakeForm(map[string]string{
		"with.title":    "Build failed",
		"with.priority": "high",
	}))
	if err != nil {
		t.Fatalf("buildStepWith error: %v", err)
	}
	if with["title"] != "Build failed" || with["priority"] != "high" {
		t.Errorf("with = %v, want title/priority set", with)
	}
	// Optional, unfilled fields are omitted rather than sent empty.
	if _, ok := with["status"]; ok {
		t.Errorf("empty optional status should be omitted, got %v", with["status"])
	}
}

func TestBuildStepWith_TicketsCreate_MissingTitle_Errors(t *testing.T) {
	_, err := buildStepWith("tickets/create", fakeForm(map[string]string{"with.priority": "high"}))
	if err == nil {
		t.Error("tickets/create without a title should error")
	}
}

func TestBuildStepWith_PathParamKey(t *testing.T) {
	// argo/sync's {name} path param fills with["name"] (distinct from the step name).
	with, err := buildStepWith("argo/sync", fakeForm(map[string]string{
		"with.name":     "my-app",
		"with.revision": "v1.2.3",
	}))
	if err != nil {
		t.Fatalf("buildStepWith error: %v", err)
	}
	if with["name"] != "my-app" || with["revision"] != "v1.2.3" {
		t.Errorf("with = %v, want name=my-app revision=v1.2.3", with)
	}
}

func TestBuildStepWith_IntField(t *testing.T) {
	with, err := buildStepWith("blueprints/backend", fakeForm(map[string]string{
		"with.ttl_secs": "3600",
	}))
	if err != nil {
		t.Fatalf("buildStepWith error: %v", err)
	}
	if with["ttl_secs"] != int64(3600) {
		t.Errorf("ttl_secs = %v (%T), want int64(3600)", with["ttl_secs"], with["ttl_secs"])
	}
}

func TestBuildStepWith_AdvancedJSONOverlaysExtraKeys(t *testing.T) {
	with, err := buildStepWith("tickets/create", fakeForm(map[string]string{
		"with.title": "hi",
		"with.__raw": `{"due_date":"2026-01-01","title":"ignored"}`,
	}))
	if err != nil {
		t.Fatalf("buildStepWith error: %v", err)
	}
	// Typed fields win over the advanced JSON for the same key…
	if with["title"] != "hi" {
		t.Errorf("title = %v, want typed field to win over JSON", with["title"])
	}
	// …but extra keys only present in the JSON come through.
	if with["due_date"] != "2026-01-01" {
		t.Errorf("due_date = %v, want it merged from advanced JSON", with["due_date"])
	}
}

func TestBuildStepWith_UnknownAction_RequiresWithJSON(t *testing.T) {
	if _, err := buildStepWith("custom/thing", fakeForm(nil)); err == nil {
		t.Error("unknown action with no With JSON should error")
	}
	with, err := buildStepWith("custom/thing", fakeForm(map[string]string{
		"with.__raw": `{"service":"conductor","path":"/webhook"}`,
	}))
	if err != nil {
		t.Fatalf("buildStepWith error: %v", err)
	}
	if with["service"] != "conductor" || with["path"] != "/webhook" {
		t.Errorf("with = %v, want raw JSON passed through for unknown action", with)
	}
}

func TestBuildStepWith_BadJSON_Errors(t *testing.T) {
	if _, err := buildStepWith("custom/thing", fakeForm(map[string]string{"with.__raw": "{not json"})); err == nil {
		t.Error("malformed With JSON should error")
	}
}

// ── Step form: action-driven field swap ────────────────────────────────────────

func TestStepsModel_CreateStepForm_SwapsFieldsOnActionChange(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"forge/run", "tickets/create"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(stepsModel)

	// forge/run is the default: its Image/Run fields exist, ticket fields don't.
	if !hasFieldKey(m.form, "with.image") {
		t.Fatal("forge/run form should have a with.image field")
	}
	if hasFieldKey(m.form, "with.title") {
		t.Fatal("forge/run form should not have a with.title field")
	}

	// Focus the Action selector (name=0, action=1) and cycle to tickets/create.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	updated, _ = updated.(stepsModel).Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(stepsModel)

	if got := m.form.value("action"); got != "tickets/create" {
		t.Fatalf("action = %q, want tickets/create after cycle", got)
	}
	// The fields swapped: ticket fields now present, forge fields gone.
	if !hasFieldKey(m.form, "with.title") {
		t.Error("after switching to tickets/create the form should have with.title")
	}
	if hasFieldKey(m.form, "with.image") {
		t.Error("after switching away from forge/run the with.image field should be gone")
	}
}

func TestStepsModel_CreateStepForm_PreservesNameAcrossActionSwap(t *testing.T) {
	m := newStepsModel()
	m.actions = []string{"forge/run", "tickets/create"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(stepsModel)

	// Type a step name (focus starts on the name field).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mystep")})
	m = updated.(stepsModel)

	// Switch action; the step-level name must survive the field rebuild.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	updated, _ = updated.(stepsModel).Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(stepsModel)

	if got := m.form.value("name"); got != "mystep" {
		t.Errorf("name = %q, want mystep preserved across action swap", got)
	}
}

// hasFieldKey reports whether the form contains a field with the given key.
func hasFieldKey(f tuiForm, key string) bool {
	for i := range f.fields {
		if f.fields[i].key == key {
			return true
		}
	}
	return false
}

// ── Auto-refresh ──────────────────────────────────────────────────────────────

func TestStepsModel_AutoRefresh_ListEmitsFetch(t *testing.T) {
	m := newStepsModel()
	m.loading = false
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in the list view should emit a fetch cmd")
	}
}

func TestStepsModel_AutoRefresh_CreateNoop(t *testing.T) {
	m := newStepsModel()
	m.view = stepsViewCreate
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd != nil {
		t.Error("auto-refresh in the create form should be a noop")
	}
}
