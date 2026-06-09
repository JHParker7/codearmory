package cmd

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var teamsCmd = &cobra.Command{
	Use:   "teams",
	Short: "Manage teams",
}

func init() {
	var createRole string

	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{"team_name": args[0]}
			if createRole != "" {
				payload["role_id"] = createRole
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/teams", body)
		},
	}
	createCmd.Flags().StringVar(&createRole, "role", "", "Role ID to assign to the team")

	updateCmd := &cobra.Command{
		Use:   "update <name-or-id> <new-name>",
		Short: "Update a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveTeamID(args[0])
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"team_name": args[1]})
			if err != nil {
				return err
			}
			return apiCall("PUT", "/teams/"+id, body)
		},
	}

	inviteCmd := &cobra.Command{
		Use:   "invite <name-or-id> <email>",
		Short: "Send an invite for a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveTeamID(args[0])
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"email": args[1]})
			if err != nil {
				return err
			}
			return apiCall("POST", "/teams/"+id+"/invites", body)
		},
	}

	teamsCmd.AddCommand(
		&cobra.Command{
			Use:   "mine",
			Short: "Get your team",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := myFieldID("team_id")
				if err != nil {
					return err
				}
				return apiCall("GET", "/teams/"+id, nil)
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all teams",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/teams", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <name-or-id>",
			Short: "Get a team",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveTeamID(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/teams/"+id, nil)
			},
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <name-or-id>",
			Short: "Delete a team",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveTeamID(args[0])
				if err != nil {
					return err
				}
				return apiCall("DELETE", "/teams/"+id, nil)
			},
		},
		inviteCmd,
	)
	rootCmd.AddCommand(teamsCmd)
}
