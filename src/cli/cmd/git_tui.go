package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// The Git TUI is a credential-broker browser: list/register/delete git
// backends, test a backend's connection, and mint short-lived clone
// credentials for a repo URL. It mirrors the `armory git` endpoints. Secrets are
// never shown in backend reads; the test result and minted credentials are the
// only live-secret surfaces, and the minted secret is redacted until revealed.

// ── API types (subset we render) ───────────────────────────────────────────────

type gtBackend struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	BaseURL  string `json:"base_url"`
	Host     string `json:"host"`
	AuthMode string `json:"auth_mode"`
}

type gtTestResult struct {
	OK          bool   `json:"ok"`
	BackendType string `json:"backend_type"`
	AuthMode    string `json:"auth_mode"`
	ExpiresAt   string `json:"expires_at"`
}

// gitCreds is the minted clone-credential payload (shared with the cobra `creds`
// command). The secret is redacted before display unless the user reveals it.
type gitCreds struct {
	Type        string `json:"type"`
	Username    string `json:"username"`
	Secret      string `json:"secret"`
	CloneURL    string `json:"clone_url"`
	Backend     string `json:"backend"`
	BackendType string `json:"backend_type"`
	ExpiresAt   string `json:"expires_at"`
}

// gtTypes / gtAuthModes drive the create form's name-selectors. The auth-mode
// selector lists every mode across types; validation on submit rejects a mode
// that doesn't pair with the chosen type, so the user need not memorise the
// matrix.
var gtTypes = []string{"github", "gitlab", "forgejo", "generic"}
var gtAuthModes = []string{"app", "pat", "token", "oauth", "admin", "basic"}

// ── Views ───────────────────────────────────────────────────────────────────

type gtView int

const (
	gtViewList gtView = iota
	gtViewForm
	gtViewResult // test result / minted credentials, rendered in a viewport
)

type gtFormKind int

const (
	gtFormBackend gtFormKind = iota
	gtFormCreds
)

// ── Messages ──────────────────────────────────────────────────────────────────

type gtBackendsMsg []gtBackend
type gtReposMsg kvCatalog
type gtErrMsg struct{ err error }
type gtDoneMsg struct{ status string }           // mutation succeeded → refresh list
type gtResultMsg struct{ content, title string } // test / creds result to display
type gtFormErrMsg struct{ err error }
type gtCredsMsg struct{ creds gitCreds } // minted creds awaiting redacted render

// ── Model ──────────────────────────────────────────────────────────────────────

type gitModel struct {
	view    gtView
	loading bool
	err     error
	width   int
	height  int

	backends []gtBackend
	// repos backs the mint form's repo name picker (shows the repo name, submits
	// the clone URL); degrades to a free-text URL when empty/unavailable.
	repos kvCatalog

	bTable table.Model
	vp     viewport.Model

	form     tuiForm
	formKind gtFormKind

	// creds holds the most recently minted credentials so the result view can
	// toggle the secret between redacted and revealed without re-fetching.
	creds         gitCreds
	credsRevealed bool
	resultTitle   string

	pending   *frPending
	status    string
	statusErr bool
}

var gtBackendCols = []tuiColSpec{{"NAME", 18, 2}, {"TYPE", 10, 1}, {"HOST", 24, 2}, {"AUTH", 10, 1}}

func newGitModel() gitModel {
	bt := table.New(table.WithFocused(true))
	bt.SetStyles(tuiTableStyles())
	m := gitModel{
		loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight,
		bTable: bt, vp: viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
	}
	m.applyTableLayout()
	return m
}

func (m *gitModel) applyTableLayout() {
	m.bTable.SetColumns(tuiFitColumns(gtBackendCols, m.width))
	m.bTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func gtFetchBackends() tea.Msg {
	data, err := doRequest("GET", "/git/backends", nil)
	if err != nil {
		return gtErrMsg{err}
	}
	var backends []gtBackend
	if err := json.Unmarshal(data, &backends); err != nil {
		return gtErrMsg{err}
	}
	return gtBackendsMsg(backends)
}

// fetchGitRepos loads the git-service repo list and builds a name→URL kvCatalog
// (label = repo name, value = HTTPS clone URL) for the name pickers in the git,
// forge, and steps TUIs. On any failure it returns an empty catalog so the
// callers degrade to a free-text URL field rather than erroring.
func fetchGitRepos() kvCatalog {
	data, err := doRequest("GET", "/git/repos", nil)
	if err != nil {
		return kvCatalog{}
	}
	var repos []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(data, &repos); err != nil {
		return kvCatalog{}
	}
	var cat kvCatalog
	for _, r := range repos {
		if r.URL == "" {
			continue
		}
		label := r.Name
		if label == "" {
			label = r.URL
		}
		cat.labels = append(cat.labels, label)
		cat.values = append(cat.values, r.URL)
	}
	return cat
}

// gtFetchRepos loads the repo catalog for the mint form's name picker.
func gtFetchRepos() tea.Msg { return gtReposMsg(fetchGitRepos()) }

func gtCreateBackend(payload map[string]any) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/git/backends", body); err != nil {
			return gtFormErrMsg{err}
		}
		return gtDoneMsg{status: "✓ backend created"}
	}
}

func gtDeleteBackend(b gtBackend) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/git/backends/"+b.ID, nil); err != nil {
			return gtErrMsg{err}
		}
		return gtDoneMsg{status: "✓ deleted " + b.Name}
	}
}

func gtTestBackend(b gtBackend) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("POST", "/git/backends/"+b.ID+"/test", nil)
		if err != nil {
			return gtErrMsg{err}
		}
		var r gtTestResult
		if err := json.Unmarshal(data, &r); err != nil {
			return gtResultMsg{title: "Test: " + b.Name, content: string(data)}
		}
		status := "✗ failed"
		if r.OK {
			status = "✓ ok"
		}
		content := fmt.Sprintf("connection: %s\nbackend_type: %s\nauth_mode: %s\nexpires_at: %s",
			status, frDash(r.BackendType), frDash(r.AuthMode), frDash(r.ExpiresAt))
		return gtResultMsg{title: "Test: " + b.Name, content: content}
	}
}

func gtMintCreds(repoURL string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"repo_url": repoURL})
		data, err := doRequest("POST", "/git/credentials", body)
		if err != nil {
			return gtFormErrMsg{err}
		}
		var c gitCreds
		if err := json.Unmarshal(data, &c); err != nil {
			return gtFormErrMsg{err}
		}
		return gtCredsMsg{creds: c}
	}
}

// gtCredsContent renders the minted credentials, redacting the secret unless
// revealed.
func gtCredsContent(c gitCreds, revealed bool) string {
	secret := "(redacted — press [s] to reveal)"
	if revealed {
		secret = c.Secret
	}
	return fmt.Sprintf("clone_url: %s\nbackend: %s (%s)\ntype: %s\nusername: %s\nsecret: %s\nexpires_at: %s",
		frDash(c.CloneURL), frDash(c.Backend), frDash(c.BackendType), frDash(c.Type),
		frDash(c.Username), secret, frDash(c.ExpiresAt))
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m gitModel) Init() tea.Cmd { return tea.Batch(gtFetchBackends, gtFetchRepos) }

func (m gitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 9
		m.applyTableLayout()
		return m, nil
	case gtErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case gtBackendsMsg:
		m.loading = false
		m.backends = []gtBackend(msg)
		rows := make([]table.Row, len(m.backends))
		for i, b := range m.backends {
			rows[i] = table.Row{b.Name, frDash(b.Type), frDash(b.Host), frDash(b.AuthMode)}
		}
		m.bTable.SetRows(rows)
		return m, nil
	case gtReposMsg:
		m.repos = kvCatalog(msg)
		// If the mint form was opened before the catalog landed, its Repo field fell
		// back to free-text; rebuild so it upgrades to the name picker, carrying the
		// entered URL across.
		if m.view == gtViewForm && m.formKind == gtFormCreds {
			old := m.form
			m.form, _ = newGitCredsForm(m.repos)
			m.form.setValues(map[string]string{"repo_url": old.value("repo_url")})
			m.form.errMsg = old.errMsg
		}
		return m, nil
	case gtResultMsg:
		m.loading = false
		m.resultTitle = msg.title
		m.vp.SetContent(msg.content)
		m.vp.GotoTop()
		m.view = gtViewResult
		return m, nil
	case gtCredsMsg:
		m.loading = false
		m.creds = msg.creds
		m.credsRevealed = false
		m.resultTitle = "Clone Credentials"
		m.vp.SetContent(gtCredsContent(m.creds, false))
		m.vp.GotoTop()
		m.view = gtViewResult
		return m, nil
	case gtDoneMsg:
		m.status = msg.status
		m.statusErr = false
		m.view = gtViewList
		m.loading = true
		return m, gtFetchBackends
	case gtFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == gtViewList {
			return m, gtFetchBackends
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
				return m, gtFetchBackends
			}
			return m, nil
		}
		switch m.view {
		case gtViewList:
			return m.keyList(msg)
		case gtViewForm:
			return m.keyForm(msg)
		case gtViewResult:
			return m.keyResult(msg)
		}
	}
	return m.delegate(msg)
}

func (m gitModel) delegate(msg tea.Msg) (gitModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case gtViewList:
		m.bTable, cmd = m.bTable.Update(msg)
	case gtViewForm:
		m.form, _, cmd = m.form.update(msg)
	case gtViewResult:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m gitModel) keyList(msg tea.KeyMsg) (gitModel, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		if s := msg.String(); s == "y" || s == "Y" {
			m.loading = true
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
		m.formKind = gtFormBackend
		m.form, _ = newTUIForm("New Git Backend",
			formInput("name", "Name", "my-backend (required)"),
			formSelect("type", "Type", gtTypes),
			formInput("base_url", "Base URL", "https://git.example.com (generic/self-hosted)"),
			formSelect("auth_mode", "Auth Mode", gtAuthModes),
			formInput("token", "Token", "pat / gitlab / forgejo token"),
			formInput("username", "Username", "forgejo / generic / github pat"),
			formInput("password", "Password", "generic basic auth"),
			formInput("app_id", "App ID", "github app id (number)"),
			formInput("installation_id", "Install ID", "github app installation id"),
			formTextarea("private_key", "Private Key", "github app PEM"),
			formInput("refresh_token", "Refresh Tok", "gitlab oauth refresh token"),
			formInput("client_id", "Client ID", "gitlab oauth client id"),
			formInput("client_secret", "Client Sec", "gitlab oauth client secret"),
			formInput("admin_token", "Admin Token", "forgejo admin token"))
		m.view = gtViewForm
		return m, nil
	case "c":
		m.formKind = gtFormCreds
		var cmd tea.Cmd
		m.form, cmd = newGitCredsForm(m.repos)
		m.view = gtViewForm
		// If the prefetch hasn't landed (or failed), the Repo field fell back to
		// free-text; fetch now so it upgrades to a picker once the catalog arrives.
		cmds := []tea.Cmd{cmd}
		if len(m.repos.values) == 0 {
			cmds = append(cmds, gtFetchRepos)
		}
		return m, tea.Batch(cmds...)
	case "t":
		if i := m.bTable.Cursor(); i >= 0 && i < len(m.backends) {
			m.loading = true
			m.status = ""
			return m, gtTestBackend(m.backends[i])
		}
	case "D":
		if i := m.bTable.Cursor(); i >= 0 && i < len(m.backends) {
			b := m.backends[i]
			m.pending = &frPending{prompt: "Delete backend " + b.Name + "? [y] confirm  [any] cancel", run: gtDeleteBackend(b)}
			return m, nil
		}
	case "r":
		m.loading = true
		m.status = ""
		return m, gtFetchBackends
	}
	var cmd tea.Cmd
	m.bTable, cmd = m.bTable.Update(msg)
	return m, cmd
}

// newGitCredsForm builds the mint-credentials form. The Repo field is a name→URL
// picker (formSelectKV: label = repo name, value = clone URL) when the repo
// catalog is loaded, degrading to a free-text URL input otherwise so the user can
// still mint for an arbitrary repo. The submitted value is always the clone URL,
// POSTed verbatim as {repo_url: <url>}.
func newGitCredsForm(repos kvCatalog) (tuiForm, tea.Cmd) {
	var repoField formField
	if len(repos.values) > 0 {
		repoField = formSelectKV("repo_url", "Repo", repos.labels, repos.values)
	} else {
		repoField = formInput("repo_url", "Repo URL", "https://github.com/owner/repo.git (required)")
	}
	return newTUIForm("Mint Clone Credentials", repoField)
}

func (m gitModel) keyForm(msg tea.KeyMsg) (gitModel, tea.Cmd) {
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
		m.view = gtViewList
		return m, nil
	case formSubmit:
		return m.submitForm()
	}
	return m, cmd
}

func (m gitModel) submitForm() (gitModel, tea.Cmd) {
	switch m.formKind {
	case gtFormCreds:
		repoURL := m.form.value("repo_url")
		if repoURL == "" {
			m.form.errMsg = "repo url is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, gtMintCreds(repoURL)
	default: // gtFormBackend
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		typ := m.form.value("type")
		mode := m.form.value("auth_mode")
		auth, err := gitBackendAuth(typ, mode, gitAuthFlags{
			mode:           mode,
			token:          m.form.value("token"),
			username:       m.form.value("username"),
			password:       m.form.value("password"),
			appID:          atoiOrZero(m.form.value("app_id")),
			installationID: atoiOrZero(m.form.value("installation_id")),
			privateKey:     m.form.value("private_key"),
			refreshToken:   m.form.value("refresh_token"),
			clientID:       m.form.value("client_id"),
			clientSecret:   m.form.value("client_secret"),
			adminToken:     m.form.value("admin_token"),
		})
		if err != nil {
			m.form.errMsg = err.Error()
			return m, nil
		}
		m.form.errMsg = ""
		payload := map[string]any{"name": name, "type": typ, "auth": auth}
		if bu := m.form.value("base_url"); bu != "" {
			payload["base_url"] = bu
		}
		return m, gtCreateBackend(payload)
	}
}

// atoiOrZero parses s as an int, returning 0 when empty or invalid (the auth
// builder reports the missing-field error when a required numeric is 0).
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func (m gitModel) keyResult(msg tea.KeyMsg) (gitModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = gtViewList
		return m, nil
	case "s":
		// Toggle secret reveal — only meaningful for minted credentials.
		if m.resultTitle == "Clone Credentials" {
			m.credsRevealed = !m.credsRevealed
			m.vp.SetContent(gtCredsContent(m.creds, m.credsRevealed))
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m gitModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case gtViewForm:
		return m.form.view(m.width, m.height)
	case gtViewResult:
		return m.viewResult()
	}
	return m.viewList()
}

func (m gitModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErrStyle.Render(m.status) + "\n"
	}
	return tuiMetaStyle.Render(m.status) + "\n"
}

func (m gitModel) viewList() string {
	title := tuiTitleStyle.Render("Git Backends")
	help := tuiHelp("[↑↓/jk] nav  [n] new  [t] test  [c] creds  [D] delete  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No backends.")
		if len(m.backends) > 0 {
			body = tuiBoxStyle.Render(m.bTable.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if len(m.backends) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No backends. Press [n] to register one.") + "\n\n" + m.statusLine() + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.bTable.View()) + "\n" + m.statusLine() + help
}

func (m gitModel) viewResult() string {
	title := tuiTitleStyle.Render(frDash(m.resultTitle))
	hint := "[↑↓/pgup/pgdn] scroll  [esc] back"
	if m.resultTitle == "Clone Credentials" {
		reveal := "[s] reveal secret"
		if m.credsRevealed {
			reveal = "[s] hide secret"
		}
		hint = reveal + "  " + hint
	}
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + tuiHelp(hint, m.width)
}
