package cmd

import (
	"encoding/json"
	"fmt"
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
}

// ── Messages ──────────────────────────────────────────────────────────────────

type forgeExecsMsg      []forgeExec
type forgeExecDetailMsg forgeExec
type forgeErrMsg        struct{ err error }
type forgeCancelledMsg  struct{}
type forgeTickMsg       struct{}

// ── Views ─────────────────────────────────────────────────────────────────────

type forgeViewID int

const (
	forgeViewList forgeViewID = iota
	forgeViewOutput
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

	eTable table.Model
	vp     viewport.Model
}

func newForgeModel() forgeModel {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "ID",       Width: 10},
			{Title: "STATUS",   Width: 12},
			{Title: "IMAGE",    Width: 26},
			{Title: "CLASS",    Width: 10},
			{Title: "STARTED",  Width: 16},
			{Title: "DURATION", Width: 9},
		}),
		table.WithFocused(true),
		table.WithHeight(14),
	)
	t.SetStyles(tuiTableStyles())
	return forgeModel{
		loading: true,
		eTable:  t,
		vp:      viewport.New(100, 20),
	}
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

func forgeTickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return forgeTickMsg{} })
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m forgeModel) Init() tea.Cmd { return forgeFetchExecs }

// ── Update ────────────────────────────────────────────────────────────────────

func (m forgeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		return m, nil

	case forgeErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case forgeExecsMsg:
		m.loading = false
		m.execs = []forgeExec(msg)
		rows := make([]table.Row, len(m.execs))
		hasActive := false
		for i, e := range m.execs {
			if e.Status == "pending" || e.Status == "running" {
				hasActive = true
			}
			rows[i] = table.Row{
				tuiShortID(e.ExecutionID),
				e.Status,
				tuiTrunc(e.Image, 26),
				tuiTrunc(e.RunnerClass, 10),
				tuiFormatTime(e.StartedAt),
				tuiFormatDur(e.StartedAt, e.EndedAt),
			}
		}
		m.eTable.SetRows(rows)
		if hasActive {
			return m, forgeTickCmd()
		}
		return m, nil

	case forgeExecDetailMsg:
		m.loading = false
		d := forgeExec(msg)
		m.detail = &d
		m.vp.SetContent(forgeRenderOutput(d))
		m.vp.GotoTop()
		return m, nil

	case forgeCancelledMsg:
		m.loading = true
		return m, forgeFetchExecs

	case forgeTickMsg:
		if m.view == forgeViewList {
			return m, forgeFetchExecs
		}
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
				return m, forgeFetchExecs
			}
			return m, nil
		}
		switch m.view {
		case forgeViewList:
			return m.forgeKeyList(msg)
		case forgeViewOutput:
			return m.forgeKeyOutput(msg)
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
	}
	return m, cmd
}

func (m forgeModel) forgeKeyList(msg tea.KeyMsg) (forgeModel, tea.Cmd) {
	switch msg.String() {
	case "q":
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
	case "x":
		i := m.eTable.Cursor()
		if i >= 0 && i < len(m.execs) {
			e := m.execs[i]
			if e.Status == "pending" || e.Status == "running" {
				return m, func() tea.Msg {
					_, _ = doRequest("DELETE", "/forge/executions/"+e.ExecutionID, nil)
					return forgeCancelledMsg{}
				}
			}
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
	case "q":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = forgeViewList
		m.detail = nil
		return m, nil
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

// ── View ──────────────────────────────────────────────────────────────────────

func (m forgeModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[q] home  [r] retry")
	}
	if m.view == forgeViewOutput {
		return m.forgeViewOutput()
	}
	return m.forgeViewList()
}

func (m forgeModel) forgeViewList() string {
	title := tuiTitleStyle.Render("Forge Executions")
	help := tuiHelpStyle.Render("[↑↓/jk] navigate  [enter] output  [x] cancel  [r] refresh  [q] home")
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
	help := tuiHelpStyle.Render("[↑↓/pgup/pgdn] scroll  [r] refresh  [b] back  [q] home")
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
}
