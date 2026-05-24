package cmd

import "github.com/spf13/cobra"

var usersCmd = &cobra.Command{
	Use:   "users",
	Short: "Manage users",
}

func init() {
	var updateData string

	updateCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(updateData)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/users/"+args[0], body)
		},
	}
	updateCmd.Flags().StringVar(&updateData, "data", "", "JSON body or @file (use - or @- for stdin)")

	usersCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List all users",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/users", nil) },
		},
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a user by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/users/"+args[0], nil) },
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a user",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/users/"+args[0], nil) },
		},
	)
	rootCmd.AddCommand(usersCmd)
}
