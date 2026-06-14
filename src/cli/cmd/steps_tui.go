package cmd

import (
	"encoding/json"
	"fmt"
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

// tuiStep mirrors a reusable step definition from /workflows/steps.
type tuiStep struct {
	StepID      string    `json:"step_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Action      string    `json:"action"`
	Timeout     int64     `json:"timeout"`
	CreatedAt   time.Time `json:"created_at"`
}

// ── Messages ──────────────────────────────────────────────────────────────────

type tuiStepsMsg []tuiStep
type tuiActionsMsg []string
type tuiImagesMsg []string
type tuiStepCreatedMsg struct{}

// ── View states ───────────────────────────────────────────────────────────────

type stepsViewID int

const (
	stepsViewList stepsViewID = iota
	stepsViewCreate
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

	form tuiForm
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

// Init loads steps and batches the action catalog and forge image allowlist so
// the create-step form's action and image selectors are ready the moment it opens.
func (m stepsModel) Init() tea.Cmd {
	return tea.Batch(tuiFetchSteps, tuiFetchActions, tuiFetchImages)
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
		return m, nil

	case tuiImagesMsg:
		m.images = []string(msg)
		return m, nil

	case tuiStepCreatedMsg:
		m.view = stepsViewList
		m.loading = true
		return m, tuiFetchSteps

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
				return m, tea.Batch(tuiFetchSteps, tuiFetchActions, tuiFetchImages)
			}
			return m, nil
		}
		switch m.view {
		case stepsViewList:
			return m.keyList(msg)
		case stepsViewCreate:
			return m.keyCreate(msg)
		}
	}

	return m.delegate(msg)
}

func (m stepsModel) delegate(msg tea.Msg) (stepsModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case stepsViewList:
		m.sTable, cmd = m.sTable.Update(msg)
	case stepsViewCreate:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m stepsModel) keyList(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "n":
		var cmd tea.Cmd
		m.form, cmd = newCIStepForm(m.actions, m.images)
		m.view = stepsViewCreate
		return m, cmd
	case "r":
		m.loading = true
		return m, tea.Batch(tuiFetchSteps, tuiFetchActions)
	}
	var cmd tea.Cmd
	m.sTable, cmd = m.sTable.Update(msg)
	return m, cmd
}

// ── Create step form ──────────────────────────────────────────────────────────

// newCIStepForm builds the step form. When the action catalog is available the
// Action field becomes a ←/→ selector (defaulting to forge/run); likewise the
// Image field becomes a selector when the forge image allowlist is available.
// Either falls back to a free-text input when its option list is empty.
func newCIStepForm(actions, images []string) (tuiForm, tea.Cmd) {
	var actionField formField
	if len(actions) > 0 {
		actionField = formSelectDefault("action", "Action", actions, "forge/run")
	} else {
		actionField = formInputDefault("action", "Action", "forge/run (required)", "forge/run")
	}
	var imageField formField
	if len(images) > 0 {
		// Image is only required for forge/run; the leading "" lets other
		// actions leave it unset (renders as "(default)").
		imageField = formSelect("image", "Image", append([]string{""}, images...))
	} else {
		imageField = formInput("image", "Image", "ubuntu:22.04 (forge/run)")
	}
	return newTUIForm("New Step",
		formInput("name", "Name", "unit_tests (required)"),
		actionField,
		imageField,
		formInput("run", "Run", "go test ./... (forge/run)"),
		formInput("env", "Env", "KEY=VALUE (forge/run, optional)"),
		formInput("with", "With", `{"k":"v"} JSON (other actions)`),
		formInput("timeout", "Timeout", "seconds (default 30)"),
		formInput("desc", "Desc", "description (optional)"),
	)
}

func (m stepsModel) keyCreate(msg tea.KeyMsg) (stepsModel, tea.Cmd) {
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
		return m.submitCreate()
	}
	return m, cmd
}

// submitCreate validates the step form and, if valid, returns the create cmd.
// The `with` config is assembled by buildWith (shared with the CLI), which
// enforces forge/run's image+run requirement and the --with JSON requirement
// for other actions.
func (m stepsModel) submitCreate() (stepsModel, tea.Cmd) {
	name := m.form.value("name")
	action := m.form.value("action")
	if name == "" {
		m.form.errMsg = "name is required"
		return m, nil
	}
	if action == "" {
		m.form.errMsg = "action is required (e.g. forge/run)"
		return m, nil
	}
	timeout := int64(30)
	if ts := m.form.value("timeout"); ts != "" {
		t, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || t < 0 {
			m.form.errMsg = "timeout must be a non-negative integer (seconds)"
			return m, nil
		}
		timeout = t
	}
	m.form.errMsg = ""
	return m, ciSubmitCreateStep(name, action, m.form.value("image"),
		m.form.value("run"), m.form.value("with"), m.form.value("env"),
		m.form.value("desc"), timeout)
}

func ciSubmitCreateStep(name, action, image, run, withJSON, envStr, desc string, timeout int64) tea.Cmd {
	return func() tea.Msg {
		var envs []string
		if strings.TrimSpace(envStr) != "" {
			toks, err := parseCommandLine(envStr)
			if err != nil {
				return tuiFormErrMsg{err}
			}
			envs = toks
		}
		with, err := buildWith(action, withJSON, image, run, envs)
		if err != nil {
			return tuiFormErrMsg{err}
		}
		payload := map[string]any{
			"name":        name,
			"description": desc,
			"action":      action,
			"with":        with,
			"timeout":     timeout,
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/workflows/steps", body); err != nil {
			return tuiFormErrMsg{err}
		}
		return tuiStepCreatedMsg{}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m stepsModel) View() string {
	if m.err != nil {
		msg := "error: " + m.err.Error()
		hint := "[q] quit"
		if strings.Contains(m.err.Error(), "401") || strings.Contains(m.err.Error(), "unauthorized") {
			hint += "  [r] retry after login\n\n  Not authenticated — run `armory auth login` first"
		}
		return tuiErrStyle.Render(msg) + "\n\n" + tuiHelpStyle.Render(hint)
	}
	if m.view == stepsViewCreate {
		return m.form.view(m.width, m.height)
	}
	return m.viewList()
}

func (m stepsModel) viewList() string {
	title := tuiTitleStyle.Render("Steps")
	help := tuiHelp("[↑↓/jk] navigate  [n] new  [r] refresh  [q] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.steps) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No steps found.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.sTable.View()) + "\n" + help
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
