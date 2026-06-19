package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type forgeExec struct {
	ExecutionID string     `json:"execution_id"`
	Image       string     `json:"image"`
	RunnerClass string     `json:"runner_class"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Stdout      *string    `json:"stdout,omitempty"`
	Stderr      *string    `json:"stderr,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	// Command, Env, and Timeout are carried so the list/output views can rerun
	// an execution without a second fetch — both the list and detail endpoints
	// return them. They are not displayed in the table.
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int64             `json:"timeout,omitempty"`
}

// ── Messages ──────────────────────────────────────────────────────────────────

type forgeExecsMsg []forgeExec
type forgeExecDetailMsg forgeExec
type forgeErrMsg struct{ err error }
type forgeCancelledMsg struct{}
type forgeCreatedMsg struct{}
type forgeFormErrMsg struct{ err error }
type forgeImagesMsg []string
type forgeRunnersMsg []string

// ── Views ─────────────────────────────────────────────────────────────────────

type forgeViewID int

const (
	forgeViewList forgeViewID = iota
	forgeViewOutput
	forgeViewCreate
)

// ── Model ─────────────────────────────────────────────────────────────────────

type forgeModel struct {
	view    forgeViewID
	loading bool
	err     error
	width   int
	height  int

	execs   []forgeExec
	selExec *forgeExec
	detail  *forgeExec

	// option lists for the create form's ←/→ selectors, fetched on startup.
	images  []string
	runners []string

	eTable table.Model
	vp     viewport.Model
	form   tuiForm
}

var forgeExecCols = []tuiColSpec{
	{"ID", 10, 0},
	{"STATUS", 12, 0},
	{"IMAGE", 20, 2},
	{"CLASS", 10, 0},
	{"STARTED", 16, 0},
	{"DURATION", 9, 0},
}

func newForgeModel() forgeModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := forgeModel{
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
		eTable:  t,
		vp:      viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8),
	}
	m.applyTableLayout()
	return m
}

// applyTableLayout resizes the executions table to the current terminal.
func (m *forgeModel) applyTableLayout() {
	m.eTable.SetColumns(tuiFitColumns(forgeExecCols, m.width))
	m.eTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func forgeFetchExecs() tea.Msg {
	data, err := doRequest("GET", "/forge/executions", nil)
	if err != nil {
		return forgeErrMsg{err}
	}
	var execs []forgeExec
	if err := json.Unmarshal(data, &execs); err != nil {
		return forgeErrMsg{err}
	}
	return forgeExecsMsg(execs)
}

func forgeFetchDetail(id string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/forge/executions/"+id, nil)
		if err != nil {
			return forgeErrMsg{err}
		}
		var e forgeExec
		if err := json.Unmarshal(data, &e); err != nil {
			return forgeErrMsg{err}
		}
		return forgeExecDetailMsg(e)
	}
}

// forgeActive reports whether an execution is still in progress and thus worth
// polling for fresh status and output.
func forgeActive(status string) bool {
	return status == "pending" || status == "running"
}

// forgeFetchImages loads the image allowlist for the create form's selector.
// Failures (e.g. an older server without /images) degrade to an empty list so
// the form falls back to a free-text image field rather than erroring.
func forgeFetchImages() tea.Msg {
	data, err := doRequest("GET", "/forge/images", nil)
	if err != nil {
		return forgeImagesMsg(nil)
	}
	var imgs []string
	if err := json.Unmarshal(data, &imgs); err != nil {
		return forgeImagesMsg(nil)
	}
	return forgeImagesMsg(imgs)
}

// forgeFetchRunners loads the names of enabled runner classes for the create
// form's selector, degrading to an empty list on failure.
func forgeFetchRunners() tea.Msg {
	data, err := doRequest("GET", "/forge/runner-classes", nil)
	if err != nil {
		return forgeRunnersMsg(nil)
	}
	var rcs []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(data, &rcs); err != nil {
		return forgeRunnersMsg(nil)
	}
	var names []string
	for _, rc := range rcs {
		if rc.Enabled {
			names = append(names, rc.Name)
		}
	}
	return forgeRunnersMsg(names)
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m forgeModel) Init() tea.Cmd {
	return tea.Batch(forgeFetchExecs, forgeFetchImages, forgeFetchRunners)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m forgeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil

	case forgeErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case forgeExecsMsg:
		m.loading = false
		m.execs = []forgeExec(msg)
		rows := make([]table.Row, len(m.execs))
		for i, e := range m.execs {
			rows[i] = table.Row{
				tuiShortID(e.ExecutionID),
				e.Status,
				e.Image,
				e.RunnerClass,
				tuiFormatTime(e.StartedAt),
				tuiFormatDur(e.StartedAt, e.EndedAt),
			}
		}
		m.eTable.SetRows(rows)
		return m, nil

	case forgeExecDetailMsg:
		// wasLoading distinguishes an explicit open / manual refresh (which
		// jumps to the top) from a silent 5s auto-refresh (which preserves the
		// reader's scroll position, or follows the tail if already at bottom).
		wasLoading := m.loading
		m.loading = false
		d := forgeExec(msg)
		m.detail = &d
		atBottom := m.vp.AtBottom()
		m.vp.SetContent(forgeRenderOutput(d)) // preserves YOffset (clamped)
		switch {
		case wasLoading:
			m.vp.GotoTop()
		case atBottom:
			m.vp.GotoBottom()
		}
		return m, nil

	case forgeCancelledMsg:
		m.loading = true
		return m, forgeFetchExecs

	case forgeCreatedMsg:
		m.view = forgeViewList
		m.loading = true
		return m, forgeFetchExecs

	case forgeFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case forgeImagesMsg:
		m.images = []string(msg)
		return m, nil

	case forgeRunnersMsg:
		m.runners = []string(msg)
		return m, nil

	case tuiAutoRefreshMsg:
		// Silent re-fetch (no loading flash) so the page updates in place: the
		// list keeps its cursor, and the output view keeps its scroll position
		// (forgeExecDetailMsg preserves it when not loading).
		switch m.view {
		case forgeViewList:
			return m, forgeFetchExecs
		case forgeViewOutput:
			if m.selExec != nil {
				return m, forgeFetchDetail(m.selExec.ExecutionID)
			}
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
				return m, forgeFetchExecs
			}
			return m, nil
		}
		switch m.view {
		case forgeViewList:
			return m.forgeKeyList(msg)
		case forgeViewOutput:
			return m.forgeKeyOutput(msg)
		case forgeViewCreate:
			return m.forgeKeyCreate(msg)
		}
	}
	return m.forgeDelegate(msg)
}

func (m forgeModel) forgeDelegate(msg tea.Msg) (forgeModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case forgeViewList:
		m.eTable, cmd = m.eTable.Update(msg)
	case forgeViewOutput:
		m.vp, cmd = m.vp.Update(msg)
	case forgeViewCreate:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m forgeModel) forgeKeyList(msg tea.KeyMsg) (forgeModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.execs) {
			m.selExec = &m.execs[i]
			m.view = forgeViewOutput
			m.loading = true
			return m, forgeFetchDetail(m.selExec.ExecutionID)
		}
	case "n":
		var cmd tea.Cmd
		m.form, cmd = newForgeCreateForm(m.images, m.runners)
		m.view = forgeViewCreate
		return m, cmd
	case "x":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.execs) {
			e := m.execs[i]
			if forgeActive(e.Status) {
				return m, func() tea.Msg {
					_, _ = doRequest("DELETE", "/forge/executions/"+e.ExecutionID, nil)
					return forgeCancelledMsg{}
				}
			}
		}
	case "R":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.execs) && len(m.execs[i].Command) > 0 {
			return m, forgeRerunExec(m.execs[i])
		}
	case "r":
		m.loading = true
		return m, forgeFetchExecs
	}
	var cmd tea.Cmd
	m.eTable, cmd = m.eTable.Update(msg)
	return m, cmd
}

func (m forgeModel) forgeKeyOutput(msg tea.KeyMsg) (forgeModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = forgeViewList
		m.detail = nil
		return m, nil
	case "R":
		// Prefer the freshly-fetched detail; fall back to the list row. Both
		// carry the command needed to rerun.
		src := m.detail
		if src == nil {
			src = m.selExec
		}
		if src != nil && len(src.Command) > 0 {
			return m, forgeRerunExec(*src)
		}
	case "r":
		if m.selExec != nil {
			m.loading = true
			return m, forgeFetchDetail(m.selExec.ExecutionID)
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── Create form ───────────────────────────────────────────────────────────────

// newForgeCreateForm builds the execution form. When the server advertises an
// image allowlist or runner classes, those fields become ←/→ selectors;
// otherwise they fall back to free-text inputs.
func newForgeCreateForm(images, runners []string) (tuiForm, tea.Cmd) {
	var imageField formField
	if len(images) > 0 {
		imageField = formSelect("image", "Image", images)
	} else {
		imageField = formInput("image", "Image", "ubuntu:22.04 (required)")
	}
	var runnerField formField
	if len(runners) > 0 {
		// Leading "" renders as "(default)" and submits no runner_class.
		runnerField = formSelect("runner", "Runner", append([]string{""}, runners...))
	} else {
		runnerField = formInput("runner", "Runner", "runner class (optional)")
	}
	return newTUIForm("New Execution",
		imageField,
		formTextarea("command", "Command", "sh -c \"echo hi\"\nmultiple lines run as a script"),
		formInput("env", "Env", "KEY=VALUE KEY2=VALUE2 (optional)"),
		formInput("timeout", "Timeout", "seconds (optional)"),
		runnerField,
	)
}

func (m forgeModel) forgeKeyCreate(msg tea.KeyMsg) (forgeModel, tea.Cmd) {
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
		m.view = forgeViewList
		return m, nil
	case formSubmit:
		return m.forgeSubmitCreate()
	}
	return m, cmd
}

// forgeSubmitCreate validates the form and, if valid, returns the POST cmd.
// Validation failures stay in the form with an inline error message.
func (m forgeModel) forgeSubmitCreate() (forgeModel, tea.Cmd) {
	image := m.form.value("image")
	if image == "" {
		m.form.errMsg = "image is required"
		return m, nil
	}
	command, err := buildForgeCommand(m.form.value("command"))
	if err != nil {
		m.form.errMsg = err.Error()
		return m, nil
	}
	if len(command) == 0 {
		m.form.errMsg = "command is required"
		return m, nil
	}
	env, err := parseEnvAssignments(m.form.value("env"))
	if err != nil {
		m.form.errMsg = err.Error()
		return m, nil
	}
	var timeout int64
	if ts := m.form.value("timeout"); ts != "" {
		t, perr := strconv.ParseInt(ts, 10, 64)
		if perr != nil || t < 0 {
			m.form.errMsg = "timeout must be a non-negative integer (seconds)"
			return m, nil
		}
		timeout = t
	}
	m.form.errMsg = ""
	return m, forgeSubmitExec(image, command, env, timeout, m.form.value("runner"))
}

func forgeSubmitExec(image string, command []string, env map[string]string, timeout int64, runner string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{"image": image, "command": command}
		if len(env) > 0 {
			payload["env"] = env
		}
		if timeout > 0 {
			payload["timeout"] = timeout
		}
		if runner != "" {
			payload["runner_class"] = runner
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/forge/executions", body); err != nil {
			return forgeFormErrMsg{err}
		}
		return forgeCreatedMsg{}
	}
}

// forgeRerunExec resubmits an execution with the same image, command, env,
// timeout, and runner class, creating a brand-new run. On success the list
// refreshes (via forgeCreatedMsg) so the fresh job appears; a failure surfaces
// as an error rather than being swallowed, so the user is never misled into
// thinking the rerun queued when it didn't.
func forgeRerunExec(e forgeExec) tea.Cmd {
	return func() tea.Msg {
		payload := forgeRerunPayload(e.Image, e.Command, e.Env, e.Timeout, e.RunnerClass)
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/forge/executions", body); err != nil {
			return forgeErrMsg{err}
		}
		return forgeCreatedMsg{}
	}
}

// buildForgeCommand turns the command field into the argv forge executes. Forge
// runs argv directly with no shell, so a multi-line entry — which the user means
// as a script, one command per line — can't be sent as bare tokens (they'd become
// arguments to the first word). A multi-line entry is therefore wrapped as
// `sh -c <script>` so every line runs; a single line is tokenised into argv as
// before, preserving raw-argv use (explicit `sh -c "…"`, or images without a shell).
func buildForgeCommand(raw string) ([]string, error) {
	if strings.Contains(raw, "\n") {
		return []string{"sh", "-c", raw}, nil
	}
	return parseCommandLine(raw)
}

// parseCommandLine splits a single command line into argv, honouring single and
// double quotes so `sh -c "echo hi"` yields ["sh","-c","echo hi"]. A backslash
// escapes the next character outside single quotes.
func parseCommandLine(s string) ([]string, error) {
	var (
		args  []string
		cur   strings.Builder
		inArg bool
		quote byte // 0, '\'' or '"'
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			switch {
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"' && i+1 < len(s):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
			inArg = true
		case c == '\'' || c == '"':
			quote = c
			inArg = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inArg = true
		case c == ' ' || c == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in command", quote)
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// parseEnvAssignments parses space-separated KEY=VALUE pairs (quotes honoured)
// into a map. Empty input yields a nil map.
func parseEnvAssignments(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	tokens, err := parseCommandLine(s)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, tok := range tokens {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid env %q: expected KEY=VALUE", tok)
		}
		env[k] = v
	}
	return env, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m forgeModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case forgeViewCreate:
		return m.form.view(m.width, m.height)
	case forgeViewOutput:
		return m.forgeViewOutput()
	}
	return m.forgeViewList()
}

func (m forgeModel) forgeViewList() string {
	title := tuiTitleStyle.Render("Forge Executions")
	help := tuiHelp("[↑↓/jk] navigate  [enter] output  [n] new  [R] rerun  [x] cancel  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.execs) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No executions found.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.eTable.View()) + "\n" + help
}

func (m forgeModel) forgeViewOutput() string {
	title := tuiTitleStyle.Render("Execution Output")
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [R] rerun  [r] refresh  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.detail == nil {
		return title + "\n\n" + tuiMetaStyle.Render("No data.") + "\n\n" + help
	}
	d := m.detail
	exitStr := ""
	if d.ExitCode != nil {
		exitStr = fmt.Sprintf("  exit: %d", *d.ExitCode)
	}
	meta := tuiMetaStyle.Render(tuiTrunc(d.Image, 36) + "  " + tuiColorStatus(d.Status) + exitStr)
	return tuiTitleStyle.Render(tuiShortID(d.ExecutionID)) + "  " + meta + "\n" +
		tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

func forgeRenderOutput(e forgeExec) string {
	var sb strings.Builder
	stdout := "(no stdout)"
	if e.Stdout != nil && *e.Stdout != "" {
		stdout = *e.Stdout
	}
	sb.WriteString("STDOUT:\n")
	sb.WriteString(stdout)
	if e.Stderr != nil && *e.Stderr != "" {
		sb.WriteString("\n\nSTDERR:\n")
		sb.WriteString(*e.Stderr)
	}
	return sb.String()
}

// ── Command registration ──────────────────────────────────────────────────────

func startForgeTUI() error {
	p := tea.NewProgram(standaloneWrap{newForgeModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	forgeCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return startForgeTUI()
	}
	forgeCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for browsing forge executions",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startForgeTUI() },
	})
	RegisterModule(Module{
		Name:    "forge",
		Order:   30,
		Command: forgeCmd,
		Screens: []HubScreen{{
			Title: "Forge",
			Desc:  "Browse sandboxed executions and their output",
			New:   func() tea.Model { return newForgeModel() },
		}},
	})
}
