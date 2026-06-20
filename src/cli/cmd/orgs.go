package cmd

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var orgsCmd = &cobra.Command{
	Use:   "orgs",
	Short: "Manage organizations",
}

func init() {
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := json.Marshal(map[string]string{"org_name": args[0]})
			if err != nil {
				return err
			}
			return apiCall("POST", "/gatekeeper/orgs", body)
		},
	}

	updateCmd := &cobra.Command{
		Use:   "update <name-or-id> <new-name>",
		Short: "Update an organization",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveOrgID(args[0])
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"org_name": args[1]})
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gatekeeper/orgs/"+id, body)
		},
	}

	inviteCmd := &cobra.Command{
		Use:   "invite <name-or-id> <email>",
		Short: "Send an invite for an organization",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveOrgID(args[0])
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"email": args[1]})
			if err != nil {
				return err
			}
			return apiCall("POST", "/gatekeeper/orgs/"+id+"/invites", body)
		},
	}

	orgsCmd.AddCommand(
		&cobra.Command{
			Use:   "mine",
			Short: "Get your organization",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := myFieldID("org_id")
				if err != nil {
					return err
				}
				return apiCall("GET", "/gatekeeper/orgs/"+id, nil)
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all organizations",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/gatekeeper/orgs", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <name-or-id>",
			Short: "Get an organization",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveOrgID(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/gatekeeper/orgs/"+id, nil)
			},
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <name-or-id>",
			Short: "Delete an organization",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveOrgID(args[0])
				if err != nil {
					return err
				}
				return apiCall("DELETE", "/gatekeeper/orgs/"+id, nil)
			},
		},
		inviteCmd,
	)
	RegisterModule(Module{Name: "orgs", Admin: true, Command: orgsCmd})
}
