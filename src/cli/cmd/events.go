package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Manage event triggers and browse the event log",
	Long: `Events are the platform's reaction plane. Every service emits JSON events; a trigger
whose field filter matches an event dispatches its actions (run a pipeline, open a ticket, ...).

This replaces the former 'armory hooks' commands. A hooks rule was a git-shaped tuple
(source + events + ref_filter); a trigger is a filter over any field of any event, so the
equivalent of "run pipeline X when main is pushed in myorg/myrepo" is:

  armory events triggers create --name ci-main \
    --match type=repo.push \
    --match subject=myorg/myrepo \
    --match data.ref=main \
    --run-pipeline <pipeline-id>`,
}

var eventsTriggersCmd = &cobra.Command{
	Use:   "triggers",
	Short: "Manage event triggers",
}

var eventsLogCmd = &cobra.Command{
	Use:   "log",
	Short: "Browse the event log",
}

// parseMatch turns repeatable --match flags into the trigger's filter document.
//
// Each flag is `field=value` (equality) or `field~value` (glob), and they are ANDed — the
// common case. Anything richer (any/not/regex/set membership) is expressed by passing the
// filter document directly with --match-json, since inventing a shell syntax for arbitrary
// boolean logic would be worse than writing the JSON.
func parseMatch(exprs []string) (map[string]any, error) {
	var leaves []map[string]any
	for _, e := range exprs {
		if field, value, ok := strings.Cut(e, "~"); ok {
			if field == "" {
				return nil, fmt.Errorf("invalid --match %q: empty field", e)
			}
			leaves = append(leaves, map[string]any{"field": field, "op": "glob", "value": value})
			continue
		}
		field, value, ok := strings.Cut(e, "=")
		if !ok || field == "" {
			return nil, fmt.Errorf("invalid --match %q: expected field=value or field~glob", e)
		}
		leaves = append(leaves, map[string]any{"field": field, "op": "eq", "value": value})
	}
	return map[string]any{"all": leaves}, nil
}

// buildActions assembles the action list from the flag set. Only run_pipeline has dedicated
// flags because it is overwhelmingly the common case; --action-json takes any action document.
func buildActions(pipelineID string, inputs []string, actionJSON string) ([]any, error) {
	var actions []any
	if pipelineID != "" {
		inputMap := map[string]any{}
		for _, kv := range inputs {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("invalid --input %q: expected key=value", kv)
			}
			inputMap[k] = v
		}
		actions = append(actions, map[string]any{
			"kind":   "run_pipeline",
			"config": map[string]any{"pipeline_id": pipelineID, "inputs": inputMap},
		})
	}
	if actionJSON != "" {
		var extra []any
		if err := json.Unmarshal([]byte(actionJSON), &extra); err != nil {
			// Also accept a single action object rather than a list.
			var one map[string]any
			if err2 := json.Unmarshal([]byte(actionJSON), &one); err2 != nil {
				return nil, fmt.Errorf("invalid --action-json: %w", err)
			}
			extra = []any{one}
		}
		actions = append(actions, extra...)
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("at least one action is required (--run-pipeline or --action-json)")
	}
	return actions, nil
}

// triggerPayload builds the create/update body shared by both verbs.
func triggerPayload(name string, match []string, matchJSON, pipelineID string, inputs []string, actionJSON string, enabled bool) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("--name is required")
	}
	var matchDoc map[string]any
	if matchJSON != "" {
		if err := json.Unmarshal([]byte(matchJSON), &matchDoc); err != nil {
			return nil, fmt.Errorf("invalid --match-json: %w", err)
		}
	} else {
		var err error
		if matchDoc, err = parseMatch(match); err != nil {
			return nil, err
		}
	}
	actions, err := buildActions(pipelineID, inputs, actionJSON)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"name":    name,
		"match":   matchDoc,
		"actions": actions,
		"enabled": enabled,
	})
}

func init() {
	// ── armory events triggers create ─────────────────────────────────────────

	var (
		name        string
		match       []string
		matchJSON   string
		pipelineID  string
		inputs      []string
		actionJSON  string
		testEventJS string
	)

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create an event trigger",
		Long: `Create a trigger that dispatches actions when an event matches its filter.

  # Run a pipeline on every push to main in one repo:
  armory events triggers create --name ci-main \
    --match type=repo.push --match subject=myorg/myrepo --match data.ref=main \
    --run-pipeline <pipeline-id> --input IMAGE_TAG='{{ data.commit }}'

  # Match a glob:
  armory events triggers create --name ci-release \
    --match type=repo.push --match 'data.ref~release/*' --run-pipeline <pipeline-id>

  # Anything the flags cannot express — any/not/regex/in — as a filter document:
  armory events triggers create --name ci --match-json '{"any":[
      {"field":"type","op":"eq","value":"repo.push"},
      {"field":"type","op":"prefix","value":"repo.pull_request."}]}' \
    --run-pipeline <pipeline-id>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := triggerPayload(name, match, matchJSON, pipelineID, inputs, actionJSON, true)
			if err != nil {
				return err
			}
			return apiCall("POST", "/events/triggers", body)
		},
	}
	createCmd.Flags().StringVar(&name, "name", "", "Trigger name")
	createCmd.Flags().StringArrayVar(&match, "match", nil, "Filter condition field=value (equality) or field~glob (repeatable, ANDed)")
	createCmd.Flags().StringVar(&matchJSON, "match-json", "", "Filter document as JSON (overrides --match)")
	createCmd.Flags().StringVar(&pipelineID, "run-pipeline", "", "Pipeline ID to run when the filter matches")
	createCmd.Flags().StringArrayVarP(&inputs, "input", "i", nil, "Pipeline input key=value; values may template event fields, e.g. '{{ data.ref }}' (repeatable)")
	createCmd.Flags().StringVar(&actionJSON, "action-json", "", "Additional action(s) as a JSON object or array")

	// ── armory events triggers list / get / delete ────────────────────────────

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List event triggers",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/events/triggers", nil) },
	}

	getCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get an event trigger",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/events/triggers/"+args[0], nil)
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an event trigger",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/events/triggers/"+args[0], nil)
		},
	}

	// ── armory events triggers update ─────────────────────────────────────────

	var (
		upName       string
		upMatch      []string
		upMatchJSON  string
		upPipelineID string
		upInputs     []string
		upActionJSON string
		upDisabled   bool
	)

	updateCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update an event trigger",
		Long:  "Replaces name, match, actions and enabled. Fetch with 'get' first if you only mean to change one.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := triggerPayload(upName, upMatch, upMatchJSON, upPipelineID, upInputs, upActionJSON, !upDisabled)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/events/triggers/"+args[0], body)
		},
	}
	updateCmd.Flags().StringVar(&upName, "name", "", "Trigger name")
	updateCmd.Flags().StringArrayVar(&upMatch, "match", nil, "Filter condition field=value or field~glob (repeatable, ANDed)")
	updateCmd.Flags().StringVar(&upMatchJSON, "match-json", "", "Filter document as JSON (overrides --match)")
	updateCmd.Flags().StringVar(&upPipelineID, "run-pipeline", "", "Pipeline ID to run when the filter matches")
	updateCmd.Flags().StringArrayVarP(&upInputs, "input", "i", nil, "Pipeline input key=value (repeatable)")
	updateCmd.Flags().StringVar(&upActionJSON, "action-json", "", "Additional action(s) as a JSON object or array")
	updateCmd.Flags().BoolVar(&upDisabled, "disabled", false, "Leave the trigger disabled")

	// ── armory events triggers test ───────────────────────────────────────────

	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Dry-run a filter against an event without saving anything",
		Long: `Answers "would this fire?".

  armory events triggers test \
    --match type=repo.push --match data.ref=main \
    --event '{"type":"repo.push","source":"git","subject":"myorg/myrepo","data":{"ref":"main"}}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if testEventJS == "" {
				return fmt.Errorf("--event is required")
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(testEventJS), &ev); err != nil {
				return fmt.Errorf("invalid --event: %w", err)
			}
			var matchDoc map[string]any
			if matchJSON != "" {
				if err := json.Unmarshal([]byte(matchJSON), &matchDoc); err != nil {
					return fmt.Errorf("invalid --match-json: %w", err)
				}
			} else {
				var err error
				if matchDoc, err = parseMatch(match); err != nil {
					return err
				}
			}
			body, err := json.Marshal(map[string]any{"match": matchDoc, "event": ev})
			if err != nil {
				return err
			}
			return apiCall("POST", "/events/triggers/test-match", body)
		},
	}
	testCmd.Flags().StringArrayVar(&match, "match", nil, "Filter condition field=value or field~glob (repeatable, ANDed)")
	testCmd.Flags().StringVar(&matchJSON, "match-json", "", "Filter document as JSON (overrides --match)")
	testCmd.Flags().StringVar(&testEventJS, "event", "", "Event envelope as JSON")

	eventsTriggersCmd.AddCommand(createCmd, listCmd, getCmd, updateCmd, deleteCmd, testCmd)

	// ── armory events log list / get ──────────────────────────────────────────

	var logType string

	logListCmd := &cobra.Command{
		Use:   "list",
		Short: "List recent events",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/events/events"
			if logType != "" {
				path += "?type=" + url.QueryEscape(logType)
			}
			return apiCall("GET", path, nil)
		},
	}
	logListCmd.Flags().StringVar(&logType, "type", "", "Filter by event type (e.g. repo.push)")

	logGetCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get one event",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/events/events/"+args[0], nil)
		},
	}

	eventsLogCmd.AddCommand(logListCmd, logGetCmd)

	eventsCmd.AddCommand(eventsTriggersCmd, eventsLogCmd)
	// The events module (command + Events screen) is registered in events_tui.go.
}
