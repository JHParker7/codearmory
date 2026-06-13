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

// The Gatekeeper TUI is a single model that browses every listable gatekeeper
// resource — teams, invites, service-permission requests, roles, orgs and
// users — behind a tab-style section switcher. Each section shares the same
// list/detail/confirm machinery; what differs per section (the list endpoint,
// the table columns, how a record maps to a row, and which actions apply) is
// described declaratively by a gkSectionDef so the Update/View loops stay
// generic. Records are kept as decoded JSON objects (gkRecord) rather than
// typed structs, which keeps the six sections compact and lets the detail view
// render any resource verbatim.

// ── Sections ────────────────────────────────────────────────────────────────

type gkSection int

const (
	gkTeams gkSection = iota
	gkInvites
	gkServiceRequests
	gkRoles
	gkOrgs
	gkUsers
)

// gkRecord is one decoded JSON object from a list endpoint.
type gkRecord = map[string]any

// gkAct is a section action invoked from the list view against the selected
// record. It performs a single bodyless request to pathFn(id); confirm gates it
// behind a y/n prompt first.
type gkAct struct {
	key     string // trigger key, e.g. "a"
	label   string // verb shown in help and status, e.g. "approve"
	method  string // HTTP method
	confirm bool   // ask before running
	pathFn  func(id string) string
}

// gkSectionDef describes one section: where its records come from, how they
// render, and what can be done to them.
type gkSectionDef struct {
	name      string // tab label, e.g. "Teams"
	label     string // singular noun for prompts/status, e.g. "team"
	path      string // list endpoint
	idKey     string // record field holding the resource id
	nameKey   string // record field used to label confirms/status
	cols      []tuiColSpec
	row       func(gkRecord) table.Row
	actions   []gkAct
	canCreate bool // 'n' opens a create form
	canInvite bool // 'i' opens an invite form for the selected record
}

var gkDefs = []gkSectionDef{
	gkTeams: {
		name: "Teams", label: "team",
		path: "/gatekeeper/teams", idKey: "team_id", nameKey: "team_name",
		cols: []tuiColSpec{{"NAME", 16, 2}, {"ROLE", 10, 0}, {"OWNER", 10, 0}, {"ACTIVE", 7, 0}, {"CREATED", 14, 0}},
		row: func(r gkRecord) table.Row {
			return table.Row{gkStr(r, "team_name"), gkDash(gkShort(r, "role_id")), gkDash(gkShort(r, "owner_id")), gkActiveDot(r), gkTimeCell(r, "created_at")}
		},
		actions:   []gkAct{{"D", "delete", "DELETE", true, func(id string) string { return "/gatekeeper/teams/" + id }}},
		canCreate: true, canInvite: true,
	},
	gkInvites: {
		name: "Invites", label: "invite",
		path: "/gatekeeper/invites", idKey: "invite_id", nameKey: "invitee_email",
		cols: []tuiColSpec{{"EMAIL", 20, 2}, {"TYPE", 6, 0}, {"STATUS", 10, 0}, {"EXPIRES", 14, 0}, {"CREATED", 14, 0}},
		row: func(r gkRecord) table.Row {
			return table.Row{gkStr(r, "invitee_email"), gkStr(r, "resource_type"), gkStr(r, "status"), gkTimeCell(r, "expires_at"), gkTimeCell(r, "created_at")}
		},
		actions: []gkAct{
			{"a", "accept", "POST", false, func(id string) string { return "/gatekeeper/invites/" + id + "/accept" }},
			{"x", "decline", "POST", false, func(id string) string { return "/gatekeeper/invites/" + id + "/decline" }},
			{"D", "delete", "DELETE", true, func(id string) string { return "/gatekeeper/invites/" + id }},
		},
	},
	gkServiceRequests: {
		name: "Service Requests", label: "request",
		path: "/gatekeeper/service-permission-requests", idKey: "request_id", nameKey: "service_name",
		cols: []tuiColSpec{{"SERVICE", 16, 2}, {"NAME", 16, 2}, {"STATUS", 10, 0}, {"CREATED", 14, 0}},
		row: func(r gkRecord) table.Row {
			return table.Row{gkStr(r, "service_name"), gkStr(r, "name"), gkStr(r, "status"), gkTimeCell(r, "created_at")}
		},
		actions: []gkAct{
			{"a", "approve", "POST", false, func(id string) string {
				return "/gatekeeper/service-permission-requests/" + id + "/approve"
			}},
			{"x", "decline", "POST", false, func(id string) string {
				return "/gatekeeper/service-permission-requests/" + id + "/decline"
			}},
		},
	},
	gkRoles: {
		name: "Roles", label: "role",
		path: "/gatekeeper/roles", idKey: "role_id", nameKey: "name",
		cols: []tuiColSpec{{"NAME", 16, 2}, {"PERMS", 6, 0}, {"OWNER", 10, 0}, {"ACTIVE", 7, 0}, {"CREATED", 14, 0}},
		row: func(r gkRecord) table.Row {
			return table.Row{gkDash(gkStr(r, "name")), gkCount(r, "permissions_ids"), gkDash(gkShort(r, "owner_id")), gkActiveDot(r), gkTimeCell(r, "created_at")}
		},
		actions: []gkAct{{"D", "delete", "DELETE", true, func(id string) string { return "/gatekeeper/roles/" + id }}},
	},
	gkOrgs: {
		name: "Orgs", label: "org",
		path: "/gatekeeper/orgs", idKey: "org_id", nameKey: "org_name",
		cols: []tuiColSpec{{"NAME", 20, 2}, {"OWNER", 12, 0}, {"ACTIVE", 7, 0}, {"CREATED", 14, 0}},
		row: func(r gkRecord) table.Row {
			return table.Row{gkStr(r, "org_name"), gkDash(gkShort(r, "owner_id")), gkActiveDot(r), gkTimeCell(r, "created_at")}
		},
		actions:   []gkAct{{"D", "delete", "DELETE", true, func(id string) string { return "/gatekeeper/orgs/" + id }}},
		canCreate: true, canInvite: true,
	},
	gkUsers: {
		name: "Users", label: "user",
		path: "/gatekeeper/users", idKey: "user_id", nameKey: "username",
		cols: []tuiColSpec{{"USERNAME", 16, 2}, {"ORG", 10, 0}, {"TEAM", 10, 0}, {"ACTIVE", 7, 0}, {"CREATED", 14, 0}},
		row: func(r gkRecord) table.Row {
			return table.Row{gkStr(r, "username"), gkDash(gkShort(r, "org_id")), gkDash(gkShort(r, "team_id")), gkActiveDot(r), gkTimeCell(r, "created_at")}
		},
		actions: []gkAct{{"D", "delete", "DELETE", true, func(id string) string { return "/gatekeeper/users/" + id }}},
	},
}

// ── Record helpers ──────────────────────────────────────────────────────────

func gkStr(r gkRecord, key string) string {
	if v, ok := r[key].(string); ok {
		return v
	}
	return ""
}

func gkShort(r gkRecord, key string) string { return tuiShortID(gkStr(r, key)) }

func gkDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func gkActiveDot(r gkRecord) string {
	if b, _ := r["active"].(bool); b {
		return "●"
	}
	return "○"
}

func gkTimeCell(r gkRecord, key string) string {
	s := gkStr(r, key)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("Jan 02 15:04")
	}
	return s
}

func gkCount(r gkRecord, key string) string {
	if a, ok := r[key].([]any); ok {
		return fmt.Sprintf("%d", len(a))
	}
	return "0"
}

// gkCapitalize upper-cases the first letter of s (ASCII), for prompt verbs.
func gkCapitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ── Views / forms ───────────────────────────────────────────────────────────

type gkViewID int

const (
	gkViewList gkViewID = iota
	gkViewDetail
	gkViewForm
)

type gkFormKind int

const (
	gkFormNone gkFormKind = iota
	gkFormCreateTeam
	gkFormCreateOrg
	gkFormInviteTeam
	gkFormInviteOrg
)

// gkPending is a destructive action awaiting y/n confirmation.
type gkPending struct {
	prompt string
	run    tea.Cmd
}

// ── Messages ────────────────────────────────────────────────────────────────

type gkRecordsMsg struct {
	section gkSection
	records []gkRecord
}
type gkErrMsg struct{ err error }
type gkActionMsg struct {
	section gkSection
	label   string
	err     error
}
type gkFormErrMsg struct{ err error }
type gkFormDoneMsg struct {
	section gkSection
	status  string
}

// ── Model ───────────────────────────────────────────────────────────────────

type gkSectionState struct {
	records []gkRecord
	table   table.Model
	loaded  bool
}

type gatekeeperModel struct {
	section gkSection
	view    gkViewID
	loading bool
	err     error
	width   int
	height  int

	sections []gkSectionState
	vp       viewport.Model

	form            tuiForm
	formKind        gkFormKind
	formTargetID    string // resource id an invite form targets
	formTargetLabel string

	pending *gkPending // confirm prompt in the list view

	selLabel string // selected record label, for the detail title

	status    string // transient one-line status (action result)
	statusErr bool

	srFilter string // service-request status filter ("" = all)
}

func newGatekeeperModel() gatekeeperModel {
	m := gatekeeperModel{
		loading:  true,
		width:    tuiDefaultWidth,
		height:   tuiDefaultHeight,
		sections: make([]gkSectionState, len(gkDefs)),
		vp:       viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
	}
	for i := range m.sections {
		t := table.New(table.WithFocused(true))
		t.SetStyles(tuiTableStyles())
		m.sections[i].table = t
	}
	m.applyTableLayout()
	return m
}

// applyTableLayout resizes every section table to the current terminal.
func (m *gatekeeperModel) applyTableLayout() {
	for i := range m.sections {
		m.sections[i].table.SetColumns(tuiFitColumns(gkDefs[i].cols, m.width))
		m.sections[i].table.SetHeight(tuiTableHeight(m.height, tuiListChrome+1)) // +1 for the section bar
	}
}

func (m *gatekeeperModel) def() gkSectionDef { return gkDefs[m.section] }

func (m *gatekeeperModel) state() *gkSectionState { return &m.sections[m.section] }

// currentRecord returns the record under the active section's cursor.
func (m *gatekeeperModel) currentRecord() (gkRecord, bool) {
	st := m.state()
	i := st.table.Cursor()
	if i >= 0 && i < len(st.records) {
		return st.records[i], true
	}
	return nil, false
}

// ── Fetch ─────────────────────────────────────────────────────────────────

// gkListPath is the list endpoint for a section, including the
// service-request status filter when one is set.
func gkListPath(section gkSection, srFilter string) string {
	path := gkDefs[section].path
	if section == gkServiceRequests && srFilter != "" {
		path += "?" + url.Values{"status": {srFilter}}.Encode()
	}
	return path
}

func gkFetch(section gkSection, srFilter string) tea.Cmd {
	path := gkListPath(section, srFilter)
	return func() tea.Msg {
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return gkErrMsg{err}
		}
		var recs []gkRecord
		if err := json.Unmarshal(data, &recs); err != nil {
			return gkErrMsg{err}
		}
		return gkRecordsMsg{section: section, records: recs}
	}
}

func gkRunAction(section gkSection, method, path, label string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest(method, path, nil); err != nil {
			return gkActionMsg{section: section, label: label, err: err}
		}
		return gkActionMsg{section: section, label: label}
	}
}

func gkCreateTeam(name, role string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{"team_name": name}
		if role != "" {
			payload["role_id"] = role
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/gatekeeper/teams", body); err != nil {
			return gkFormErrMsg{err}
		}
		return gkFormDoneMsg{section: gkTeams, status: "✓ team created"}
	}
}

func gkCreateOrg(name string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"org_name": name})
		if _, err := doRequest("POST", "/gatekeeper/orgs", body); err != nil {
			return gkFormErrMsg{err}
		}
		return gkFormDoneMsg{section: gkOrgs, status: "✓ org created"}
	}
}

func gkSendInvite(section gkSection, path, email string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"email": email})
		if _, err := doRequest("POST", path, body); err != nil {
			return gkFormErrMsg{err}
		}
		return gkFormDoneMsg{section: section, status: "✓ invite sent to " + email}
	}
}

// ── Init / Update ───────────────────────────────────────────────────────────

func (m gatekeeperModel) Init() tea.Cmd { return gkFetch(m.section, m.srFilter) }

func (m gatekeeperModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 9
		m.applyTableLayout()
		return m, nil

	case gkErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case gkRecordsMsg:
		m.loading = false
		st := &m.sections[msg.section]
		st.records = msg.records
		st.loaded = true
		rows := make([]table.Row, len(msg.records))
		for i, r := range msg.records {
			rows[i] = gkDefs[msg.section].row(r)
		}
		st.table.SetRows(rows)
		return m, nil

	case gkActionMsg:
		if msg.err != nil {
			m.status = "✗ " + msg.label + ": " + msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.status = "✓ " + msg.label + "d"
		m.statusErr = false
		m.loading = true
		return m, gkFetch(msg.section, m.srFilter)

	case gkFormDoneMsg:
		m.view = gkViewList
		m.formKind = gkFormNone
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, gkFetch(msg.section, m.srFilter)

	case gkFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "q":
				return m, goHome
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, gkFetch(m.section, m.srFilter)
			}
			return m, nil
		}
		switch m.view {
		case gkViewList:
			return m.keyList(msg)
		case gkViewDetail:
			return m.keyDetail(msg)
		case gkViewForm:
			return m.keyForm(msg)
		}
	}
	return m.delegate(msg)
}

func (m gatekeeperModel) delegate(msg tea.Msg) (gatekeeperModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case gkViewList:
		m.sections[m.section].table, cmd = m.state().table.Update(msg)
	case gkViewDetail:
		m.vp, cmd = m.vp.Update(msg)
	case gkViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

// switchSection moves to section s, lazily fetching it the first time.
func (m gatekeeperModel) switchSection(s gkSection) (gatekeeperModel, tea.Cmd) {
	m.section = s
	m.view = gkViewList
	m.pending = nil
	m.status = ""
	if !m.state().loaded {
		m.loading = true
		return m, gkFetch(s, m.srFilter)
	}
	return m, nil
}

func (m gatekeeperModel) keyList(msg tea.KeyMsg) (gatekeeperModel, tea.Cmd) {
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

	n := gkSection(len(gkDefs))
	switch msg.String() {
	case "q":
		return m, goHome
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "]":
		return m.switchSection((m.section + 1) % n)
	case "shift+tab", "[":
		return m.switchSection((m.section - 1 + n) % n)
	case "1", "2", "3", "4", "5", "6":
		if s := gkSection(msg.String()[0] - '1'); s < n {
			return m.switchSection(s)
		}
	case "enter":
		if rec, ok := m.currentRecord(); ok {
			m.selLabel = gkStr(rec, m.def().nameKey)
			pretty, _ := json.MarshalIndent(rec, "", "  ")
			m.vp.SetContent(string(pretty))
			m.vp.GotoTop()
			m.view = gkViewDetail
		}
		return m, nil
	case "r":
		m.loading = true
		m.status = ""
		return m, gkFetch(m.section, m.srFilter)
	case "f":
		if m.section == gkServiceRequests {
			m.srFilter = gkNextFilter(m.srFilter)
			m.loading = true
			return m, gkFetch(m.section, m.srFilter)
		}
	case "n":
		if m.def().canCreate {
			kind := gkFormCreateTeam
			if m.section == gkOrgs {
				kind = gkFormCreateOrg
			}
			return m.openForm(kind)
		}
	case "i":
		if m.def().canInvite {
			rec, ok := m.currentRecord()
			if !ok {
				return m, nil
			}
			m.formTargetID = gkStr(rec, m.def().idKey)
			m.formTargetLabel = gkStr(rec, m.def().nameKey)
			kind := gkFormInviteTeam
			if m.section == gkOrgs {
				kind = gkFormInviteOrg
			}
			return m.openForm(kind)
		}
	}

	// Section-specific actions (accept/decline/approve/delete).
	for _, a := range m.def().actions {
		if a.key != msg.String() {
			continue
		}
		rec, ok := m.currentRecord()
		if !ok {
			return m, nil
		}
		id := gkStr(rec, m.def().idKey)
		cmd := gkRunAction(m.section, a.method, a.pathFn(id), a.label)
		if a.confirm {
			name := gkStr(rec, m.def().nameKey)
			m.pending = &gkPending{
				prompt: fmt.Sprintf("%s %s %q? [y] confirm  [any] cancel", gkCapitalize(a.label), m.def().label, name),
				run:    cmd,
			}
			return m, nil
		}
		return m, cmd
	}

	var cmd tea.Cmd
	m.sections[m.section].table, cmd = m.state().table.Update(msg)
	return m, cmd
}

func (m gatekeeperModel) keyDetail(msg tea.KeyMsg) (gatekeeperModel, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, goHome
	case "ctrl+c":
		return m, tea.Quit
	case "b", "esc":
		m.view = gkViewList
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// gkNextFilter cycles the service-request status filter: all → pending →
// approved → declined → all.
func gkNextFilter(cur string) string {
	switch cur {
	case "":
		return "pending"
	case "pending":
		return "approved"
	case "approved":
		return "declined"
	default:
		return ""
	}
}

// ── Forms ─────────────────────────────────────────────────────────────────

func (m gatekeeperModel) openForm(kind gkFormKind) (gatekeeperModel, tea.Cmd) {
	var (
		f   tuiForm
		cmd tea.Cmd
	)
	switch kind {
	case gkFormCreateTeam:
		f, cmd = newTUIForm("New Team",
			formInput("name", "Name", "team name (required)"),
			formInput("role", "Role", "role id (optional)"))
	case gkFormCreateOrg:
		f, cmd = newTUIForm("New Organization",
			formInput("name", "Name", "org name (required)"))
	case gkFormInviteTeam:
		f, cmd = newTUIForm("Invite to "+m.formTargetLabel,
			formInput("email", "Email", "user@example.com (required)"))
	case gkFormInviteOrg:
		f, cmd = newTUIForm("Invite to "+m.formTargetLabel,
			formInput("email", "Email", "user@example.com (required)"))
	}
	m.form = f
	m.formKind = kind
	m.view = gkViewForm
	return m, cmd
}

func (m gatekeeperModel) keyForm(msg tea.KeyMsg) (gatekeeperModel, tea.Cmd) {
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
		m.view = gkViewList
		m.formKind = gkFormNone
		return m, nil
	case formSubmit:
		return m.submitForm()
	}
	return m, cmd
}

func (m gatekeeperModel) submitForm() (gatekeeperModel, tea.Cmd) {
	switch m.formKind {
	case gkFormCreateTeam:
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, gkCreateTeam(name, m.form.value("role"))
	case gkFormCreateOrg:
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, gkCreateOrg(name)
	case gkFormInviteTeam:
		email := m.form.value("email")
		if email == "" {
			m.form.errMsg = "email is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, gkSendInvite(gkTeams, "/gatekeeper/teams/"+m.formTargetID+"/invites", email)
	case gkFormInviteOrg:
		email := m.form.value("email")
		if email == "" {
			m.form.errMsg = "email is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, gkSendInvite(gkOrgs, "/gatekeeper/orgs/"+m.formTargetID+"/invites", email)
	}
	return m, nil
}

// ── View ────────────────────────────────────────────────────────────────────

func (m gatekeeperModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" +
			tuiHelpStyle.Render("[q] home  [r] retry")
	}
	switch m.view {
	case gkViewForm:
		return m.form.view(m.width, m.height)
	case gkViewDetail:
		return m.viewDetail()
	}
	return m.viewList()
}

// sectionBar renders the tab strip with the active section highlighted.
func (m gatekeeperModel) sectionBar() string {
	active := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(activeTheme.Accent))
	inactive := lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	labels := make([]string, len(gkDefs))
	for i, d := range gkDefs {
		if gkSection(i) == m.section {
			labels[i] = active.Render(d.name)
		} else {
			labels[i] = inactive.Render(d.name)
		}
	}
	return strings.Join(labels, tuiMetaStyle.Render("  ·  "))
}

func (m gatekeeperModel) listHelp() string {
	parts := []string{"[tab] section", "[↑↓/jk] nav", "[enter] detail"}
	for _, a := range m.def().actions {
		parts = append(parts, "["+a.key+"] "+a.label)
	}
	if m.def().canCreate {
		parts = append(parts, "[n] new")
	}
	if m.def().canInvite {
		parts = append(parts, "[i] invite")
	}
	if m.section == gkServiceRequests {
		parts = append(parts, "[f] filter")
	}
	parts = append(parts, "[r] refresh", "[q] home")
	return strings.Join(parts, "  ")
}

func (m gatekeeperModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErrStyle.Render(m.status) + "\n"
	}
	return tuiMetaStyle.Render(m.status) + "\n"
}

func (m gatekeeperModel) viewList() string {
	title := tuiTitleStyle.Render("Gatekeeper")
	if m.section == gkServiceRequests && m.srFilter != "" {
		title += "  " + tuiMetaStyle.Render("("+m.srFilter+")")
	}
	bar := m.sectionBar()
	help := tuiHelp(m.listHelp(), m.width)
	header := title + "\n" + bar

	if m.loading {
		return header + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No " + m.def().name + ".")
		if len(m.state().records) > 0 {
			body = tuiBoxStyle.Render(m.state().table.View())
		}
		return header + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if len(m.state().records) == 0 {
		return header + "\n\n" + tuiMetaStyle.Render("No "+strings.ToLower(m.def().name)+".") + "\n\n" + m.statusLine() + help
	}
	return header + "\n" + tuiBoxStyle.Render(m.state().table.View()) + "\n" + m.statusLine() + help
}

func (m gatekeeperModel) viewDetail() string {
	title := tuiTitleStyle.Render(m.def().name + " detail")
	if m.selLabel != "" {
		title = tuiTitleStyle.Render(m.selLabel) + "  " + tuiMetaStyle.Render(m.def().label)
	}
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [b] back  [q] home", m.width)
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Command registration ──────────────────────────────────────────────────────

func startGatekeeperTUI() error {
	p := tea.NewProgram(standaloneWrap{newGatekeeperModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

var gatekeeperCmd = &cobra.Command{
	Use:     "gatekeeper",
	Aliases: []string{"gk"},
	Short:   "Interactive TUI for teams, invites, service requests, roles, orgs and users",
	Args:    cobra.NoArgs,
	RunE:    func(cmd *cobra.Command, args []string) error { return startGatekeeperTUI() },
}

func init() {
	gatekeeperCmd.AddCommand(&cobra.Command{
		Use:   "tui",
		Short: "Interactive TUI for gatekeeper resources",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return startGatekeeperTUI() },
	})
	RegisterModule(Module{
		Name:    "gatekeeper",
		Order:   60,
		Command: gatekeeperCmd,
		Screens: []HubScreen{{
			Title: "Gatekeeper",
			Desc:  "Teams, roles, orgs, users and invites",
			New:   func() tea.Model { return newGatekeeperModel() },
		}},
	})
}
