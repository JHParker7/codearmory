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
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ── Styles ────────────────────────────────────────────────────────────────────

// These styles are shared by every list/detail TUI (ci, forge, audit, steps,
// hooks). buildCITUIStyles rebuilds them, plus the status color map, from the
// active theme.
var (
	tuiBoxStyle   lipgloss.Style
	tuiTitleStyle lipgloss.Style
	tuiMetaStyle  lipgloss.Style
	tuiHelpStyle  lipgloss.Style
	tuiErrStyle   lipgloss.Style

	tuiStatusColors map[string]lipgloss.Color
)

func buildCITUIStyles() {
	tuiBoxStyle = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border))

	tuiTitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Accent))

	tuiMetaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	tuiHelpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	tuiErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Danger))

	tuiStatusColors = map[string]lipgloss.Color{
		"completed": lipgloss.Color(activeTheme.Accent),
		"running":   lipgloss.Color(activeTheme.Warning),
		"pending":   lipgloss.Color(activeTheme.Muted),
		"failed":    lipgloss.Color(activeTheme.Danger),
		"cancelled": lipgloss.Color(activeTheme.Muted),
	}
}

func tuiColorStatus(s string) string {
	if c, ok := tuiStatusColors[s]; ok {
		return lipgloss.NewStyle().Foreground(c).Render(s)
	}
	return s
}

// ── API types ─────────────────────────────────────────────────────────────────

type tuiPipeline struct {
	WorkflowID  string    `json:"workflow_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

type tuiRun struct {
	RunID       string     `json:"run_id"`
	WorkflowID  string     `json:"workflow_id"`
	TriggeredBy string     `json:"triggered_by"`
	Status      string     `json:"status"`
	CurrentStep int        `json:"current_step"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"`
}

type tuiStepRun struct {
	StepRunID string     `json:"step_run_id"`
	RunID     string     `json:"run_id"`
	StepIndex int        `json:"step_index"`
	StepName  string     `json:"step_name"`
	Status    string     `json:"status"`
	Output    *string    `json:"output"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

type tuiRunFull struct {
	tuiRun
	StepRuns []tuiStepRun `json:"step_runs"`
}

// ── View states ───────────────────────────────────────────────────────────────

type tuiViewID int

const (
	tuiViewPipelines tuiViewID = iota
	tuiViewRuns
	tuiViewRunDetail
	tuiViewOutput
	tuiViewCreate
)

// ── Messages ──────────────────────────────────────────────────────────────────

type tuiPipelinesMsg []tuiPipeline
type tuiRunsMsg []tuiRun
type tuiRunDetailMsg tuiRunFull
type tuiErrMsg struct{ err error }
type tuiTickMsg struct{}
type tuiPipelineCreatedMsg struct{}
type tuiFormErrMsg struct{ err error }

// ── Model ─────────────────────────────────────────────────────────────────────

type tuiModel struct {
	view    tuiViewID
	loading bool
	err     error
	width   int
	height  int

	pTable table.Model
	rTable table.Model
	dTable table.Model
	vp     viewport.Model

	pipelines []tuiPipeline
	runs      []tuiRun
	runFull   *tuiRunFull

	selPipeline *tuiPipeline
	selRun      *tuiRun
	outputTitle string

	form tuiForm
}

func tuiTableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border)).
		BorderBottom(true).
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Text))
	s.Selected = s.Selected.
		Foreground(lipgloss.Color(activeTheme.Accent)).
		Background(lipgloss.Color(activeTheme.SelBg)).
		Bold(false)
	return s
}

// Responsive column specs: flexible columns (description, name, triggered-by,
// step) expand to fill a wider terminal; the rest stay at their minimum.
var (
	ciPipelineCols = []tuiColSpec{
		{"NAME", 16, 2},
		{"DESCRIPTION", 20, 3},
		{"ACTIVE", 6, 0},
		{"CREATED", 14, 0},
	}
	ciRunCols = []tuiColSpec{
		{"RUN ID", 10, 0},
		{"STATUS", 11, 0},
		{"TRIGGERED BY", 16, 1},
		{"STARTED", 18, 0},
		{"DURATION", 10, 0},
	}
	ciStepCols = []tuiColSpec{
		{"#", 3, 0},
		{"STEP", 16, 1},
		{"STATUS", 11, 0},
		{"STARTED", 18, 0},
		{"DURATION", 10, 0},
	}
)

func newTUIModel() tuiModel {
	m := tuiModel{
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
	}
	m.pTable = table.New(table.WithFocused(true))
	m.pTable.SetStyles(tuiTableStyles())
	m.rTable = table.New(table.WithFocused(true))
	m.rTable.SetStyles(tuiTableStyles())
	m.dTable = table.New(table.WithFocused(true))
	m.dTable.SetStyles(tuiTableStyles())
	m.vp = viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8)
	m.applyTableLayout()
	return m
}

// applyTableLayout resizes every table to the current terminal dimensions.
func (m *tuiModel) applyTableLayout() {
	m.pTable.SetColumns(tuiFitColumns(ciPipelineCols, m.width))
	m.pTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
	m.rTable.SetColumns(tuiFitColumns(ciRunCols, m.width))
	m.rTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
	m.dTable.SetColumns(tuiFitColumns(ciStepCols, m.width))
	m.dTable.SetHeight(tuiTableHeight(m.height, tuiDetailChrome))
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m tuiModel) Init() tea.Cmd {
	return tuiFetchPipelines
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func tuiFetchPipelines() tea.Msg {
	data, err := doRequest("GET", "/workflows/pipelines", nil)
	if err != nil {
		return tuiErrMsg{err}
	}
	var ps []tuiPipeline
	if err := json.Unmarshal(data, &ps); err != nil {
		return tuiErrMsg{err}
	}
	return tuiPipelinesMsg(ps)
}

func tuiFetchRuns(workflowID string) tea.Cmd {
	return func() tea.Msg {
		path := "/workflows/runs"
		if workflowID != "" {
			path += "?workflow_id=" + url.QueryEscape(workflowID)
		}
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return tuiErrMsg{err}
		}
		var rs []tuiRun
		if err := json.Unmarshal(data, &rs); err != nil {
			return tuiErrMsg{err}
		}
		return tuiRunsMsg(rs)
	}
}

func tuiFetchRunDetail(runID string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/workflows/runs/"+runID, nil)
		if err != nil {
			return tuiErrMsg{err}
		}
		var r tuiRunFull
		if err := json.Unmarshal(data, &r); err != nil {
			return tuiErrMsg{err}
		}
		return tuiRunDetailMsg(r)
	}
}

func tuiTickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tuiTickMsg{} })
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil

	case tuiErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case tuiPipelinesMsg:
		m.loading = false
		m.pipelines = []tuiPipeline(msg)
		rows := make([]table.Row, len(m.pipelines))
		for i, p := range m.pipelines {
			active := "●"
			if !p.Active {
				active = "○"
			}
			rows[i] = table.Row{
				p.Name,
				p.Description,
				active,
				p.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.pTable.SetRows(rows)
		return m, nil

	case tuiRunsMsg:
		m.loading = false
		m.runs = []tuiRun(msg)
		rows := make([]table.Row, len(m.runs))
		for i, r := range m.runs {
			rows[i] = table.Row{
				tuiShortID(r.RunID),
				r.Status,
				r.TriggeredBy,
				tuiFormatTime(r.StartedAt),
				tuiFormatDur(r.StartedAt, r.EndedAt),
			}
		}
		m.rTable.SetRows(rows)
		return m, nil

	case tuiRunDetailMsg:
		m.loading = false
		rf := tuiRunFull(msg)
		m.runFull = &rf
		rows := make([]table.Row, len(rf.StepRuns))
		for i, sr := range rf.StepRuns {
			rows[i] = table.Row{
				fmt.Sprintf("%d", sr.StepIndex+1),
				sr.StepName,
				sr.Status,
				tuiFormatTime(sr.StartedAt),
				tuiFormatDur(sr.StartedAt, sr.EndedAt),
			}
		}
		m.dTable.SetRows(rows)
		if rf.Status == "running" || rf.Status == "pending" {
			return m, tuiTickCmd()
		}
		return m, nil

	case tuiTickMsg:
		if m.view == tuiViewRunDetail && m.selRun != nil {
			return m, tuiFetchRunDetail(m.selRun.RunID)
		}
		return m, nil

	case tuiPipelineCreatedMsg:
		m.view = tuiViewPipelines
		m.loading = true
		return m, tuiFetchPipelines

	case tuiFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "q":
				return m, func() tea.Msg { return goHomeMsg{} }
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, tuiFetchPipelines
			}
			return m, nil
		}
		switch m.view {
		case tuiViewPipelines:
			return m.tuiKeyPipelines(msg)
		case tuiViewRuns:
			return m.tuiKeyRuns(msg)
		case tuiViewRunDetail:
			return m.tuiKeyRunDetail(msg)
		case tuiViewOutput:
			return m.tuiKeyOutput(msg)
		case tuiViewCreate:
			return m.tuiKeyCreate(msg)
		}
	}

	return m.tuiDelegate(msg)
}

func (m tuiModel) tuiDelegate(msg tea.Msg) (tuiModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case tuiViewPipelines:
		m.pTable, cmd = m.pTable.Update(msg)
	case tuiViewRuns:
		m.rTable, cmd = m.rTable.Update(msg)
	case tuiViewRunDetail:
		m.dTable, cmd = m.dTable.Update(msg)
	case tuiViewOutput:
		m.vp, cmd = m.vp.Update(msg)
	case tuiViewCreate:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m tuiModel) tuiKeyPipelines(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i := m.pTable.Cursor()
		if i < len(m.pipelines) {
			m.selPipeline = &m.pipelines[i]
			m.view = tuiViewRuns
			m.loading = true
			return m, tuiFetchRuns(m.selPipeline.WorkflowID)
		}
	case "n":
		var cmd tea.Cmd
		m.form, cmd = newCIPipelineForm()
		m.view = tuiViewCreate
		return m, cmd
	case "r":
		m.loading = true
		return m, tuiFetchPipelines
	}
	var cmd tea.Cmd
	m.pTable, cmd = m.pTable.Update(msg)
	return m, cmd
}

// ── Create form ───────────────────────────────────────────────────────────────

func newCIPipelineForm() (tuiForm, tea.Cmd) {
	return newTUIForm("New Pipeline",
		formInput("name", "Name", "my-pipeline (required)"),
		formInput("steps", "Steps", "build->test->deploy (required)"),
		formInput("desc", "Desc", "description (optional)"),
	)
}

func (m tuiModel) tuiKeyCreate(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
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
		m.view = tuiViewPipelines
		return m, nil
	case formSubmit:
		return m.tuiSubmitCreate()
	}
	return m, cmd
}

// tuiSubmitCreate validates the form and, if valid, returns the create cmd.
func (m tuiModel) tuiSubmitCreate() (tuiModel, tea.Cmd) {
	name := m.form.value("name")
	dsl := m.form.value("steps")
	if name == "" {
		m.form.errMsg = "name is required"
		return m, nil
	}
	if dsl == "" {
		m.form.errMsg = "steps are required (e.g. build->test->deploy)"
		return m, nil
	}
	m.form.errMsg = ""
	return m, ciSubmitCreatePipeline(name, m.form.value("desc"), dsl)
}

// ciSubmitCreatePipeline resolves the DSL step names to IDs and POSTs the new
// pipeline. Name/DSL resolution errors surface as an inline form error.
func ciSubmitCreatePipeline(name, desc, dsl string) tea.Cmd {
	return func() tea.Msg {
		nodes, err := parseDSL(dsl)
		if err != nil {
			return tuiFormErrMsg{err}
		}
		refs, err := dslToRefs(nodes)
		if err != nil {
			return tuiFormErrMsg{err}
		}
		payload := map[string]any{"name": name, "steps": refs}
		if desc != "" {
			payload["description"] = desc
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/workflows/pipelines", body); err != nil {
			return tuiFormErrMsg{err}
		}
		return tuiPipelineCreatedMsg{}
	}
}

func (m tuiModel) tuiKeyRuns(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = tuiViewPipelines
		return m, nil
	case "enter":
		i := m.rTable.Cursor()
		if i < len(m.runs) {
			m.selRun = &m.runs[i]
			m.view = tuiViewRunDetail
			m.loading = true
			return m, tuiFetchRunDetail(m.selRun.RunID)
		}
	case "c":
		i := m.rTable.Cursor()
		if i < len(m.runs) {
			r := m.runs[i]
			if r.Status == "pending" || r.Status == "running" {
				_, _ = doRequest("DELETE", "/workflows/runs/"+r.RunID, nil)
				m.loading = true
				wid := ""
				if m.selPipeline != nil {
					wid = m.selPipeline.WorkflowID
				}
				return m, tuiFetchRuns(wid)
			}
		}
	case "r":
		m.loading = true
		wid := ""
		if m.selPipeline != nil {
			wid = m.selPipeline.WorkflowID
		}
		return m, tuiFetchRuns(wid)
	}
	var cmd tea.Cmd
	m.rTable, cmd = m.rTable.Update(msg)
	return m, cmd
}

func (m tuiModel) tuiKeyRunDetail(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = tuiViewRuns
		return m, nil
	case "enter":
		if m.runFull == nil {
			return m, nil
		}
		i := m.dTable.Cursor()
		if i < len(m.runFull.StepRuns) {
			sr := m.runFull.StepRuns[i]
			output := "(no output)"
			if sr.Output != nil && *sr.Output != "" {
				output = *sr.Output
			}
			m.outputTitle = fmt.Sprintf("Step %d: %s  [%s]", sr.StepIndex+1, sr.StepName, sr.Status)
			m.vp.SetContent(output)
			m.vp.GotoTop()
			m.view = tuiViewOutput
		}
	case "r":
		if m.selRun != nil {
			m.loading = true
			return m, tuiFetchRunDetail(m.selRun.RunID)
		}
	}
	var cmd tea.Cmd
	m.dTable, cmd = m.dTable.Update(msg)
	return m, cmd
}

func (m tuiModel) tuiKeyOutput(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = tuiViewRunDetail
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m tuiModel) View() string {
	var content string
	if m.err != nil {
		msg := "error: " + m.err.Error()
		hint := "[q] quit"
		if strings.Contains(m.err.Error(), "401") || strings.Contains(m.err.Error(), "unauthorized") {
			hint += "  [r] retry after login\n\n  Not authenticated — run `armory auth login` first"
		}
		content = tuiErrStyle.Render(msg) + "\n\n" + tuiHelpStyle.Render(hint)
	} else {
		switch m.view {
		case tuiViewPipelines:
			content = m.tuiViewPipelines()
		case tuiViewRuns:
			content = m.tuiViewRuns()
		case tuiViewRunDetail:
			content = m.tuiViewRunDetail()
		case tuiViewOutput:
			content = m.tuiViewOutput()
		case tuiViewCreate:
			content = m.form.view(m.width, m.height)
		}
	}
	return content
}

func (m tuiModel) tuiViewPipelines() string {
	title := tuiTitleStyle.Render("Pipelines")
	help := tuiHelp("[↑↓/jk] navigate  [enter] runs  [n] new  [r] refresh  [q] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.pipelines) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No pipelines found.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.pTable.View()) + "\n" + help
}

func (m tuiModel) tuiViewRuns() string {
	name := ""
	if m.selPipeline != nil {
		name = ": " + m.selPipeline.Name
	}
	title := tuiTitleStyle.Render("Runs" + name)
	help := tuiHelp("[↑↓/jk] navigate  [enter] detail  [c] cancel  [r] refresh  [b] back  [q] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.runs) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No runs found.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + help
}

func (m tuiModel) tuiViewRunDetail() string {
	title := tuiTitleStyle.Render("Run Detail")
	help := tuiHelp("[↑↓/jk] navigate  [enter] output  [r] refresh  [b] back  [q] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.runFull == nil {
		return title + "\n\n" + tuiMetaStyle.Render("No data.") + "\n\n" + help
	}
	d := m.runFull
	live := ""
	if d.Status == "running" || d.Status == "pending" {
		live = "  " + tuiMetaStyle.Render("(auto-refreshing)")
	}
	meta := fmt.Sprintf("run %s  status: %s  triggered: %s",
		tuiShortID(d.RunID),
		tuiColorStatus(d.Status),
		tuiTrunc(d.TriggeredBy, 20),
	)
	if d.StartedAt != nil {
		meta += "  started: " + d.StartedAt.Local().Format("Jan 02 15:04:05")
	}
	return title + live + "\n" + tuiMetaStyle.Render(meta) + "\n" +
		tuiBoxStyle.Render(m.dTable.View()) + "\n" + help
}

func (m tuiModel) tuiViewOutput() string {
	title := tuiTitleStyle.Render(m.outputTitle)
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [b] back  [q] home", m.width)
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func tuiShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func tuiTrunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func tuiFormatTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("Jan 02 15:04:05")
}

func tuiFormatDur(start, end *time.Time) string {
	if start == nil {
		return "—"
	}
	e := time.Now()
	if end != nil {
		e = *end
	}
	d := e.Sub(*start).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ── Command ───────────────────────────────────────────────────────────────────

var ciTUICmd = &cobra.Command{
	Use:   "tui",
	Short: "Interactive TUI for browsing pipelines and runs",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return startCITUI() },
}

func startCITUI() error {
	p := tea.NewProgram(standaloneWrap{newTUIModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	RegisterModule(Module{
		Name:    "workflows",
		Order:   10,
		Command: ciCmd,
		Screens: []HubScreen{
			{Title: "CI / Pipelines", Desc: "Browse workflow pipelines and run history", New: func() tea.Model { return newTUIModel() }},
			{Title: "Steps", Desc: "Browse and create reusable pipeline steps", New: func() tea.Model { return newStepsModel() }},
		},
	})
}
