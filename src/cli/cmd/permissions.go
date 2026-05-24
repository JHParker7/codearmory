package cmd

import "github.com/spf13/cobra"

var permissionsCmd = &cobra.Command{
	Use:   "permissions",
	Short: "Manage permissions",
}

func init() {
	var createData, updateData string

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a permission",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(createData)
			if err != nil {
				return err
			}
			return apiCall("POST", "/permissions", body)
		},
	}
	createCmd.Flags().StringVar(&createData, "data", "", "JSON body or @file")

	updateCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a permission",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(updateData)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/permissions/"+args[0], body)
		},
	}
	updateCmd.Flags().StringVar(&updateData, "data", "", "JSON body or @file")

	permissionsCmd.AddCommand(
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a permission by ID",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/permissions/"+args[0], nil) },
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a permission",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/permissions/"+args[0], nil) },
		},
	)
	rootCmd.AddCommand(permissionsCmd)
}
