package cmd

import "github.com/spf13/cobra"

var invitesCmd = &cobra.Command{
	Use:   "invites",
	Short: "Manage invites",
}

func init() {
	invitesCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List all invites",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/invites", nil) },
		},
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get an invite by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/invites/"+args[0], nil) },
		},
		&cobra.Command{
			Use:   "accept <id>",
			Short: "Accept an invite",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("POST", "/invites/"+args[0]+"/accept", nil) },
		},
		&cobra.Command{
			Use:   "decline <id>",
			Short: "Decline an invite",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("POST", "/invites/"+args[0]+"/decline", nil) },
		},
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete an invite",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/invites/"+args[0], nil) },
		},
	)
	rootCmd.AddCommand(invitesCmd)
}
