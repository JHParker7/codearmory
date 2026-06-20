package cmd

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var permissionsCmd = &cobra.Command{
	Use:   "permissions",
	Short: "Manage permissions",
}

func init() {
	var createActions, createResources []string
	var updateActions, updateResources []string

	createCmd := &cobra.Command{
		Use:   "create <name> <service>",
		Short: "Create a permission",
		Long: `Create a permission defining allowed actions on resources for a service.

  armory permissions create read-repos forge \
    --action read --resource "forge/repos/*"

  armory permissions create ci-ops workflows \
    --action run --action cancel --resource "workflows/pipelines/*"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := json.Marshal(map[string]any{
				"name":      args[0],
				"service":   args[1],
				"actions":   createActions,
				"resources": createResources,
			})
			if err != nil {
				return err
			}
			return apiCall("POST", "/gatekeeper/permissions", body)
		},
	}
	createCmd.Flags().StringArrayVar(&createActions, "action", nil, "Allowed action (repeatable, e.g. read, write)")
	createCmd.Flags().StringArrayVar(&createResources, "resource", nil, "Resource pattern (repeatable, e.g. forge/repos/*)")

	updateCmd := &cobra.Command{
		Use:   "update <id> <name> <service>",
		Short: "Update a permission",
		Long: `Replace a permission's fields. All fields are replaced.

  armory permissions update <id> read-repos forge \
    --action read --resource "forge/repos/*"`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := json.Marshal(map[string]any{
				"name":      args[1],
				"service":   args[2],
				"actions":   updateActions,
				"resources": updateResources,
			})
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gatekeeper/permissions/"+args[0], body)
		},
	}
	updateCmd.Flags().StringArrayVar(&updateActions, "action", nil, "Allowed action (repeatable)")
	updateCmd.Flags().StringArrayVar(&updateResources, "resource", nil, "Resource pattern (repeatable)")

	permissionsCmd.AddCommand(
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a permission by ID",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/gatekeeper/permissions/"+args[0], nil)
			},
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a permission",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/gatekeeper/permissions/"+args[0], nil)
			},
		},
	)
	RegisterModule(Module{Name: "permissions", Admin: true, Command: permissionsCmd})
}
