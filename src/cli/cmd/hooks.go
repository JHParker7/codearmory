package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Manage webhook pipeline rules and event history",
}

var hooksRulesCmd = &cobra.Command{
	Use:   "rules",
	Short: "Manage webhook pipeline rules",
}

var hooksEventsCmd = &cobra.Command{
	Use:   "events",
	Short: "View received webhook events",
}

func init() {
	// ── armory hooks rules create ─────────────────────────────────────────────

	var (
		ruleName      string
		ruleRepo      string
		ruleEvents    []string
		ruleWorkflow  string
		ruleRefFilter string
		ruleSecret    string
		ruleInputs    []string
	)

	createRuleCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a webhook pipeline rule",
		Long: `Create a rule that triggers a pipeline when a webhook event is received.

  armory hooks rules create \
    --name ci-push \
    --repo myorg/myrepo \
    --events push \
    --workflow <pipeline-id>

  # With ref filter and input mapping:
  armory hooks rules create \
    --name ci-main \
    --repo myorg/myrepo \
    --events push \
    --workflow <pipeline-id> \
    --ref-filter refs/heads/main \
    --input IMAGE_TAG=commit

  # Multiple events:
  armory hooks rules create --name ci --repo myorg/myrepo \
    --events push --events pull_request --workflow <pipeline-id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ruleName == "" {
				return fmt.Errorf("--name is required")
			}
			if ruleRepo == "" {
				return fmt.Errorf("--repo is required")
			}
			if len(ruleEvents) == 0 {
				return fmt.Errorf("--events is required (at least one)")
			}
			if ruleWorkflow == "" {
				return fmt.Errorf("--workflow is required")
			}

			inputMapping := map[string]string{}
			for _, kv := range ruleInputs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --input %q: expected key=value", kv)
				}
				inputMapping[k] = v
			}

			payload := map[string]any{
				"name":          ruleName,
				"source":        ruleRepo,
				"events":        ruleEvents,
				"workflow_id":   ruleWorkflow,
				"ref_filter":    ruleRefFilter,
				"input_mapping": inputMapping,
			}
			if ruleSecret != "" {
				payload["secret"] = ruleSecret
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/hooks/rules", body)
		},
	}
	createRuleCmd.Flags().StringVar(&ruleName, "name", "", "Rule name")
	createRuleCmd.Flags().StringVar(&ruleRepo, "repo", "", "Repository (e.g. myorg/myrepo)")
	createRuleCmd.Flags().StringArrayVar(&ruleEvents, "events", nil, "Event type(s) to match (repeatable, e.g. push)")
	createRuleCmd.Flags().StringVar(&ruleWorkflow, "workflow", "", "Pipeline ID to trigger")
	createRuleCmd.Flags().StringVar(&ruleRefFilter, "ref-filter", "", "Ref filter (e.g. refs/heads/main or refs/heads/*)")
	createRuleCmd.Flags().StringVar(&ruleSecret, "secret", "", "HMAC secret for verifying X-Hub-Signature-256")
	createRuleCmd.Flags().StringArrayVarP(&ruleInputs, "input", "i", nil, "Input mapping key=value (repeatable, maps payload fields to pipeline inputs)")

	// ── armory hooks rules list ───────────────────────────────────────────────

	listRulesCmd := &cobra.Command{
		Use:   "list",
		Short: "List webhook pipeline rules",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/hooks/rules", nil) },
	}

	// ── armory hooks rules get ────────────────────────────────────────────────

	getRuleCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a webhook pipeline rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/hooks/rules/"+args[0], nil)
		},
	}

	// ── armory hooks rules update ─────────────────────────────────────────────

	var (
		updateRuleName      string
		updateRuleRepo      string
		updateRuleEvents    []string
		updateRuleWorkflow  string
		updateRuleRefFilter string
		updateRuleSecret    string
		updateRuleClearSecret bool
		updateRuleInputs    []string
	)

	updateRuleCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a webhook pipeline rule",
		Long: `Update a rule. All fields are replaced — fetch first with 'get' if you only want to change one field.

  armory hooks rules update <id> --name new-name --repo myorg/myrepo \
    --events push --workflow <pipeline-id>

  # Clear the HMAC secret:
  armory hooks rules update <id> ... --clear-secret`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if updateRuleName == "" {
				return fmt.Errorf("--name is required")
			}
			if updateRuleRepo == "" {
				return fmt.Errorf("--repo is required")
			}
			if len(updateRuleEvents) == 0 {
				return fmt.Errorf("--events is required (at least one)")
			}
			if updateRuleWorkflow == "" {
				return fmt.Errorf("--workflow is required")
			}

			inputMapping := map[string]string{}
			for _, kv := range updateRuleInputs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --input %q: expected key=value", kv)
				}
				inputMapping[k] = v
			}

			payload := map[string]any{
				"name":          updateRuleName,
				"source":        updateRuleRepo,
				"events":        updateRuleEvents,
				"workflow_id":   updateRuleWorkflow,
				"ref_filter":    updateRuleRefFilter,
				"input_mapping": inputMapping,
			}
			// secret update semantics: omit = leave unchanged, "" = clear, non-empty = replace.
			if updateRuleClearSecret {
				payload["secret"] = ""
			} else if updateRuleSecret != "" {
				payload["secret"] = updateRuleSecret
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/hooks/rules/"+args[0], body)
		},
	}
	updateRuleCmd.Flags().StringVar(&updateRuleName, "name", "", "Rule name")
	updateRuleCmd.Flags().StringVar(&updateRuleRepo, "repo", "", "Repository (e.g. myorg/myrepo)")
	updateRuleCmd.Flags().StringArrayVar(&updateRuleEvents, "events", nil, "Event type(s) to match (repeatable)")
	updateRuleCmd.Flags().StringVar(&updateRuleWorkflow, "workflow", "", "Pipeline ID to trigger")
	updateRuleCmd.Flags().StringVar(&updateRuleRefFilter, "ref-filter", "", "Ref filter")
	updateRuleCmd.Flags().StringVar(&updateRuleSecret, "secret", "", "Replace the HMAC secret")
	updateRuleCmd.Flags().BoolVar(&updateRuleClearSecret, "clear-secret", false, "Remove the HMAC secret")
	updateRuleCmd.Flags().StringArrayVarP(&updateRuleInputs, "input", "i", nil, "Input mapping key=value (repeatable)")

	// ── armory hooks rules delete ─────────────────────────────────────────────

	deleteRuleCmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a webhook pipeline rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/hooks/rules/"+args[0], nil)
		},
	}

	hooksRulesCmd.AddCommand(createRuleCmd, listRulesCmd, getRuleCmd, updateRuleCmd, deleteRuleCmd)

	// ── armory hooks events list ──────────────────────────────────────────────

	var listEventsRepo string

	listEventsCmd := &cobra.Command{
		Use:   "list",
		Short: "List received webhook events",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/hooks/events"
			if listEventsRepo != "" {
				path += "?repo=" + url.QueryEscape(listEventsRepo)
			}
			return apiCall("GET", path, nil)
		},
	}
	listEventsCmd.Flags().StringVar(&listEventsRepo, "repo", "", "Filter by repository")

	// ── armory hooks events get ───────────────────────────────────────────────

	getEventCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a webhook event with its trigger details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/hooks/events/"+args[0], nil)
		},
	}

	hooksEventsCmd.AddCommand(listEventsCmd, getEventCmd)

	hooksCmd.AddCommand(hooksRulesCmd, hooksEventsCmd)
	rootCmd.AddCommand(hooksCmd)
}
