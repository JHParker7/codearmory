package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type hookRule struct {
	RuleID     string    `json:"rule_id"`
	Name       string    `json:"name"`
	Repo       string    `json:"source"`
	Events     []string  `json:"events"`
	RefFilter  string    `json:"ref_filter"`
	WorkflowID string    `json:"workflow_id"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	// InputMapping is carried so an edit can preserve it — the rule form doesn't
	// expose it, and the update endpoint replaces it wholesale (a missing mapping
	// would wipe it).
	InputMapping map[string]string `json:"input_mapping,omitempty"`
}

type hookEvent struct {
	EventID      string        `json:"event_id"`
	Repo         string        `json:"source"`
	EventType    string        `json:"event_type"`
	Ref          string        `json:"ref"`
	RulesMatched int           `json:"rules_matched"`
	Status       string        `json:"status"`
	Triggers     []hookTrigger `json:"triggers"`
	CreatedAt    time.Time     `json:"created_at"`
}

type hookTrigger struct {
	TriggerID  string    `json:"trigger_id"`
	RuleID     string    `json:"rule_id"`
	WorkflowID string    `json:"workflow_id"`
	RunID      *string   `json:"run_id,omitempty"`
	Status     string    `json:"status"`
	Error      *string   `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ── Messages ──────────────────────────────────────────────────────────────────

type hookRulesMsg []hookRule
type hookEventsMsg []hookEvent
type hookEventDetailMsg hookEvent
type hookPipelinesMsg []tuiPipeline
type hooksErrMsg struct{ err error }
type hookDeletedMsg struct{}
type hookRuleCreatedMsg struct{}
type hooksFormErrMsg struct{ err error }

// hooksFormMode distinguishes the rule form's create vs edit behaviour.
type hooksFormMode int

const (
	hooksFormCreate hooksFormMode = iota
	hooksFormEdit
)

// ── Views ─────────────────────────────────────────────────────────────────────

type hooksViewID int

const (
	hooksViewRules hooksViewID = iota
	hooksViewEvents
	hooksViewEventDetail
	hooksViewCreate
)

// ── Model ─────────────────────────────────────────────────────────────────────

type hooksModel struct {
	view    hooksViewID
	loading bool
	err     error
	width   int
	height  int

	rules      []hookRule
	events     []hookEvent
	pipelines  []tuiPipeline // workflow catalog for the rule form's workflow selector
	selRule    *hookRule
	selEvent   *hookEvent
	confirmDel bool

	formMode         hooksFormMode     // create vs edit for the rule form
	editRuleID       string            // rule being edited (PUT target); "" when creating
	editInputMapping map[string]string // preserved across an edit (the form doesn't expose it)

	rTable table.Model
	eTable table.Model
	vp     viewport.Model
	form   tuiForm
}

var (
	hookRuleCols = []tuiColSpec{
		{"NAME", 16, 1},
		{"REPO", 18, 2},
		{"EVENTS", 14, 1},
		{"ACTIVE", 7, 0},
		{"CREATED", 14, 0},
	}
	// Events have no human name, so the primary label is meaningful context —
	// event type + repo + ref — and the short id is a secondary detail column.
	hookEventCols = []tuiColSpec{
		{"TYPE", 14, 1},
		{"REPO", 18, 2},
		{"REF", 14, 1},
		{"MATCHED", 8, 0},
		{"TIME", 14, 0},
		{"ID", 10, 0},
	}
)

func newHooksModel() hooksModel {
	rTable := table.New(table.WithFocused(true))
	rTable.SetStyles(tuiTableStyles())
	eTable := table.New(table.WithFocused(true))
	eTable.SetStyles(tuiTableStyles())

	m := hooksModel{
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
		rTable:  rTable,
		eTable:  eTable,
		vp:      viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8),
	}
	m.applyTableLayout()
	return m
}

// applyTableLayout resizes both hooks tables to the current terminal.
func (m *hooksModel) applyTableLayout() {
	m.rTable.SetColumns(tuiFitColumns(hookRuleCols, m.width))
	m.rTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
	m.eTable.SetColumns(tuiFitColumns(hookEventCols, m.width))
	m.eTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func hooksFetchRules() tea.Msg {
	data, err := doRequest("GET", "/hooks/rules", nil)
	if err != nil {
		return hooksErrMsg{err}
	}
	var rules []hookRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return hooksErrMsg{err}
	}
	return hookRulesMsg(rules)
}

func hooksFetchEvents(repo string) tea.Cmd {
	return func() tea.Msg {
		path := "/hooks/events"
		if repo != "" {
			path += "?repo=" + url.QueryEscape(repo)
		}
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return hooksErrMsg{err}
		}
		var events []hookEvent
		if err := json.Unmarshal(data, &events); err != nil {
			return hooksErrMsg{err}
		}
		return hookEventsMsg(events)
	}
}

func hooksFetchEventDetail(eventID string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/hooks/events/"+eventID, nil)
		if err != nil {
			return hooksErrMsg{err}
		}
		var e hookEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return hooksErrMsg{err}
		}
		return hookEventDetailMsg(e)
	}
}

func hooksDeleteRule(ruleID string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/hooks/rules/"+ruleID, nil); err != nil {
			return hooksErrMsg{err}
		}
		return hookDeletedMsg{}
	}
}

// hooksFetchPipelines loads the workflow catalog for the rule form's workflow
// selector, degrading to an empty list on failure so the field falls back to a
// free-text input (and a workflows permission error never blanks the TUI).
func hooksFetchPipelines() tea.Msg {
	data, err := doRequest("GET", "/workflows/pipelines", nil)
	if err != nil {
		return hookPipelinesMsg(nil)
	}
	var ps []tuiPipeline
	if err := json.Unmarshal(data, &ps); err != nil {
		return hookPipelinesMsg(nil)
	}
	return hookPipelinesMsg(ps)
}

// ── Init ──────────────────────────────────────────────────────────────────────

// Init loads the rules and batches the pipeline catalog so the create-rule
// form's workflow selector is ready the moment it opens.
func (m hooksModel) Init() tea.Cmd { return tea.Batch(hooksFetchRules, hooksFetchPipelines) }

// ── Update ────────────────────────────────────────────────────────────────────

func (m hooksModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil

	case hooksErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case hookRulesMsg:
		m.loading = false
		m.rules = []hookRule(msg)
		rows := make([]table.Row, len(m.rules))
		for i, r := range m.rules {
			active := "●"
			if !r.Active {
				active = "○"
			}
			rows[i] = table.Row{
				r.Name,
				r.Repo,
				strings.Join(r.Events, ","),
				active,
				r.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.rTable.SetRows(rows)
		return m, nil

	case hookPipelinesMsg:
		m.pipelines = []tuiPipeline(msg)
		return m, nil

	case hookEventsMsg:
		m.loading = false
		m.events = []hookEvent(msg)
		rows := make([]table.Row, len(m.events))
		for i, e := range m.events {
			rows[i] = table.Row{
				e.EventType,
				e.Repo,
				e.Ref,
				fmt.Sprintf("%d", e.RulesMatched),
				e.CreatedAt.Local().Format("Jan 02 15:04"),
				tuiShortID(e.EventID),
			}
		}
		m.eTable.SetRows(rows)
		return m, nil

	case hookEventDetailMsg:
		m.loading = false
		e := hookEvent(msg)
		m.selEvent = &e
		m.vp.SetContent(hooksRenderEventDetail(e, m.pipelineNames()))
		m.vp.GotoTop()
		m.view = hooksViewEventDetail
		return m, nil

	case hookDeletedMsg:
		m.loading = true
		m.selRule = nil
		return m, hooksFetchRules

	case hookRuleCreatedMsg:
		m.view = hooksViewRules
		m.loading = true
		return m, hooksFetchRules

	case hooksFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		// Silently re-fetch the current list so new rules/events appear without a
		// loading flash or losing the cursor. The event-detail view shows an
		// immutable past event, so it needs no refresh. The create form must not
		// be disturbed.
		switch m.view {
		case hooksViewRules:
			return m, hooksFetchRules
		case hooksViewEvents:
			repo := ""
			if m.selRule != nil {
				repo = m.selRule.Repo
			}
			return m, hooksFetchEvents(repo)
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
				return m, hooksFetchRules
			}
			return m, nil
		}
		switch m.view {
		case hooksViewRules:
			return m.hooksKeyRules(msg)
		case hooksViewEvents:
			return m.hooksKeyEvents(msg)
		case hooksViewEventDetail:
			return m.hooksKeyEventDetail(msg)
		case hooksViewCreate:
			return m.hooksKeyCreate(msg)
		}
	}
	return m.hooksDelegate(msg)
}

func (m hooksModel) hooksDelegate(msg tea.Msg) (hooksModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case hooksViewRules:
		m.rTable, cmd = m.rTable.Update(msg)
	case hooksViewEvents:
		m.eTable, cmd = m.eTable.Update(msg)
	case hooksViewEventDetail:
		m.vp, cmd = m.vp.Update(msg)
	case hooksViewCreate:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m hooksModel) hooksKeyRules(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
	if m.confirmDel {
		switch msg.String() {
		case "y", "Y":
			m.confirmDel = false
			if m.selRule != nil {
				return m, hooksDeleteRule(m.selRule.RuleID)
			}
		default:
			m.confirmDel = false
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i := m.rTable.Cursor()
		if i >= 0 && i < len(m.rules) {
			m.selRule = &m.rules[i]
			m.view = hooksViewEvents
			m.loading = true
			return m, hooksFetchEvents(m.selRule.Repo)
		}
	case "a":
		m.view = hooksViewEvents
		m.selRule = nil
		m.loading = true
		return m, hooksFetchEvents("")
	case "n":
		m.formMode = hooksFormCreate
		m.editRuleID = ""
		var cmd tea.Cmd
		m.form, cmd = newHooksRuleForm(m.pipelines)
		m.view = hooksViewCreate
		return m, cmd
	case "e":
		i := m.rTable.Cursor()
		if i >= 0 && i < len(m.rules) {
			m.formMode = hooksFormEdit
			m.editRuleID = m.rules[i].RuleID
			m.editInputMapping = m.rules[i].InputMapping
			var cmd tea.Cmd
			m.form, cmd = newHooksRuleEditForm(m.rules[i], m.pipelines)
			m.view = hooksViewCreate
			return m, cmd
		}
	case "D":
		i := m.rTable.Cursor()
		if i >= 0 && i < len(m.rules) {
			m.selRule = &m.rules[i]
			m.confirmDel = true
		}
	case "r":
		m.loading = true
		return m, hooksFetchRules
	}
	var cmd tea.Cmd
	m.rTable, cmd = m.rTable.Update(msg)
	return m, cmd
}

func (m hooksModel) hooksKeyEvents(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = hooksViewRules
		return m, nil
	case "enter":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.events) {
			m.loading = true
			m.view = hooksViewEventDetail
			return m, hooksFetchEventDetail(m.events[i].EventID)
		}
	case "r":
		m.loading = true
		repo := ""
		if m.selRule != nil {
			repo = m.selRule.Repo
		}
		return m, hooksFetchEvents(repo)
	}
	var cmd tea.Cmd
	m.eTable, cmd = m.eTable.Update(msg)
	return m, cmd
}

func (m hooksModel) hooksKeyEventDetail(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = hooksViewEvents
		m.selEvent = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── Create form ───────────────────────────────────────────────────────────────

func newHooksRuleForm(pipelines []tuiPipeline) (tuiForm, tea.Cmd) {
	return newTUIForm("New Hook Rule",
		formInput("name", "Name", "ci-push (required)"),
		formInput("repo", "Repo", "myorg/myrepo (required)"),
		formInput("events", "Events", "push pull_request (required)"),
		hooksWorkflowField(pipelines),
		formInput("secret", "Secret", "HMAC secret (required)"),
		formInput("ref", "Ref filter", "refs/heads/main (optional)"),
	)
}

// newHooksRuleEditForm builds the rule form pre-filled from an existing rule. The
// secret is never returned by the API, so the field is left blank with a hint
// that blank keeps the current secret (the update endpoint treats an omitted
// secret as "leave unchanged").
func newHooksRuleEditForm(r hookRule, pipelines []tuiPipeline) (tuiForm, tea.Cmd) {
	wf := hooksWorkflowField(pipelines)
	wf.setValue(r.WorkflowID)
	f, cmd := newTUIForm("Edit Hook Rule",
		formInputDefault("name", "Name", "ci-push (required)", r.Name),
		formInputDefault("repo", "Repo", "myorg/myrepo (required)", r.Repo),
		formInputDefault("events", "Events", "push pull_request (required)", strings.Join(r.Events, " ")),
		wf,
		formInput("secret", "Secret", "leave blank to keep current"),
		formInputDefault("ref", "Ref filter", "refs/heads/main (optional)", r.RefFilter),
	)
	return f, cmd
}

// hooksWorkflowField builds the rule form's workflow picker: a selector of
// pipeline names (submitting the workflow_id) when the catalog is available, or
// a free-text id fallback when it could not be loaded.
func hooksWorkflowField(pipelines []tuiPipeline) formField {
	if len(pipelines) == 0 {
		return formInput("workflow", "Workflow", "pipeline id (required)")
	}
	labels := make([]string, len(pipelines))
	ids := make([]string, len(pipelines))
	for i, p := range pipelines {
		labels[i] = p.Name
		ids[i] = p.WorkflowID
	}
	return formSelectKV("workflow", "Workflow", labels, ids)
}

func (m hooksModel) hooksKeyCreate(msg tea.KeyMsg) (hooksModel, tea.Cmd) {
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
		m.view = hooksViewRules
		return m, nil
	case formSubmit:
		return m.hooksSubmitCreate()
	}
	return m, cmd
}

// hooksSubmitCreate validates the form and, if valid, returns the POST cmd.
func (m hooksModel) hooksSubmitCreate() (hooksModel, tea.Cmd) {
	name := m.form.value("name")
	repo := m.form.value("repo")
	events := splitEvents(m.form.value("events"))
	workflow := m.form.value("workflow")
	secret := m.form.value("secret")
	switch {
	case name == "":
		m.form.errMsg = "name is required"
	case repo == "":
		m.form.errMsg = "repo is required"
	case len(events) == 0:
		m.form.errMsg = "at least one event is required"
	case workflow == "":
		m.form.errMsg = "a workflow is required"
	case m.formMode == hooksFormCreate && secret == "":
		m.form.errMsg = "secret is required (webhook rules must have an HMAC secret)"
	default:
		m.form.errMsg = ""
		mapping := map[string]string{}
		if m.formMode == hooksFormEdit && m.editInputMapping != nil {
			mapping = m.editInputMapping
		}
		return m, hooksSubmitRule(m.formMode, m.editRuleID, name, repo, events, workflow, secret, m.form.value("ref"), mapping)
	}
	return m, nil
}

// hooksSubmitRule POSTs a new rule or PUTs an existing one. On edit a blank
// secret is omitted so the server keeps the current one; input_mapping is passed
// through (the form doesn't expose it) so an edit never silently clears it.
func hooksSubmitRule(mode hooksFormMode, ruleID, name, repo string, events []string, workflow, secret, ref string, inputMapping map[string]string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{
			"name":          name,
			"source":        repo,
			"events":        events,
			"workflow_id":   workflow,
			"ref_filter":    ref,
			"input_mapping": inputMapping,
		}
		if secret != "" {
			payload["secret"] = secret
		}
		method, path := "POST", "/hooks/rules"
		if mode == hooksFormEdit {
			method, path = "PUT", "/hooks/rules/"+ruleID
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest(method, path, body); err != nil {
			return hooksFormErrMsg{err}
		}
		return hookRuleCreatedMsg{}
	}
}

// splitEvents tokenises an events field on spaces and commas.
func splitEvents(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m hooksModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case hooksViewCreate:
		return m.form.view(m.width, m.height)
	case hooksViewEvents:
		return m.hooksViewEvents()
	case hooksViewEventDetail:
		return m.hooksViewEventDetail()
	}
	return m.hooksViewRules()
}

func (m hooksModel) hooksViewRules() string {
	title := tuiTitleStyle.Render("Hooks Rules")
	help := tuiHelp("[↑↓/jk] nav  [enter] events  [a] all events  [n] new  [e] edit  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.confirmDel {
		name := ""
		if m.selRule != nil {
			name = " '" + m.selRule.Name + "'"
		}
		confirm := tuiErrStyle.
			Render("Delete rule" + name + "? [y] confirm  [any] cancel")
		if len(m.rules) == 0 {
			return title + "\n\n" + tuiMetaStyle.Render("No rules defined.") + "\n\n" + confirm
		}
		return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + confirm
	}
	if len(m.rules) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No rules defined.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + help
}

func (m hooksModel) hooksViewEvents() string {
	subtitle := "Events"
	if m.selRule != nil {
		subtitle += ": " + m.selRule.Repo
	}
	title := tuiTitleStyle.Render("Hooks " + subtitle)
	help := tuiHelp("[↑↓/jk] navigate  [enter] detail  [r] refresh  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.events) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No events.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.eTable.View()) + "\n" + help
}

func (m hooksModel) hooksViewEventDetail() string {
	title := tuiTitleStyle.Render("Event Detail")
	if m.selEvent != nil {
		title = tuiTitleStyle.Render(m.selEvent.EventType) + "  " +
			tuiMetaStyle.Render(m.selEvent.Repo)
	}
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// pipelineNames maps each known workflow_id to its pipeline name, for rendering
// triggers by name rather than by opaque id.
func (m hooksModel) pipelineNames() map[string]string {
	names := make(map[string]string, len(m.pipelines))
	for _, p := range m.pipelines {
		names[p.WorkflowID] = p.Name
	}
	return names
}

// hooksWorkflowLabel renders a workflow's human-readable name when known,
// falling back to its short id.
func hooksWorkflowLabel(id string, names map[string]string) string {
	if n := names[id]; n != "" {
		return n
	}
	return tuiShortID(id)
}

func hooksRenderEventDetail(e hookEvent, wfNames map[string]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"Event ID:      %s\nRepo:          %s\nType:          %s\nRef:           %s\nStatus:        %s\nRules matched: %d\nReceived:      %s\n",
		e.EventID, e.Repo, e.EventType, e.Ref, e.Status,
		e.RulesMatched, e.CreatedAt.Local().Format(time.RFC3339),
	)
	if len(e.Triggers) > 0 {
		sb.WriteString("\nTRIGGERS:\n")
		for i, t := range e.Triggers {
			fmt.Fprintf(&sb, "\n  [%d] rule: %s  workflow: %s  status: %s\n",
				i+1, tuiShortID(t.RuleID), hooksWorkflowLabel(t.WorkflowID, wfNames), t.Status,
			)
			if t.RunID != nil {
				fmt.Fprintf(&sb, "      run: %s\n", *t.RunID)
			}
			if t.Error != nil {
				fmt.Fprintf(&sb, "      error: %s\n", *t.Error)
			}
		}
	}
	return sb.String()
}

// ── Command registration ──────────────────────────────────────────────────────

func startHooksTUI() error {
	p := tea.NewProgram(standaloneWrap{newHooksModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	hooksCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return startHooksTUI()
	}
	hooksCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for browsing webhook rules and events",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startHooksTUI() },
	})
	RegisterModule(Module{
		Name:    "hooks",
		Service: "hooks",
		Order:   40,
		Command: hooksCmd,
		Screens: []HubScreen{{
			Title: "Hooks",
			Desc:  "Webhook rules and event history",
			New:   func() tea.Model { return newHooksModel() },
		}},
	})
}
