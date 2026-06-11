package cmd

import "github.com/spf13/cobra"

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage sessions",
}

func init() {
	sessionsCmd.AddCommand(
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a session by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/gatekeeper/sessions/"+args[0], nil) },
		},
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Revoke a session",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/gatekeeper/sessions/"+args[0], nil) },
		},
	)
	rootCmd.AddCommand(sessionsCmd)
}
