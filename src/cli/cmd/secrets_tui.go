package cmd

import (
	"encoding/json"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// The Secrets TUI lists the org's secrets (names only — values are never
// returned by the API) and supports create (name + value), update (new value),
// and delete. It mirrors the `armory secrets` CLI endpoints.

type secretRec struct {
	SecretID  string    `json:"secret_id"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

type secretsListMsg []secretRec
type secretsErrMsg struct{ err error }
type secretsDoneMsg struct{ status string }
type secretsFormErrMsg struct{ err error }

type secretsViewID int

const (
	secretsViewList secretsViewID = iota
	secretsViewForm
)

type secretsModel struct {
	view    secretsViewID
	loading bool
	err     error
	width   int
	height  int

	secrets []secretRec
	table   table.Model

	form     tuiForm
	formMode string // "create" | "edit"
	editID   string
	editName string

	pending   *frPending // delete confirmation
	status    string
	statusErr bool
}

var secretCols = []tuiColSpec{{"NAME", 24, 2}, {"CREATED BY", 14, 1}, {"UPDATED", 14, 0}}

func newSecretsModel() secretsModel {
	t := table.New(table.WithFocused(true))
	t.SetStyles(tuiTableStyles())
	m := secretsModel{loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight, table: t}
	m.applyTableLayout()
	return m
}

func (m *secretsModel) applyTableLayout() {
	m.table.SetColumns(tuiFitColumns(secretCols, m.width))
	m.table.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func secretsFetch() tea.Msg {
	data, err := doRequest("GET", "/gatekeeper/secrets", nil)
	if err != nil {
		return secretsErrMsg{err}
	}
	var secs []secretRec
	if err := json.Unmarshal(data, &secs); err != nil {
		return secretsErrMsg{err}
	}
	return secretsListMsg(secs)
}

func secretsDelete(id, name string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/gatekeeper/secrets/"+id, nil); err != nil {
			return secretsErrMsg{err}
		}
		return secretsDoneMsg{status: "✓ deleted " + name}
	}
}

func secretsSubmit(mode, id, name, value string) tea.Cmd {
	return func() tea.Msg {
		var (
			payload map[string]string
			method  string
			path    string
		)
		if mode == "edit" {
			payload, method, path = map[string]string{"value": value}, "PUT", "/gatekeeper/secrets/"+id
		} else {
			payload, method, path = map[string]string{"name": name, "value": value}, "POST", "/gatekeeper/secrets"
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest(method, path, body); err != nil {
			return secretsFormErrMsg{err}
		}
		verb := "created"
		if mode == "edit" {
			verb = "updated"
		}
		return secretsDoneMsg{status: "✓ secret " + verb}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m secretsModel) Init() tea.Cmd { return secretsFetch }

func (m secretsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applyTableLayout()
		return m, nil
	case secretsErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case secretsListMsg:
		m.loading = false
		m.secrets = []secretRec(msg)
		rows := make([]table.Row, len(m.secrets))
		for i, s := range m.secrets {
			rows[i] = table.Row{s.Name, frDash(s.CreatedBy), s.UpdatedAt.Local().Format("Jan 02 15:04")}
		}
		m.table.SetRows(rows)
		return m, nil
	case secretsDoneMsg:
		m.view = secretsViewList
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, secretsFetch
	case secretsFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == secretsViewList {
			return m, secretsFetch
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
				return m, secretsFetch
			}
			return m, nil
		}
		switch m.view {
		case secretsViewList:
			return m.keyList(msg)
		case secretsViewForm:
			return m.keyForm(msg)
		}
	}
	var cmd tea.Cmd
	switch m.view {
	case secretsViewList:
		m.table, cmd = m.table.Update(msg)
	case secretsViewForm:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m secretsModel) keyList(msg tea.KeyMsg) (secretsModel, tea.Cmd) {
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
		m.editID, m.editName = "", ""
		m.form, _ = newTUIForm("New Secret",
			formInput("name", "Name", "DATABASE_URL (required)"),
			formInput("value", "Value", "secret value (required)"))
		m.view = secretsViewForm
		return m, nil
	case "e":
		if i := m.table.Cursor(); i >= 0 && i < len(m.secrets) {
			m.formMode = "edit"
			m.editID, m.editName = m.secrets[i].SecretID, m.secrets[i].Name
			m.form, _ = newTUIForm("Edit "+m.editName,
				formInput("value", "Value", "new value (required)"))
			m.view = secretsViewForm
			return m, nil
		}
	case "D":
		if i := m.table.Cursor(); i >= 0 && i < len(m.secrets) {
			s := m.secrets[i]
			m.pending = &frPending{
				prompt: "Delete secret " + s.Name + "? [y] confirm  [any] cancel",
				run:    secretsDelete(s.SecretID, s.Name),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, secretsFetch
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m secretsModel) keyForm(msg tea.KeyMsg) (secretsModel, tea.Cmd) {
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
		m.view = secretsViewList
		return m, nil
	case formSubmit:
		value := m.form.value("value")
		if value == "" {
			m.form.errMsg = "value is required"
			return m, nil
		}
		if m.formMode == "create" && m.form.value("name") == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, secretsSubmit(m.formMode, m.editID, m.form.value("name"), value)
	}
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m secretsModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	if m.view == secretsViewForm {
		return m.form.view(m.width, m.height)
	}
	title := tuiTitleStyle.Render("Secrets")
	help := tuiHelp("[↑↓/jk] nav  [n] new  [e] edit value  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No secrets.")
		if len(m.secrets) > 0 {
			body = tuiBoxStyle.Render(m.table.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	status := ""
	if m.status != "" {
		status = tuiMetaStyle.Render(m.status) + "\n"
	}
	if len(m.secrets) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No secrets. Press [n] to add one.") + "\n\n" + status + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.table.View()) + "\n" + status + help
}
