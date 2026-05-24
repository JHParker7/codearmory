package cmd

import "github.com/spf13/cobra"

var rolesCmd = &cobra.Command{
	Use:   "roles",
	Short: "Manage roles",
}

func init() {
	var createData, updateData string

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a role",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(createData)
			if err != nil {
				return err
			}
			return apiCall("POST", "/roles", body)
		},
	}
	createCmd.Flags().StringVar(&createData, "data", "", "JSON body or @file")

	updateCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(updateData)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/roles/"+args[0], body)
		},
	}
	updateCmd.Flags().StringVar(&updateData, "data", "", "JSON body or @file")

	rolesCmd.AddCommand(
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a role by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/roles/"+args[0], nil) },
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a role",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/roles/"+args[0], nil) },
		},
	)
	rootCmd.AddCommand(rolesCmd)
}
