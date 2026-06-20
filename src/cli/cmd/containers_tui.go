package cmd

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// The Containers TUI is a read + delete browser over the image registry:
// repositories → tags → manifest. Push/pull are not proxied (users push with
// docker/podman directly), so there is no create. Deleting a tag deletes the
// underlying manifest by digest — the API has no delete-by-tag, so a delete
// resolves the tag's manifest first to obtain its digest.

type ctViewID int

const (
	ctViewRepos ctViewID = iota
	ctViewTags
	ctViewManifest
)

type ctReposMsg []string
type ctTagsMsg struct {
	repo string
	tags []string
}
type ctManifestMsg struct{ content string }
type ctErrMsg struct{ err error }
type ctDoneMsg struct{ status string }

type containersModel struct {
	view    ctViewID
	loading bool
	err     error
	width   int
	height  int

	repos []string
	tags  []string
	repo  string // selected "namespace/image"

	rTable table.Model
	tTable table.Model
	vp     viewport.Model

	pending   *frPending
	status    string
	statusErr bool
}

var ctRepoCols = []tuiColSpec{{"REPOSITORY", 40, 2}}
var ctTagCols = []tuiColSpec{{"TAG", 40, 2}}

func newContainersModel() containersModel {
	rt := table.New(table.WithFocused(true))
	rt.SetStyles(tuiTableStyles())
	tt := table.New(table.WithFocused(true))
	tt.SetStyles(tuiTableStyles())
	m := containersModel{
		loading: true, width: tuiDefaultWidth, height: tuiDefaultHeight,
		rTable: rt, tTable: tt, vp: viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8),
	}
	m.applyTableLayout()
	return m
}

func (m *containersModel) applyTableLayout() {
	m.rTable.SetColumns(tuiFitColumns(ctRepoCols, m.width))
	m.rTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
	m.tTable.SetColumns(tuiFitColumns(ctTagCols, m.width))
	m.tTable.SetHeight(tuiTableHeight(m.height, tuiListChrome))
}

// ctSplit splits a "namespace/image" repository name into its two path segments.
func ctSplit(repo string) (ns, img string) {
	ns, img, _ = strings.Cut(repo, "/")
	return ns, img
}

func ctRepoPath(repo string) string {
	ns, img := ctSplit(repo)
	return "/containers/repositories/" + ns + "/" + img
}

// ── Fetch / mutate ────────────────────────────────────────────────────────────

func ctFetchRepos() tea.Msg {
	data, err := doRequest("GET", "/containers/repositories", nil)
	if err != nil {
		return ctErrMsg{err}
	}
	var repos []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &repos); err != nil {
		return ctErrMsg{err}
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return ctReposMsg(names)
}

func ctFetchTags(repo string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", ctRepoPath(repo)+"/tags", nil)
		if err != nil {
			return ctErrMsg{err}
		}
		var tl struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(data, &tl); err != nil {
			return ctErrMsg{err}
		}
		return ctTagsMsg{repo: repo, tags: tl.Tags}
	}
}

func ctFetchManifest(repo, tag string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", ctRepoPath(repo)+"/manifests/"+tag, nil)
		if err != nil {
			return ctErrMsg{err}
		}
		var pretty any
		if json.Unmarshal(data, &pretty) == nil {
			if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
				return ctManifestMsg{content: string(b)}
			}
		}
		return ctManifestMsg{content: string(data)}
	}
}

// ctDeleteTag resolves a tag to its manifest digest, then deletes by digest
// (the registry has no delete-by-tag).
func ctDeleteTag(repo, tag string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", ctRepoPath(repo)+"/manifests/"+tag, nil)
		if err != nil {
			return ctErrMsg{err}
		}
		var man struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(data, &man); err != nil || man.Digest == "" {
			return ctErrMsg{errEmptyDigest(tag)}
		}
		if _, err := doRequest("DELETE", ctRepoPath(repo)+"/manifests/"+man.Digest, nil); err != nil {
			return ctErrMsg{err}
		}
		return ctDoneMsg{status: "✓ deleted " + tag}
	}
}

type ctNoDigestErr struct{ tag string }

func (e ctNoDigestErr) Error() string {
	return "could not resolve a digest for tag " + e.tag + " (nothing deleted)"
}
func errEmptyDigest(tag string) error { return ctNoDigestErr{tag} }

// ── Init / Update ─────────────────────────────────────────────────────────────

func (m containersModel) Init() tea.Cmd { return ctFetchRepos }

func (m containersModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil
	case ctErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case ctReposMsg:
		m.loading = false
		m.repos = []string(msg)
		rows := make([]table.Row, len(m.repos))
		for i, r := range m.repos {
			rows[i] = table.Row{r}
		}
		m.rTable.SetRows(rows)
		return m, nil
	case ctTagsMsg:
		m.loading = false
		m.repo = msg.repo
		m.tags = msg.tags
		rows := make([]table.Row, len(m.tags))
		for i, tg := range m.tags {
			rows[i] = table.Row{tg}
		}
		m.tTable.SetRows(rows)
		m.view = ctViewTags
		return m, nil
	case ctManifestMsg:
		m.loading = false
		m.vp.SetContent(msg.content)
		m.vp.GotoTop()
		m.view = ctViewManifest
		return m, nil
	case ctDoneMsg:
		m.status = msg.status
		m.statusErr = false
		m.loading = true
		return m, ctFetchTags(m.repo)
	case tuiAutoRefreshMsg:
		switch m.view {
		case ctViewRepos:
			return m, ctFetchRepos
		case ctViewTags:
			if m.repo != "" {
				return m, ctFetchTags(m.repo)
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
				return m, ctFetchRepos
			}
			return m, nil
		}
		switch m.view {
		case ctViewRepos:
			return m.keyRepos(msg)
		case ctViewTags:
			return m.keyTags(msg)
		case ctViewManifest:
			return m.keyManifest(msg)
		}
	}
	var cmd tea.Cmd
	switch m.view {
	case ctViewRepos:
		m.rTable, cmd = m.rTable.Update(msg)
	case ctViewTags:
		m.tTable, cmd = m.tTable.Update(msg)
	case ctViewManifest:
		m.vp, cmd = m.vp.Update(msg)
	}
	return m, cmd
}

func (m containersModel) keyRepos(msg tea.KeyMsg) (containersModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if i := m.rTable.Cursor(); i >= 0 && i < len(m.repos) {
			m.loading = true
			m.status = ""
			return m, ctFetchTags(m.repos[i])
		}
	case "r":
		m.loading = true
		return m, ctFetchRepos
	}
	var cmd tea.Cmd
	m.rTable, cmd = m.rTable.Update(msg)
	return m, cmd
}

func (m containersModel) keyTags(msg tea.KeyMsg) (containersModel, tea.Cmd) {
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
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = ctViewRepos
		m.status = ""
		return m, nil
	case "enter":
		if i := m.tTable.Cursor(); i >= 0 && i < len(m.tags) {
			m.loading = true
			return m, ctFetchManifest(m.repo, m.tags[i])
		}
	case "D":
		if i := m.tTable.Cursor(); i >= 0 && i < len(m.tags) {
			tag := m.tags[i]
			m.pending = &frPending{
				prompt: "Delete tag " + m.repo + ":" + tag + " (by digest)? [y] confirm  [any] cancel",
				run:    ctDeleteTag(m.repo, tag),
			}
			return m, nil
		}
	case "r":
		m.loading = true
		return m, ctFetchTags(m.repo)
	}
	var cmd tea.Cmd
	m.tTable, cmd = m.tTable.Update(msg)
	return m, cmd
}

func (m containersModel) keyManifest(msg tea.KeyMsg) (containersModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = ctViewTags
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m containersModel) View() string {
	if m.err != nil {
		return tuiErrStyle.Render("error: "+m.err.Error()) + "\n\n" + tuiHelpStyle.Render("[esc] home/back  [r] retry")
	}
	switch m.view {
	case ctViewManifest:
		title := tuiTitleStyle.Render("Manifest") + "  " + tuiMetaStyle.Render(m.repo)
		help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
		if m.loading {
			return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
		}
		return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
	case ctViewTags:
		title := tuiTitleStyle.Render("Tags") + "  " + tuiMetaStyle.Render(m.repo)
		help := tuiHelp("[↑↓/jk] nav  [enter] manifest  [D] delete tag  [r] refresh  [esc] back", m.width)
		if m.loading {
			return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
		}
		if m.pending != nil {
			body := tuiMetaStyle.Render("No tags.")
			if len(m.tags) > 0 {
				body = tuiBoxStyle.Render(m.tTable.View())
			}
			return title + "\n" + body + "\n" + tuiErrStyle.Render(m.pending.prompt)
		}
		status := ""
		if m.status != "" {
			status = tuiMetaStyle.Render(m.status) + "\n"
		}
		if len(m.tags) == 0 {
			return title + "\n\n" + tuiMetaStyle.Render("No tags.") + "\n\n" + status + help
		}
		return title + "\n" + tuiBoxStyle.Render(m.tTable.View()) + "\n" + status + help
	}
	title := tuiTitleStyle.Render("Container Repositories")
	help := tuiHelp("[↑↓/jk] nav  [enter] tags  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.repos) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No repositories.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" + help
}
