package cmd

import "github.com/spf13/cobra"

var orgsCmd = &cobra.Command{
	Use:   "orgs",
	Short: "Manage organizations",
}

func init() {
	var createData, updateData, inviteData string

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create an organization",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(createData)
			if err != nil {
				return err
			}
			return apiCall("POST", "/orgs", body)
		},
	}
	createCmd.Flags().StringVar(&createData, "data", "", "JSON body or @file")

	updateCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(updateData)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/orgs/"+args[0], body)
		},
	}
	updateCmd.Flags().StringVar(&updateData, "data", "", "JSON body or @file")

	inviteCmd := &cobra.Command{
		Use:   "invite <id>",
		Short: "Send an invite for an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(inviteData)
			if err != nil {
				return err
			}
			return apiCall("POST", "/orgs/"+args[0]+"/invites", body)
		},
	}
	inviteCmd.Flags().StringVar(&inviteData, "data", "", "JSON body or @file")

	orgsCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List all organizations",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/orgs", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get an organization by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/orgs/"+args[0], nil) },
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete an organization",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/orgs/"+args[0], nil) },
		},
		inviteCmd,
	)
	rootCmd.AddCommand(orgsCmd)
}
