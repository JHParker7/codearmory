package cmd

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// goHomeMsg is sent by sub-TUIs when the user presses esc to return home.
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
	width    int
	height   int
	home     homeModel
	active   tea.Model   // currently-shown screen; nil means the home menu
	screens  []HubScreen // home-menu screens, indexed by menu position
	subtitle string      // home header subtitle ("platform" / "admin")
	admin    bool        // which hub: the user hub gates on sign-in, the admin hub doesn't
}

// newAppModel builds the user hub. When signed in it shows the service menu
// filtered to the services actually registered for the caller (enabledScreensFor).
// When signed out it shows the sign-in gate instead: without a token
// registeredServices() can't filter, so the menu would fail open and list every
// service. The filtered menu is built on the first return-home after sign-in (see
// Update's goHomeMsg branch). Crucially, enabledScreensFor is NOT called while
// signed out, so the routing-table probe isn't cached as a fail-open (nil) result
// before the user has a token.
func newAppModel() appModel {
	if !isSignedIn() {
		m := appModel{subtitle: "platform"}
		m.active = newSignInGateModel()
		m.home = newHomeModel(nil, m.subtitle) // placeholder; rebuilt after sign-in
		return m
	}
	return newHubModel(false, "platform")
}

// newAdminAppModel builds the admin hub. Its screens are permission-gated
// server-side, so it builds eagerly without the sign-in gate and labels the
// header so it's unmistakable which surface you're on.
func newAdminAppModel() appModel { return newHubModel(true, "admin") }

// newHubModel assembles a hub over the screens of the matching modules, filtered
// to the services registered for the caller.
func newHubModel(admin bool, subtitle string) appModel {
	m := appModel{subtitle: subtitle, admin: admin}
	m.screens = enabledScreensFor(admin)
	m.home = newHomeModel(m.screens, subtitle)
	return m
}

// Init starts the single auto-refresh ticker. It runs for the whole session and
// is rescheduled in Update; the active screen re-fetches whenever it fires.
func (m appModel) Init() tea.Cmd { return tuiAutoRefreshCmd() }

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
		// User hub while signed out: show the sign-in gate, never an unfiltered
		// menu. (The admin hub is permission-gated server-side and isn't gated.)
		if !m.admin && !isSignedIn() {
			m.active = m.sized(newSignInGateModel())
			return m, nil
		}
		// Rebuild the menu so it reflects the services registered for the caller —
		// after a sign-in this is the first time registeredServices() resolves with
		// a token. registeredServices() is cached, so the rebuild is cheap.
		m.active = nil
		m.screens = enabledScreensFor(m.admin)
		m.home = m.sized(newHomeModel(m.screens, m.subtitle)).(homeModel)
		return m, nil
	}
	if _, ok := msg.(launchSettingsMsg); ok {
		m.active = m.sized(newSettingsModel())
		return m, m.active.Init()
	}
	if lm, ok := msg.(launchMsg); ok {
		if lm.idx < 0 || lm.idx >= len(m.screens) {
			return m, nil
		}
		m.active = m.sized(m.screens[lm.idx].New())
		return m, m.active.Init()
	}
	// The auto-refresh ticker runs for the whole session. Reschedule it on every
	// fire and forward the tick to the active screen so it re-fetches; the home
	// menu has nothing to refresh, so it is skipped.
	if _, ok := msg.(tuiAutoRefreshMsg); ok {
		if m.active == nil {
			return m, tuiAutoRefreshCmd()
		}
		next, cmd := m.active.Update(msg)
		m.active = next
		return m, tea.Batch(cmd, tuiAutoRefreshCmd())
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
	entries  []homeEntry
	cursor   int
	width    int
	height   int
	subtitle string // header subtitle shown next to "codearmory"
	project  string // current project badge, read once at build time
}

// newHomeModel builds the launcher menu from the registry-provided screens, in
// the same order they are dispatched (launchMsg.idx indexes appModel.screens).
// subtitle labels the header ("platform" for the user hub, "admin" for the
// admin hub).
func newHomeModel(screens []HubScreen, subtitle string) homeModel {
	entries := make([]homeEntry, len(screens))
	for i, s := range screens {
		entries[i] = homeEntry{name: s.Title, desc: s.Desc}
	}
	// Read the current project once here rather than on every View() render
	// (Bubbletea re-renders on every key/resize/tick). newHomeModel runs on
	// launch and on each return-to-home, so the badge stays current.
	return homeModel{entries: entries, subtitle: subtitle, project: loadConfig().CurrentProject}
}

func (m homeModel) Init() tea.Cmd { return nil }

func (m homeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
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

	help := tuiHelpStyle.Render("[↑↓/jk] navigate   [enter] open   [esc] quit")

	subtitle := m.subtitle
	if subtitle == "" {
		subtitle = "platform"
	}
	header := homeTitleStyle.Render("codearmory") + "  " + homeSubtitleStyle.Render(subtitle)
	if m.project != "" {
		header += "  " + lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent)).Render("⬡ "+m.project)
	}
	block := header + "\n\n" + homeBoxStyle.Render(strings.Join(rows, "\n")) + "\n\n" + help
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
	}
	return "\n  " + block + "\n"
}

// ── Standalone wrapper ────────────────────────────────────────────────────────

// standaloneWrap adapts a sub-TUI for direct use (e.g. `armory pipelines tui`):
// goHomeMsg becomes tea.Quit since there is no home screen to return to.
type standaloneWrap struct{ inner tea.Model }

func (w standaloneWrap) Init() tea.Cmd { return tea.Batch(w.inner.Init(), tuiAutoRefreshCmd()) }
func (w standaloneWrap) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(goHomeMsg); ok {
		return w, tea.Quit
	}
	// standaloneWrap owns the auto-refresh ticker for a screen run directly (e.g.
	// `armory forge tui`): reschedule it and forward the tick to the inner model.
	if _, ok := msg.(tuiAutoRefreshMsg); ok {
		var cmd tea.Cmd
		w.inner, cmd = w.inner.Update(msg)
		return w, tea.Batch(cmd, tuiAutoRefreshCmd())
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
