package cmd

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var rolesCmd = &cobra.Command{
	Use:   "roles",
	Short: "Manage roles",
}

func init() {
	var createPerms []string
	var createOrg, createName string
	var updatePerms []string
	var updateOrg, updateName string

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a role",
		Long: `Create a role with an optional name and permission IDs.

  armory roles create --name ci-runners --permission <perm-id>
  armory roles create  # creates an unnamed empty role`,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{"permissions_ids": createPerms}
			if createName != "" {
				payload["name"] = createName
			}
			if createOrg != "" {
				payload["org_id"] = createOrg
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/gatekeeper/roles", body)
		},
	}
	createCmd.Flags().StringVar(&createName, "name", "", "Human-readable name for the role")
	createCmd.Flags().StringArrayVar(&createPerms, "permission", nil, "Permission ID to include (repeatable)")
	createCmd.Flags().StringVar(&createOrg, "org", "", "Organization ID to scope the role to")

	updateCmd := &cobra.Command{
		Use:   "update <name-or-id>",
		Short: "Update a role",
		Long: `Replace a role's permission list (and optionally rename it).

  armory roles update ci-runners --permission <perm-id>
  armory roles update ci-runners --name ci-ops --permission <perm-id>
  armory roles update <id>  # clears all permissions`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveRoleID(args[0])
			if err != nil {
				return err
			}
			payload := map[string]any{"permissions_ids": updatePerms}
			if updateName != "" {
				payload["name"] = updateName
			}
			if updateOrg != "" {
				payload["org_id"] = updateOrg
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gatekeeper/roles/"+id, body)
		},
	}
	updateCmd.Flags().StringVar(&updateName, "name", "", "New name for the role")
	updateCmd.Flags().StringArrayVar(&updatePerms, "permission", nil, "Permission ID to include (repeatable; replaces current list)")
	updateCmd.Flags().StringVar(&updateOrg, "org", "", "Organization ID to scope the role to")

	rolesCmd.AddCommand(
		&cobra.Command{
			Use:   "mine",
			Short: "Get your role",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := myFieldID("role_id")
				if err != nil {
					return err
				}
				return apiCall("GET", "/gatekeeper/roles/"+id, nil)
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all roles",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/gatekeeper/roles", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <name-or-id>",
			Short: "Get a role",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveRoleID(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/gatekeeper/roles/"+id, nil)
			},
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <name-or-id>",
			Short: "Delete a role",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveRoleID(args[0])
				if err != nil {
					return err
				}
				return apiCall("DELETE", "/gatekeeper/roles/"+id, nil)
			},
		},
	)
	RegisterModule(Module{Name: "roles", Admin: true, Command: rolesCmd})
}
