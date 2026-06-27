package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ── Shared types ──────────────────────────────────────────────────────────────

// stepDef mirrors the Step response from the API.
type stepDef struct {
	StepID string `json:"step_id"`
	Name   string `json:"name"`
}

// matrixConfig fans a step out into one execution per value in a list, binding
// ${matrix.<var>} per execution. Mutually exclusive with parallel_group.
type matrixConfig struct {
	Var        string   `json:"var"`
	Values     []string `json:"values,omitempty"`
	ValuesFrom string   `json:"values_from,omitempty"`
}

// approvalGate is an inline manual-approval pause on a pipeline step ref — no
// separate step is needed. A ref carries either a step_id or an approval gate.
type approvalGate struct {
	Message   string   `json:"message,omitempty"`
	Approvers []string `json:"approvers,omitempty"`
}

// workflowStepRef is the per-step payload inside a create/update workflow request.
type workflowStepRef struct {
	StepID        string        `json:"step_id,omitempty"`
	ParallelGroup *int          `json:"parallel_group,omitempty"`
	Matrix        *matrixConfig `json:"matrix,omitempty"`
	Approval      *approvalGate `json:"approval,omitempty"`
}

// pipelineFile is the JSON file format for -f pipeline creation.
type pipelineFile struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Steps       []workflowStepRef `json:"steps,omitempty"`
}

// ── DSL parser ────────────────────────────────────────────────────────────────

type dslNode struct {
	names      []string
	groupIndex int // unique across parallel groups in this DSL
}

// parseDSL tokenises a pipeline DSL string into ordered nodes.
// Grammar: steps are separated by "->"; a parallel group is written as
// "[step1,step2,...]" and results in a single node with multiple names.
// Examples:
//
//	"build->test->deploy"          — three sequential steps
//	"build->[lint,test]->deploy"   — lint and test run in parallel
func parseDSL(dsl string) ([]dslNode, error) {
	segments := strings.Split(dsl, "->")
	var nodes []dslNode
	groupIdx := 0
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, fmt.Errorf("empty segment in pipeline DSL")
		}
		if strings.HasPrefix(seg, "[") {
			if !strings.HasSuffix(seg, "]") {
				return nil, fmt.Errorf("unclosed '[' in pipeline DSL: %q", seg)
			}
			parts := strings.Split(seg[1:len(seg)-1], ",")
			if len(parts) < 2 {
				return nil, fmt.Errorf("parallel group must contain at least two steps: %q", seg)
			}
			var names []string
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					return nil, fmt.Errorf("empty step name in parallel group %q", seg)
				}
				names = append(names, p)
			}
			nodes = append(nodes, dslNode{names: names, groupIndex: groupIdx})
			groupIdx++
		} else {
			nodes = append(nodes, dslNode{names: []string{seg}})
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("pipeline DSL produced no steps")
	}
	return nodes, nil
}

// resolveStepName looks up a step by name and returns its ID.
func resolveStepName(name string) (string, error) {
	data, err := doRequest("GET", "/workflows/steps?name="+url.QueryEscape(name), nil)
	if err != nil {
		return "", fmt.Errorf("looking up step %q: %w", name, err)
	}
	var steps []stepDef
	if err := json.Unmarshal(data, &steps); err != nil {
		return "", fmt.Errorf("parsing step response for %q: %w", name, err)
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("step %q not found — create it first:\n  armory pipelines create step %s --service <svc> --path <path>", name, name)
	}
	return steps[0].StepID, nil
}

// dslToRefs resolves DSL nodes to workflow step refs (step IDs + parallel groups).
func dslToRefs(nodes []dslNode) ([]workflowStepRef, error) {
	var refs []workflowStepRef
	for _, node := range nodes {
		for _, name := range node.names {
			id, err := resolveStepName(name)
			if err != nil {
				return nil, err
			}
			ref := workflowStepRef{StepID: id}
			if len(node.names) > 1 {
				g := node.groupIndex
				ref.ParallelGroup = &g
			}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// ── Command tree ──────────────────────────────────────────────────────────────

var ciCmd = &cobra.Command{
	Use:   "pipelines",
	Short: "Manage pipelines, steps and runs",
}

var ciCreateCmd = &cobra.Command{Use: "create", Short: "Create a pipeline resource"}
var ciListCmd = &cobra.Command{Use: "list", Short: "List pipeline resources"}
var ciGetCmd = &cobra.Command{Use: "get", Short: "Get a pipeline resource"}
var ciUpdateCmd = &cobra.Command{Use: "update", Short: "Update a pipeline resource"}
var ciDeleteCmd = &cobra.Command{Use: "delete", Short: "Delete a pipeline resource"}
var ciRunCmd = &cobra.Command{Use: "run", Short: "Trigger a pipeline run"}
var ciCancelCmd = &cobra.Command{Use: "cancel", Short: "Cancel a pipeline run"}
var ciApproveCmd = &cobra.Command{Use: "approve", Short: "Approve a pipeline run paused on a manual gate"}
var ciRejectCmd = &cobra.Command{Use: "reject", Short: "Reject a pipeline run paused on a manual gate"}

func init() {
	// ── armory pipelines create step ─────────────────────────────────────────────────

	var (
		stepFile        string
		stepAction      string
		stepWith        string
		stepImage       string
		stepRun         string
		stepRunnerClass string
		stepEnvs        []string
		stepDescription string
		stepTimeout     int64
	)

	createStepCmd := &cobra.Command{
		Use:   "step <name>",
		Short: "Create a reusable pipeline step",
		Long: `Create a reusable step definition that can be composed into pipelines.

Use "armory pipelines list actions" to see all registered catalog actions.

  # Run a shell command inside a container via forge (convenience flags):
  armory pipelines create step unit_tests \
    --action forge/run \
    --image ubuntu:22.04 \
    --run "go test ./..." \
    --timeout 300

  # Catalog action with explicit --with JSON:
  armory pipelines create step open_ticket \
    --action tickets/create \
    --with '{"title":"Build failed: ${HOOK_REPO}","priority":"high"}'

  # Raw HTTP call (escape hatch — calls any registered service):
  armory pipelines create step notify \
    --action http \
    --with '{"service":"conductor","path":"/webhook","method":"POST"}'

  # From a JSON file (-f):
  armory pipelines create step my_step -f step.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var payload map[string]any

			if stepFile != "" {
				data, err := os.ReadFile(stepFile)
				if err != nil {
					return fmt.Errorf("reading step file: %w", err)
				}
				if err := json.Unmarshal(data, &payload); err != nil {
					return fmt.Errorf("parsing step file: %w", err)
				}
				if _, ok := payload["name"]; !ok {
					payload["name"] = name
				}
			} else {
				if stepAction == "" {
					return fmt.Errorf("--action is required (run \"armory pipelines list actions\" to see available actions)")
				}
				with, err := buildWith(stepAction, stepWith, stepImage, stepRun, stepRunnerClass, stepEnvs)
				if err != nil {
					return err
				}
				payload = map[string]any{
					"name":        name,
					"description": stepDescription,
					"action":      stepAction,
					"with":        with,
					"timeout":     stepTimeout,
				}
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/workflows/steps", body)
		},
	}
	createStepCmd.Flags().StringVarP(&stepFile, "file", "f", "", "JSON step definition file")
	createStepCmd.Flags().StringVar(&stepAction, "action", "", "Action name (run \"armory pipelines list actions\" to see available actions)")
	createStepCmd.Flags().StringVar(&stepWith, "with", "", "Step configuration as a JSON object")
	createStepCmd.Flags().StringVar(&stepImage, "image", "", "Container image (forge/run convenience flag, e.g. ubuntu:22.04)")
	createStepCmd.Flags().StringVar(&stepRun, "run", "", "Shell command to run (forge/run convenience flag)")
	createStepCmd.Flags().StringVar(&stepRunnerClass, "runner-class", "", "Forge runner class (forge/run convenience flag, e.g. large; defaults to standard)")
	createStepCmd.Flags().StringArrayVar(&stepEnvs, "env", nil, "Env var KEY=VALUE (forge/run convenience flag, repeatable)")
	createStepCmd.Flags().StringVar(&stepDescription, "description", "", "Human-readable description")
	createStepCmd.Flags().Int64Var(&stepTimeout, "timeout", 30, "Step timeout in seconds")
	ciCreateCmd.AddCommand(createStepCmd)

	// ── armory pipelines list steps ──────────────────────────────────────────────────

	ciListCmd.AddCommand(&cobra.Command{
		Use:   "steps",
		Short: "List reusable steps",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/workflows/steps", nil) },
	})

	// ── armory pipelines get step ────────────────────────────────────────────────────

	ciGetCmd.AddCommand(&cobra.Command{
		Use:   "step <id>",
		Short: "Get a step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/workflows/steps/"+args[0], nil)
		},
	})

	// ── armory pipelines update step ─────────────────────────────────────────────────

	var (
		updateStepFile        string
		updateStepAction      string
		updateStepWith        string
		updateStepImage       string
		updateStepRun         string
		updateStepRunnerClass string
		updateStepEnvs        []string
		updateStepDescription string
		updateStepTimeout     int64
	)

	updateStepCmd := &cobra.Command{
		Use:   "step <id>",
		Short: "Update a step (affects all pipelines that reference it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var payload map[string]any

			if updateStepFile != "" {
				data, err := os.ReadFile(updateStepFile)
				if err != nil {
					return fmt.Errorf("reading step file: %w", err)
				}
				if err := json.Unmarshal(data, &payload); err != nil {
					return fmt.Errorf("parsing step file: %w", err)
				}
			} else {
				if updateStepAction == "" {
					return fmt.Errorf("--action is required (or use -f <file>)")
				}
				with, err := buildWith(updateStepAction, updateStepWith, updateStepImage, updateStepRun, updateStepRunnerClass, updateStepEnvs)
				if err != nil {
					return err
				}
				payload = map[string]any{
					"description": updateStepDescription,
					"action":      updateStepAction,
					"with":        with,
					"timeout":     updateStepTimeout,
				}
			}

			// PUT requires name; fetch current step name if not in payload.
			if _, ok := payload["name"]; !ok {
				data, err := doRequest("GET", "/workflows/steps/"+args[0], nil)
				if err != nil {
					return fmt.Errorf("fetching current step: %w", err)
				}
				var current struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(data, &current); err != nil || current.Name == "" {
					return fmt.Errorf("could not determine step name; pass it in -f")
				}
				payload["name"] = current.Name
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/workflows/steps/"+args[0], body)
		},
	}
	updateStepCmd.Flags().StringVarP(&updateStepFile, "file", "f", "", "JSON step definition file")
	updateStepCmd.Flags().StringVar(&updateStepAction, "action", "", "Action name (run \"armory pipelines list actions\" to see available actions)")
	updateStepCmd.Flags().StringVar(&updateStepWith, "with", "", "Step configuration as a JSON object")
	updateStepCmd.Flags().StringVar(&updateStepImage, "image", "", "Container image (forge/run convenience flag)")
	updateStepCmd.Flags().StringVar(&updateStepRun, "run", "", "Shell command to run (forge/run convenience flag)")
	updateStepCmd.Flags().StringVar(&updateStepRunnerClass, "runner-class", "", "Forge runner class (forge/run convenience flag, e.g. large; defaults to standard)")
	updateStepCmd.Flags().StringArrayVar(&updateStepEnvs, "env", nil, "Env var KEY=VALUE (forge/run convenience flag, repeatable)")
	updateStepCmd.Flags().StringVar(&updateStepDescription, "description", "", "Human-readable description")
	updateStepCmd.Flags().Int64Var(&updateStepTimeout, "timeout", 30, "Step timeout in seconds")
	ciUpdateCmd.AddCommand(updateStepCmd)

	// ── armory pipelines delete step ─────────────────────────────────────────────────

	ciDeleteCmd.AddCommand(&cobra.Command{
		Use:   "step <id>",
		Short: "Delete a step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/workflows/steps/"+args[0], nil)
		},
	})

	// ── armory pipelines create pipeline ─────────────────────────────────────────────

	var pipelineFileFlag string

	createPipelineCmd := &cobra.Command{
		Use:   "pipeline <repo> <branch> [dsl]",
		Short: "Create a CI/CD pipeline",
		Long: `Create a pipeline by composing existing steps.

DSL syntax — step names must match previously-created steps:
  armory pipelines create pipeline myrepo main push->unit_tests->deploy
  armory pipelines create pipeline myrepo main "push->[unit_tests,security_scan]->deploy"

JSON file (-f) — uses step IDs directly:
  {
    "name": "optional name",
    "steps": [
      {"step_id": "<id>"},
      {"step_id": "<id>", "parallel_group": 0},
      {"step_id": "<id>", "parallel_group": 0},
      {"step_id": "<id>"}
    ]
  }`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("requires <repo> and <branch>")
			}
			if pipelineFileFlag == "" && len(args) < 3 {
				return fmt.Errorf("requires a DSL string or -f <file>")
			}
			if pipelineFileFlag != "" && len(args) > 2 {
				return fmt.Errorf("cannot use both DSL string and -f")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, branch := args[0], args[1]

			var payload struct {
				Name        string            `json:"name"`
				Description string            `json:"description"`
				Project     string            `json:"project,omitempty"`
				Steps       []workflowStepRef `json:"steps,omitempty"`
			}

			if pipelineFileFlag != "" {
				data, err := os.ReadFile(pipelineFileFlag)
				if err != nil {
					return fmt.Errorf("reading pipeline file: %w", err)
				}
				var pf pipelineFile
				if err := json.Unmarshal(data, &pf); err != nil {
					return fmt.Errorf("parsing pipeline file: %w", err)
				}
				payload.Name = pf.Name
				payload.Description = pf.Description
				payload.Steps = pf.Steps
			} else {
				nodes, err := parseDSL(args[2])
				if err != nil {
					return err
				}
				refs, err := dslToRefs(nodes)
				if err != nil {
					return err
				}
				payload.Steps = refs
			}

			if payload.Name == "" {
				payload.Name = repo + "/" + branch
			}
			if payload.Description == "" {
				payload.Description = "Pipeline for " + repo + " on " + branch
			}
			// Tag the new pipeline with the project the user is working in, unless
			// overridden by --project or suppressed by --all.
			payload.Project = projectFilter()

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/workflows/pipelines", body)
		},
	}
	createPipelineCmd.Flags().StringVarP(&pipelineFileFlag, "file", "f", "", "JSON pipeline definition file")
	ciCreateCmd.AddCommand(createPipelineCmd)

	// ── armory pipelines list pipelines ──────────────────────────────────────────────

	ciListCmd.AddCommand(&cobra.Command{
		Use:   "pipelines",
		Short: "List pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", appendProjectParam("/workflows/pipelines"), nil)
		},
	})

	// ── armory pipelines get pipeline ────────────────────────────────────────────────

	ciGetCmd.AddCommand(&cobra.Command{
		Use:   "pipeline <id>",
		Short: "Get a pipeline with its full step definitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/workflows/pipelines/"+args[0], nil)
		},
	})

	// ── armory pipelines delete pipeline ─────────────────────────────────────────────

	ciDeleteCmd.AddCommand(&cobra.Command{
		Use:   "pipeline <id>",
		Short: "Delete a pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/workflows/pipelines/"+args[0], nil)
		},
	})

	// ── armory pipelines run pipeline ────────────────────────────────────────────────

	var runInputs []string
	runPipelineCmd := &cobra.Command{
		Use:   "pipeline <id>",
		Short: "Trigger a pipeline run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs := map[string]string{}
			for _, kv := range runInputs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid input %q: expected key=value", kv)
				}
				inputs[k] = v
			}
			body, err := json.Marshal(map[string]any{"inputs": inputs})
			if err != nil {
				return err
			}
			return apiCall("POST", "/workflows/pipelines/"+args[0]+"/runs", body)
		},
	}
	runPipelineCmd.Flags().StringArrayVarP(&runInputs, "input", "i", nil, "Input variable (key=value, repeatable)")
	ciRunCmd.AddCommand(runPipelineCmd)

	// ── armory pipelines list runs ───────────────────────────────────────────────────

	var listRunsPipeline string
	listRunsCmd := &cobra.Command{
		Use:   "runs",
		Short: "List pipeline runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/workflows/runs"
			if listRunsPipeline != "" {
				path += "?workflow_id=" + url.QueryEscape(listRunsPipeline)
			}
			return apiCall("GET", path, nil)
		},
	}
	listRunsCmd.Flags().StringVar(&listRunsPipeline, "pipeline", "", "Filter by pipeline ID")
	ciListCmd.AddCommand(listRunsCmd)

	// ── armory pipelines get run ─────────────────────────────────────────────────────

	ciGetCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Get a pipeline run with step details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/workflows/runs/"+args[0], nil)
		},
	})

	// ── armory pipelines update pipeline ────────────────────────────────────────────

	var (
		updatePipelineFile string
		updatePipelineName string
		updatePipelineDesc string
	)

	updatePipelineCmd := &cobra.Command{
		Use:   "pipeline <id> [dsl]",
		Short: "Update a pipeline's steps",
		Long: `Replace a pipeline's step list using a DSL string or JSON file.

  armory pipelines update pipeline <id> push->unit_tests->deploy
  armory pipelines update pipeline <id> "push->[unit_tests,security_scan]->deploy"
  armory pipelines update pipeline <id> -f pipeline.json

  # Override the name or description at the same time:
  armory pipelines update pipeline <id> push->deploy --name "slim pipeline"`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("requires <id>")
			}
			if updatePipelineFile == "" && len(args) < 2 {
				return fmt.Errorf("requires a DSL string or -f <file>")
			}
			if updatePipelineFile != "" && len(args) > 1 {
				return fmt.Errorf("cannot use both DSL string and -f")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			var steps []workflowStepRef
			if updatePipelineFile != "" {
				data, err := os.ReadFile(updatePipelineFile)
				if err != nil {
					return fmt.Errorf("reading pipeline file: %w", err)
				}
				var pf pipelineFile
				if err := json.Unmarshal(data, &pf); err != nil {
					return fmt.Errorf("parsing pipeline file: %w", err)
				}
				steps = pf.Steps
				if updatePipelineName == "" {
					updatePipelineName = pf.Name
				}
				if updatePipelineDesc == "" {
					updatePipelineDesc = pf.Description
				}
			} else {
				nodes, err := parseDSL(args[1])
				if err != nil {
					return err
				}
				steps, err = dslToRefs(nodes)
				if err != nil {
					return err
				}
			}

			// Name is required by the API — fetch the current one if not provided.
			if updatePipelineName == "" {
				data, err := doRequest("GET", "/workflows/pipelines/"+id, nil)
				if err != nil {
					return fmt.Errorf("fetching current pipeline: %w", err)
				}
				var current struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(data, &current); err != nil || current.Name == "" {
					return fmt.Errorf("could not determine pipeline name; pass --name explicitly")
				}
				updatePipelineName = current.Name
			}

			payload := map[string]any{
				"name":  updatePipelineName,
				"steps": steps,
			}
			if updatePipelineDesc != "" {
				payload["description"] = updatePipelineDesc
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/workflows/pipelines/"+id, body)
		},
	}
	updatePipelineCmd.Flags().StringVarP(&updatePipelineFile, "file", "f", "", "JSON pipeline definition file")
	updatePipelineCmd.Flags().StringVar(&updatePipelineName, "name", "", "Pipeline name (fetched automatically if omitted)")
	updatePipelineCmd.Flags().StringVar(&updatePipelineDesc, "description", "", "Pipeline description")
	ciUpdateCmd.AddCommand(updatePipelineCmd)

	// ── armory pipelines cancel run ──────────────────────────────────────────────────

	ciCancelCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Cancel a pending, running, or awaiting-approval pipeline run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/workflows/runs/"+args[0], nil)
		},
	})

	// ── armory pipelines approve/reject run ──────────────────────────────────────────

	ciApproveCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Approve a run paused on a manual-approval gate, resuming it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("POST", "/workflows/runs/"+args[0]+"/approve", nil)
		},
	})
	ciRejectCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Reject a run paused on a manual-approval gate, failing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("POST", "/workflows/runs/"+args[0]+"/reject", nil)
		},
	})

	// ── armory pipelines list actions ────────────────────────────────────────────────

	ciListCmd.AddCommand(&cobra.Command{
		Use:   "actions",
		Short: "List available workflow actions from the service catalog",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/workflows/actions", nil) },
	})

	// ── armory pipelines test step ───────────────────────────────────────────────────

	ciTestCmd := &cobra.Command{Use: "test", Short: "Test workflow building blocks in isolation"}
	var testStepInputs []string
	testStepCmd := &cobra.Command{
		Use:   "step <id>",
		Short: "Run a single step in isolation (throwaway pipeline) to test it before composing",
		Long: `Run one step by itself to test it before adding it to a pipeline.

The step runs through a throwaway single-step pipeline that is deleted afterward.
Supply a value for each ${...} reference the step uses with --input:

  armory pipelines test step <id> \
    --input inputs.ENV=staging \
    --input steps.build.output='build ok'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := doRequest("GET", "/workflows/steps/"+args[0], nil)
			if err != nil {
				return fmt.Errorf("fetching step: %w", err)
			}
			var step struct {
				Name    string         `json:"name"`
				Action  string         `json:"action"`
				With    map[string]any `json:"with"`
				Timeout int64          `json:"timeout"`
			}
			if err := json.Unmarshal(data, &step); err != nil {
				return fmt.Errorf("parsing step: %w", err)
			}

			vals := map[string]string{}
			for _, kv := range testStepInputs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --input %q: expected ref=value", kv)
				}
				vals[k] = v
			}
			for _, ref := range stepWithRefs(step.With) {
				if _, ok := vals[ref]; !ok {
					fmt.Printf("note: no value supplied for ${%s}; leaving it unresolved\n", ref)
				}
			}

			outcome, err := runStepTest(step.Name, step.Action, resolveStepWith(step.With, vals), step.Timeout)
			if err != nil {
				return err
			}
			fmt.Printf("status: %s\n", outcome.status)
			if out := strings.TrimSpace(outcome.output); out != "" {
				fmt.Printf("\noutput:\n%s\n", out)
			}
			if outcome.status != "completed" {
				return fmt.Errorf("step test did not complete (status: %s)", outcome.status)
			}
			return nil
		},
	}
	testStepCmd.Flags().StringArrayVar(&testStepInputs, "input", nil, "Test value for a ${ref}, as ref=value (repeatable)")
	ciTestCmd.AddCommand(testStepCmd)

	ciCmd.AddCommand(ciCreateCmd, ciListCmd, ciGetCmd, ciUpdateCmd, ciDeleteCmd, ciRunCmd, ciCancelCmd, ciApproveCmd, ciRejectCmd, ciTestCmd, ciTUICmd, stepsTUICmd)
	// The workflows module (ci command + the CI/Pipelines and Steps home
	// screens) is registered in ci_tui.go.
}

// buildWith constructs the step With map from CLI flags.
// For forge/run, --image, --run, --env, and --runner-class are convenience flags
// merged into any --with JSON base. For all other actions, --with JSON is required.
func buildWith(action, withJSON, image, run, runnerClass string, envs []string) (map[string]any, error) {
	with := map[string]any{}

	if withJSON != "" {
		if err := json.Unmarshal([]byte(withJSON), &with); err != nil {
			return nil, fmt.Errorf("--with is not valid JSON: %w", err)
		}
	}

	if action == "forge/run" {
		if image != "" {
			with["image"] = image
		}
		if run != "" {
			with["run"] = run
		}
		if runnerClass != "" {
			with["runner_class"] = runnerClass
		}
		if len(envs) > 0 {
			env := map[string]string{}
			for _, kv := range envs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return nil, fmt.Errorf("invalid --env %q: expected KEY=VALUE", kv)
				}
				env[k] = v
			}
			with["env"] = env
		}
		if _, ok := with["image"]; !ok {
			return nil, fmt.Errorf("forge/run requires --image or \"image\" in --with JSON")
		}
		if _, hasRun := with["run"]; !hasRun {
			if _, hasCmd := with["command"]; !hasCmd {
				return nil, fmt.Errorf("forge/run requires --run or \"run\"/\"command\" in --with JSON")
			}
		}
		return with, nil
	}

	if withJSON == "" {
		return nil, fmt.Errorf("--with <json> is required for action %q", action)
	}
	return with, nil
}
