package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The Steps TUI is a top-level browser for reusable step definitions, separate
// from the Pipelines/Runs TUI (ci_tui.go). Pipelines compose these steps by
// name, but the two are managed independently.

// ── API type ──────────────────────────────────────────────────────────────────

// tuiStep mirrors a reusable step definition from /workflows/steps. With is kept
// so the test runner can scan a step's ${...} references and bake test values in
// without a second fetch.
type tuiStep struct {
	StepID      string         `json:"step_id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Action      string         `json:"action"`
	With        map[string]any `json:"with"`
	Timeout     int64          `json:"timeout"`
	CreatedAt   time.Time      `json:"created_at"`
}

// ── Messages ──────────────────────────────────────────────────────────────────

type tuiStepsMsg []tuiStep
type tuiActionsMsg []string
type tuiImagesMsg []string
type tuiTicketsMsg kvCatalog
type tuiOutpostsMsg kvCatalog
type tuiStepCreatedMsg struct{}

// stepTestDoneMsg carries the result of a standalone step test (run via a
// throwaway pipeline). err is set when the test could not be run at all.
type stepTestDoneMsg struct {
	outcome stepTestOutcome
	err     error
}

// ── View states ───────────────────────────────────────────────────────────────

type stepsViewID int

const (
	stepsViewList stepsViewID = iota
	stepsViewCreate
	stepsViewEdit       // form: edit an existing step (pre-filled, PUT on submit)
	stepsViewTest       // form: fill in the step's ${...} references
	stepsViewTesting    // throwaway pipeline running
	stepsViewTestResult // run outcome + output
)

// Responsive column spec for the step-definition table.
var stepDefCols = []tuiColSpec{
	{"NAME", 18, 2},
	{"ACTION", 14, 1},
	{"DESCRIPTION", 20, 3},
	{"TIMEOUT", 8, 0},
	{"CREATED", 14, 0},
}

// ── Model ─────────────────────────────────────────────────────────────────────

type stepsModel struct {
	view    stepsViewID
	loading bool
	err     error
	width   int
	height  int

	sTable table.Model

	steps []tuiStep
	// actions feeds the step form's ←/→ action-type selector.
	actions []string
	// images feeds the step form's ←/→ image selector (forge/run steps).
	images []string
	// tickets and outposts back the form's name→id pickers, so ids are never
	// typed by hand (tickets/update|delete reference a ticket; argo/chaos an
	// outpost). Each degrades to a free-text id input when unavailable.
	tickets  kvCatalog
	outposts kvCatalog

	form tuiForm
	// editStepID is the id of the step being edited (stepsViewEdit); the edit form
	// reuses the create form's fields but submits a PUT to this id on save.
	editStepID string

	// Standalone step test (run via a throwaway pipeline).
	testStep   tuiStep
	testResult stepTestOutcome
	testErr    error
}

// catalogs bundles the model's option sources for the create-step form.
func (m stepsModel) catalogs() stepCatalogs {
	return stepCatalogs{images: m.images, tickets: m.tickets, outposts: m.outposts}
}

func newStepsModel() stepsModel {
	m := stepsModel{
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
	}
	m.sTable = table.New(table.WithFocused(true))
	m.sTable.SetStyles(tuiTableStyles())
	m.applyLayout()
	return m
}

// applyLayout resizes the table to the current terminal dimensions.
func (m *stepsModel) applyLayout() {
	m.sTable.SetColumns(tuiFitColumns(stepDefCols, m.width))
	m.sTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Init ──────────────────────────────────────────────────────────────────────

// Init loads steps and batches the form's option catalogs (actions, forge images,
// tickets, outposts) so every selector — including the name→id pickers — is ready
// the moment the create-step form opens.
func (m stepsModel) Init() tea.Cmd {
	return tea.Batch(tuiFetchSteps, tuiFetchActions, tuiFetchImages, tuiFetchTickets, tuiFetchOutposts)
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func tuiFetchSteps() tea.Msg {
	data, err := doRequest("GET", "/workflows/steps", nil)
	if err != nil {
		return tuiErrMsg{err}
	}
	var ss []tuiStep
	if err := json.Unmarshal(data, &ss); err != nil {
		return tuiErrMsg{err}
	}
	return tuiStepsMsg(ss)
}

// tuiFetchActions loads the workflow action catalog for the step form's
// action-type selector, degrading to an empty list on failure.
func tuiFetchActions() tea.Msg {
	data, err := doRequest("GET", "/workflows/actions", nil)
	if err != nil {
		return tuiActionsMsg(nil)
	}
	var defs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &defs); err != nil {
		return tuiActionsMsg(nil)
	}
	var names []string
	for _, d := range defs {
		if d.Name != "" {
			names = append(names, d.Name)
		}
	}
	return tuiActionsMsg(names)
}

// tuiFetchImages loads the forge image allowlist for the step form's image
// selector (used by forge/run steps), degrading to an empty list on failure so
// the field falls back to a free-text input.
func tuiFetchImages() tea.Msg {
	data, err := doRequest("GET", "/forge/images", nil)
	if err != nil {
		return tuiImagesMsg(nil)
	}
	var imgs []string
	if err := json.Unmarshal(data, &imgs); err != nil {
		return tuiImagesMsg(nil)
	}
	return tuiImagesMsg(imgs)
}

// tuiFetchTickets loads the ticket list so the tickets/update|delete forms can show
// ticket titles and submit the ticket_id, degrading to an empty catalog (free-text
// id fallback) on failure.
func tuiFetchTickets() tea.Msg {
	data, err := doRequest("GET", "/tickets/tickets", nil)
	if err != nil {
		return tuiTicketsMsg{}
	}
	var ts []struct {
		ID    string `json:"ticket_id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &ts); err != nil {
		return tuiTicketsMsg{}
	}
	var cat kvCatalog
	for _, t := range ts {
		if t.ID == "" {
			continue
		}
		label := t.Title
		if label == "" {
			label = t.ID
		}
		cat.labels = append(cat.labels, label)
		cat.values = append(cat.values, t.ID)
	}
	return tuiTicketsMsg(cat)
}

// tuiFetchOutposts loads the outpost list so the argo/chaos forms can show outpost
// names and submit the outpost_id, degrading to an empty catalog (free-text id
// fallback) on failure.
func tuiFetchOutposts() tea.Msg {
	data, err := doRequest("GET", "/outpost-gateway/outposts", nil)
	if err != nil {
		return tuiOutpostsMsg{}
	}
	var os []struct {
		ID   string `json:"outpost_id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &os); err != nil {
		return tuiOutpostsMsg{}
	}
	var cat kvCatalog
	for _, o := range os {
		if o.ID == "" {
			continue
		}
		label := o.Name
		if label == "" {
			label = o.ID
		}
		cat.labels = append(cat.labels, label)
		cat.values = append(cat.values, o.ID)
	}
	return tuiOutpostsMsg(cat)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m stepsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applyLayout()
		return m, nil

	case tuiErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case tuiStepsMsg:
		m.loading = false
		m.steps = []tuiStep(msg)
		rows := make([]table.Row, len(m.steps))
		for i, s := range m.steps {
			rows[i] = table.Row{
				s.Name,
				s.Action,
				s.Description,
				fmt.Sprintf("%ds", s.Timeout),
				s.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.sTable.SetRows(rows)
		return m, nil

	case tuiActionsMsg:
		m.actions = []string(msg)
		// If the form was opened before the catalog landed, its Action field
		// fell back to free-text; rebuild so it upgrades to the ←/→ selector.
		if m.view == stepsViewCreate || m.view == stepsViewEdit {
			m.form = rebuildStepForm(m.form, m.actions, m.catalogs())
		}
		return m, nil

	case tuiImagesMsg:
		m.images = []string(msg)
		if m.view == stepsViewCreate || m.view == stepsViewEdit {
			m.form = rebuildStepForm(m.form, m.actions, m.catalogs())
		}
		return m, nil

	case tuiTicketsMsg:
		m.tickets = kvCatalog(msg)
		// Upgrade an open ticket-reference field from free-text to a name picker.
		if m.view == stepsViewCreate || m.view == stepsViewEdit {
			m.form = rebuildStepForm(m.form, m.actions, m.catalogs())
		}
		return m, nil

	case tuiOutpostsMsg:
		m.outposts = kvCatalog(msg)
		if m.view == stepsViewCreate || m.view == stepsViewEdit {
			m.form = rebuildStepForm(m.form, m.actions, m.catalogs())
		}
		return m, nil

	case tuiStepCreatedMsg:
		m.view = stepsViewList
		m.loading = true
		return m, tuiFetchSteps

	case stepTestDoneMsg:
		// A late result after the user navigated away is ignored (the throwaway
		// pipeline was still cleaned up inside the command).
		if m.view != stepsViewTesting {
			return m, nil
		}
		m.testErr = msg.err
		m.testResult = msg.outcome
		m.view = stepsViewTestResult
		return m, nil

	case tuiFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		// Silently re-fetch the step list so new definitions appear without a
		// loading flash or losing the cursor. The create form is left untouched.
		if m.view == stepsViewList {
			return m, tuiFetchSteps
		}
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "esc":
				return m, func() tea.Msg { return goHomeMsg{} }
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, tea.Batch(tuiFetchSteps, tuiFetchActions, tuiFetchImages, tuiFetchTickets, tuiFetchOutposts)
			}
			return m, nil
		}
		switch m.view {
		case stepsViewList:
			return m.keyList(msg)
		case stepsViewCreate:
			return m.keyCreate(msg)
		case stepsViewEdit:
			return m.keyEdit(msg)
		case stepsViewTest:
			return m.keyTest(msg)
		case stepsViewTesting:
			return m.keyTesting(msg)
		case stepsViewTestResult:
			return m.keyTestResult(msg)
		}
	}

	return m.delegate(msg)
}

func (m stepsModel) delegate(msg tea.Msg) (stepsModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case stepsViewList:
		m.sTable, cmd = m.sTable.Update(msg)
	case stepsViewCreate, stepsViewEdit, stepsViewTest:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m stepsModel) keyList(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "n":
		var cmd tea.Cmd
		m.form, cmd = newCIStepForm("", m.actions, m.catalogs())
		m.view = stepsViewCreate
		// If the Init prefetch hasn't landed (or failed), the action/image/ticket/
		// outpost fields fell back to free-text; fetch now so they upgrade to
		// pickers the moment each catalog arrives (handled in the *Msg cases).
		cmds := []tea.Cmd{cmd}
		if len(m.actions) == 0 {
			cmds = append(cmds, tuiFetchActions)
		}
		if len(m.images) == 0 {
			cmds = append(cmds, tuiFetchImages)
		}
		if len(m.tickets.values) == 0 {
			cmds = append(cmds, tuiFetchTickets)
		}
		if len(m.outposts.values) == 0 {
			cmds = append(cmds, tuiFetchOutposts)
		}
		return m, tea.Batch(cmds...)
	case "e", "enter":
		return m.startStepEdit()
	case "t":
		return m.startStepTest()
	case "r":
		m.loading = true
		return m, tea.Batch(tuiFetchSteps, tuiFetchActions)
	}
	var cmd tea.Cmd
	m.sTable, cmd = m.sTable.Update(msg)
	return m, cmd
}

// ── Create step form ──────────────────────────────────────────────────────────

// newCIStepForm builds the step form for the given action. The fields between the
// Action selector and Timeout are tailored to the action (schemaForAction): e.g.
// forge/run shows Image/Run/Env, tickets/create shows Title/Priority/…, and an
// unknown action shows a single With JSON field. The Action field becomes a ←/→
// selector when the catalog is available (else free text); the forge Image field
// becomes a selector when the allowlist is available (else free text).
//
// action is the desired selection ("" defaults to forge/run); it is resolved
// against the catalog so the rendered fields always match the action the form
// will actually report.
func newCIStepForm(action string, actions []string, cats stepCatalogs) (tuiForm, tea.Cmd) {
	form, cmd := newTUIForm("New Step", stepFormFields(action, actions, cats)...)
	form.help = stepTemplateHelp
	return form, cmd
}

// stepTemplateHelp documents the run-time templating any text field accepts, so a
// step's inputs can be wired from run inputs and earlier steps' outputs when the
// step is used in a pipeline.
const stepTemplateHelp = "refs: ${inputs.NAME} · ${steps.STEP.output} · ${steps.STEP.output.field}"

// stepFormFields assembles the full field list: the fixed Name/Action header, the
// action-specific schema fields, then the fixed Timeout/Desc footer.
func stepFormFields(action string, actions []string, cats stepCatalogs) []formField {
	eff := effectiveAction(action, actions)

	fields := []formField{
		formInput("name", "Name", "unit_tests (required)"),
		stepActionField(eff, actions),
	}
	for _, sf := range schemaForAction(eff) {
		fields = append(fields, stepSchemaField(sf, cats))
	}
	return append(fields,
		formInput("timeout", "Timeout", "seconds (default 30)"),
		formTextarea("desc", "Desc", "description (optional)"),
	)
}

// effectiveAction resolves the action the form will report: defaulting to
// forge/run, and (when a catalog is present) snapping to the first catalog entry
// if the requested action isn't offered — so the rendered schema fields can't
// drift from the Action selector's actual value.
func effectiveAction(action string, actions []string) string {
	if action == "" {
		action = "forge/run"
	}
	if len(actions) == 0 {
		return action
	}
	if slices.Contains(actions, action) {
		return action
	}
	return actions[0]
}

// stepActionField builds the Action control: a ←/→ catalog selector when actions
// are known, otherwise a free-text input. eff is pre-resolved to be in the catalog.
func stepActionField(eff string, actions []string) formField {
	if len(actions) > 0 {
		return formSelectDefault("action", "Action", actions, eff)
	}
	return formInputDefault("action", "Action", "forge/run (required)", eff)
}

// stepSchemaField converts a schema field into a form field. A catalog-backed
// field renders as a picker when its catalog is loaded — image as a plain ←/→
// selector, ticket/outpost as a name→id selector (shows the name, submits the id)
// — and degrades to a free-text input otherwise. Non-catalog fields are always
// free text (parsed at submit time by buildStepWith).
func stepSchemaField(sf stepField, cats stepCatalogs) formField {
	key := withKeyPrefix + sf.key
	switch sf.catalog {
	case catImage:
		if len(cats.images) > 0 {
			return formSelect(key, sf.label, cats.images)
		}
	case catTicket, catOutpost:
		if c := cats.forCatalog(sf.catalog); len(c.values) > 0 {
			return idPickerField(key, sf.label, sf.required, c)
		}
	}
	// Commands and JSON are naturally multi-line.
	if sf.multiline || sf.kind == stepFieldJSON {
		return formTextarea(key, sf.label, sf.placeholder)
	}
	return formInput(key, sf.label, sf.placeholder)
}

// idPickerField builds a name→id selector (formSelectKV) over a kvCatalog. Optional
// references gain a leading "(none)" choice that submits ""; required ones must
// resolve to a real id.
func idPickerField(key, label string, required bool, c kvCatalog) formField {
	labels, values := c.labels, c.values
	if !required {
		labels = append([]string{""}, labels...)
		values = append([]string{""}, values...)
	}
	return formSelectKV(key, label, labels, values)
}

// rebuildStepForm rebuilds the create-step form against the current action
// selection and the latest catalogs. It is used both when a late catalog prefetch
// upgrades free-text fields to pickers, and when the user changes the action so the
// tailored fields must swap. Entered values (matched by key, which for id pickers
// means the submitted id), focus, and any inline error are carried over; the focus
// index is clamped because the field count changes between actions.
func rebuildStepForm(old tuiForm, actions []string, cats stepCatalogs) tuiForm {
	form, _ := newCIStepForm(old.value("action"), actions, cats)
	form.title = old.title // preserve "Edit Step" (or "New Step") across rebuilds
	form.focus = old.focus
	if form.focus >= len(form.fields) {
		form.focus = len(form.fields) - 1
	}
	if form.focus < 0 {
		form.focus = 0
	}
	form.errMsg = old.errMsg
	for i := range form.fields {
		form.fields[i].setValue(old.value(form.fields[i].key))
	}
	form.focusActive()
	return form
}

func (m stepsModel) keyCreate(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	prevAction := m.form.value("action")
	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)
	switch action {
	case formCancel:
		m.view = stepsViewList
		return m, nil
	case formSubmit:
		return m.submitCreate()
	}
	// Swap the action-specific fields when the selection moves to a different
	// schema bucket (e.g. forge/run → tickets/create, or a known action → a
	// custom one). schemaKey buckets all unknown actions together so typing a
	// custom action name doesn't churn the fields on every keystroke.
	if now := m.form.value("action"); now != prevAction && schemaKey(now) != schemaKey(prevAction) {
		m.form = rebuildStepForm(m.form, m.actions, m.catalogs())
	}
	return m, cmd
}

// stepPayload is the validated, assembled step definition read off the form.
type stepPayload struct {
	name    string
	action  string
	desc    string
	with    map[string]any
	timeout int64
}

// parseStepForm validates the step form (shared by create and edit) and assembles
// the `with` map via buildStepWith, which checks required fields and parses typed
// inputs (env tokens, ints, the advanced With JSON). On any problem it returns a
// non-empty error string for the caller to surface inline.
func parseStepForm(form tuiForm) (stepPayload, string) {
	name := form.value("name")
	action := form.value("action")
	if name == "" {
		return stepPayload{}, "name is required"
	}
	if action == "" {
		return stepPayload{}, "action is required (e.g. forge/run)"
	}
	timeout := int64(30)
	if ts := form.value("timeout"); ts != "" {
		t, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || t < 0 {
			return stepPayload{}, "timeout must be a non-negative integer (seconds)"
		}
		timeout = t
	}
	with, err := buildStepWith(action, form.value)
	if err != nil {
		return stepPayload{}, err.Error()
	}
	return stepPayload{name: name, action: action, desc: form.value("desc"), with: with, timeout: timeout}, ""
}

// submitCreate validates the step form and, if valid, returns the create cmd.
func (m stepsModel) submitCreate() (stepsModel, tea.Cmd) {
	p, errMsg := parseStepForm(m.form)
	if errMsg != "" {
		m.form.errMsg = errMsg
		return m, nil
	}
	m.form.errMsg = ""
	return m, ciPostStep(p.name, p.action, p.desc, p.with, p.timeout)
}

// ciPostStep POSTs an assembled step definition to the workflows service.
func ciPostStep(name, action, desc string, with map[string]any, timeout int64) tea.Cmd {
	return ciSaveStep("POST", "/workflows/steps", name, action, desc, with, timeout)
}

// ── Edit step form ──────────────────────────────────────────────────────────────

// startStepEdit opens the edit form pre-filled with the highlighted step. It mirrors
// the create form (same action-tailored fields) but submits a PUT on save. Like the
// create path it re-fetches any catalog that hasn't landed so free-text fields
// upgrade to pickers — and, because the form is pre-filled, those picker values are
// carried over by rebuildStepForm when each catalog arrives.
func (m stepsModel) startStepEdit() (stepsModel, tea.Cmd) {
	cur := m.sTable.Cursor()
	if cur < 0 || cur >= len(m.steps) {
		return m, nil
	}
	s := m.steps[cur]
	form, cmd := newCIStepForm(s.Action, m.actions, m.catalogs())
	form.title = "Edit Step"
	form.setValues(stepEditValues(s))
	cmd = form.focusActive()
	m.form = form
	m.editStepID = s.StepID
	m.view = stepsViewEdit

	cmds := []tea.Cmd{cmd}
	if len(m.actions) == 0 {
		cmds = append(cmds, tuiFetchActions)
	}
	if len(m.images) == 0 {
		cmds = append(cmds, tuiFetchImages)
	}
	if len(m.tickets.values) == 0 {
		cmds = append(cmds, tuiFetchTickets)
	}
	if len(m.outposts.values) == 0 {
		cmds = append(cmds, tuiFetchOutposts)
	}
	return m, tea.Batch(cmds...)
}

// keyEdit drives the edit form. It is identical to keyCreate except submission PUTs
// to the step being edited; the action-change rebuild is shared so changing the
// action swaps the tailored fields just like in create.
func (m stepsModel) keyEdit(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	prevAction := m.form.value("action")
	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)
	switch action {
	case formCancel:
		m.view = stepsViewList
		return m, nil
	case formSubmit:
		return m.submitEdit()
	}
	if now := m.form.value("action"); now != prevAction && schemaKey(now) != schemaKey(prevAction) {
		m.form = rebuildStepForm(m.form, m.actions, m.catalogs())
	}
	return m, cmd
}

// submitEdit validates the edit form and, if valid, returns the update cmd.
func (m stepsModel) submitEdit() (stepsModel, tea.Cmd) {
	p, errMsg := parseStepForm(m.form)
	if errMsg != "" {
		m.form.errMsg = errMsg
		return m, nil
	}
	m.form.errMsg = ""
	return m, ciPutStep(m.editStepID, p.name, p.action, p.desc, p.with, p.timeout)
}

// ciPutStep PUTs an updated step definition (affects every pipeline referencing it).
func ciPutStep(id, name, action, desc string, with map[string]any, timeout int64) tea.Cmd {
	return ciSaveStep("PUT", "/workflows/steps/"+id, name, action, desc, with, timeout)
}

// ciSaveStep marshals a step definition and sends it with the given method/path,
// backing both create (POST) and edit (PUT); both return to the refreshed list.
func ciSaveStep(method, path, name, action, desc string, with map[string]any, timeout int64) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{
			"name":        name,
			"description": desc,
			"action":      action,
			"with":        with,
			"timeout":     timeout,
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest(method, path, body); err != nil {
			return tuiFormErrMsg{err}
		}
		return tuiStepCreatedMsg{}
	}
}

// stepEditValues reverses buildStepWith: it maps an existing step back to the
// create-form's string values (keyed by the prefixed form keys) so the edit form
// opens pre-populated. Typed schema fields are formatted to match how the form
// reads them back (ints as digits, env maps as KEY=VALUE tokens); any `with` keys
// the action's schema doesn't cover fall into the advanced With JSON field.
func stepEditValues(s tuiStep) map[string]string {
	vals := map[string]string{
		"name":    s.Name,
		"action":  s.Action,
		"timeout": strconv.FormatInt(s.Timeout, 10),
		"desc":    s.Description,
	}
	consumed := map[string]bool{}
	for _, f := range schemaForAction(s.Action) {
		if f.kind == stepFieldJSON {
			continue // the advanced With field collects leftovers below
		}
		v, ok := s.With[f.key]
		if !ok {
			continue
		}
		consumed[f.key] = true
		if f.kind == stepFieldEnv {
			vals[withKeyPrefix+f.key] = formatEnvTokens(v)
		} else {
			vals[withKeyPrefix+f.key] = formatScalarValue(v)
		}
	}
	leftover := map[string]any{}
	for k, v := range s.With {
		if !consumed[k] {
			leftover[k] = v
		}
	}
	if len(leftover) > 0 {
		if b, err := json.Marshal(leftover); err == nil {
			vals[withKeyPrefix+rawWithKey] = string(b)
		}
	}
	return vals
}

// formatScalarValue renders a with-map scalar back into the form's text form,
// printing whole-number floats (how JSON decodes ints) without a decimal point and
// falling back to JSON for anything unexpected.
func formatScalarValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// formatEnvTokens renders an env map (stepFieldEnv) back to space-separated
// KEY=VALUE tokens, quoting values that contain whitespace or quotes so the form's
// parseCommandLine tokeniser round-trips them. Keys are sorted for stable output.
func formatEnvTokens(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return formatScalarValue(v)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	toks := make([]string, 0, len(keys))
	for _, k := range keys {
		val := formatScalarValue(m[k])
		if strings.ContainsAny(val, " \t\"'") {
			val = strconv.Quote(val)
		}
		toks = append(toks, k+"="+val)
	}
	return strings.Join(toks, " ")
}

// ── Test step (throwaway pipeline) ──────────────────────────────────────────────

// startStepTest begins testing the highlighted step. If the step uses any ${...}
// references it opens a form to collect a value for each; otherwise it runs the
// test immediately.
func (m stepsModel) startStepTest() (stepsModel, tea.Cmd) {
	cur := m.sTable.Cursor()
	if cur < 0 || cur >= len(m.steps) {
		return m, nil
	}
	m.testStep = m.steps[cur]
	m.testErr = nil
	m.testResult = stepTestOutcome{}

	refs := stepWithRefs(m.testStep.With)
	if len(refs) == 0 {
		m.view = stepsViewTesting
		return m, testStepRun(m.testStep, nil)
	}
	var cmd tea.Cmd
	m.form, cmd = newStepTestForm(m.testStep.Name, refs)
	m.view = stepsViewTest
	return m, cmd
}

// newStepTestForm builds a form with one field per ${...} reference the step uses,
// so the user can supply a test value for each before the step runs.
func newStepTestForm(stepName string, refs []string) (tuiForm, tea.Cmd) {
	fields := make([]formField, len(refs))
	for i, ref := range refs {
		fields[i] = formInput(ref, ref, "value for ${"+ref+"}")
	}
	form, cmd := newTUIForm("Test "+stepName, fields...)
	form.help = "values are baked into a throwaway run of this step — nothing is saved"
	return form, cmd
}

func (m stepsModel) keyTest(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)
	switch action {
	case formCancel:
		m.view = stepsViewList
		return m, nil
	case formSubmit:
		vals := map[string]string{}
		for _, ref := range stepWithRefs(m.testStep.With) {
			vals[ref] = m.form.value(ref)
		}
		m.view = stepsViewTesting
		return m, testStepRun(m.testStep, vals)
	}
	return m, cmd
}

func (m stepsModel) keyTesting(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		// Stop watching; the throwaway pipeline is still cleaned up by the command.
		m.view = stepsViewList
		return m, nil
	}
	return m, nil
}

func (m stepsModel) keyTestResult(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "enter":
		m.view = stepsViewList
		return m, nil
	}
	return m, nil
}

// testStepRun runs the step in isolation: bake the test values into a throwaway
// copy of its With map and execute it through a one-off pipeline.
func testStepRun(step tuiStep, vals map[string]string) tea.Cmd {
	return func() tea.Msg {
		with := resolveStepWith(step.With, vals)
		outcome, err := runStepTest(step.Name, step.Action, with, step.Timeout)
		return stepTestDoneMsg{outcome: outcome, err: err}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m stepsModel) View() string {
	if m.err != nil {
		msg := "error: " + m.err.Error()
		hint := "[esc] quit"
		if strings.Contains(m.err.Error(), "401") || strings.Contains(m.err.Error(), "unauthorized") {
			hint += "  [r] retry after login\n\n  Not authenticated — run `armory auth login` first"
		}
		return tuiErrStyle.Render(msg) + "\n\n" + tuiHelpStyle.Render(hint)
	}
	switch m.view {
	case stepsViewCreate, stepsViewEdit, stepsViewTest:
		return m.form.view(m.width, m.height)
	case stepsViewTesting:
		return m.viewTesting()
	case stepsViewTestResult:
		return m.viewTestResult()
	}
	return m.viewList()
}

func (m stepsModel) viewList() string {
	title := tuiTitleStyle.Render("Steps")
	help := tuiHelp("[↑↓/jk] navigate  [n] new  [e] edit  [t] test  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.steps) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No steps found.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.sTable.View()) + "\n" + help
}

// viewTesting renders the placeholder shown while the throwaway pipeline runs.
func (m stepsModel) viewTesting() string {
	title := tuiTitleStyle.Render("Testing " + m.testStep.Name)
	body := tuiMetaStyle.Render("Running the step in a throwaway pipeline… (this can take a few seconds)")
	help := tuiHelp("[esc] stop watching", m.width)
	return title + "\n\n" + body + "\n\n" + help
}

// viewTestResult renders the outcome (status + output) of a standalone step test.
func (m stepsModel) viewTestResult() string {
	title := tuiTitleStyle.Render("Test result: " + m.testStep.Name)
	help := tuiHelp("[esc] back to steps", m.width)

	if m.testErr != nil {
		return title + "\n\n" + tuiErrStyle.Render("could not run test: "+m.testErr.Error()) + "\n\n" + help
	}

	statusLine := tuiMetaStyle.Render("status: ") + tuiColorStatus(m.testResult.status)
	var body string
	if out := strings.TrimSpace(m.testResult.output); out != "" {
		label := "output"
		if m.testResult.status != "completed" {
			label = "error"
		}
		body = "\n\n" + tuiMetaStyle.Render(label+":") + "\n" + tuiBoxStyle.Render(truncate(out, 4000))
	} else {
		body = "\n\n" + tuiMetaStyle.Render("(no output)")
	}
	return title + "\n\n" + statusLine + body + "\n\n" + help
}

// ── Command ───────────────────────────────────────────────────────────────────

var stepsTUICmd = &cobra.Command{
	Use:   "steps",
	Short: "Interactive TUI for browsing and creating reusable steps",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return startStepsTUI() },
}

func startStepsTUI() error {
	p := tea.NewProgram(standaloneWrap{newStepsModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
