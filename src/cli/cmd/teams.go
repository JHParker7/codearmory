package cmd

import "github.com/spf13/cobra"

var teamsCmd = &cobra.Command{
	Use:   "teams",
	Short: "Manage teams",
}

func init() {
	var createData, updateData, inviteData string

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a team",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(createData)
			if err != nil {
				return err
			}
			return apiCall("POST", "/teams", body)
		},
	}
	createCmd.Flags().StringVar(&createData, "data", "", "JSON body or @file")

	updateCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(updateData)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/teams/"+args[0], body)
		},
	}
	updateCmd.Flags().StringVar(&updateData, "data", "", "JSON body or @file")

	inviteCmd := &cobra.Command{
		Use:   "invite <id>",
		Short: "Send an invite for a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(inviteData)
			if err != nil {
				return err
			}
			return apiCall("POST", "/teams/"+args[0]+"/invites", body)
		},
	}
	inviteCmd.Flags().StringVar(&inviteData, "data", "", "JSON body or @file")

	teamsCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List all teams",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/teams", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a team by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/teams/"+args[0], nil) },
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a team",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/teams/"+args[0], nil) },
		},
		inviteCmd,
	)
	rootCmd.AddCommand(teamsCmd)
}
