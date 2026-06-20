package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Scriptable subcommands for outposts. The `outpost` command var, its TUI launch
// (RunE), and the hub-screen registration live in outpost_tui.go; this file adds
// the non-interactive `list/create/get/delete` verbs under it.

func init() {
	var (
		createName    string
		createModules []string
	)
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Register a new outpost (prints its single-use enrollment token)",
		Long: `Register a new outpost and mint its single-use enrollment token.

  armory admin outpost create --name prod-cluster --module chaos --module argo

The enrollment token is returned once — copy it to deploy the outpost agent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(createName) == "" {
				return fmt.Errorf("--name is required")
			}
			payload := map[string]any{"name": createName, "modules": createModules}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/outpost-gateway/outposts", body)
		},
	}
	createCmd.Flags().StringVar(&createName, "name", "", "Outpost name (required)")
	createCmd.Flags().StringArrayVar(&createModules, "module", nil, "Enabled module: chaos or argo (repeatable)")

	outpostCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List outposts",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/outpost-gateway/outposts", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get an outpost",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/outpost-gateway/outposts/"+args[0], nil)
			},
		},
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete an outpost",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/outpost-gateway/outposts/"+args[0], nil)
			},
		},
	)
}
