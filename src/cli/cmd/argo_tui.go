package cmd

import (
	"encoding/json"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The Argo TUI lists Argo CD applications reported by the cluster outpost and
// triggers a sync. Apps are not created/deleted here (they originate in the
// cluster); the actions are view-detail and sync. Mirrors the argo service API.

type argoApp struct {
	Name           string `json:"name"`
	SyncStatus     string `json:"sync_status"`
	HealthStatus   string `json:"health_status"`
	Revision       string `json:"revision"`
	OperationPhase string `json:"operation_phase"`
}

type argoListMsg []argoApp
type argoDetailMsg struct{ content string }
type argoErrMsg struct{ err error }
type argoStatusMsg struct {
	status string
	isErr  bool
}

type argoViewID int

const (
	argoViewList argoViewID = iota
	argoViewDetail
)

type argoModel struct {
	view    argoViewID
	loading bool
	err     error
	width   int
	height  int

	apps  []argoApp
	table table.Model
	vp    viewport.Model

	status    string
	statusErr bool
}

var argoCols = []tuiColSpec{{"NAME", 22, 2}, {"SYNC", 12, 1}, {"HEALTH", 12, 1}, {"REVISION", 12, 0}, {"PHASE", 12, 1}}

func newArgoModel() argoModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := argoModel{loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight, table: t, vp: viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8)}
	m.applyTableLayout()
	return m
}

func (m *argoModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(argoCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func argoFetch() tea.Msg {
	data, err := doRequest("GET", "/argo/apps", nil)
	if err != nil {
		return argoErrMsg{err}
	}
	var apps []argoApp
	if err := json.Unmarshal(data, &apps); err != nil {
		return argoErrMsg{err}
	}
	return argoListMsg(apps)
}

func argoFetchDetail(name string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/argo/apps/"+name, nil)
		if err != nil {
			return argoErrMsg{err}
		}
		var pretty any
		if json.Unmarshal(data, &pretty) == nil {
			if b, e := json.MarshalIndent(pretty, "", "  "); e == nil {
				return argoDetailMsg{content: string(b)}
			}
		}
		return argoDetailMsg{content: string(data)}
	}
}

// argoSync triggers a sync to latest for an app. outpost_id/revision are left to
// the server (it uses the app's reported outpost and HEAD).
func argoSync(name string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("POST", "/argo/apps/"+name+"/sync", []byte("{}")); err != nil {
			return argoStatusMsg{status: "✗ sync " + name + ": " + err.Error(), isErr: true}
		}
		return argoStatusMsg{status: "✓ sync started for " + name}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m argoModel) Init() tea.Cmd { return argoFetch }

func (m argoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil
	case argoErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case argoListMsg:
		m.loading = false
		m.apps = []argoApp(msg)
		rows := make([]table.Row, len(m.apps))
		for i, a := range m.apps {
			rows[i] = table.Row{a.Name, frDash(a.SyncStatus), frDash(a.HealthStatus), frDash(tuiShortID(a.Revision)), frDash(a.OperationPhase)}
		}
		m.table.SetRows(rows)
		return m, nil
	case argoDetailMsg:
		m.loading = false
		m.vp.SetContent(msg.content)
		m.vp.GotoTop()
		m.view = argoViewDetail
		return m, nil
	case argoStatusMsg:
		m.status = msg.status
		m.statusErr = msg.isErr
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == argoViewList {
			return m, argoFetch
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
				return m, argoFetch
			}
			return m, nil
		}
		switch m.view {
		case argoViewList:
			return m.keyList(msg)
		case argoViewDetail:
			return m.keyDetail(msg)
		}
	}
	var cmd tea.Cmd
	switch m.view {
	case argoViewList:
		m.table, cmd = m.table.Update(msg)
	case argoViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m argoModel) keyList(msg tea.KeyMsg) (argoModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if i := m.table.Cursor(); i >= 0 && i < len(m.apps) {
			m.loading = true
			return m, argoFetchDetail(m.apps[i].Name)
		}
	case "s":
		if i := m.table.Cursor(); i >= 0 && i < len(m.apps) {
			a := m.apps[i]
			m.status = "syncing " + a.Name + "…"
			m.statusErr = false
			return m, argoSync(a.Name)
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, argoFetch
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m argoModel) keyDetail(msg tea.KeyMsg) (argoModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = argoViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m argoModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	if m.view == argoViewDetail {
		return tuiTitleStyle.Render("Argo App") + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	}
	title := tuiTitleStyle.Render("Argo Applications")
	help := tuiHelp("[↑↓/jk] nav  [enter] detail  [s] sync  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	status := ""
	if m.status != "" {
		if m.statusErr {
			status = tuiErrStyle.Render(m.status) + "\n"
		} else {
			status = tuiMetaStyle.Render(m.status) + "\n"
		}
	}
	if len(m.apps) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No applications reported. Ensure an outpost with the argo module is enrolled.") + "\n\n" + status + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + status + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startArgoTUI() error {
	p := tea.NewProgram(standaloneWrap{newArgoModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

var argoCmd = &cobra.Command{
	Use:   "argo",
	Short: "View and sync Argo CD applications (TUI)",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return startArgoTUI() },
}

func init() {
	argoCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for Argo CD applications",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startArgoTUI() },
	})
	RegisterModule(Module{
		Name:    "argo",
		Service: "argo",
		Order:   46,
		Command: argoCmd,
		Screens: []HubScreen{{
			Title: "Argo",
			Desc:  "Argo CD applications and sync status",
			New:   func() tea.Model { return newArgoModel() },
		}},
	})
}
