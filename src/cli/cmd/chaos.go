package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Scriptable subcommands for chaos experiments. The `chaos` command var, its TUI
// launch, and hub-screen registration live in chaos_tui.go; this file adds the
// non-interactive verbs under it.

func init() {
	var (
		expOutpost string
		expType    string
		expNS      string
		expLabel   string
		expKind    string
		expParams  []string
	)
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Start a chaos experiment",
		Long: `Start a chaos experiment against a target workload via an outpost.

  armory chaos create --outpost <id> --type pod-delete \
    --namespace prod --label app.kubernetes.io/name=web \
    --param TOTAL_CHAOS_DURATION=30`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case expOutpost == "":
				return fmt.Errorf("--outpost is required")
			case expType == "":
				return fmt.Errorf("--type is required (see: armory chaos types)")
			case expNS == "":
				return fmt.Errorf("--namespace is required")
			case expLabel == "":
				return fmt.Errorf("--label is required (e.g. app.kubernetes.io/name=web)")
			}
			params, err := parseConfigPairs(expParams)
			if err != nil {
				return err
			}
			payload := map[string]any{
				"outpost_id":       expOutpost,
				"experiment_type":  expType,
				"target_app_ns":    expNS,
				"target_app_label": expLabel,
				"target_app_kind":  expKind,
				"params":           params,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/chaos/experiments", body)
		},
	}
	createCmd.Flags().StringVar(&expOutpost, "outpost", "", "Target outpost id (required)")
	createCmd.Flags().StringVar(&expType, "type", "", "Experiment type (required)")
	createCmd.Flags().StringVar(&expNS, "namespace", "", "Target namespace (required)")
	createCmd.Flags().StringVar(&expLabel, "label", "", "Target label selector (required)")
	createCmd.Flags().StringVar(&expKind, "kind", "deployment", "Target workload kind")
	createCmd.Flags().StringArrayVar(&expParams, "param", nil, "Experiment param KEY=VALUE (repeatable)")

	chaosCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List chaos experiments",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/chaos/experiments", nil) },
		},
		&cobra.Command{
			Use:   "types",
			Short: "List available experiment types",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/chaos/experiment-types", nil) },
		},
		createCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a chaos experiment",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/chaos/experiments/"+args[0], nil)
			},
		},
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a chaos experiment",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/chaos/experiments/"+args[0], nil)
			},
		},
	)
}
