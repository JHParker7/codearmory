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

// eventTrigger is the subscription: a filter plus the actions it dispatches. Match and Actions
// are kept as decoded documents rather than typed structs because the filter grammar is
// recursive and the action config is kind-specific — the TUI edits the common shape and passes
// anything richer through untouched.
type eventTrigger struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Match     matchNode       `json:"match"`
	Actions   []triggerAction `json:"actions"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
}

// matchNode mirrors the service's filter node: a group (all/any/not) or a leaf (field/op/value).
type matchNode struct {
	All   []matchNode `json:"all,omitempty"`
	Any   []matchNode `json:"any,omitempty"`
	Not   *matchNode  `json:"not,omitempty"`
	Field string      `json:"field,omitempty"`
	Op    string      `json:"op,omitempty"`
	Value any         `json:"value,omitempty"`
}

type triggerAction struct {
	Kind   string         `json:"kind"`
	Config map[string]any `json:"config,omitempty"`
}

// logEvent is one row of the append-only event log.
type logEvent struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Source     string         `json:"source"`
	Subject    string         `json:"subject"`
	Actor      logActor       `json:"actor"`
	OccurredAt string         `json:"occurred_at"`
	Data       map[string]any `json:"data,omitempty"`
}

type logActor struct {
	OrgID  string `json:"org_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

// leafValue finds the value of the first leaf matching field anywhere in the tree, so the edit
// form can pre-fill from a filter it did not necessarily author.
func (m matchNode) leafValue(field string) string {
	if m.Field == field {
		if s, ok := m.Value.(string); ok {
			return s
		}
		if m.Value != nil {
			return fmt.Sprint(m.Value)
		}
		return ""
	}
	for _, sub := range append(append([]matchNode{}, m.All...), m.Any...) {
		if v := sub.leafValue(field); v != "" {
			return v
		}
	}
	if m.Not != nil {
		return m.Not.leafValue(field)
	}
	return ""
}

// summary renders a filter compactly for the list column: "type=repo.push data.ref=main".
func (m matchNode) summary() string {
	var parts []string
	var walk func(n matchNode)
	walk = func(n matchNode) {
		if n.Field != "" {
			op := "="
			if n.Op != "" && n.Op != "eq" {
				op = " " + n.Op + " "
			}
			parts = append(parts, n.Field+op+fmt.Sprint(n.Value))
			return
		}
		for _, s := range n.All {
			walk(s)
		}
		for _, s := range n.Any {
			walk(s)
		}
		if n.Not != nil {
			walk(*n.Not)
		}
	}
	walk(m)
	if len(parts) == 0 {
		return "(any event)"
	}
	return strings.Join(parts, " ")
}

// pipelineID returns the pipeline the trigger's first run_pipeline action starts, if any.
func (t eventTrigger) pipelineID() string {
	for _, a := range t.Actions {
		if a.Kind == "run_pipeline" {
			if id, ok := a.Config["pipeline_id"].(string); ok {
				return id
			}
		}
	}
	return ""
}

// actionSummary lists the trigger's action kinds for the list column.
func (t eventTrigger) actionSummary() string {
	if len(t.Actions) == 0 {
		return "—"
	}
	kinds := make([]string, len(t.Actions))
	for i, a := range t.Actions {
		kinds[i] = a.Kind
	}
	return strings.Join(kinds, ",")
}

// ── Messages ──────────────────────────────────────────────────────────────────

type eventTriggersMsg []eventTrigger
type eventLogMsg []logEvent
type eventDetailMsg logEvent
type eventPipelinesMsg []tuiPipeline
type eventsErrMsg struct{ err error }
type eventTriggerDeletedMsg struct{}
type eventTriggerSavedMsg struct{}
type eventsFormErrMsg struct{ err error }

// eventsFormMode distinguishes the trigger form's create vs edit behaviour.
type eventsFormMode int

const (
	eventsFormCreate eventsFormMode = iota
	eventsFormEdit
)

// ── Views ─────────────────────────────────────────────────────────────────────

type eventsViewID int

const (
	eventsViewTriggers eventsViewID = iota
	eventsViewLog
	eventsViewDetail
	eventsViewForm
)

// ── Model ─────────────────────────────────────────────────────────────────────

type eventsModel struct {
	view    eventsViewID
	loading bool
	err     error
	width   int
	height  int

	triggers   []eventTrigger
	events     []logEvent
	pipelines  []tuiPipeline // workflow catalog for the form's pipeline selector
	selTrigger *eventTrigger
	selEvent   *logEvent
	confirmDel bool

	formMode      eventsFormMode
	editTriggerID string          // trigger being edited (PUT target); "" when creating
	editExtraActs []triggerAction // non-run_pipeline actions preserved across an edit

	tTable table.Model
	eTable table.Model
	vp     viewport.Model
	form   tuiForm
}

var (
	eventTriggerCols = []tuiColSpec{
		{"NAME", 16, 1},
		{"MATCH", 26, 2},
		{"ACTIONS", 14, 1},
		{"ON", 4, 0},
		{"CREATED", 14, 0},
	}
	// Events have no human name, so the primary label is meaningful context — type, subject
	// and source — and the short id is a secondary detail column.
	eventLogCols = []tuiColSpec{
		{"TYPE", 18, 1},
		{"SUBJECT", 20, 2},
		{"SOURCE", 12, 1},
		{"TIME", 14, 0},
		{"ID", 10, 0},
	}
)

func newEventsModel() eventsModel {
	tTable := table.New(table.WithFocused(true))
	tTable.SetStyles(tuiTableStyles())
	eTable := table.New(table.WithFocused(true))
	eTable.SetStyles(tuiTableStyles())

	m := eventsModel{
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
		tTable:  tTable,
		eTable:  eTable,
		vp:      viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8),
	}
	m.applyTableLayout()
	return m
}

// applyTableLayout resizes both tables to the current terminal.
func (m *eventsModel) applyTableLayout() {
	m.tTable.SetColumns(tuiFitColumns(eventTriggerCols, m.width))
	m.tTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
	m.eTable.SetColumns(tuiFitColumns(eventLogCols, m.width))
	m.eTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func eventsFetchTriggers() tea.Msg {
	data, err := doRequest("GET", "/events/triggers", nil)
	if err != nil {
		return eventsErrMsg{err}
	}
	var ts []eventTrigger
	if err := json.Unmarshal(data, &ts); err != nil {
		return eventsErrMsg{err}
	}
	return eventTriggersMsg(ts)
}

func eventsFetchLog(eventType string) tea.Cmd {
	return func() tea.Msg {
		path := "/events/events"
		if eventType != "" {
			path += "?type=" + url.QueryEscape(eventType)
		}
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return eventsErrMsg{err}
		}
		var evs []logEvent
		if err := json.Unmarshal(data, &evs); err != nil {
			return eventsErrMsg{err}
		}
		return eventLogMsg(evs)
	}
}

func eventsFetchDetail(eventID string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/events/events/"+eventID, nil)
		if err != nil {
			return eventsErrMsg{err}
		}
		var e logEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return eventsErrMsg{err}
		}
		return eventDetailMsg(e)
	}
}

func eventsDeleteTrigger(id string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/events/triggers/"+id, nil); err != nil {
			return eventsErrMsg{err}
		}
		return eventTriggerDeletedMsg{}
	}
}

// eventsFetchPipelines loads the workflow catalog for the form's pipeline selector, degrading
// to an empty list on failure so the field falls back to a free-text input (and a workflows
// permission error never blanks the TUI).
func eventsFetchPipelines() tea.Msg {
	data, err := doRequest("GET", "/workflows/pipelines", nil)
	if err != nil {
		return eventPipelinesMsg(nil)
	}
	var ps []tuiPipeline
	if err := json.Unmarshal(data, &ps); err != nil {
		return eventPipelinesMsg(nil)
	}
	return eventPipelinesMsg(ps)
}

// ── Init ──────────────────────────────────────────────────────────────────────

// Init loads the triggers and batches the pipeline catalog so the form's selector is ready the
// moment it opens.
func (m eventsModel) Init() tea.Cmd { return tea.Batch(eventsFetchTriggers, eventsFetchPipelines) }

// ── Update ────────────────────────────────────────────────────────────────────

func (m eventsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil

	case eventsErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case eventTriggersMsg:
		m.loading = false
		m.triggers = []eventTrigger(msg)
		rows := make([]table.Row, len(m.triggers))
		for i, t := range m.triggers {
			on := "●"
			if !t.Enabled {
				on = "○"
			}
			rows[i] = table.Row{
				t.Name,
				t.Match.summary(),
				t.actionSummary(),
				on,
				t.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.tTable.SetRows(rows)
		return m, nil

	case eventPipelinesMsg:
		m.pipelines = []tuiPipeline(msg)
		return m, nil

	case eventLogMsg:
		m.loading = false
		m.events = []logEvent(msg)
		rows := make([]table.Row, len(m.events))
		for i, e := range m.events {
			rows[i] = table.Row{
				e.Type,
				e.Subject,
				e.Source,
				eventsFormatTime(e.OccurredAt),
				tuiShortID(e.ID),
			}
		}
		m.eTable.SetRows(rows)
		return m, nil

	case eventDetailMsg:
		m.loading = false
		e := logEvent(msg)
		m.selEvent = &e
		m.vp.SetContent(eventsRenderDetail(e))
		m.vp.GotoTop()
		m.view = eventsViewDetail
		return m, nil

	case eventTriggerDeletedMsg:
		m.loading = true
		m.selTrigger = nil
		return m, eventsFetchTriggers

	case eventTriggerSavedMsg:
		m.view = eventsViewTriggers
		m.loading = true
		return m, eventsFetchTriggers

	case eventsFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		// Silently re-fetch the current list so new triggers/events appear without a loading
		// flash or losing the cursor. The detail view shows an immutable past event, so it
		// needs no refresh. The form must not be disturbed.
		switch m.view {
		case eventsViewTriggers:
			return m, eventsFetchTriggers
		case eventsViewLog:
			return m, eventsFetchLog(m.selectedType())
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
				return m, eventsFetchTriggers
			}
			return m, nil
		}
		switch m.view {
		case eventsViewTriggers:
			return m.eventsKeyTriggers(msg)
		case eventsViewLog:
			return m.eventsKeyLog(msg)
		case eventsViewDetail:
			return m.eventsKeyDetail(msg)
		case eventsViewForm:
			return m.eventsKeyForm(msg)
		}
	}
	return m.eventsDelegate(msg)
}

// selectedType is the event type the log view is filtered by — the selected trigger's `type`
// leaf, or "" for the unfiltered log.
func (m eventsModel) selectedType() string {
	if m.selTrigger == nil {
		return ""
	}
	return m.selTrigger.Match.leafValue("type")
}

func (m eventsModel) eventsDelegate(msg tea.Msg) (eventsModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case eventsViewTriggers:
		m.tTable, cmd = m.tTable.Update(msg)
	case eventsViewLog:
		m.eTable, cmd = m.eTable.Update(msg)
	case eventsViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	case eventsViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m eventsModel) eventsKeyTriggers(msg tea.KeyMsg) (eventsModel, tea.Cmd) {
	if m.confirmDel {
		switch msg.String() {
		case "y", "Y":
			m.confirmDel = false
			if m.selTrigger != nil {
				return m, eventsDeleteTrigger(m.selTrigger.ID)
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
		i := m.tTable.Cursor()
		if i >= 0 && i < len(m.triggers) {
			m.selTrigger = &m.triggers[i]
			m.view = eventsViewLog
			m.loading = true
			return m, eventsFetchLog(m.selectedType())
		}
	case "a":
		m.view = eventsViewLog
		m.selTrigger = nil
		m.loading = true
		return m, eventsFetchLog("")
	case "n":
		m.formMode = eventsFormCreate
		m.editTriggerID = ""
		m.editExtraActs = nil
		var cmd tea.Cmd
		m.form, cmd = newEventsTriggerForm(m.pipelines)
		m.view = eventsViewForm
		return m, cmd
	case "e":
		i := m.tTable.Cursor()
		if i >= 0 && i < len(m.triggers) {
			t := m.triggers[i]
			m.formMode = eventsFormEdit
			m.editTriggerID = t.ID
			// Preserve every action the form does not model, so editing the pipeline never
			// silently drops a notify or webhook_out the user configured elsewhere.
			m.editExtraActs = nil
			for _, a := range t.Actions {
				if a.Kind != "run_pipeline" {
					m.editExtraActs = append(m.editExtraActs, a)
				}
			}
			var cmd tea.Cmd
			m.form, cmd = newEventsTriggerEditForm(t, m.pipelines)
			m.view = eventsViewForm
			return m, cmd
		}
	case "D":
		i := m.tTable.Cursor()
		if i >= 0 && i < len(m.triggers) {
			m.selTrigger = &m.triggers[i]
			m.confirmDel = true
		}
	case "r":
		m.loading = true
		return m, eventsFetchTriggers
	}
	var cmd tea.Cmd
	m.tTable, cmd = m.tTable.Update(msg)
	return m, cmd
}

func (m eventsModel) eventsKeyLog(msg tea.KeyMsg) (eventsModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = eventsViewTriggers
		return m, nil
	case "enter":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.events) {
			m.loading = true
			m.view = eventsViewDetail
			return m, eventsFetchDetail(m.events[i].ID)
		}
	case "r":
		m.loading = true
		return m, eventsFetchLog(m.selectedType())
	}
	var cmd tea.Cmd
	m.eTable, cmd = m.eTable.Update(msg)
	return m, cmd
}

func (m eventsModel) eventsKeyDetail(msg tea.KeyMsg) (eventsModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = eventsViewLog
		m.selEvent = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── Trigger form ──────────────────────────────────────────────────────────────

// The form models the common CI shape — "run this pipeline when this kind of event happens to
// this subject" — as three filter fields ANDed together. A filter the form cannot express is
// still editable through the CLI's --match-json, and an existing one is never mangled: only the
// leaves the form owns are rewritten on save.

func newEventsTriggerForm(pipelines []tuiPipeline) (tuiForm, tea.Cmd) {
	return newTUIForm("New Event Trigger",
		formInput("name", "Name", "ci-push (required)"),
		formInput("type", "Event type", "repo.push (required)"),
		formInput("subject", "Subject", "myorg/myrepo (optional, exact)"),
		formInput("ref", "Ref", "main (optional, matches data.ref)"),
		eventsPipelineField(pipelines),
	)
}

// newEventsTriggerEditForm builds the form pre-filled from an existing trigger.
func newEventsTriggerEditForm(t eventTrigger, pipelines []tuiPipeline) (tuiForm, tea.Cmd) {
	pf := eventsPipelineField(pipelines)
	pf.setValue(t.pipelineID())
	return newTUIForm("Edit Event Trigger",
		formInputDefault("name", "Name", "ci-push (required)", t.Name),
		formInputDefault("type", "Event type", "repo.push (required)", t.Match.leafValue("type")),
		formInputDefault("subject", "Subject", "myorg/myrepo (optional)", t.Match.leafValue("subject")),
		formInputDefault("ref", "Ref", "main (optional)", t.Match.leafValue("data.ref")),
		pf,
	)
}

// eventsPipelineField builds the form's pipeline picker: a selector of pipeline names
// (submitting the pipeline id) when the catalog is available, or a free-text id fallback when
// it could not be loaded.
func eventsPipelineField(pipelines []tuiPipeline) formField {
	if len(pipelines) == 0 {
		return formInput("pipeline", "Pipeline", "pipeline id (required)")
	}
	labels := make([]string, len(pipelines))
	ids := make([]string, len(pipelines))
	for i, p := range pipelines {
		labels[i] = p.Name
		ids[i] = p.WorkflowID
	}
	return formSelectKV("pipeline", "Pipeline", labels, ids)
}

func (m eventsModel) eventsKeyForm(msg tea.KeyMsg) (eventsModel, tea.Cmd) {
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
		m.view = eventsViewTriggers
		return m, nil
	case formSubmit:
		return m.eventsSubmitForm()
	}
	return m, cmd
}

// eventsSubmitForm validates the form and, if valid, returns the save cmd.
func (m eventsModel) eventsSubmitForm() (eventsModel, tea.Cmd) {
	name := m.form.value("name")
	eventType := m.form.value("type")
	pipeline := m.form.value("pipeline")
	switch {
	case name == "":
		m.form.errMsg = "name is required"
	case eventType == "":
		m.form.errMsg = "an event type is required"
	case pipeline == "":
		m.form.errMsg = "a pipeline is required"
	default:
		m.form.errMsg = ""
		return m, eventsSubmitTrigger(m.formMode, m.editTriggerID, name, eventType,
			m.form.value("subject"), m.form.value("ref"), pipeline, m.editExtraActs)
	}
	return m, nil
}

// buildMatch ANDs the non-empty form fields into a filter document.
func buildMatch(eventType, subject, ref string) matchNode {
	leaves := []matchNode{{Field: "type", Op: "eq", Value: eventType}}
	if subject != "" {
		leaves = append(leaves, matchNode{Field: "subject", Op: "eq", Value: subject})
	}
	if ref != "" {
		leaves = append(leaves, matchNode{Field: "data.ref", Op: "eq", Value: ref})
	}
	return matchNode{All: leaves}
}

// eventsSubmitTrigger POSTs a new trigger or PUTs an existing one. Actions the form does not
// model are appended back so an edit never silently drops them.
func eventsSubmitTrigger(mode eventsFormMode, id, name, eventType, subject, ref, pipeline string, extra []triggerAction) tea.Cmd {
	return func() tea.Msg {
		actions := []triggerAction{{
			Kind:   "run_pipeline",
			Config: map[string]any{"pipeline_id": pipeline},
		}}
		actions = append(actions, extra...)

		payload := map[string]any{
			"name":    name,
			"match":   buildMatch(eventType, subject, ref),
			"actions": actions,
			"enabled": true,
		}
		method, path := "POST", "/events/triggers"
		if mode == eventsFormEdit {
			method, path = "PUT", "/events/triggers/"+id
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest(method, path, body); err != nil {
			return eventsFormErrMsg{err}
		}
		return eventTriggerSavedMsg{}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m eventsModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case eventsViewForm:
		return m.form.view(m.width, m.height)
	case eventsViewLog:
		return m.eventsViewLog()
	case eventsViewDetail:
		return m.eventsViewDetail()
	}
	return m.eventsViewTriggers()
}

func (m eventsModel) eventsViewTriggers() string {
	title := tuiTitleStyle.Render("Event Triggers")
	help := tuiHelp("[↑↓/jk] nav  [enter] events  [a] all events  [n] new  [e] edit  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.confirmDel {
		name := ""
		if m.selTrigger != nil {
			name = " '" + m.selTrigger.Name + "'"
		}
		confirm := tuiErrStyle.Render("Delete trigger" + name + "? [y] confirm  [any] cancel")
		if len(m.triggers) == 0 {
			return title + "\n\n" + tuiMetaStyle.Render("No triggers defined.") + "\n\n" + confirm
		}
		return title + "\n" + tuiBoxStyle.Render(m.tTable.View()) + "\n" + confirm
	}
	if len(m.triggers) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No triggers defined.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.tTable.View()) + "\n" + help
}

func (m eventsModel) eventsViewLog() string {
	subtitle := "Log"
	if t := m.selectedType(); t != "" {
		subtitle += ": " + t
	}
	title := tuiTitleStyle.Render("Events " + subtitle)
	help := tuiHelp("[↑↓/jk] navigate  [enter] detail  [r] refresh  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.events) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No events.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.eTable.View()) + "\n" + help
}

func (m eventsModel) eventsViewDetail() string {
	title := tuiTitleStyle.Render("Event Detail")
	if m.selEvent != nil {
		title = tuiTitleStyle.Render(m.selEvent.Type) + "  " + tuiMetaStyle.Render(m.selEvent.Subject)
	}
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// eventsFormatTime renders an RFC3339 occurred_at in local time, falling back to the raw
// string when it does not parse — a malformed timestamp should show as-is, not vanish.
func eventsFormatTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("Jan 02 15:04")
}

func eventsRenderDetail(e logEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"Event ID:  %s\nType:      %s\nSource:    %s\nSubject:   %s\nOccurred:  %s\n",
		e.ID, e.Type, e.Source, e.Subject, e.OccurredAt,
	)
	if e.Actor.OrgID != "" {
		fmt.Fprintf(&sb, "Org:       %s\n", e.Actor.OrgID)
	}
	if e.Actor.UserID != "" {
		fmt.Fprintf(&sb, "User:      %s\n", e.Actor.UserID)
	}
	if len(e.Data) > 0 {
		sb.WriteString("\nDATA:\n")
		// Marshal rather than range: encoding/json sorts keys, so the same event always
		// renders identically instead of shuffling between views.
		if b, err := json.MarshalIndent(e.Data, "  ", "  "); err == nil {
			sb.WriteString("  ")
			sb.Write(b)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// ── Command registration ──────────────────────────────────────────────────────

func startEventsTUI() error {
	p := tea.NewProgram(standaloneWrap{newEventsModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	eventsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return startEventsTUI()
	}
	eventsCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for browsing event triggers and the event log",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startEventsTUI() },
	})
	RegisterModule(Module{
		Name:    "events",
		Service: "events",
		Order:   40,
		Command: eventsCmd,
		Screens: []HubScreen{{
			Title: "Events",
			Desc:  "Event triggers and the event log",
			New:   func() tea.Model { return newEventsModel() },
		}},
	})
}
