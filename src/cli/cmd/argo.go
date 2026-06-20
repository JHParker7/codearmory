package cmd

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

// Scriptable subcommands for Argo CD apps. The `argo` command var, its TUI
// launch, and hub-screen registration live in argo_tui.go; this file adds the
// non-interactive verbs under it.

func init() {
	var (
		syncRevision string
		syncOutpost  string
	)
	syncCmd := &cobra.Command{
		Use:   "sync <name>",
		Short: "Trigger a sync for an application",
		Long: `Trigger an Argo CD sync. Revision and outpost default to the app's
last-reported values when omitted.

  armory argo sync my-app
  armory argo sync my-app --revision HEAD`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{}
			if syncRevision != "" {
				payload["revision"] = syncRevision
			}
			if syncOutpost != "" {
				payload["outpost_id"] = syncOutpost
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/argo/apps/"+args[0]+"/sync", body)
		},
	}
	syncCmd.Flags().StringVar(&syncRevision, "revision", "", "Git revision to sync to (default: app's latest)")
	syncCmd.Flags().StringVar(&syncOutpost, "outpost", "", "Outpost id (default: the app's reported outpost)")

	argoCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List Argo CD applications",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/argo/apps", nil) },
		},
		&cobra.Command{
			Use:   "get <name>",
			Short: "Get an Argo CD application",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/argo/apps/"+args[0], nil)
			},
		},
		syncCmd,
		&cobra.Command{
			Use:   "sync-status <sync-id>",
			Short: "Get the status of a sync operation",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/argo/syncs/"+args[0], nil)
			},
		},
	)
}
