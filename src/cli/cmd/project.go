package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// Project (a.k.a. workspace) is a free-text label attached to pipelines,
// tickets, forge executions and repos. It is purely a view filter: it never
// changes what you can access, only which of your accessible items are shown.
//
// The "current project" is sticky CLI state (config.CurrentProject). When set,
// list views auto-filter to it and new resources are tagged with it. Per command
// this can be overridden with the persistent --project flag, or ignored with
// --all.

var (
	flagProject     string
	flagAllProjects bool
)

func init() {
	rootCmd.PersistentFlags().StringVar(&flagProject, "project", "", "filter by project (overrides the current project)")
	rootCmd.PersistentFlags().BoolVar(&flagAllProjects, "all", false, "ignore the current project and show items from every project")
}

// projectFilter resolves the active project label for filtering and tagging.
// Precedence: --all (no filter) > --project <name> > sticky current project.
func projectFilter() string {
	if flagAllProjects {
		return ""
	}
	if flagProject != "" {
		return flagProject
	}
	return loadConfig().CurrentProject
}

// appendProjectParam adds project=<filter> to path's query string when a project
// filter is active, choosing ? or & based on whether path already has a query.
func appendProjectParam(path string) string {
	p := projectFilter()
	if p == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "project=" + url.QueryEscape(p)
}

// setCurrentProject persists the sticky current project (empty clears it).
func setCurrentProject(name string) error {
	cfg := loadConfig()
	cfg.CurrentProject = name
	return saveConfig(cfg)
}

// projectListEndpoints are the list APIs whose items carry a project label.
// fetchKnownProjects aggregates the distinct labels in use across them.
var projectListEndpoints = []string{
	"/workflows/pipelines",
	"/tickets/tickets",
	"/forge/executions",
	"/gitea_integration/repos",
}

// fetchKnownProjects returns the sorted distinct project labels currently in use
// across the caller's accessible resources. It is best-effort: an endpoint that
// errors (e.g. a service the user can't reach) is skipped, not fatal.
func fetchKnownProjects() []string {
	// The endpoints are independent, so fetch them concurrently rather than
	// serially — wall-clock latency drops to the slowest single call.
	labels := make([][]string, len(projectListEndpoints))
	var wg sync.WaitGroup
	for i, ep := range projectListEndpoints {
		wg.Add(1)
		go func(i int, ep string) {
			defer wg.Done()
			data, err := doRequest("GET", ep, nil)
			if err != nil {
				return
			}
			var items []struct {
				Project string `json:"project"`
			}
			if json.Unmarshal(data, &items) != nil {
				return
			}
			for _, it := range items {
				if it.Project != "" {
					labels[i] = append(labels[i], it.Project)
				}
			}
		}(i, ep)
	}
	wg.Wait()

	seen := map[string]struct{}{}
	for _, ls := range labels {
		for _, l := range ls {
			seen[l] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func newProjectCmd() *cobra.Command {
	projectCmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"workspace", "ws"},
		Short:   "Manage the current project (workspace) used to filter your interface",
		Long: `A project is a free-text label on pipelines, tickets, forge executions and
repos. Set the one you're working in with "armory project use <name>" and every
list command and TUI screen filters to it (override with --project or --all).

A project is a view filter, not a permission boundary — it never widens or
narrows what you can access.`,
	}

	projectCmd.AddCommand(&cobra.Command{
		Use:   "use <name>",
		Short: "Set the current project; lists auto-filter to it until changed",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := setCurrentProject(args[0]); err != nil {
				return err
			}
			fmt.Printf("✓ now working in project: %s\n", args[0])
			return nil
		},
	})

	projectCmd.AddCommand(&cobra.Command{
		Use:   "clear",
		Short: "Clear the current project; lists show every project again",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := setCurrentProject(""); err != nil {
				return err
			}
			fmt.Println("✓ current project cleared")
			return nil
		},
	})

	projectCmd.AddCommand(&cobra.Command{
		Use:   "current",
		Short: "Show the current project",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if p := loadConfig().CurrentProject; p != "" {
				fmt.Println(p)
			} else {
				fmt.Println("(no current project — showing all)")
			}
			return nil
		},
	})

	projectCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the project labels currently in use across your resources",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			projects := fetchKnownProjects()
			if len(projects) == 0 {
				fmt.Println("(no projects in use yet)")
				return nil
			}
			current := loadConfig().CurrentProject
			for _, p := range projects {
				marker := "  "
				if p == current {
					marker = "▶ "
				}
				fmt.Printf("%s%s\n", marker, p)
			}
			return nil
		},
	})

	return projectCmd
}

func init() {
	RegisterModule(Module{
		Name:    "project",
		Order:   1,
		Command: newProjectCmd(),
		Screens: []HubScreen{{
			Title: "Project",
			Desc:  "Switch the workspace your interface is filtered to",
			New:   func() tea.Model { return newProjectPickerModel() },
		}},
	})
}

// ── Project picker screen ──────────────────────────────────────────────────────

type projectsLoadedMsg struct{ projects []string }

func fetchProjectsCmd() tea.Msg { return projectsLoadedMsg{projects: fetchKnownProjects()} }

// projectPickerModel lets the user switch the current project from the TUI hub.
// options[0] is always the "(all projects)" clear entry; the rest are the labels
// returned by fetchKnownProjects.
type projectPickerModel struct {
	options       []string
	cursor        int
	current       string
	loading       bool
	width, height int
}

func newProjectPickerModel() projectPickerModel {
	return projectPickerModel{loading: true, current: loadConfig().CurrentProject}
}

func (m projectPickerModel) Init() tea.Cmd { return fetchProjectsCmd }

func (m projectPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case projectsLoadedMsg:
		m.loading = false
		m.options = append([]string{""}, msg.projects...) // "" = clear/all
		// Park the cursor on the current project so it's pre-selected.
		for i, p := range m.options {
			if p == m.current {
				m.cursor = i
				break
			}
		}
		return m, nil
	case tuiAutoRefreshMsg:
		// Refresh the known-project list on the shared ticker.
		return m, fetchProjectsCmd
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			return m, func() tea.Msg { return goHomeMsg{} }
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case "enter", " ":
			if m.cursor >= 0 && m.cursor < len(m.options) {
				_ = setCurrentProject(m.options[m.cursor]) //nolint:errcheck — best-effort; nav home regardless
			}
			return m, func() tea.Msg { return goHomeMsg{} }
		}
	}
	return m, nil
}

func (m projectPickerModel) View() string {
	title := homeTitleStyle.Render("project") + "  " + homeSubtitleStyle.Render("switch workspace")

	var body string
	switch {
	case m.loading:
		body = homeSubtitleStyle.Render("loading projects…")
	case len(m.options) == 0:
		body = homeSubtitleStyle.Render("no projects in use yet")
	default:
		rows := make([]string, len(m.options))
		for i, p := range m.options {
			label := p
			if p == "" {
				label = "(all projects)"
			}
			if p != "" && p == m.current {
				label += "  (current)"
			}
			if i == m.cursor {
				cur := lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent)).Render("▶")
				rows[i] = cur + " " + lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent)).Bold(true).Render(label)
			} else {
				rows[i] = "  " + lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text)).Render(label)
			}
		}
		body = homeBoxStyle.Render(strings.Join(rows, "\n"))
	}

	help := tuiHelpStyle.Render("[↑↓/jk] navigate   [enter] select   [esc] back")
	block := title + "\n\n" + body + "\n\n" + help
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, block)
	}
	return "\n  " + block + "\n"
}
