package cmd

import (
	"encoding/json"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The Outpost TUI manages the customer-cluster agents that back the chaos/argo
// integrations: list, register (which mints a single-use enrollment token shown
// once), and delete. It is an admin control (`armory admin outpost`) — outposts
// are infrastructure, not a per-developer resource.

type outpostRec struct {
	OutpostID  string     `json:"outpost_id"`
	Name       string     `json:"name"`
	Modules    string     `json:"modules"` // csv, e.g. "chaos,argo"
	Status     string     `json:"status"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type opListMsg []outpostRec
type opErrMsg struct{ err error }
type opCreatedMsg struct{ token string }
type opDoneMsg struct{ status string }
type opFormErrMsg struct{ err error }

type opViewID int

const (
	opViewList opViewID = iota
	opViewForm
	opViewToken // one-time enrollment token reveal
)

type outpostModel struct {
	view    opViewID
	loading bool
	err     error
	width   int
	height  int

	outposts []outpostRec
	table    table.Model
	form     tuiForm
	vp       viewport.Model

	pending   *frPending
	status    string
	statusErr bool
}

var outpostCols = []tuiColSpec{{"NAME", 20, 2}, {"MODULES", 14, 1}, {"STATUS", 10, 0}, {"LAST SEEN", 14, 0}, {"CREATED", 14, 0}}

func newOutpostModel() outpostModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := outpostModel{loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight, table: t, vp: viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8)}
	m.applyTableLayout()
	return m
}

func (m *outpostModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(outpostCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// opTime renders an optional timestamp, "—" when never set (e.g. an outpost that
// has not yet connected).
func opTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Local().Format("Jan 02 15:04")
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func opFetch() tea.Msg {
	data, err := doRequest("GET", "/outpost-gateway/outposts", nil)
	if err != nil {
		return opErrMsg{err}
	}
	var ops []outpostRec
	if err := json.Unmarshal(data, &ops); err != nil {
		return opErrMsg{err}
	}
	return opListMsg(ops)
}

func opDelete(id, name string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/outpost-gateway/outposts/"+id, nil); err != nil {
			return opErrMsg{err}
		}
		return opDoneMsg{status: "✓ deleted " + name}
	}
}

func opCreate(name string, modules []string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]any{"name": name, "modules": modules})
		data, err := doRequest("POST", "/outpost-gateway/outposts", body)
		if err != nil {
			return opFormErrMsg{err}
		}
		var resp struct {
			EnrollmentToken string `json:"enrollment_token"`
		}
		_ = json.Unmarshal(data, &resp)
		return opCreatedMsg{token: resp.EnrollmentToken}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m outpostModel) Init() tea.Cmd { return opFetch }

func (m outpostModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil
	case opErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case opListMsg:
		m.loading = false
		m.outposts = []outpostRec(msg)
		rows := make([]table.Row, len(m.outposts))
		for i, o := range m.outposts {
			rows[i] = table.Row{o.Name, frDash(o.Modules), frDash(o.Status), opTime(o.LastSeenAt), o.CreatedAt.Local().Format("Jan 02 15:04")}
		}
		m.table.SetRows(rows)
		return m, nil
	case opCreatedMsg:
		// Reveal the one-time enrollment token; it cannot be retrieved again.
		m.vp.SetContent(opTokenContent(msg.token))
		m.vp.GotoTop()
		m.view = opViewToken
		return m, nil
	case opDoneMsg:
		m.view = opViewList
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, opFetch
	case opFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == opViewList {
			return m, opFetch
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
				return m, opFetch
			}
			return m, nil
		}
		switch m.view {
		case opViewList:
			return m.keyList(msg)
		case opViewForm:
			return m.keyForm(msg)
		case opViewToken:
			return m.keyToken(msg)
		}
	}
	var cmd tea.Cmd
	switch m.view {
	case opViewList:
		m.table, cmd = m.table.Update(msg)
	case opViewForm:
		m.form, _, cmd = m.form.update(msg)
	case opViewToken:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m outpostModel) keyList(msg tea.KeyMsg) (outpostModel, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		if s := msg.String(); s == "y" || s == "Y" {
			return m, run
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if i := m.table.Cursor(); i >= 0 && i < len(m.outposts) {
			pretty, _ := json.MarshalIndent(m.outposts[i], "", "  ")
			m.vp.SetContent(string(pretty))
			m.vp.GotoTop()
			m.view = opViewToken // reuse the scrollable text view for detail
			return m, nil
		}
	case "n":
		m.form, _ = newTUIForm("Register Outpost",
			formInput("name", "Name", "prod-cluster (required)"),
			formInput("modules", "Modules", "chaos,argo (comma-separated)"))
		m.view = opViewForm
		return m, nil
	case "D":
		if i := m.table.Cursor(); i >= 0 && i < len(m.outposts) {
			o := m.outposts[i]
			m.pending = &frPending{prompt: "Delete outpost " + o.Name + "? [y] confirm  [any] cancel", run: opDelete(o.OutpostID, o.Name)}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, opFetch
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m outpostModel) keyForm(msg tea.KeyMsg) (outpostModel, tea.Cmd) {
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
		m.view = opViewList
		return m, nil
	case formSubmit:
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, opCreate(name, splitList(m.form.value("modules")))
	}
	return m, cmd
}

func (m outpostModel) keyToken(msg tea.KeyMsg) (outpostModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		// Leaving the token reveal returns to the list and refreshes it.
		m.view = opViewList
		m.loading = true
		return m, opFetch
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func opTokenContent(token string) string {
	if token == "" {
		return "Outpost registered, but no enrollment token was returned."
	}
	return "Outpost registered.\n\nEnrollment token (shown ONCE — copy it now):\n\n  " + token +
		"\n\nUse it when deploying the outpost agent so it can enroll with the gateway.\nIt cannot be retrieved again; delete and re-create the outpost if lost."
}

// ── View ────────────────────────────────────────────────────────────────────

func (m outpostModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case opViewForm:
		return m.form.view(m.width, m.height)
	case opViewToken:
		return tuiTitleStyle.Render("Outpost") + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	}
	title := tuiTitleStyle.Render("Outposts")
	help := tuiHelp("[↑↓/jk] nav  [enter] detail  [n] register  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No outposts.")
		if len(m.outposts) > 0 {
			body = tuiBoxStyle.Render(m.table.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	status := ""
	if m.status != "" {
		status = tuiMetaStyle.Render(m.status) + "\n"
	}
	if len(m.outposts) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No outposts. Press [n] to register one.") + "\n\n" + status + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + status + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startOutpostTUI() error {
	p := tea.NewProgram(standaloneWrap{newOutpostModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

var outpostCmd = &cobra.Command{
	Use:     "outpost",
	Aliases: []string{"outposts"},
	Short:   "Manage customer-cluster outposts (TUI)",
	Args:    cobra.NoArgs,
	RunE:    func(cmd *cobra.Command, args []string) error { return startOutpostTUI() },
}

func init() {
	outpostCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for outposts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startOutpostTUI() },
	})
	RegisterModule(Module{
		Name:    "outpost",
		Admin:   true,
		Order:   32,
		Command: outpostCmd,
		Screens: []HubScreen{{
			Title: "Outposts",
			Desc:  "Register and manage cluster outpost agents",
			New:   func() tea.Model { return newOutpostModel() },
		}},
	})
}
