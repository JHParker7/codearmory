package cmd

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// adminCmd groups platform-administration controls — gatekeeper (identity/RBAC),
// forge runtimes, the audit log, and the org/team/user/role/permission
// management commands — under `armory admin`, keeping the top-level `armory`
// surface focused on day-to-day developer tasks. Run with no subcommand it opens
// the admin TUI hub.
//
// The split is organisational, not a security boundary: permission enforcement
// stays server-side, so a user without admin grants can open the hub but their
// mutating actions come back as a 403 status line (the same contract every
// admin screen already uses).
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Platform administration: gatekeeper, forge runtimes, audit (requires permissions)",
	Long: `Platform administration controls, separate from the user-focused armory commands.

Run 'armory admin' with no subcommand to open the admin TUI hub (gatekeeper,
forge runtimes, audit log). Individual controls are also available as
subcommands, e.g. 'armory admin gatekeeper', 'armory admin forge-runtimes',
'armory admin roles'. All actions are still gated by your server-side permissions.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error { return runAdminTUI() },
}

// runAdminTUI launches the admin hub: the same registry-driven hub machinery as
// the user hub, populated from the admin modules' screens.
func runAdminTUI() error {
	p := tea.NewProgram(newAdminAppModel(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
