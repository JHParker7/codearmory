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
	homeTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#39ff14"))

	homeSubtitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#4a5346"))

	homeBoxStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#1a2018")).
			Padding(0, 2)
)

// ── App model (top-level container) ──────────────────────────────────────────

type appView int

const (
	appViewHome appView = iota
	appViewCI
	appViewBoard
	appViewForge
	appViewHooks
	appViewAudit
)

type appModel struct {
	view  appView
	home  homeModel
	ci    tuiModel
	board boardModel
	forge forgeModel
	hooks hooksModel
	audit auditModel
}

func newAppModel() appModel {
	return appModel{view: appViewHome, home: newHomeModel()}
}

func (m appModel) Init() tea.Cmd { return nil }

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(goHomeMsg); ok {
		m.view = appViewHome
		m.home = newHomeModel()
		return m, nil
	}
	if lm, ok := msg.(launchMsg); ok {
		switch lm.idx {
		case 0:
			m.view = appViewCI
			m.ci = newTUIModel()
			return m, m.ci.Init()
		case 1:
			m.view = appViewBoard
			m.board = newBoardModel()
			return m, m.board.Init()
		case 2:
			m.view = appViewForge
			m.forge = newForgeModel()
			return m, m.forge.Init()
		case 3:
			m.view = appViewHooks
			m.hooks = newHooksModel()
			return m, m.hooks.Init()
		case 4:
			m.view = appViewAudit
			m.audit = newAuditModel()
			return m, m.audit.Init()
		}
		return m, nil
	}

	switch m.view {
	case appViewHome:
		next, cmd := m.home.Update(msg)
		m.home = next.(homeModel)
		return m, cmd
	case appViewCI:
		next, cmd := m.ci.Update(msg)
		m.ci = next.(tuiModel)
		return m, cmd
	case appViewBoard:
		next, cmd := m.board.Update(msg)
		m.board = next.(boardModel)
		return m, cmd
	case appViewForge:
		next, cmd := m.forge.Update(msg)
		m.forge = next.(forgeModel)
		return m, cmd
	case appViewHooks:
		next, cmd := m.hooks.Update(msg)
		m.hooks = next.(hooksModel)
		return m, cmd
	case appViewAudit:
		next, cmd := m.audit.Update(msg)
		m.audit = next.(auditModel)
		return m, cmd
	}
	return m, nil
}

func (m appModel) View() string {
	switch m.view {
	case appViewCI:
		return m.ci.View()
	case appViewBoard:
		return m.board.View()
	case appViewForge:
		return m.forge.View()
	case appViewHooks:
		return m.hooks.View()
	case appViewAudit:
		return m.audit.View()
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
}

func newHomeModel() homeModel {
	return homeModel{
		entries: []homeEntry{
			{"CI / Pipelines", "Browse workflow pipelines and run history"},
			{"Tickets Board", "Interactive kanban board"},
			{"Forge", "Browse sandboxed executions and their output"},
			{"Hooks", "Webhook rules and event history"},
			{"Audit Log", "Browse the platform audit trail"},
		},
	}
}

func (m homeModel) Init() tea.Cmd { return nil }

func (m homeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
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

func (m homeModel) View() string {
	rows := make([]string, len(m.entries))
	for i, e := range m.entries {
		nameStyle := lipgloss.NewStyle().Width(20)
		descStyle := lipgloss.NewStyle().Width(44)
		if i == m.cursor {
			cur := lipgloss.NewStyle().Foreground(lipgloss.Color("#39ff14")).Render("▶")
			name := nameStyle.Foreground(lipgloss.Color("#39ff14")).Bold(true).Render(e.name)
			desc := descStyle.Foreground(lipgloss.Color("#4a5346")).Render(e.desc)
			rows[i] = cur + " " + name + "  " + desc
		} else {
			name := nameStyle.Foreground(lipgloss.Color("#d6dcd2")).Render(e.name)
			desc := descStyle.Foreground(lipgloss.Color("#7d8a78")).Render(e.desc)
			rows[i] = "  " + name + "  " + desc
		}
	}

	help := tuiHelpStyle.Render("[↑↓/jk] navigate   [enter] open   [q] quit")

	return "\n  " + homeTitleStyle.Render("codearmory") + "  " +
		homeSubtitleStyle.Render("platform") + "\n\n" +
		homeBoxStyle.Render(strings.Join(rows, "\n")) + "\n\n" +
		"  " + help + "\n"
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
