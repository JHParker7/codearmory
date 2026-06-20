package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// The Repos TUI is a Gitea/Forgejo browser: list/create/delete repositories,
// drill into a repo's branches/tags/commits/pull-requests, create and merge PRs,
// and link/unlink the backing Gitea account. It mirrors the `armory repos`
// endpoints. Repos are addressed by full_name ("owner/name").

// ── API types (subset we render) ───────────────────────────────────────────────

type rpRepo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

type rpBranch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}
type rpTag struct {
	Name string `json:"name"`
}
type rpCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}
type rpPRBranch struct {
	Ref string `json:"ref"`
}
type rpPull struct {
	Number int64      `json:"number"`
	Title  string     `json:"title"`
	State  string     `json:"state"`
	Merged bool       `json:"merged"`
	Head   rpPRBranch `json:"head"`
	Base   rpPRBranch `json:"base"`
}

// ── Views ───────────────────────────────────────────────────────────────────

type rpView int

const (
	rpViewRepos rpView = iota
	rpViewRepo         // tabbed branches/tags/commits/pulls
	rpViewForm
	rpViewAccount
	rpViewPR // PR detail
)

type rpSection int

const (
	rpBranches rpSection = iota
	rpTags
	rpCommits
	rpPulls
)

var rpSectionNames = []string{"Branches", "Tags", "Commits", "Pulls"}

type rpFormKind int

const (
	rpFormRepo rpFormKind = iota
	rpFormPR
	rpFormLink
)

// ── Messages ──────────────────────────────────────────────────────────────────

type rpReposMsg []rpRepo
type rpBranchesMsg []rpBranch
type rpTagsMsg []rpTag
type rpCommitsMsg []rpCommit
type rpPullsMsg []rpPull
type rpAccountMsg struct {
	username string
	linked   bool
}
type rpErrMsg struct{ err error }
type rpSectionErrMsg struct{ err error } // a section fetch failed; keep the repo view
type rpDoneMsg struct{ status string }   // mutation succeeded → refresh repos
type rpFormErrMsg struct{ err error }
type rpPRDetailMsg struct{ content string }

// ── Model ──────────────────────────────────────────────────────────────────────

type reposModel struct {
	view    rpView
	loading bool
	err     error
	width   int
	height  int

	repos   []rpRepo
	selRepo *rpRepo

	section  rpSection
	branches []rpBranch
	tags     []rpTag
	commits  []rpCommit
	pulls    []rpPull

	rTable table.Model
	sTable table.Model
	vp     viewport.Model

	form     tuiForm
	formKind rpFormKind

	account rpAccountMsg

	pending   *frPending
	status    string
	statusErr bool
}

var rpRepoCols = []tuiColSpec{{"NAME", 20, 2}, {"PRIVATE", 8, 0}, {"DEFAULT", 12, 1}, {"DESCRIPTION", 24, 2}}

func newReposModel() reposModel {
	rt := table.New(table.WithFocused(true))
	rt.SetStyles(tuiTableStyles())
	st := table.New(table.WithFocused(true))
	st.SetStyles(tuiTableStyles())
	m := reposModel{
		loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight,
		rTable: rt, sTable: st, vp: viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-9),
	}
	m.applyTableLayout()
	return m
}

func (m *reposModel) applyTableLayout() {
	m.rTable.SetColumns(tuiFitColumns(rpRepoCols, m.width))
	m.rTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
	m.sTable.SetColumns(tuiFitColumns(rpSectionCols(m.section), m.width))
	m.sTable.SetHeight(tuiTableHeight(m.height, tuiListChrome+1)) // +1 for the section bar
}

func rpSectionCols(s rpSection) []tuiColSpec {
	switch s {
	case rpBranches:
		return []tuiColSpec{{"BRANCH", 28, 2}, {"PROTECTED", 10, 0}}
	case rpTags:
		return []tuiColSpec{{"TAG", 40, 2}}
	case rpCommits:
		return []tuiColSpec{{"SHA", 10, 0}, {"MESSAGE", 36, 2}, {"AUTHOR", 14, 1}}
	default: // rpPulls
		return []tuiColSpec{{"#", 5, 0}, {"TITLE", 30, 2}, {"STATE", 8, 0}, {"HEAD→BASE", 20, 1}}
	}
}

// rpSplit splits "owner/name" (a repo full_name) into its parts.
func rpSplit(fullName string) (owner, name string) {
	owner, name, _ = strings.Cut(fullName, "/")
	return owner, name
}

func rpBase(r *rpRepo) string {
	owner, name := rpSplit(r.FullName)
	return "/gitea_integration/repos/" + owner + "/" + name
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func rpFetchRepos() tea.Msg {
	data, err := doRequest("GET", "/gitea_integration/repos", nil)
	if err != nil {
		return rpErrMsg{err}
	}
	var repos []rpRepo
	if err := json.Unmarshal(data, &repos); err != nil {
		return rpErrMsg{err}
	}
	return rpReposMsg(repos)
}

func rpFetchAccount() tea.Msg {
	data, err := doRequest("GET", "/gitea_integration/account", nil)
	if err != nil {
		return rpAccountMsg{linked: false}
	}
	var a struct {
		GiteaUsername string `json:"gitea_username"`
	}
	if err := json.Unmarshal(data, &a); err != nil || a.GiteaUsername == "" {
		return rpAccountMsg{linked: false}
	}
	return rpAccountMsg{username: a.GiteaUsername, linked: true}
}

// rpFetchSection fetches the data for a repo's active section.
func rpFetchSection(base string, s rpSection) tea.Cmd {
	return func() tea.Msg {
		path := base + "/" + map[rpSection]string{rpBranches: "branches", rpTags: "tags", rpCommits: "commits", rpPulls: "pulls"}[s]
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return rpSectionErrMsg{err}
		}
		switch s {
		case rpBranches:
			var v []rpBranch
			if err := json.Unmarshal(data, &v); err != nil {
				return rpSectionErrMsg{err}
			}
			return rpBranchesMsg(v)
		case rpTags:
			var v []rpTag
			if err := json.Unmarshal(data, &v); err != nil {
				return rpSectionErrMsg{err}
			}
			return rpTagsMsg(v)
		case rpCommits:
			var v []rpCommit
			if err := json.Unmarshal(data, &v); err != nil {
				return rpSectionErrMsg{err}
			}
			return rpCommitsMsg(v)
		default:
			var v []rpPull
			if err := json.Unmarshal(data, &v); err != nil {
				return rpSectionErrMsg{err}
			}
			return rpPullsMsg(v)
		}
	}
}

func rpCreateRepo(name, desc string, private bool) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{"name": name, "private": private}
		if desc != "" {
			payload["description"] = desc
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest("POST", "/gitea_integration/repos", body); err != nil {
			return rpFormErrMsg{err}
		}
		return rpDoneMsg{status: "✓ repo created"}
	}
}

func rpDeleteRepo(r rpRepo) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", rpBase(&r), nil); err != nil {
			return rpErrMsg{err}
		}
		return rpDoneMsg{status: "✓ deleted " + r.FullName}
	}
}

func rpCreatePR(base, title, head, baseBranch, body string) tea.Cmd {
	return func() tea.Msg {
		payload := map[string]any{"title": title, "head": head, "base": baseBranch}
		if body != "" {
			payload["body"] = body
		}
		b, _ := json.Marshal(payload)
		if _, err := doRequest("POST", base+"/pulls", b); err != nil {
			return rpFormErrMsg{err}
		}
		return rpDoneMsg{status: "✓ pull request created"}
	}
}

func rpMergePR(base string, number int64) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("POST", fmt.Sprintf("%s/pulls/%d/merge", base, number), nil); err != nil {
			return rpErrMsg{err}
		}
		return rpDoneMsg{status: fmt.Sprintf("✓ merged PR #%d", number)}
	}
}

func rpLinkAccount(token string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"token": token})
		if _, err := doRequest("PUT", "/gitea_integration/account", body); err != nil {
			return rpFormErrMsg{err}
		}
		return rpDoneMsg{status: "✓ account linked"}
	}
}

func rpUnlinkAccount() tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/gitea_integration/account", nil); err != nil {
			return rpErrMsg{err}
		}
		return rpDoneMsg{status: "✓ account unlinked"}
	}
}

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m reposModel) Init() tea.Cmd { return rpFetchRepos }

func (m reposModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 9
		m.applyTableLayout()
		return m, nil
	case rpErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case rpReposMsg:
		m.loading = false
		m.repos = []rpRepo(msg)
		rows := make([]table.Row, len(m.repos))
		for i, r := range m.repos {
			rows[i] = table.Row{r.Name, frBoolDotBool(r.Private), frDash(r.DefaultBranch), r.Description}
		}
		m.rTable.SetRows(rows)
		return m, nil
	case rpAccountMsg:
		m.account = msg
		return m, nil
	case rpSectionErrMsg:
		m.loading = false
		m.status = "✗ " + msg.err.Error()
		m.statusErr = true
		return m, nil
	case rpBranchesMsg:
		m.loading = false
		m.branches = []rpBranch(msg)
		m.refreshSection()
		return m, nil
	case rpTagsMsg:
		m.loading = false
		m.tags = []rpTag(msg)
		m.refreshSection()
		return m, nil
	case rpCommitsMsg:
		m.loading = false
		m.commits = []rpCommit(msg)
		m.refreshSection()
		return m, nil
	case rpPullsMsg:
		m.loading = false
		m.pulls = []rpPull(msg)
		m.refreshSection()
		return m, nil
	case rpPRDetailMsg:
		m.loading = false
		m.vp.SetContent(msg.content)
		m.vp.GotoTop()
		m.view = rpViewPR
		return m, nil
	case rpDoneMsg:
		m.status = msg.status
		m.statusErr = false
		return m.afterMutation()
	case rpFormErrMsg:
		m.form.errMsg = msg.err.Error()
		return m, nil
	case tuiAutoRefreshMsg:
		if m.view == rpViewRepos {
			return m, rpFetchRepos
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
				return m, rpFetchRepos
			}
			return m, nil
		}
		switch m.view {
		case rpViewRepos:
			return m.keyRepos(msg)
		case rpViewRepo:
			return m.keyRepo(msg)
		case rpViewForm:
			return m.keyForm(msg)
		case rpViewAccount:
			return m.keyAccount(msg)
		case rpViewPR:
			return m.keyPR(msg)
		}
	}
	return m.delegate(msg)
}

func (m reposModel) delegate(msg tea.Msg) (reposModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case rpViewRepos:
		m.rTable, cmd = m.rTable.Update(msg)
	case rpViewRepo:
		m.sTable, cmd = m.sTable.Update(msg)
	case rpViewForm:
		m.form, _, cmd = m.form.update(msg)
	case rpViewPR:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

// afterMutation routes back to the right view after a create/delete/merge/link
// succeeds and refreshes that view's data.
func (m reposModel) afterMutation() (tea.Model, tea.Cmd) {
	switch m.view {
	case rpViewForm:
		switch m.formKind {
		case rpFormPR:
			m.view = rpViewRepo
			m.loading = true
			return m, rpFetchSection(rpBase(m.selRepo), rpPulls)
		case rpFormLink:
			m.view = rpViewAccount
			return m, rpFetchAccount
		default: // rpFormRepo
			m.view = rpViewRepos
			m.loading = true
			return m, rpFetchRepos
		}
	case rpViewAccount: // unlink succeeded
		return m, rpFetchAccount
	case rpViewRepo: // merge succeeded → refresh the pulls section
		m.loading = true
		return m, rpFetchSection(rpBase(m.selRepo), m.section)
	default: // delete from the repos list
		m.loading = true
		return m, rpFetchRepos
	}
}

func (m *reposModel) refreshSection() {
	m.sTable.SetColumns(tuiFitColumns(rpSectionCols(m.section), m.width))
	var rows []table.Row
	switch m.section {
	case rpBranches:
		rows = make([]table.Row, len(m.branches))
		for i, b := range m.branches {
			rows[i] = table.Row{b.Name, frBoolDotBool(b.Protected)}
		}
	case rpTags:
		rows = make([]table.Row, len(m.tags))
		for i, t := range m.tags {
			rows[i] = table.Row{t.Name}
		}
	case rpCommits:
		rows = make([]table.Row, len(m.commits))
		for i, c := range m.commits {
			rows[i] = table.Row{rpShortSHA(c.SHA), rpFirstLine(c.Commit.Message), c.Commit.Author.Name}
		}
	case rpPulls:
		rows = make([]table.Row, len(m.pulls))
		for i, p := range m.pulls {
			rows[i] = table.Row{fmt.Sprintf("%d", p.Number), p.Title, rpPRState(p), p.Head.Ref + "→" + p.Base.Ref}
		}
	}
	m.sTable.SetRows(rows)
}

func rpShortSHA(s string) string {
	if len(s) > 9 {
		return s[:9]
	}
	return s
}
func rpFirstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}
func rpPRState(p rpPull) string {
	if p.Merged {
		return "merged"
	}
	return p.State
}

func (m reposModel) keyRepos(msg tea.KeyMsg) (reposModel, tea.Cmd) {
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
		if i := m.rTable.Cursor(); i >= 0 && i < len(m.repos) {
			m.selRepo = &m.repos[i]
			m.section = rpBranches
			m.view = rpViewRepo
			m.loading = true
			m.status = ""
			return m, rpFetchSection(rpBase(m.selRepo), rpBranches)
		}
	case "n":
		m.formKind = rpFormRepo
		m.form, _ = newTUIForm("New Repository",
			formInput("name", "Name", "my-project (required)"),
			formInput("desc", "Description", "(optional)"),
			formSelect("private", "Private", []string{"false", "true"}))
		m.view = rpViewForm
		return m, nil
	case "D":
		if i := m.rTable.Cursor(); i >= 0 && i < len(m.repos) {
			r := m.repos[i]
			m.pending = &frPending{prompt: "Delete repo " + r.FullName + "? [y] confirm  [any] cancel", run: rpDeleteRepo(r)}
			return m, nil
		}
	case "A":
		m.view = rpViewAccount
		return m, rpFetchAccount
	case "r":
		m.loading = true
		m.status = ""
		return m, rpFetchRepos
	}
	var cmd tea.Cmd
	m.rTable, cmd = m.rTable.Update(msg)
	return m, cmd
}

func (m reposModel) keyRepo(msg tea.KeyMsg) (reposModel, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		if s := msg.String(); s == "y" || s == "Y" {
			return m, run
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = rpViewRepos
		m.status = ""
		return m, nil
	case "tab", "]":
		m.section = (m.section + 1) % 4
		m.loading = true
		return m, rpFetchSection(rpBase(m.selRepo), m.section)
	case "shift+tab", "[":
		m.section = (m.section + 3) % 4
		m.loading = true
		return m, rpFetchSection(rpBase(m.selRepo), m.section)
	case "1", "2", "3", "4":
		m.section = rpSection(msg.String()[0] - '1')
		m.loading = true
		return m, rpFetchSection(rpBase(m.selRepo), m.section)
	case "r":
		m.loading = true
		return m, rpFetchSection(rpBase(m.selRepo), m.section)
	case "enter":
		if m.section == rpPulls {
			if i := m.sTable.Cursor(); i >= 0 && i < len(m.pulls) {
				p := m.pulls[i]
				pretty, _ := json.MarshalIndent(p, "", "  ")
				return m, func() tea.Msg { return rpPRDetailMsg{content: string(pretty)} }
			}
		}
	case "n":
		if m.section == rpPulls {
			m.formKind = rpFormPR
			base := m.selRepo.DefaultBranch
			m.form, _ = newTUIForm("New Pull Request",
				formInput("title", "Title", "Fix bug (required)"),
				formInput("head", "Head", "feature-branch (required)"),
				formInputDefault("base", "Base", "main (required)", base),
				formTextarea("body", "Body", "(optional)"))
			m.view = rpViewForm
			return m, nil
		}
	case "m":
		if m.section == rpPulls {
			if i := m.sTable.Cursor(); i >= 0 && i < len(m.pulls) {
				p := m.pulls[i]
				m.pending = &frPending{prompt: fmt.Sprintf("Merge PR #%d %q? [y] confirm  [any] cancel", p.Number, p.Title), run: rpMergePR(rpBase(m.selRepo), p.Number)}
				return m, nil
			}
		}
	}
	var cmd tea.Cmd
	m.sTable, cmd = m.sTable.Update(msg)
	return m, cmd
}

func (m reposModel) keyForm(msg tea.KeyMsg) (reposModel, tea.Cmd) {
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
		switch m.formKind {
		case rpFormPR:
			m.view = rpViewRepo
		case rpFormLink:
			m.view = rpViewAccount
		default:
			m.view = rpViewRepos
		}
		return m, nil
	case formSubmit:
		return m.submitForm()
	}
	return m, cmd
}

func (m reposModel) submitForm() (reposModel, tea.Cmd) {
	switch m.formKind {
	case rpFormRepo:
		name := m.form.value("name")
		if name == "" {
			m.form.errMsg = "name is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, rpCreateRepo(name, m.form.value("desc"), m.form.value("private") == "true")
	case rpFormPR:
		title, head, base := m.form.value("title"), m.form.value("head"), m.form.value("base")
		switch {
		case title == "":
			m.form.errMsg = "title is required"
		case head == "":
			m.form.errMsg = "head branch is required"
		case base == "":
			m.form.errMsg = "base branch is required"
		default:
			m.form.errMsg = ""
			return m, rpCreatePR(rpBase(m.selRepo), title, head, base, m.form.value("body"))
		}
		return m, nil
	case rpFormLink:
		token := m.form.value("token")
		if token == "" {
			m.form.errMsg = "token is required"
			return m, nil
		}
		m.form.errMsg = ""
		return m, rpLinkAccount(token)
	}
	return m, nil
}

func (m reposModel) keyAccount(msg tea.KeyMsg) (reposModel, tea.Cmd) {
	if m.pending != nil {
		run := m.pending.run
		m.pending = nil
		if s := msg.String(); s == "y" || s == "Y" {
			return m, run
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = rpViewRepos
		return m, nil
	case "l":
		m.formKind = rpFormLink
		m.form, _ = newTUIForm("Link Gitea Account",
			formInput("token", "Token", "Gitea personal access token (required)"))
		m.view = rpViewForm
		return m, nil
	case "u":
		if m.account.linked {
			m.pending = &frPending{prompt: "Unlink Gitea account? [y] confirm  [any] cancel", run: rpUnlinkAccount()}
		}
		return m, nil
	}
	return m, nil
}

func (m reposModel) keyPR(msg tea.KeyMsg) (reposModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = rpViewRepo
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m reposModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home  [r] retry")
	}
	switch m.view {
	case rpViewForm:
		return m.form.view(m.width, m.height)
	case rpViewAccount:
		return m.viewAccount()
	case rpViewPR:
		title := tuiTitleStyle.Render("Pull Request")
		return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	case rpViewRepo:
		return m.viewRepo()
	}
	return m.viewRepos()
}

func (m reposModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErrStyle.Render(m.status) + "\n"
	}
	return tuiMetaStyle.Render(m.status) + "\n"
}

func (m reposModel) viewRepos() string {
	title := tuiTitleStyle.Render("Repositories")
	help := tuiHelp("[↑↓/jk] nav  [enter] open  [n] new  [D] delete  [A] account  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		body := tuiMetaStyle.Render("No repositories.")
		if len(m.repos) > 0 {
			body = tuiBoxStyle.Render(m.rTable.View())
		}
		return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if len(m.repos) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No repositories. Press [n] to create one, or [A] to link an account.") + "\n\n" + m.statusLine() + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + m.statusLine() + help
}

func (m reposModel) viewRepo() string {
	name := ""
	if m.selRepo != nil {
		name = m.selRepo.FullName
	}
	bar := rpSectionBar(m.section)
	title := tuiTitleStyle.Render(name) + "  " + bar
	pullsHelp := ""
	if m.section == rpPulls {
		pullsHelp = "[enter] detail  [n] new PR  [m] merge  "
	}
	help := tuiHelp("[tab] section  [↑↓/jk] nav  "+pullsHelp+"[r] refresh  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.pending != nil {
		return title + "\n" + tuiBoxStyle.Render(m.sTable.View()) + "\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	if m.sTable.Rows() == nil || len(m.sTable.Rows()) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No "+strings.ToLower(rpSectionNames[m.section])+".") + "\n\n" + m.statusLine() + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.sTable.View()) + "\n" + m.statusLine() + help
}

func rpSectionBar(active rpSection) string {
	parts := make([]string, len(rpSectionNames))
	for i, n := range rpSectionNames {
		if rpSection(i) == active {
			parts[i] = tuiTitleStyle.Render(n)
		} else {
			parts[i] = tuiMetaStyle.Render(n)
		}
	}
	return strings.Join(parts, tuiMetaStyle.Render(" · "))
}

func (m reposModel) viewAccount() string {
	title := tuiTitleStyle.Render("Gitea Account")
	var body string
	help := tuiHelp("[l] link  [esc] back", m.width)
	if m.account.linked {
		body = tuiMetaStyle.Render("Linked as ") + m.account.username
		help = tuiHelp("[l] relink  [u] unlink  [esc] back", m.width)
	} else {
		body = tuiMetaStyle.Render("No Gitea account linked. Press [l] to link with a personal access token.")
	}
	if m.pending != nil {
		return title + "\n\n" + body + "\n\n" + tuiErrStyle.Render(m.pending.prompt)
	}
	return title + "\n\n" + body + "\n\n" + m.statusLine() + help
}
