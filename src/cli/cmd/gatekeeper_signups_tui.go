package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The Sign-ups TUI is the admin surface for invite-only registration. It pairs a
// one-line registration policy (open vs invite-only, toggled in place) with the
// sign-up allowlist — the emails and @domain rules permitted to register while
// invite-only is on. It reuses the same list/detail/form machinery as the forge
// runtimes and gatekeeper screens; the allowlist is a single list section, so the
// section switcher is dropped and the policy toggle takes its place.
//
// Every action is enforced server-side (all endpoints are admin-gated with no
// default grant): a user without the grant still sees the affordances but gets a
// clear 403 in the status line rather than the capability being hidden.

// ── Columns ───────────────────────────────────────────────────────────────────

var salCols = []tuiColSpec{
	{Title: "EMAIL / @DOMAIN", Min: 26, Flex: 2},
	{Title: "NOTE", Min: 18, Flex: 1},
	{Title: "ADDED", Min: 14, Flex: 0},
}

func salRow(r gkRecord) table.Row {
	return table.Row{
		gkStr(r, "email"),
		gkDash(gkStr(r, "note")),
		gkTimeCell(r, "created_at"),
	}
}

// ── Views ─────────────────────────────────────────────────────────────────────

type salViewID int

const (
	salViewList salViewID = iota
	salViewDetail
	salViewForm
)

// salPending is a destructive action awaiting y/n confirmation.
type salPending struct {
	prompt string
	run    tea.Cmd
}

// ── Messages ──────────────────────────────────────────────────────────────────

type salEntriesMsg struct{ records []gkRecord }
type salPolicyMsg struct {
	inviteOnly bool
	loaded     bool
	changed    bool // true when this reflects an admin toggle (drives the status line)
}
type salErrMsg struct{ err error }
type salActionMsg struct {
	label string
	err   error
}
type salFormErrMsg struct{ err error }
type salFormDoneMsg struct{ status string }

// ── Model ─────────────────────────────────────────────────────────────────────

type gkSignupsModel struct {
	view    salViewID
	loading bool
	err     error
	width   int
	height  int

	records []gkRecord
	table   table.Model
	vp      viewport.Model

	inviteOnly   bool
	policyLoaded bool

	form    tuiForm
	pending *salPending

	selLabel  string
	status    string
	statusErr bool
}

func newGkSignupsModel() gkSignupsModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := gkSignupsModel{
		loading: true,
		width:   tuiDefaultWidth,
		height:  tuiDefaultHeight,
		table:   t,
		vp:      viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
	}
	m.applyTableLayout()
	return m
}

func (m *gkSignupsModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(salCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome+1)) // +1 for the policy line
}

// currentRecord returns the record under the list cursor.
func (m *gkSignupsModel) currentRecord() (gkRecord, bool) {
	i := m.table.Cursor()
	if i >= 0 && i < len(m.records) {
		return m.records[i], true
	}
	return nil, false
}

// ── Commands ──────────────────────────────────────────────────────────────────

func salFetchEntries() tea.Msg {
	data, err := doRequest("GET", "/gatekeeper/signup-allowlist", nil)
	if err != nil {
		return salErrMsg{err}
	}
	var recs []gkRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return salErrMsg{err}
	}
	return salEntriesMsg{records: recs}
}

// salFetchPolicy loads the invite-only flag. A failure degrades to loaded=false
// (the policy line shows "unknown") rather than blanking the whole screen, so a
// transient policy read never hides the allowlist.
func salFetchPolicy() tea.Msg {
	data, err := doRequest("GET", "/gatekeeper/signup-policy", nil)
	if err != nil {
		return salPolicyMsg{loaded: false}
	}
	var p struct {
		InviteOnly bool `json:"invite_only"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return salPolicyMsg{loaded: false}
	}
	return salPolicyMsg{inviteOnly: p.InviteOnly, loaded: true}
}

func salDelete(id string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/gatekeeper/signup-allowlist/"+id, nil); err != nil {
			return salActionMsg{label: "delete", err: err}
		}
		return salActionMsg{label: "delete"}
	}
}

func salSetPolicy(inviteOnly bool) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]bool{"invite_only": inviteOnly})
		if _, err := doRequest("PUT", "/gatekeeper/signup-policy", body); err != nil {
			return salActionMsg{label: "policy", err: err}
		}
		return salPolicyMsg{inviteOnly: inviteOnly, loaded: true, changed: true}
	}
}

func salSubmit(email, note string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"email": email, "note": note})
		if _, err := doRequest("POST", "/gatekeeper/signup-allowlist", body); err != nil {
			return salFormErrMsg{err}
		}
		return salFormDoneMsg{status: "✓ added to allowlist"}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m gkSignupsModel) Init() tea.Cmd {
	return tea.Batch(salFetchEntries, salFetchPolicy)
}

func (m gkSignupsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 9
		m.applyTableLayout()
		return m, nil

	case salErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case salEntriesMsg:
		m.loading = false
		m.records = msg.records
		rows := make([]table.Row, len(msg.records))
		for i, r := range msg.records {
			rows[i] = salRow(r)
		}
		m.table.SetRows(rows)
		return m, nil

	case salPolicyMsg:
		m.policyLoaded = msg.loaded
		if msg.loaded {
			m.inviteOnly = msg.inviteOnly
		}
		if msg.changed {
			if msg.inviteOnly {
				m.status = "✓ invite-only enabled — only allowlisted emails can register"
			} else {
				m.status = "✓ invite-only disabled — registration is open"
			}
			m.statusErr = false
		}
		return m, nil

	case salActionMsg:
		if msg.err != nil {
			m.status = "✗ " + msg.label + ": " + msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.status = "✓ " + msg.label + "d"
		m.statusErr = false
		m.loading = true
		return m, salFetchEntries

	case salFormDoneMsg:
		m.view = salViewList
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, salFetchEntries

	case salFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tuiAutoRefreshMsg:
		// Silently re-sync the list and policy without a loading flash or moving the
		// cursor. Detail renders a snapshot and the form must not be disturbed.
		if m.view == salViewList {
			return m, tea.Batch(salFetchEntries, salFetchPolicy)
		}
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "esc":
				return m, salGoHome
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, tea.Batch(salFetchEntries, salFetchPolicy)
			}
			return m, nil
		}
		switch m.view {
		case salViewList:
			return m.keyList(msg)
		case salViewDetail:
			return m.keyDetail(msg)
		case salViewForm:
			return m.keyForm(msg)
		}
	}
	return m.delegate(msg)
}

// salGoHome returns the hub (or quits, in standalone mode) via the shared goHomeMsg.
func salGoHome() tea.Msg { return goHomeMsg{} }

func (m gkSignupsModel) delegate(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case salViewList:
		m.table, cmd = m.table.Update(msg)
	case salViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	case salViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m gkSignupsModel) keyList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		switch msg.String() {
		case "y", "Y":
			return m, run
		default:
			return m, nil
		}
	}

	switch msg.String() {
	case "esc":
		return m, salGoHome
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if rec, ok := m.currentRecord(); ok {
			m.selLabel = gkStr(rec, "email")
			m.view = salViewDetail
			pretty, _ := json.MarshalIndent(rec, "", "  ")
			m.vp.SetContent(string(pretty))
			m.vp.GotoTop()
		}
		return m, nil
	case "n":
		return m.openForm()
	case "t":
		// Toggle invite-only in place. Guarded on a known current state so a failed
		// policy read can't flip it blindly.
		if !m.policyLoaded {
			m.status = "✗ policy state unknown — refresh first"
			m.statusErr = true
			return m, nil
		}
		return m, salSetPolicy(!m.inviteOnly)
	case "D":
		if rec, ok := m.currentRecord(); ok {
			email := gkStr(rec, "email")
			id := gkStr(rec, "entry_id")
			m.pending = &salPending{
				prompt: fmt.Sprintf("Remove %q from the allowlist? [y] confirm  [any] cancel", email),
				run:    salDelete(id),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, tea.Batch(salFetchEntries, salFetchPolicy)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m gkSignupsModel) keyDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = salViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── Form ──────────────────────────────────────────────────────────────────────

func (m gkSignupsModel) openForm() (tea.Model, tea.Cmd) {
	f, cmd := newTUIForm("Add to sign-up allowlist",
		formInput("email", "Email or @domain", "alice@co.com or @co.com (required)"),
		formInput("note", "Note", "optional — e.g. new hire"),
	)
	f.help = "A full address matches one person; a @domain rule (@co.com) matches everyone at that domain."
	m.form = f
	m.view = salViewForm
	return m, cmd
}

func (m gkSignupsModel) keyForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		m.view = salViewList
		return m, nil
	case formSubmit:
		email := m.form.value("email")
		if email == "" {
			m.form.errMsg = "email is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, salSubmit(email, m.form.value("note"))
	}
	return m, cmd
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m gkSignupsModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case salViewForm:
		return m.form.view(m.width, m.height)
	case salViewDetail:
		return m.viewDetail()
	}
	return m.viewList()
}

// policyLine renders the current registration policy above the allowlist.
func (m gkSignupsModel) policyLine() string {
	state := tuiMetaStyle.Render("unknown")
	if m.policyLoaded {
		if m.inviteOnly {
			state = tuiTitleStyle.Render("invite-only")
		} else {
			state = tuiMetaStyle.Render("open")
		}
	}
	return tuiMetaStyle.Render("registration: ") + state
}

func (m gkSignupsModel) listHelp() string {
	toggle := "[t] enable invite-only"
	if m.policyLoaded && m.inviteOnly {
		toggle = "[t] disable invite-only"
	}
	parts := []string{"[↑↓/jk] nav", "[enter] detail", "[n] add", "[D] delete", toggle, "[r] refresh", "[esc] home"}
	return strings.Join(parts, "  ")
}

func (m gkSignupsModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErrStyle.Render(m.status) + "\n"
	}
	return tuiMetaStyle.Render(m.status) + "\n"
}

func (m gkSignupsModel) viewList() string {
	header := tuiTitleStyle.Render("Sign-ups") + "\n" + m.policyLine()
	help := tuiHelp(m.listHelp(), m.width)

	if m.loading {
		return header + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No allowlisted emails.")
		if len(m.records) > 0 {
			body = tuiBoxStyle.Render(m.table.View())
		}
		return header + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if len(m.records) == 0 {
		empty := "No allowlisted emails."
		if m.policyLoaded && m.inviteOnly {
			empty = "No allowlisted emails — nobody can register until you add one."
		}
		return header + "\n\n" + tuiMetaStyle.Render(empty) + "\n\n" + m.statusLine() + help
	}
	return header + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + m.statusLine() + help
}

func (m gkSignupsModel) viewDetail() string {
	title := tuiTitleStyle.Render("allowlist entry")
	if m.selLabel != "" {
		title = tuiTitleStyle.Render(m.selLabel) + "  " + tuiMetaStyle.Render("allowlist entry")
	}
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startGkSignupsTUI() error {
	p := tea.NewProgram(standaloneWrap{newGkSignupsModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// gkSignupsCmd is an admin control: `armory admin signups` opens the invite-only
// registration TUI (policy toggle + sign-up allowlist). It sits with the other
// platform-administration tools rather than the user-facing commands.
var gkSignupsCmd = &cobra.Command{
	Use:     "signups",
	Aliases: []string{"signup-allowlist", "invite-only"},
	Short:   "Manage invite-only registration and the sign-up allowlist (TUI)",
	Args:    cobra.NoArgs,
	RunE:    func(cmd *cobra.Command, args []string) error { return startGkSignupsTUI() },
}

func init() {
	gkSignupsCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for invite-only registration and the sign-up allowlist",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startGkSignupsTUI() },
	})
	RegisterModule(Module{
		Name:    "gatekeeper-signups",
		Admin:   true,
		Order:   61, // right after gatekeeper (60) in the admin hub
		Command: gkSignupsCmd,
		Screens: []HubScreen{{
			Title: "Sign-ups",
			Desc:  "Invite-only registration and the sign-up allowlist",
			New:   func() tea.Model { return newGkSignupsModel() },
		}},
	})
}
