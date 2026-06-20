package cmd

import (
	"encoding/json"
	"strconv"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// The Notifications TUI manages notification channels (Slack, email, webhook, …):
// list, create, edit, delete, and send a test. Provider types are fetched from
// /notifications/providers to drive the type selector; config is entered as
// key=value lines. It mirrors the `armory notifications channels` endpoints.

type channelRec struct {
	ChannelID string            `json:"channel_id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Config    map[string]string `json:"config"`
	Enabled   bool              `json:"enabled"`
}

type chanListMsg []channelRec
type chanProvidersMsg []string
type chanErrMsg struct{ err error }
type chanDoneMsg struct{ status string }
type chanFormErrMsg struct{ err error }
type chanStatusMsg struct {
	status string
	isErr  bool
}

type chanViewID int

const (
	chanViewList chanViewID = iota
	chanViewForm
)

type notificationsModel struct {
	view    chanViewID
	loading bool
	err     error
	width   int
	height  int

	channels  []channelRec
	providers []string // provider type names for the form's type selector
	table     table.Model

	form     tuiForm
	formMode string // "create" | "edit"
	editID   string

	pending   *frPending
	status    string
	statusErr bool
}

var channelCols = []tuiColSpec{{"NAME", 22, 2}, {"TYPE", 12, 0}, {"ENABLED", 8, 0}, {"CONFIG", 7, 0}}

func newNotificationsModel() notificationsModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := notificationsModel{loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight, table: t}
	m.applyTableLayout()
	return m
}

func (m *notificationsModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(channelCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func chanFetch() tea.Msg {
	data, err := doRequest("GET", "/notifications/channels", nil)
	if err != nil {
		return chanErrMsg{err}
	}
	var cs []channelRec
	if err := json.Unmarshal(data, &cs); err != nil {
		return chanErrMsg{err}
	}
	return chanListMsg(cs)
}

// chanFetchProviders loads the provider type catalog for the type selector,
// degrading to an empty list on failure so the field falls back to free text.
func chanFetchProviders() tea.Msg {
	data, err := doRequest("GET", "/notifications/providers", nil)
	if err != nil {
		return chanProvidersMsg(nil)
	}
	var ps []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &ps); err != nil {
		return chanProvidersMsg(nil)
	}
	types := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.Type != "" {
			types = append(types, p.Type)
		}
	}
	return chanProvidersMsg(types)
}

func chanDelete(id, name string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/notifications/channels/"+id, nil); err != nil {
			return chanErrMsg{err}
		}
		return chanDoneMsg{status: "✓ deleted " + name}
	}
}

func chanTest(id, name string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]any{"subject": "armory test", "body": "Test notification from the armory TUI."})
		if _, err := doRequest("POST", "/notifications/channels/"+id+"/test", body); err != nil {
			return chanStatusMsg{status: "✗ test " + name + ": " + err.Error(), isErr: true}
		}
		return chanStatusMsg{status: "✓ test sent to " + name}
	}
}

func chanSubmit(mode, id string, payload map[string]any) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		method, path := "POST", "/notifications/channels"
		if mode == "edit" {
			method, path = "PUT", "/notifications/channels/"+id
		}
		if _, err := doRequest(method, path, body); err != nil {
			return chanFormErrMsg{err}
		}
		verb := "created"
		if mode == "edit" {
			verb = "updated"
		}
		return chanDoneMsg{status: "✓ channel " + verb}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m notificationsModel) Init() tea.Cmd { return tea.Batch(chanFetch, chanFetchProviders) }

func (m notificationsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applyTableLayout()
		return m, nil
	case chanErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case chanListMsg:
		m.loading = false
		m.channels = []channelRec(msg)
		rows := make([]table.Row, len(m.channels))
		for i, c := range m.channels {
			rows[i] = table.Row{c.Name, frDash(c.Type), frBoolDotBool(c.Enabled), frMapCountM(c.Config)}
		}
		m.table.SetRows(rows)
		return m, nil
	case chanProvidersMsg:
		m.providers = []string(msg)
		return m, nil
	case chanDoneMsg:
		m.view = chanViewList
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, chanFetch
	case chanStatusMsg:
		m.status = msg.status
		m.statusErr = msg.isErr
		return m, nil
	case chanFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == chanViewList {
			return m, chanFetch
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
				return m, chanFetch
			}
			return m, nil
		}
		switch m.view {
		case chanViewList:
			return m.keyList(msg)
		case chanViewForm:
			return m.keyForm(msg)
		}
	}
	var cmd tea.Cmd
	switch m.view {
	case chanViewList:
		m.table, cmd = m.table.Update(msg)
	case chanViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m notificationsModel) keyList(msg tea.KeyMsg) (notificationsModel, tea.Cmd) {
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
	case "n":
		m.formMode = "create"
		m.editID = ""
		m.form, _ = m.newChannelForm(channelRec{Enabled: true})
		m.view = chanViewForm
		return m, nil
	case "e":
		if i := m.table.Cursor(); i >= 0 && i < len(m.channels) {
			m.formMode = "edit"
			m.editID = m.channels[i].ChannelID
			m.form, _ = m.newChannelForm(m.channels[i])
			m.view = chanViewForm
			return m, nil
		}
	case "t":
		if i := m.table.Cursor(); i >= 0 && i < len(m.channels) {
			c := m.channels[i]
			return m, chanTest(c.ChannelID, c.Name)
		}
	case "D":
		if i := m.table.Cursor(); i >= 0 && i < len(m.channels) {
			c := m.channels[i]
			m.pending = &frPending{
				prompt: "Delete channel " + c.Name + "? [y] confirm  [any] cancel",
				run:    chanDelete(c.ChannelID, c.Name),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, chanFetch
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// newChannelForm builds the create/edit dialog, pre-filled from c. The type is a
// selector when the provider catalog is known, else free text.
func (m notificationsModel) newChannelForm(c channelRec) (tuiForm, tea.Cmd) {
	title := "New Channel"
	if m.formMode == "edit" {
		title = "Edit Channel"
	}
	var typeField formField
	if len(m.providers) > 0 {
		typeField = formSelectDefault("type", "Type", m.providers, c.Type)
	} else {
		typeField = formInputDefault("type", "Type", "slack, email, webhook (required)", c.Type)
	}
	enabled := "true"
	if !c.Enabled {
		enabled = "false"
	}
	f, cmd := newTUIForm(title,
		formInputDefault("name", "Name", "Team Slack (required)", c.Name),
		typeField,
		formSelectDefault("enabled", "Enabled", []string{"true", "false"}, enabled),
		formTextarea("config", "Config", "key=value per line\ne.g. webhook_url=https://hooks.slack.com/…"),
	)
	if len(c.Config) > 0 {
		f.setValues(map[string]string{"config": frMapToLinesM(c.Config)})
	}
	return f, cmd
}

func (m notificationsModel) keyForm(msg tea.KeyMsg) (notificationsModel, tea.Cmd) {
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
		m.view = chanViewList
		return m, nil
	case formSubmit:
		name := m.form.value("name")
		ctype := m.form.value("type")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		if ctype == "" {
			m.form.errMsg = "type is required"
			return m, nil
		}
		config, err := parseKVLines(m.form.value("config"))
		if err != nil {
			m.form.errMsg = "config: " + err.Error()
			return m, nil
		}
		m.form.errMsg = ""
		payload := map[string]any{
			"name":    name,
			"type":    ctype,
			"enabled": m.form.value("enabled") == "true",
			"config":  config,
		}
		return m, chanSubmit(m.formMode, m.editID, payload)
	}
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m notificationsModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	if m.view == chanViewForm {
		return m.form.view(m.width, m.height)
	}
	title := tuiTitleStyle.Render("Notification Channels")
	help := tuiHelp("[↑↓/jk] nav  [n] new  [e] edit  [t] test  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No channels.")
		if len(m.channels) > 0 {
			body = tuiBoxStyle.Render(m.table.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	status := ""
	if m.status != "" {
		if m.statusErr {
			status = tuiErrStyle.Render(m.status) + "\n"
		} else {
			status = tuiMetaStyle.Render(m.status) + "\n"
		}
	}
	if len(m.channels) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No channels. Press [n] to add one.") + "\n\n" + status + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + status + help
}

// ── small helpers (typed-map variants of the gkRecord helpers) ─────────────────

func frBoolDotBool(b bool) string {
	if b {
		return "●"
	}
	return "○"
}

func frMapCountM(m map[string]string) string {
	return strconv.Itoa(len(m))
}

func frMapToLinesM(m map[string]string) string {
	conv := make(map[string]any, len(m))
	for k, v := range m {
		conv[k] = v
	}
	return frMapToLines(frRecord{"x": conv}, "x")
}
