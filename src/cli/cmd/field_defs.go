package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

func init() {
	fieldDefsCmd := &cobra.Command{
		Use:   "field-defs",
		Short: "Manage custom statuses, priorities, and timescales",
	}

	// ── list ─────────────────────────────────────────────────────────────────

	var listKind string

	listFieldDefsCmd := &cobra.Command{
		Use:   "list",
		Short: "List field definitions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/tickets/field-defs"
			if listKind != "" {
				path += "?kind=" + url.QueryEscape(listKind)
			}
			return apiCall("GET", path, nil)
		},
	}
	listFieldDefsCmd.Flags().StringVar(&listKind, "kind", "", "Filter by kind: status, priority, timescale")

	// ── create ───────────────────────────────────────────────────────────────

	var (
		createKind     string
		createValue    string
		createLabel    string
		createColor    string
		createPosition int
	)

	createFieldDefCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a custom field definition",
		Long: `Create a custom status, priority, or timescale value for your org.

  armory tickets field-defs create --kind status --value in_review --label "In Review"
  armory tickets field-defs create --kind priority --value urgent --label Urgent --color "#ff0000"
  armory tickets field-defs create --kind timescale --value q1-2026 --label "Q1 2026"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if createKind == "" {
				return fmt.Errorf("--kind is required (status, priority, or timescale)")
			}
			if createValue == "" {
				return fmt.Errorf("--value is required")
			}
			if createLabel == "" {
				return fmt.Errorf("--label is required")
			}
			payload := map[string]any{
				"kind":     createKind,
				"value":    createValue,
				"label":    createLabel,
				"position": createPosition,
			}
			if createColor != "" {
				payload["color"] = createColor
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/tickets/field-defs", body)
		},
	}
	createFieldDefCmd.Flags().StringVar(&createKind, "kind", "", "Field kind: status, priority, or timescale (required)")
	createFieldDefCmd.Flags().StringVar(&createValue, "value", "", "Stored value (required)")
	createFieldDefCmd.Flags().StringVar(&createLabel, "label", "", "Display label (required)")
	createFieldDefCmd.Flags().StringVar(&createColor, "color", "", "Hex color for display (e.g. #ff6600)")
	createFieldDefCmd.Flags().IntVar(&createPosition, "position", 0, "Sort position within the kind")

	// ── update ───────────────────────────────────────────────────────────────

	var (
		updateLabel    string
		updateColor    string
		updatePosition int
	)

	updateFieldDefCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a field definition's label, color, or position",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{}
			if updateLabel != "" {
				payload["label"] = updateLabel
			}
			if cmd.Flags().Changed("color") {
				payload["color"] = updateColor
			}
			if cmd.Flags().Changed("position") {
				payload["position"] = updatePosition
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/tickets/field-defs/"+args[0], body)
		},
	}
	updateFieldDefCmd.Flags().StringVar(&updateLabel, "label", "", "New display label")
	updateFieldDefCmd.Flags().StringVar(&updateColor, "color", "", "New hex color (pass empty string to clear)")
	updateFieldDefCmd.Flags().IntVar(&updatePosition, "position", 0, "New sort position")

	// ── delete ───────────────────────────────────────────────────────────────

	deleteFieldDefCmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a field definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/tickets/field-defs/"+args[0], nil)
		},
	}

	fieldDefsCmd.AddCommand(listFieldDefsCmd, createFieldDefCmd, updateFieldDefCmd, deleteFieldDefCmd)
	ticketsCmd.AddCommand(fieldDefsCmd)
}
