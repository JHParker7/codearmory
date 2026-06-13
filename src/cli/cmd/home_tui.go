package cmd

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// goHomeMsg is sent by sub-TUIs when the user presses 'q' to return home.
type goHomeMsg struct{}

// launchMsg is sent by the home screen when the user selects an entry.
type launchMsg struct{ idx int }

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	homeTitleStyle    lipgloss.Style
	homeSubtitleStyle lipgloss.Style
	homeBoxStyle      lipgloss.Style
)

// buildHomeStyles rebuilds the home screen styles from the active theme.
func buildHomeStyles() {
	homeTitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Accent))

	homeSubtitleStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Subtle))

	homeBoxStyle = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border)).
		Padding(0, 2)
}

// ── App model (top-level container) ──────────────────────────────────────────

// appModel is the registry-driven TUI hub. It shows the home menu until the
// user opens a screen, then delegates every message to that screen until it
// sends goHomeMsg. The menu and dispatch are built from the module registry
// (hubScreens), so a new service appears here simply by registering a Module
// with Screens — no edits to this file.
type appModel struct {
	width   int
	height  int
	home    homeModel
	active  tea.Model   // currently-shown screen; nil means the home menu
	screens []HubScreen // home-menu screens, indexed by menu position
}

func newAppModel() appModel {
	screens := hubScreens()
	return appModel{home: newHomeModel(screens), screens: screens}
}

func (m appModel) Init() tea.Cmd { return nil }

// sized feeds the current terminal dimensions to a freshly-created sub-model so
// it lays out correctly the moment it is shown, rather than waiting for the
// next resize event (which may never come).
func (m appModel) sized(model tea.Model) tea.Model {
	if m.width <= 0 || m.height <= 0 {
		return model
	}
	next, _ := model.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	return next
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = ws.Width, ws.Height
		// fall through so the active view also receives the resize.
	}
	if _, ok := msg.(goHomeMsg); ok {
		m.active = nil
		m.home = m.sized(newHomeModel(m.screens)).(homeModel)
		return m, nil
	}
	if lm, ok := msg.(launchMsg); ok {
		if lm.idx < 0 || lm.idx >= len(m.screens) {
			return m, nil
		}
		m.active = m.sized(m.screens[lm.idx].New())
		return m, m.active.Init()
	}

	if m.active != nil {
		next, cmd := m.active.Update(msg)
		m.active = next
		return m, cmd
	}
	next, cmd := m.home.Update(msg)
	m.home = next.(homeModel)
	return m, cmd
}

func (m appModel) View() string {
	if m.active != nil {
		return m.active.View()
	}
	return m.home.View()
}

// ── Home model ────────────────────────────────────────────────────────────────

type homeEntry struct {
	name string
	desc string
}

type homeModel struct {
	entries []homeEntry
	cursor  int
	width   int
	height  int
}

// newHomeModel builds the launcher menu from the registry-provided screens, in
// the same order they are dispatched (launchMsg.idx indexes appModel.screens).
func newHomeModel(screens []HubScreen) homeModel {
	entries := make([]homeEntry, len(screens))
	for i, s := range screens {
		entries[i] = homeEntry{name: s.Title, desc: s.Desc}
	}
	return homeModel{entries: entries}
}

func (m homeModel) Init() tea.Cmd { return nil }

func (m homeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
		case "enter", " ":
			idx := m.cursor
			return m, func() tea.Msg { return launchMsg{idx} }
		}
	}
	return m, nil
}

// Home-menu column widths (display cells). Each row is built by plain string
// concatenation, so a name or description wider than its column would wrap to a
// second line starting at column 0 — under the name column. truncate (print.go)
// clips both to one line to prevent that.
const (
	homeNameWidth = 20
	homeDescWidth = 44
)

// homeRows renders one display line per menu entry. Name and description are
// truncated to their column widths so a row never wraps onto a second line.
func homeRows(entries []homeEntry, cursor int) []string {
	rows := make([]string, len(entries))
	for i, e := range entries {
		nameStyle := lipgloss.NewStyle().Width(homeNameWidth)
		descStyle := lipgloss.NewStyle().Width(homeDescWidth)
		nameTxt := truncate(e.name, homeNameWidth)
		descTxt := truncate(e.desc, homeDescWidth)
		if i == cursor {
			cur := lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent)).Render("▶")
			name := nameStyle.Foreground(lipgloss.Color(activeTheme.Accent)).Bold(true).Render(nameTxt)
			desc := descStyle.Foreground(lipgloss.Color(activeTheme.Muted)).Render(descTxt)
			rows[i] = cur + " " + name + "  " + desc
		} else {
			name := nameStyle.Foreground(lipgloss.Color(activeTheme.Text)).Render(nameTxt)
			desc := descStyle.Foreground(lipgloss.Color(activeTheme.Medium)).Render(descTxt)
			rows[i] = "  " + name + "  " + desc
		}
	}
	return rows
}

func (m homeModel) View() string {
	rows := homeRows(m.entries, m.cursor)

	help := tuiHelpStyle.Render("[↑↓/jk] navigate   [enter] open   [q] quit")

	header := homeTitleStyle.Render("codearmory") + "  " + homeSubtitleStyle.Render("platform")
	block := header + "\n\n" + homeBoxStyle.Render(strings.Join(rows, "\n")) + "\n\n" + help
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
	}
	return "\n  " + block + "\n"
}

// ── Standalone wrapper ────────────────────────────────────────────────────────

// standaloneWrap adapts a sub-TUI for direct use (e.g. `armory ci tui`):
// goHomeMsg becomes tea.Quit since there is no home screen to return to.
type standaloneWrap struct{ inner tea.Model }

func (w standaloneWrap) Init() tea.Cmd { return w.inner.Init() }
func (w standaloneWrap) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(goHomeMsg); ok {
		return w, tea.Quit
	}
	var cmd tea.Cmd
	w.inner, cmd = w.inner.Update(msg)
	return w, cmd
}
func (w standaloneWrap) View() string { return w.inner.View() }

// ── Entry point ───────────────────────────────────────────────────────────────

func runHomeTUI() error {
	p := tea.NewProgram(newAppModel(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	rootCmd.RunE = func(_ *cobra.Command, _ []string) error {
		return runHomeTUI()
	}
}
