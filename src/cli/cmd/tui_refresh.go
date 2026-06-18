package cmd

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// tuiRefreshInterval is how often every data-browsing TUI silently re-fetches
// the records for its current view, so lists and live detail pages stay current
// without the user pressing [r].
const tuiRefreshInterval = 5 * time.Second

// tuiAutoRefreshMsg is delivered on a fixed cadence by the TUI host (appModel
// when running in the hub, standaloneWrap when running a screen directly). Each
// screen handles it by silently re-fetching its current view's data. The host
// owns the single ticker and reschedules it, so screens never schedule ticks
// themselves — a per-screen ticker would compound into parallel chains every
// time a screen is reopened from the home menu.
type tuiAutoRefreshMsg struct{}

// tuiAutoRefreshCmd schedules the next auto-refresh tick.
func tuiAutoRefreshCmd() tea.Cmd {
	return tea.Tick(tuiRefreshInterval, func(time.Time) tea.Msg { return tuiAutoRefreshMsg{} })
}
